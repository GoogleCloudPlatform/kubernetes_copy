/*
Copyright 2021 The Kubernetes Authors.

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

package state

import (
	"reflect"
	"sort"
	"testing"

	"k8s.io/utils/cpuset"

	"github.com/google/go-cmp/cmp"
)

func TestClone(t *testing.T) {
	expect := ContainerCPUAssignments{
		"pod": map[string]cpuset.CPUSet{
			"container1": cpuset.New(4, 5, 6),
			"container2": cpuset.New(1, 2, 3),
		},
	}
	actual := expect.Clone()
	if &expect == &actual || !reflect.DeepEqual(expect, actual) {
		t.Fail()
	}
}

func TestString(t *testing.T) {
	testCases := []struct {
		assignments    ContainerCPUAssignments
		expectedOutput string
	}{
		{
			assignments: ContainerCPUAssignments{
				"pod1": {
					"container1": cpuset.New(1, 2),
					"container2": cpuset.New(3, 4),
				},
				"pod2": {
					"container3": cpuset.New(5),
				},
			},
			expectedOutput: `{"pod1":{"container1":"1-2","container2":"3-4"},"pod2":{"container3":"5"}}`,
		},
		{
			assignments: ContainerCPUAssignments{
				"pod1": {
					"container1": cpuset.New(1, 2),
					"container2": cpuset.New(3, 4),
				},
				"pod2": {
					"container3": cpuset.New(5),
				},
			},
			expectedOutput: `{"pod2":{"container3":"5"},"pod1":{"container1":"1-2","container2":"3-4"}}`,
		},
		{
			assignments: ContainerCPUAssignments{
				"pod1": {
					"container1": cpuset.New(1, 2),
					"container2": cpuset.New(3, 4),
				},
				"pod2": {
					"container3": cpuset.New(5),
				},
			},
			expectedOutput: `{"pod2":{"container3":"5"},"pod1":{"container1":"1-2","container2":"3-4"}}`,
		},
		{
			assignments: ContainerCPUAssignments{
				"pod1": {
					"container1": cpuset.New(1, 2),
					"container2": cpuset.New(3, 4),
				},
				"pod2": {
					"container3": cpuset.New(5),
				},
			},
			expectedOutput: `{"pod2":{"container3":"5"},"pod1":{"container2":"3-4","container1":"1-2"}}`,
		},
		{
			assignments: ContainerCPUAssignments{
				"pod1": {
					"container1": cpuset.New(1, 5),
				},
				"pod3": {
					"container5": cpuset.New(9),
					"container2": cpuset.New(3, 4, 5, 6, 7, 8),
				},
				"pod2": {
					"container3": cpuset.New(5),
					"container2": cpuset.New(3, 8),
				},
			},
			expectedOutput: `{"pod2":{"container3":"5","container2":"3,8"},"pod1":{"container1":"1,5"},"pod3":{"container5":"9","container2":"3-8"}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.expectedOutput, func(t *testing.T) {
			if diff := cmp.Diff(tc.assignments.String(), tc.expectedOutput); diff != "" {
				// maps iteration is inconsistent, so we might get a different output than expected.
				// because of that, we have a fallback that compares the bytes of both outputs
				// this way guarantees that the character set is identical, although not order in the same fashion.
				if !compareBytes(tc.assignments.String(), tc.expectedOutput) {
					t.Errorf("String() output mismatch.\n diff=%s got(+) want(-)", diff)
				}
			}
		})
	}
}

func compareBytes(s1, s2 string) bool {
	bytes1 := []byte(s1)
	bytes2 := []byte(s2)
	sort.Slice(bytes1, func(i, j int) bool { return bytes1[i] < bytes1[j] })
	sort.Slice(bytes2, func(i, j int) bool { return bytes2[i] < bytes2[j] })
	return cmp.Equal(bytes1, bytes2)
}
