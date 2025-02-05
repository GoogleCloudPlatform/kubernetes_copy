/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cpumanager

import (
	"fmt"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	pkgfeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/utils/cpuset"
)

type staticPolicyTest struct {
	description     string
	topo            *topology.CPUTopology
	numReservedCPUs int
	reservedCPUs    *cpuset.CPUSet
	podUID          string
	options         map[string]string
	containerName   string
	stAssignments   state.ContainerCPUAssignments
	stDefaultCPUSet cpuset.CPUSet
	pod             *v1.Pod
	topologyHint    *topologymanager.TopologyHint
	expErr          error
	expCPUAlloc     bool
	expCSet         cpuset.CPUSet
}

// this is not a real Clone() - hence Pseudo- - because we don't clone some
// objects which are accessed read-only
func (spt staticPolicyTest) PseudoClone() staticPolicyTest {
	return staticPolicyTest{
		description:     spt.description,
		topo:            spt.topo, // accessed in read-only
		numReservedCPUs: spt.numReservedCPUs,
		podUID:          spt.podUID,
		options:         spt.options, // accessed in read-only
		containerName:   spt.containerName,
		stAssignments:   spt.stAssignments.Clone(),
		stDefaultCPUSet: spt.stDefaultCPUSet.Clone(),
		pod:             spt.pod, // accessed in read-only
		expErr:          spt.expErr,
		expCPUAlloc:     spt.expCPUAlloc,
		expCSet:         spt.expCSet.Clone(),
	}
}

func TestStaticPolicyName(t *testing.T) {
	policy, _ := NewStaticPolicy(topoSingleSocketHT, 1, cpuset.New(), topologymanager.NewFakeManager(), nil)

	policyName := policy.Name()
	if policyName != "static" {
		t.Errorf("StaticPolicy Name() error. expected: static, returned: %v",
			policyName)
	}
}

func TestStaticPolicyStart(t *testing.T) {
	testCases := []staticPolicyTest{
		{
			description: "non-corrupted state",
			topo:        topoDualSocketHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"0": cpuset.New(0),
				},
			},
			stDefaultCPUSet: cpuset.New(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expCSet:         cpuset.New(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
		},
		{
			description:     "empty cpuset",
			topo:            topoDualSocketHT,
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(),
			expCSet:         cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
		},
		{
			description:     "reserved cores 0 & 6 are not present in available cpuset",
			topo:            topoDualSocketHT,
			numReservedCPUs: 2,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1),
			expErr:          fmt.Errorf("not all reserved cpus: \"0,6\" are present in defaultCpuSet: \"0-1\""),
		},
		{
			description:     "some of reserved cores are present in available cpuset (StrictCPUReservationOption)",
			topo:            topoDualSocketHT,
			numReservedCPUs: 2,
			options:         map[string]string{StrictCPUReservationOption: "true"},
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1),
			expErr:          fmt.Errorf("some of strictly reserved cpus: \"0\" are present in defaultCpuSet: \"0-1\""),
		},
		{
			description: "assigned core 2 is still present in available cpuset",
			topo:        topoDualSocketHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"0": cpuset.New(0, 1, 2),
				},
			},
			stDefaultCPUSet: cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expErr:          fmt.Errorf("pod: fakePod, container: 0 cpuset: \"0-2\" overlaps with default cpuset \"2-11\""),
		},
		{
			description: "assigned core 2 is still present in available cpuset (StrictCPUReservationOption)",
			topo:        topoDualSocketHT,
			options:     map[string]string{StrictCPUReservationOption: "true"},
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"0": cpuset.New(0, 1, 2),
				},
			},
			stDefaultCPUSet: cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expErr:          fmt.Errorf("pod: fakePod, container: 0 cpuset: \"0-2\" overlaps with default cpuset \"2-11\""),
		},
		{
			description: "core 12 is not present in topology but is in state cpuset",
			topo:        topoDualSocketHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"0": cpuset.New(0, 1, 2),
					"1": cpuset.New(3, 4),
				},
			},
			stDefaultCPUSet: cpuset.New(5, 6, 7, 8, 9, 10, 11, 12),
			expErr:          fmt.Errorf("current set of available CPUs \"0-11\" doesn't match with CPUs in state \"0-12\""),
		},
		{
			description: "core 11 is present in topology but is not in state cpuset",
			topo:        topoDualSocketHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"0": cpuset.New(0, 1, 2),
					"1": cpuset.New(3, 4),
				},
			},
			stDefaultCPUSet: cpuset.New(5, 6, 7, 8, 9, 10),
			expErr:          fmt.Errorf("current set of available CPUs \"0-11\" doesn't match with CPUs in state \"0-10\""),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.CPUManagerPolicyAlphaOptions, true)
			p, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpuset.New(), topologymanager.NewFakeManager(), testCase.options)
			policy := p.(*staticPolicy)
			st := &mockState{
				assignments:   testCase.stAssignments,
				defaultCPUSet: testCase.stDefaultCPUSet,
			}
			err := policy.Start(st)
			if !reflect.DeepEqual(err, testCase.expErr) {
				t.Errorf("StaticPolicy Start() error (%v). expected error: %v but got: %v",
					testCase.description, testCase.expErr, err)
			}
			if err != nil {
				return
			}

			if !testCase.stDefaultCPUSet.IsEmpty() {
				for cpuid := 1; cpuid < policy.topology.NumCPUs; cpuid++ {
					if !st.defaultCPUSet.Contains(cpuid) {
						t.Errorf("StaticPolicy Start() error. expected cpuid %d to be present in defaultCPUSet", cpuid)
					}
				}
			}
			if !st.GetDefaultCPUSet().Equals(testCase.expCSet) {
				t.Errorf("State CPUSet is different than expected. Have %q wants: %q", st.GetDefaultCPUSet(),
					testCase.expCSet)
			}

		})
	}
}

func TestStaticPolicyAdd(t *testing.T) {
	var largeTopoCPUids []int
	var largeTopoSock0CPUids []int
	var largeTopoSock1CPUids []int
	largeTopo := *topoQuadSocketFourWayHT
	for cpuid, val := range largeTopo.CPUDetails {
		largeTopoCPUids = append(largeTopoCPUids, cpuid)
		if val.SocketID == 0 {
			largeTopoSock0CPUids = append(largeTopoSock0CPUids, cpuid)
		} else if val.SocketID == 1 {
			largeTopoSock1CPUids = append(largeTopoSock1CPUids, cpuid)
		}
	}
	largeTopoCPUSet := cpuset.New(largeTopoCPUids...)
	largeTopoSock0CPUSet := cpuset.New(largeTopoSock0CPUids...)
	largeTopoSock1CPUSet := cpuset.New(largeTopoSock1CPUids...)

	// these are the cases which must behave the same regardless the policy options.
	// So we will permutate the options to ensure this holds true.

	optionsInsensitiveTestCases := []staticPolicyTest{
		{
			description:     "GuPodMultipleCores, SingleSocketHT, ExpectAllocOneCore",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(2, 3, 6, 7),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 4, 5),
			pod:             makePod("fakePod", "fakeContainer3", "2000m", "2000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 5),
		},
		{
			description:     "GuPodMultipleCores, DualSocketHT, ExpectAllocOneSocket",
			topo:            topoDualSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(2),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainer3", "6000m", "6000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 3, 5, 7, 9, 11),
		},
		{
			description:     "GuPodMultipleCores, DualSocketHT, ExpectAllocThreeCores",
			topo:            topoDualSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(1, 5),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 2, 3, 4, 6, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainer3", "6000m", "6000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(2, 3, 4, 8, 9, 10),
		},
		{
			description:     "GuPodMultipleCores, DualSocketNoHT, ExpectAllocOneSocket",
			topo:            topoDualSocketNoHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer1", "4000m", "4000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(4, 5, 6, 7),
		},
		{
			description:     "GuPodMultipleCores, DualSocketNoHT, ExpectAllocFourCores",
			topo:            topoDualSocketNoHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(4, 5),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 3, 6, 7),
			pod:             makePod("fakePod", "fakeContainer1", "4000m", "4000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 3, 6, 7),
		},
		{
			description:     "GuPodMultipleCores, DualSocketHT, ExpectAllocOneSocketOneCore",
			topo:            topoDualSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(2),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainer3", "8000m", "8000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 3, 4, 5, 7, 9, 10, 11),
		},
		{
			description:     "NonGuPod, SingleSocketHT, NoAlloc",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer1", "1000m", "2000m"),
			expErr:          nil,
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			description:     "GuPodNonIntegerCore, SingleSocketHT, NoAlloc",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer4", "977m", "977m"),
			expErr:          nil,
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			// All the CPUs from Socket 0 are available. Some CPUs from each
			// Socket have been already assigned.
			// Expect all CPUs from Socket 0.
			description: "GuPodMultipleCores, topoQuadSocketFourWayHT, ExpectAllocSock0",
			topo:        topoQuadSocketFourWayHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(3, 11, 4, 5, 6, 7),
				},
			},
			stDefaultCPUSet: largeTopoCPUSet.Difference(cpuset.New(3, 11, 4, 5, 6, 7)),
			pod:             makePod("fakePod", "fakeContainer5", "72000m", "72000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         largeTopoSock0CPUSet,
		},
		{
			// Only 2 full cores from three Sockets and some partial cores are available.
			// Expect CPUs from the 2 full cores available from the three Sockets.
			description: "GuPodMultipleCores, topoQuadSocketFourWayHT, ExpectAllocAllFullCoresFromThreeSockets",
			topo:        topoQuadSocketFourWayHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": largeTopoCPUSet.Difference(cpuset.New(1, 25, 13, 38, 2, 9, 11, 35, 23, 48, 12, 51,
						53, 173, 113, 233, 54, 61)),
				},
			},
			stDefaultCPUSet: cpuset.New(1, 25, 13, 38, 2, 9, 11, 35, 23, 48, 12, 51, 53, 173, 113, 233, 54, 61),
			pod:             makePod("fakePod", "fakeCcontainer5", "12000m", "12000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 25, 13, 38, 11, 35, 23, 48, 53, 173, 113, 233),
		},
		{
			// All CPUs from Socket 1, 1 full core and some partial cores are available.
			// Expect all CPUs from Socket 1 and the hyper-threads from the full core.
			description: "GuPodMultipleCores, topoQuadSocketFourWayHT, ExpectAllocAllSock1+FullCore",
			topo:        topoQuadSocketFourWayHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": largeTopoCPUSet.Difference(largeTopoSock1CPUSet.Union(cpuset.New(10, 34, 22, 47, 53,
						173, 61, 181, 108, 228, 115, 235))),
				},
			},
			stDefaultCPUSet: largeTopoSock1CPUSet.Union(cpuset.New(10, 34, 22, 47, 53, 173, 61, 181, 108, 228,
				115, 235)),
			pod:         makePod("fakePod", "fakeContainer5", "76000m", "76000m"),
			expErr:      nil,
			expCPUAlloc: true,
			expCSet:     largeTopoSock1CPUSet.Union(cpuset.New(10, 34, 22, 47)),
		},
	}

	// testcases for the default behaviour of the policy.
	defaultOptionsTestCases := []staticPolicyTest{
		{
			description:     "GuPodSingleCore, SingleSocketHT, ExpectAllocOneCPU",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer2", "1000m", "1000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(4), // expect sibling of partial core
		},
		{
			// Only partial cores are available in the entire system.
			// Expect allocation of all the CPUs from the partial cores.
			description: "GuPodMultipleCores, topoQuadSocketFourWayHT, ExpectAllocCPUs",
			topo:        topoQuadSocketFourWayHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": largeTopoCPUSet.Difference(cpuset.New(10, 11, 53, 37, 55, 67, 52)),
				},
			},
			stDefaultCPUSet: cpuset.New(10, 11, 53, 67, 52),
			pod:             makePod("fakePod", "fakeContainer5", "5000m", "5000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(10, 11, 53, 67, 52),
		},
		{
			description:     "GuPodSingleCore, SingleSocketHT, ExpectError",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer2", "8000m", "8000m"),
			expErr:          fmt.Errorf("not enough cpus available to satisfy request: requested=8, available=7"),
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			description:     "GuPodMultipleCores, SingleSocketHT, ExpectSameAllocation",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer3": cpuset.New(2, 3, 6, 7),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 4, 5),
			pod:             makePod("fakePod", "fakeContainer3", "4000m", "4000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(2, 3, 6, 7),
		},
		{
			description:     "GuPodMultipleCores, DualSocketHT, NoAllocExpectError",
			topo:            topoDualSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(1, 2, 3),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 4, 5, 6, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainer5", "10000m", "10000m"),
			expErr:          fmt.Errorf("not enough cpus available to satisfy request: requested=10, available=8"),
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			description:     "GuPodMultipleCores, SingleSocketHT, NoAllocExpectError",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(1, 2, 3, 4, 5, 6),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 7),
			pod:             makePod("fakePod", "fakeContainer5", "2000m", "2000m"),
			expErr:          fmt.Errorf("not enough cpus available to satisfy request: requested=2, available=1"),
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			// Only 7 CPUs are available.
			// Pod requests 76 cores.
			// Error is expected since available CPUs are less than the request.
			description: "GuPodMultipleCores, topoQuadSocketFourWayHT, NoAlloc",
			topo:        topoQuadSocketFourWayHT,
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": largeTopoCPUSet.Difference(cpuset.New(10, 11, 53, 37, 55, 67, 52)),
				},
			},
			stDefaultCPUSet: cpuset.New(10, 11, 53, 37, 55, 67, 52),
			pod:             makePod("fakePod", "fakeContainer5", "76000m", "76000m"),
			expErr:          fmt.Errorf("not enough cpus available to satisfy request: requested=76, available=7"),
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
	}

	// testcases for the FullPCPUsOnlyOption
	smtalignOptionTestCases := []staticPolicyTest{
		{
			description: "GuPodSingleCore, SingleSocketHT, ExpectAllocOneCPU",
			topo:        topoSingleSocketHT,
			options: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer2", "1000m", "1000m"),
			expErr:          SMTAlignmentError{RequestedCPUs: 1, CpusPerCore: 2},
			expCPUAlloc:     false,
			expCSet:         cpuset.New(), // reject allocation of sibling of partial core
		},
		{
			// test SMT-level != 2 - which is the default on x86_64
			description: "GuPodMultipleCores, topoQuadSocketFourWayHT, ExpectAllocOneCPUs",
			topo:        topoQuadSocketFourWayHT,
			options: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			numReservedCPUs: 8,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: largeTopoCPUSet,
			pod:             makePod("fakePod", "fakeContainer15", "15000m", "15000m"),
			expErr:          SMTAlignmentError{RequestedCPUs: 15, CpusPerCore: 4},
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			description: "GuPodManyCores, topoDualSocketHT, ExpectDoNotAllocPartialCPU",
			topo:        topoDualSocketHT,
			options: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			numReservedCPUs: 2,
			reservedCPUs:    newCPUSetPtr(1, 6),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 2, 3, 4, 5, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainerBug113537_1", "10000m", "10000m"),
			expErr:          SMTAlignmentError{RequestedCPUs: 10, CpusPerCore: 2, AvailablePhysicalCPUs: 8, CausedByPhysicalCPUs: true},
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			description: "GuPodManyCores, topoDualSocketHT, AutoReserve, ExpectAllocAllCPUs",
			topo:        topoDualSocketHT,
			options: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			numReservedCPUs: 2,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(1, 2, 3, 4, 5, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainerBug113537_2", "10000m", "10000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 2, 3, 4, 5, 7, 8, 9, 10, 11),
		},
		{
			description: "GuPodManyCores, topoDualSocketHT, ExpectAllocAllCPUs",
			topo:        topoDualSocketHT,
			options: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			numReservedCPUs: 2,
			reservedCPUs:    newCPUSetPtr(0, 6),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(1, 2, 3, 4, 5, 7, 8, 9, 10, 11),
			pod:             makePod("fakePod", "fakeContainerBug113537_2", "10000m", "10000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(1, 2, 3, 4, 5, 7, 8, 9, 10, 11),
		},
	}
	newNUMAAffinity := func(bits ...int) bitmask.BitMask {
		affinity, _ := bitmask.NewBitMask(bits...)
		return affinity
	}
	alignBySocketOptionTestCases := []staticPolicyTest{
		{
			description: "Align by socket: true, cpu's within same socket of numa in hint are part of allocation",
			topo:        topoDualSocketMultiNumaPerSocketHT,
			options: map[string]string{
				AlignBySocketOption: "true",
			},
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(2, 11, 21, 22),
			pod:             makePod("fakePod", "fakeContainer2", "2000m", "2000m"),
			topologyHint:    &topologymanager.TopologyHint{NUMANodeAffinity: newNUMAAffinity(0, 2), Preferred: true},
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(2, 11),
		},
		{
			description: "Align by socket: false, cpu's are taken strictly from NUMA nodes in hint",
			topo:        topoDualSocketMultiNumaPerSocketHT,
			options: map[string]string{
				AlignBySocketOption: "false",
			},
			numReservedCPUs: 1,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(2, 11, 21, 22),
			pod:             makePod("fakePod", "fakeContainer2", "2000m", "2000m"),
			topologyHint:    &topologymanager.TopologyHint{NUMANodeAffinity: newNUMAAffinity(0, 2), Preferred: true},
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(2, 21),
		},
	}

	for _, testCase := range optionsInsensitiveTestCases {
		for _, options := range []map[string]string{
			nil,
			{
				FullPCPUsOnlyOption: "true",
			},
		} {
			tCase := testCase.PseudoClone()
			tCase.description = fmt.Sprintf("options=%v %s", options, testCase.description)
			tCase.options = options
			runStaticPolicyTestCase(t, tCase)
		}
	}

	for _, testCase := range defaultOptionsTestCases {
		runStaticPolicyTestCase(t, testCase)
	}
	for _, testCase := range smtalignOptionTestCases {
		runStaticPolicyTestCase(t, testCase)
	}
	for _, testCase := range alignBySocketOptionTestCases {
		runStaticPolicyTestCaseWithFeatureGate(t, testCase)
	}
}

func runStaticPolicyTestCase(t *testing.T, testCase staticPolicyTest) {
	tm := topologymanager.NewFakeManager()
	if testCase.topologyHint != nil {
		tm = topologymanager.NewFakeManagerWithHint(testCase.topologyHint)
	}
	cpus := cpuset.New()
	if testCase.reservedCPUs != nil {
		cpus = testCase.reservedCPUs.Clone()
	}
	policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpus, tm, testCase.options)

	st := &mockState{
		assignments:   testCase.stAssignments,
		defaultCPUSet: testCase.stDefaultCPUSet,
	}

	container := &testCase.pod.Spec.Containers[0]
	err := policy.Allocate(st, testCase.pod, container)
	if !reflect.DeepEqual(err, testCase.expErr) {
		t.Errorf("StaticPolicy Allocate() error (%v). expected add error: %q but got: %q",
			testCase.description, testCase.expErr, err)
	}

	if testCase.expCPUAlloc {
		cset, found := st.assignments[string(testCase.pod.UID)][container.Name]
		if !found {
			t.Errorf("StaticPolicy Allocate() error (%v). expected container %v to be present in assignments %v",
				testCase.description, container.Name, st.assignments)
		}

		if !reflect.DeepEqual(cset, testCase.expCSet) {
			t.Errorf("StaticPolicy Allocate() error (%v). expected cpuset %v but got %v",
				testCase.description, testCase.expCSet, cset)
		}

		if !cset.Intersection(st.defaultCPUSet).IsEmpty() {
			t.Errorf("StaticPolicy Allocate() error (%v). expected cpuset %v to be disoint from the shared cpuset %v",
				testCase.description, cset, st.defaultCPUSet)
		}
	}

	if !testCase.expCPUAlloc {
		_, found := st.assignments[string(testCase.pod.UID)][container.Name]
		if found {
			t.Errorf("StaticPolicy Allocate() error (%v). Did not expect container %v to be present in assignments %v",
				testCase.description, container.Name, st.assignments)
		}
	}
}

func runStaticPolicyTestCaseWithFeatureGate(t *testing.T, testCase staticPolicyTest) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.CPUManagerPolicyAlphaOptions, true)
	runStaticPolicyTestCase(t, testCase)
}

func TestStaticPolicyReuseCPUs(t *testing.T) {
	testCases := []struct {
		staticPolicyTest
		expCSetAfterAlloc  cpuset.CPUSet
		expCSetAfterRemove cpuset.CPUSet
	}{
		{
			staticPolicyTest: staticPolicyTest{
				description: "SingleSocketHT, DeAllocOneInitContainer",
				topo:        topoSingleSocketHT,
				pod: makeMultiContainerPod(
					[]struct{ request, limit string }{
						{"4000m", "4000m"}}, // 0, 1, 4, 5
					[]struct{ request, limit string }{
						{"2000m", "2000m"}}), // 0, 4
				containerName:   "initContainer-0",
				stAssignments:   state.ContainerCPUAssignments{},
				stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			},
			expCSetAfterAlloc:  cpuset.New(2, 3, 6, 7),
			expCSetAfterRemove: cpuset.New(1, 2, 3, 5, 6, 7),
		},
	}

	for _, testCase := range testCases {
		policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpuset.New(), topologymanager.NewFakeManager(), nil)

		st := &mockState{
			assignments:   testCase.stAssignments,
			defaultCPUSet: testCase.stDefaultCPUSet,
		}
		pod := testCase.pod

		// allocate
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			policy.Allocate(st, pod, &container)
		}
		if !reflect.DeepEqual(st.defaultCPUSet, testCase.expCSetAfterAlloc) {
			t.Errorf("StaticPolicy Allocate() error (%v). expected default cpuset %v but got %v",
				testCase.description, testCase.expCSetAfterAlloc, st.defaultCPUSet)
		}

		// remove
		policy.RemoveContainer(st, string(pod.UID), testCase.containerName)

		if !reflect.DeepEqual(st.defaultCPUSet, testCase.expCSetAfterRemove) {
			t.Errorf("StaticPolicy RemoveContainer() error (%v). expected default cpuset %v but got %v",
				testCase.description, testCase.expCSetAfterRemove, st.defaultCPUSet)
		}
		if _, found := st.assignments[string(pod.UID)][testCase.containerName]; found {
			t.Errorf("StaticPolicy RemoveContainer() error (%v). expected (pod %v, container %v) not be in assignments %v",
				testCase.description, testCase.podUID, testCase.containerName, st.assignments)
		}
	}
}

func TestStaticPolicyDoNotReuseCPUs(t *testing.T) {
	testCases := []struct {
		staticPolicyTest
		expCSetAfterAlloc cpuset.CPUSet
	}{
		{
			staticPolicyTest: staticPolicyTest{
				description: "SingleSocketHT, Don't reuse CPUs of a restartable init container",
				topo:        topoSingleSocketHT,
				pod: makeMultiContainerPodWithOptions(
					[]*containerOptions{
						{request: "4000m", limit: "4000m", restartPolicy: v1.ContainerRestartPolicyAlways}}, // 0, 1, 4, 5
					[]*containerOptions{
						{request: "2000m", limit: "2000m"}}), // 2, 6
				stAssignments:   state.ContainerCPUAssignments{},
				stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			},
			expCSetAfterAlloc: cpuset.New(3, 7),
		},
	}

	for _, testCase := range testCases {
		policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpuset.New(), topologymanager.NewFakeManager(), nil)

		st := &mockState{
			assignments:   testCase.stAssignments,
			defaultCPUSet: testCase.stDefaultCPUSet,
		}
		pod := testCase.pod

		// allocate
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			err := policy.Allocate(st, pod, &container)
			if err != nil {
				t.Errorf("StaticPolicy Allocate() error (%v). expected no error but got %v",
					testCase.description, err)
			}
		}
		if !reflect.DeepEqual(st.defaultCPUSet, testCase.expCSetAfterAlloc) {
			t.Errorf("StaticPolicy Allocate() error (%v). expected default cpuset %v but got %v",
				testCase.description, testCase.expCSetAfterAlloc, st.defaultCPUSet)
		}
	}
}

func TestStaticPolicyRemove(t *testing.T) {
	testCases := []staticPolicyTest{
		{
			description:   "SingleSocketHT, DeAllocOneContainer",
			topo:          topoSingleSocketHT,
			podUID:        "fakePod",
			containerName: "fakeContainer1",
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer1": cpuset.New(1, 2, 3),
				},
			},
			stDefaultCPUSet: cpuset.New(4, 5, 6, 7),
			expCSet:         cpuset.New(1, 2, 3, 4, 5, 6, 7),
		},
		{
			description:   "SingleSocketHT, DeAllocOneContainer, BeginEmpty",
			topo:          topoSingleSocketHT,
			podUID:        "fakePod",
			containerName: "fakeContainer1",
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer1": cpuset.New(1, 2, 3),
					"fakeContainer2": cpuset.New(4, 5, 6, 7),
				},
			},
			stDefaultCPUSet: cpuset.New(),
			expCSet:         cpuset.New(1, 2, 3),
		},
		{
			description:   "SingleSocketHT, DeAllocTwoContainer",
			topo:          topoSingleSocketHT,
			podUID:        "fakePod",
			containerName: "fakeContainer1",
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer1": cpuset.New(1, 3, 5),
					"fakeContainer2": cpuset.New(2, 4),
				},
			},
			stDefaultCPUSet: cpuset.New(6, 7),
			expCSet:         cpuset.New(1, 3, 5, 6, 7),
		},
		{
			description:   "SingleSocketHT, NoDeAlloc",
			topo:          topoSingleSocketHT,
			podUID:        "fakePod",
			containerName: "fakeContainer2",
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer1": cpuset.New(1, 3, 5),
				},
			},
			stDefaultCPUSet: cpuset.New(2, 4, 6, 7),
			expCSet:         cpuset.New(2, 4, 6, 7),
		},
	}

	for _, testCase := range testCases {
		policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpuset.New(), topologymanager.NewFakeManager(), nil)

		st := &mockState{
			assignments:   testCase.stAssignments,
			defaultCPUSet: testCase.stDefaultCPUSet,
		}

		policy.RemoveContainer(st, testCase.podUID, testCase.containerName)

		if !reflect.DeepEqual(st.defaultCPUSet, testCase.expCSet) {
			t.Errorf("StaticPolicy RemoveContainer() error (%v). expected default cpuset %v but got %v",
				testCase.description, testCase.expCSet, st.defaultCPUSet)
		}

		if _, found := st.assignments[testCase.podUID][testCase.containerName]; found {
			t.Errorf("StaticPolicy RemoveContainer() error (%v). expected (pod %v, container %v) not be in assignments %v",
				testCase.description, testCase.podUID, testCase.containerName, st.assignments)
		}
	}
}

func TestTopologyAwareAllocateCPUs(t *testing.T) {
	testCases := []struct {
		description     string
		topo            *topology.CPUTopology
		stAssignments   state.ContainerCPUAssignments
		stDefaultCPUSet cpuset.CPUSet
		numRequested    int
		socketMask      bitmask.BitMask
		expCSet         cpuset.CPUSet
	}{
		{
			description:     "Request 2 CPUs, No BitMask",
			topo:            topoDualSocketHT,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			numRequested:    2,
			socketMask:      nil,
			expCSet:         cpuset.New(0, 6),
		},
		{
			description:     "Request 2 CPUs, BitMask on Socket 0",
			topo:            topoDualSocketHT,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			numRequested:    2,
			socketMask: func() bitmask.BitMask {
				mask, _ := bitmask.NewBitMask(0)
				return mask
			}(),
			expCSet: cpuset.New(0, 6),
		},
		{
			description:     "Request 2 CPUs, BitMask on Socket 1",
			topo:            topoDualSocketHT,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			numRequested:    2,
			socketMask: func() bitmask.BitMask {
				mask, _ := bitmask.NewBitMask(1)
				return mask
			}(),
			expCSet: cpuset.New(1, 7),
		},
		{
			description:     "Request 8 CPUs, BitMask on Socket 0",
			topo:            topoDualSocketHT,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			numRequested:    8,
			socketMask: func() bitmask.BitMask {
				mask, _ := bitmask.NewBitMask(0)
				return mask
			}(),
			expCSet: cpuset.New(0, 6, 2, 8, 4, 10, 1, 7),
		},
		{
			description:     "Request 8 CPUs, BitMask on Socket 1",
			topo:            topoDualSocketHT,
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			numRequested:    8,
			socketMask: func() bitmask.BitMask {
				mask, _ := bitmask.NewBitMask(1)
				return mask
			}(),
			expCSet: cpuset.New(1, 7, 3, 9, 5, 11, 0, 6),
		},
	}
	for _, tc := range testCases {
		p, _ := NewStaticPolicy(tc.topo, 0, cpuset.New(), topologymanager.NewFakeManager(), nil)
		policy := p.(*staticPolicy)
		st := &mockState{
			assignments:   tc.stAssignments,
			defaultCPUSet: tc.stDefaultCPUSet,
		}
		err := policy.Start(st)
		if err != nil {
			t.Errorf("StaticPolicy Start() error (%v)", err)
			continue
		}

		cset, err := policy.allocateCPUs(st, tc.numRequested, tc.socketMask, cpuset.New())
		if err != nil {
			t.Errorf("StaticPolicy allocateCPUs() error (%v). expected CPUSet %v not error %v",
				tc.description, tc.expCSet, err)
			continue
		}

		if !reflect.DeepEqual(tc.expCSet, cset) {
			t.Errorf("StaticPolicy allocateCPUs() error (%v). expected CPUSet %v but got %v",
				tc.description, tc.expCSet, cset)
		}
	}
}

// above test cases are without kubelet --reserved-cpus cmd option
// the following tests are with --reserved-cpus configured
type staticPolicyTestWithResvList struct {
	description      string
	topo             *topology.CPUTopology
	numReservedCPUs  int
	reserved         cpuset.CPUSet
	cpuPolicyOptions map[string]string
	stAssignments    state.ContainerCPUAssignments
	stDefaultCPUSet  cpuset.CPUSet
	pod              *v1.Pod
	expErr           error
	expNewErr        error
	expCPUAlloc      bool
	expCSet          cpuset.CPUSet
}

func TestStaticPolicyStartWithResvList(t *testing.T) {
	testCases := []staticPolicyTestWithResvList{
		{
			description:     "empty cpuset",
			topo:            topoDualSocketHT,
			numReservedCPUs: 2,
			reserved:        cpuset.New(0, 1),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(),
			expCSet:         cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
		},
		{
			description:      "empty cpuset with StrictCPUReservationOption enabled",
			topo:             topoDualSocketHT,
			numReservedCPUs:  2,
			reserved:         cpuset.New(0, 1),
			cpuPolicyOptions: map[string]string{StrictCPUReservationOption: "true"},
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(),
			expCSet:          cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
		},
		{
			description:     "reserved cores 0 & 1 are not present in available cpuset",
			topo:            topoDualSocketHT,
			numReservedCPUs: 2,
			reserved:        cpuset.New(0, 1),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(2, 3, 4, 5),
			expErr:          fmt.Errorf("not all reserved cpus: \"0-1\" are present in defaultCpuSet: \"2-5\""),
		},
		{
			description:      "reserved cores 0 & 1 are present in available cpuset with StrictCPUReservationOption enabled",
			topo:             topoDualSocketHT,
			numReservedCPUs:  2,
			reserved:         cpuset.New(0, 1),
			cpuPolicyOptions: map[string]string{StrictCPUReservationOption: "true"},
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5),
			expErr:           fmt.Errorf("some of strictly reserved cpus: \"0-1\" are present in defaultCpuSet: \"0-5\""),
		},
		{
			description:     "inconsistency between numReservedCPUs and reserved",
			topo:            topoDualSocketHT,
			numReservedCPUs: 1,
			reserved:        cpuset.New(0, 1),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expNewErr:       fmt.Errorf("[cpumanager] unable to reserve the required amount of CPUs (size of 0-1 did not equal 1)"),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.CPUManagerPolicyAlphaOptions, true)
			p, err := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, testCase.reserved, topologymanager.NewFakeManager(), testCase.cpuPolicyOptions)
			if !reflect.DeepEqual(err, testCase.expNewErr) {
				t.Errorf("StaticPolicy Start() error (%v). expected error: %v but got: %v",
					testCase.description, testCase.expNewErr, err)
			}
			if err != nil {
				return
			}
			policy := p.(*staticPolicy)
			st := &mockState{
				assignments:   testCase.stAssignments,
				defaultCPUSet: testCase.stDefaultCPUSet,
			}
			err = policy.Start(st)
			if !reflect.DeepEqual(err, testCase.expErr) {
				t.Errorf("StaticPolicy Start() error (%v). expected error: %v but got: %v",
					testCase.description, testCase.expErr, err)
			}
			if err != nil {
				return
			}

			if !st.GetDefaultCPUSet().Equals(testCase.expCSet) {
				t.Errorf("State CPUSet is different than expected. Have %q wants: %q", st.GetDefaultCPUSet(),
					testCase.expCSet)
			}

		})
	}
}

func TestStaticPolicyAddWithResvList(t *testing.T) {

	testCases := []staticPolicyTestWithResvList{
		{
			description:     "GuPodSingleCore, SingleSocketHT, ExpectError",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 1,
			reserved:        cpuset.New(0),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer2", "8000m", "8000m"),
			expErr:          fmt.Errorf("not enough cpus available to satisfy request: requested=8, available=7"),
			expCPUAlloc:     false,
			expCSet:         cpuset.New(),
		},
		{
			description:     "GuPodSingleCore, SingleSocketHT, ExpectAllocOneCPU",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 2,
			reserved:        cpuset.New(0, 1),
			stAssignments:   state.ContainerCPUAssignments{},
			stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			pod:             makePod("fakePod", "fakeContainer2", "1000m", "1000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(4), // expect sibling of partial core
		},
		{
			description:     "GuPodMultipleCores, SingleSocketHT, ExpectAllocOneCore",
			topo:            topoSingleSocketHT,
			numReservedCPUs: 2,
			reserved:        cpuset.New(0, 1),
			stAssignments: state.ContainerCPUAssignments{
				"fakePod": map[string]cpuset.CPUSet{
					"fakeContainer100": cpuset.New(2, 3, 6, 7),
				},
			},
			stDefaultCPUSet: cpuset.New(0, 1, 4, 5),
			pod:             makePod("fakePod", "fakeContainer3", "2000m", "2000m"),
			expErr:          nil,
			expCPUAlloc:     true,
			expCSet:         cpuset.New(4, 5),
		},
	}

	for _, testCase := range testCases {
		policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, testCase.reserved, topologymanager.NewFakeManager(), nil)

		st := &mockState{
			assignments:   testCase.stAssignments,
			defaultCPUSet: testCase.stDefaultCPUSet,
		}

		container := &testCase.pod.Spec.Containers[0]
		err := policy.Allocate(st, testCase.pod, container)
		if !reflect.DeepEqual(err, testCase.expErr) {
			t.Errorf("StaticPolicy Allocate() error (%v). expected add error: %v but got: %v",
				testCase.description, testCase.expErr, err)
		}

		if testCase.expCPUAlloc {
			cset, found := st.assignments[string(testCase.pod.UID)][container.Name]
			if !found {
				t.Errorf("StaticPolicy Allocate() error (%v). expected container %v to be present in assignments %v",
					testCase.description, container.Name, st.assignments)
			}

			if !reflect.DeepEqual(cset, testCase.expCSet) {
				t.Errorf("StaticPolicy Allocate() error (%v). expected cpuset %v but got %v",
					testCase.description, testCase.expCSet, cset)
			}

			if !cset.Intersection(st.defaultCPUSet).IsEmpty() {
				t.Errorf("StaticPolicy Allocate() error (%v). expected cpuset %v to be disoint from the shared cpuset %v",
					testCase.description, cset, st.defaultCPUSet)
			}
		}

		if !testCase.expCPUAlloc {
			_, found := st.assignments[string(testCase.pod.UID)][container.Name]
			if found {
				t.Errorf("StaticPolicy Allocate() error (%v). Did not expect container %v to be present in assignments %v",
					testCase.description, container.Name, st.assignments)
			}
		}
	}
}

func WithPodUID(pod *v1.Pod, podUID string) *v1.Pod {
	ret := pod.DeepCopy()
	ret.UID = types.UID(podUID)
	return ret
}

func TestStaticPolicyAddWithUncoreAlignment(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.CPUManagerPolicyAlphaOptions, true)

	testCases := []staticPolicyTestWithResvList{
		{
			description:     "GuPodSingleContainerSaturating, DualSocketHTUncore, ExpectAllocOneUncore, FullUncoreAvail",
			topo:            topoDualSocketMultiNumaPerSocketUncore,
			numReservedCPUs: 8,
			reserved:        cpuset.New(0, 1, 96, 97, 192, 193, 288, 289), // note 4 cpus taken from uncore 0, 4 from uncore 16
			cpuPolicyOptions: map[string]string{
				FullPCPUsOnlyOption:            "true",
				PreferAlignByUnCoreCacheOption: "true",
			},
			stAssignments: state.ContainerCPUAssignments{},
			// remove partially used uncores from the available CPUs to simulate fully clean slate
			stDefaultCPUSet: topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUs().Difference(
				cpuset.New().Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(0),
				).Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(16),
				),
			),
			pod: WithPodUID(
				makeMultiContainerPod(
					[]struct{ request, limit string }{}, // init container
					[]struct{ request, limit string }{ // app container
						{"16000m", "16000m"}, // CpusPerUncore=16 with this topology
					},
				),
				"with-app-container-saturating",
			),
		},

		{
			description:     "GuPodMainAndSidecarContainer, DualSocketHTUncore, ExpectAllocOneUncore, FullUncoreAvail",
			topo:            topoDualSocketMultiNumaPerSocketUncore,
			numReservedCPUs: 8,
			reserved:        cpuset.New(0, 1, 96, 97, 192, 193, 288, 289), // note 4 cpus taken from uncore 0, 4 from uncore 16
			cpuPolicyOptions: map[string]string{
				FullPCPUsOnlyOption:            "true",
				PreferAlignByUnCoreCacheOption: "true",
			},
			stAssignments: state.ContainerCPUAssignments{},
			// remove partially used uncores from the available CPUs to simulate fully clean slate
			stDefaultCPUSet: topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUs().Difference(
				cpuset.New().Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(0),
				).Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(16),
				),
			),
			pod: WithPodUID(
				makeMultiContainerPod(
					[]struct{ request, limit string }{}, // init container
					[]struct{ request, limit string }{ // app container
						{"12000m", "12000m"},
						{"2000m", "2000m"},
					},
				),
				"with-app-container-and-sidecar",
			),
		},
		{
			description:     "GuPodSidecarAndMainContainer, DualSocketHTUncore, ExpectAllocOneUncore, FullUncoreAvail",
			topo:            topoDualSocketMultiNumaPerSocketUncore,
			numReservedCPUs: 8,
			reserved:        cpuset.New(0, 1, 96, 97, 192, 193, 288, 289), // note 4 cpus taken from uncore 0, 4 from uncore 16
			cpuPolicyOptions: map[string]string{
				FullPCPUsOnlyOption:            "true",
				PreferAlignByUnCoreCacheOption: "true",
			},
			stAssignments: state.ContainerCPUAssignments{},
			// remove partially used uncores from the available CPUs to simulate fully clean slate
			stDefaultCPUSet: topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUs().Difference(
				cpuset.New().Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(0),
				).Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(16),
				),
			),
			pod: WithPodUID(
				makeMultiContainerPod(
					[]struct{ request, limit string }{}, // init container
					[]struct{ request, limit string }{ // app container
						{"2000m", "2000m"},
						{"12000m", "12000m"},
					},
				),
				"with-sidecar-and-app-container",
			),
		},
		{
			description:     "GuPodMainAndManySidecarContainer, DualSocketHTUncore, ExpectAllocOneUncore, FullUncoreAvail",
			topo:            topoDualSocketMultiNumaPerSocketUncore,
			numReservedCPUs: 8,
			reserved:        cpuset.New(0, 1, 96, 97, 192, 193, 288, 289), // note 4 cpus taken from uncore 0, 4 from uncore 16
			cpuPolicyOptions: map[string]string{
				FullPCPUsOnlyOption:            "true",
				PreferAlignByUnCoreCacheOption: "true",
			},
			stAssignments: state.ContainerCPUAssignments{},
			// remove partially used uncores from the available CPUs to simulate fully clean slate
			stDefaultCPUSet: topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUs().Difference(
				cpuset.New().Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(0),
				).Union(
					topoDualSocketMultiNumaPerSocketUncore.CPUDetails.CPUsInUncoreCaches(16),
				),
			),
			pod: WithPodUID(
				makeMultiContainerPod(
					[]struct{ request, limit string }{}, // init container
					[]struct{ request, limit string }{ // app container
						{"10000m", "10000m"},
						{"2000m", "2000m"},
						{"2000m", "2000m"},
						{"2000m", "2000m"},
					},
				),
				"with-app-container-and-multi-sidecar",
			),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			policy, err := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, testCase.reserved, topologymanager.NewFakeManager(), testCase.cpuPolicyOptions)
			if err != nil {
				t.Fatalf("NewStaticPolicy() failed with %v", err)
			}

			st := &mockState{
				assignments:   testCase.stAssignments,
				defaultCPUSet: testCase.stDefaultCPUSet.Difference(testCase.reserved), // ensure the cpumanager invariant
			}

			for idx := range testCase.pod.Spec.Containers {
				container := &testCase.pod.Spec.Containers[idx]
				err := policy.Allocate(st, testCase.pod, container)
				if err != nil {
					t.Fatalf("Allocate failed: pod=%q container=%q", testCase.pod.UID, container.Name)
				}
			}

			uncoreCacheIDs, err := getPodUncoreCacheIDs(st, testCase.topo, testCase.pod)
			if err != nil {
				t.Fatalf("uncore cache check: %v", err.Error())
			}
			ids := cpuset.New(uncoreCacheIDs...)
			if ids.Size() != 1 {
				t.Fatalf("not all container on the same uncore cache: %s", ids.String())
			}
		})
	}
}

type staticPolicyOptionTestCase struct {
	description   string
	policyOptions map[string]string
	expectedError bool
	expectedValue StaticPolicyOptions
}

func TestStaticPolicyOptions(t *testing.T) {
	testCases := []staticPolicyOptionTestCase{
		{
			description:   "nil args",
			policyOptions: nil,
			expectedError: false,
			expectedValue: StaticPolicyOptions{},
		},
		{
			description:   "empty args",
			policyOptions: map[string]string{},
			expectedError: false,
			expectedValue: StaticPolicyOptions{},
		},
		{
			description: "bad single arg",
			policyOptions: map[string]string{
				"badValue1": "",
			},
			expectedError: true,
		},
		{
			description: "bad multiple arg",
			policyOptions: map[string]string{
				"badValue1": "",
				"badvalue2": "aaaa",
			},
			expectedError: true,
		},
		{
			description: "good arg",
			policyOptions: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			expectedError: false,
			expectedValue: StaticPolicyOptions{
				FullPhysicalCPUsOnly: true,
			},
		},
		{
			description: "good arg, bad value",
			policyOptions: map[string]string{
				FullPCPUsOnlyOption: "enabled!",
			},
			expectedError: true,
		},

		{
			description: "bad arg intermixed",
			policyOptions: map[string]string{
				FullPCPUsOnlyOption: "1",
				"badvalue2":         "lorem ipsum",
			},
			expectedError: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			opts, err := NewStaticPolicyOptions(testCase.policyOptions)
			gotError := (err != nil)
			if gotError != testCase.expectedError {
				t.Fatalf("error with args %v expected error %v got %v: %v",
					testCase.policyOptions, testCase.expectedError, gotError, err)
			}

			if testCase.expectedError {
				return
			}

			if !reflect.DeepEqual(opts, testCase.expectedValue) {
				t.Fatalf("value mismatch with args %v expected value %v got %v",
					testCase.policyOptions, testCase.expectedValue, opts)
			}
		})
	}
}

func TestSMTAlignmentErrorText(t *testing.T) {
	type smtErrTestCase struct {
		name     string
		err      SMTAlignmentError
		expected string
	}

	testCases := []smtErrTestCase{
		{
			name:     "base SMT alignment error",
			err:      SMTAlignmentError{RequestedCPUs: 15, CpusPerCore: 4},
			expected: `SMT Alignment Error: requested 15 cpus not multiple cpus per core = 4`,
		},
		{
			// Note the explicit 0. The intent is to signal the lack of physical CPUs, but
			// in the corner case of no available physical CPUs at all, without the explicit
			// flag we cannot distinguish the case, and before PR#127959 we printed the old message
			name:     "base SMT alignment error, no physical CPUs, missing flag",
			err:      SMTAlignmentError{RequestedCPUs: 4, CpusPerCore: 2, AvailablePhysicalCPUs: 0},
			expected: `SMT Alignment Error: requested 4 cpus not multiple cpus per core = 2`,
		},
		{
			name:     "base SMT alignment error, no physical CPUs, explicit flag",
			err:      SMTAlignmentError{RequestedCPUs: 4, CpusPerCore: 2, AvailablePhysicalCPUs: 0, CausedByPhysicalCPUs: true},
			expected: `SMT Alignment Error: not enough free physical CPUs: available physical CPUs = 0, requested CPUs = 4, CPUs per core = 2`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := testCase.err.Error()
			if got != testCase.expected {
				t.Errorf("got=%v expected=%v", got, testCase.expected)
			}
		})
	}
}

func newCPUSetPtr(cpus ...int) *cpuset.CPUSet {
	ret := cpuset.New(cpus...)
	return &ret
}

func getPodUncoreCacheIDs(s state.Reader, topo *topology.CPUTopology, pod *v1.Pod) ([]int, error) {
	var uncoreCacheIDs []int
	for idx := range pod.Spec.Containers {
		container := &pod.Spec.Containers[idx]
		cset, ok := s.GetCPUSet(string(pod.UID), container.Name)
		if !ok {
			return nil, fmt.Errorf("GetCPUSet(%s, %s) not ok", pod.UID, container.Name)
		}
		for _, cpuID := range cset.UnsortedList() {
			info, ok := topo.CPUDetails[cpuID]
			if !ok {
				return nil, fmt.Errorf("cpuID %v not in topo.CPUDetails", cpuID)
			}
			uncoreCacheIDs = append(uncoreCacheIDs, info.UncoreCacheID)
		}
	}
	return uncoreCacheIDs, nil
}
