/*
Copyright 2024 The Kubernetes Authors.

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

package structured

import (
	"flag"
	"fmt"
	"slices"
	"testing"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	"github.com/onsi/gomega/types"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/ptr"
)

const (
	region1     = "region-1"
	region2     = "region-2"
	node1       = "node-1"
	node2       = "node-2"
	classA      = "class-a"
	classB      = "class-b"
	driverA     = "driver-a"
	driverB     = "driver-b"
	pool1       = "pool-1"
	pool2       = "pool-2"
	pool3       = "pool-3"
	pool4       = "pool-4"
	req0        = "req-0"
	req1        = "req-1"
	req2        = "req-2"
	req3        = "req-3"
	subReq0     = "subReq-0"
	subReq1     = "subReq-1"
	req0SubReq0 = "req-0/subReq-0"
	req0SubReq1 = "req-0/subReq-1"
	req1SubReq0 = "req-1/subReq-0"
	req1SubReq1 = "req-1/subReq-1"
	claim0      = "claim-0"
	claim1      = "claim-1"
	slice1      = "slice-1"
	slice2      = "slice-2"
	device1     = "device-1"
	device2     = "device-2"
	device3     = "device-3"
	device4     = "device-4"
)

func init() {
	// Bump up the default verbosity for testing. Allocate uses very
	// high thresholds because it is used in the scheduler's per-node
	// filter operation.
	ktesting.DefaultConfig = ktesting.NewConfig(ktesting.Verbosity(7))
	ktesting.DefaultConfig.AddFlags(flag.CommandLine)
}

// Test objects generators

const (
	fieldNameKey     = "metadata.name"
	regionKey        = "region"
	planetKey        = "planet"
	planetValueEarth = "earth"
)

// generate a node object given a name and a region
func node(name, region string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				regionKey: region,
				planetKey: planetValueEarth,
			},
		},
	}
}

// generate a DeviceClass object with the given name and a driver CEL selector.
// driver name is assumed to be the same as the class name.
func class(name, driver string) *resourceapi.DeviceClass {
	return &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourceapi.DeviceClassSpec{
			Selectors: []resourceapi.DeviceSelector{
				{
					CEL: &resourceapi.CELDeviceSelector{
						Expression: fmt.Sprintf(`device.driver == "%s"`, driver),
					},
				},
			},
		},
	}
}

// generate a DeviceConfiguration object with the given driver and attribute.
func deviceConfiguration(driver, attribute string) resourceapi.DeviceConfiguration {
	return resourceapi.DeviceConfiguration{
		Opaque: &resourceapi.OpaqueDeviceConfiguration{
			Driver: driver,
			Parameters: runtime.RawExtension{
				Raw: []byte(fmt.Sprintf("{\"%s\":\"%s\"}", attribute, attribute+"Value")),
			},
		},
	}
}

// generate a DeviceClass object with the given name and attribute.
// attribute is used to generate device configuration parameters in a form of JSON {attribute: attributeValue}.
func classWithConfig(name, driver, attribute string) *resourceapi.DeviceClass {
	class := class(name, driver)
	class.Spec.Config = []resourceapi.DeviceClassConfiguration{
		{
			DeviceConfiguration: deviceConfiguration(driver, attribute),
		},
	}
	return class
}

// generate a ResourceClaim object with the given name and device requests.
func claimWithRequests(name string, constraints []resourceapi.DeviceConstraint, requests ...resourceapi.DeviceRequest) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests:    requests,
				Constraints: constraints,
			},
		},
	}
}

// generate a DeviceRequest object with the given name, class and selectors.
func request(name, class string, count int64, selectors ...resourceapi.DeviceSelector) resourceapi.DeviceRequest {
	return resourceapi.DeviceRequest{
		Name:            name,
		Count:           count,
		AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
		DeviceClassName: class,
		Selectors:       selectors,
	}
}

func subRequest(name, class string, count int64, selectors ...resourceapi.DeviceSelector) resourceapi.DeviceSubRequest {
	return resourceapi.DeviceSubRequest{
		Name:            name,
		Count:           count,
		AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
		DeviceClassName: class,
		Selectors:       selectors,
	}
}

// genereate a DeviceRequest with the given name and list of prioritized requests.
func requestWithPrioritizedList(name string, prioritizedRequests ...resourceapi.DeviceSubRequest) resourceapi.DeviceRequest {
	return resourceapi.DeviceRequest{
		Name:           name,
		FirstAvailable: prioritizedRequests,
	}
}

// generate a ResourceClaim object with the given name, request and class.
func claim(name, req, class string, constraints ...resourceapi.DeviceConstraint) *resourceapi.ResourceClaim {
	claim := claimWithRequests(name, constraints, request(req, class, 1))
	return claim
}

// generate a ResourceClaim object with the given name, request, class, and attribute.
// attribute is used to generate parameters in a form of JSON {attribute: attributeValue}.
func claimWithDeviceConfig(name, request, class, driver, attribute string) *resourceapi.ResourceClaim {
	claim := claim(name, request, class)
	claim.Spec.Devices.Config = []resourceapi.DeviceClaimConfiguration{
		{
			DeviceConfiguration: deviceConfiguration(driver, attribute),
		},
	}
	return claim
}

func claimWithAll(name string, requests []resourceapi.DeviceRequest, constraints []resourceapi.DeviceConstraint, configs []resourceapi.DeviceClaimConfiguration) *resourceapi.ResourceClaim {
	claim := claimWithRequests(name, constraints, requests...)
	claim.Spec.Devices.Config = configs
	return claim
}

func deviceClaimConfig(requests []string, deviceConfig resourceapi.DeviceConfiguration) resourceapi.DeviceClaimConfiguration {
	return resourceapi.DeviceClaimConfiguration{
		Requests:            requests,
		DeviceConfiguration: deviceConfig,
	}
}

// generate a Device object with the given name, capacity and attributes.
func device(name string, capacity map[resourceapi.QualifiedName]resource.Quantity, attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) resourceapi.Device {
	device := resourceapi.Device{
		Name: name,
		Basic: &resourceapi.BasicDevice{
			Attributes: attributes,
		},
	}
	device.Basic.Capacity = make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity, len(capacity))
	for name, quantity := range capacity {
		device.Basic.Capacity[name] = resourceapi.DeviceCapacity{Value: quantity}
	}
	return device
}

// generate a ResourceSlice object with the given name, node,
// driver and pool names, generation and a list of devices.
// The nodeSelection parameter may be a string (= node name),
// true (= all nodes), or a node selector (= specific nodes).
func slice(name string, nodeSelection any, pool, driver string, devices ...resourceapi.Device) *resourceapi.ResourceSlice {
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver: driver,
			Pool: resourceapi.ResourcePool{
				Name:               pool,
				ResourceSliceCount: 1,
				Generation:         1,
			},
			Devices: devices,
		},
	}

	switch nodeSelection := nodeSelection.(type) {
	case *v1.NodeSelector:
		slice.Spec.NodeSelector = nodeSelection
	case bool:
		if !nodeSelection {
			panic("nodeSelection == false is not valid")
		}
		slice.Spec.AllNodes = true
	case string:
		slice.Spec.NodeName = nodeSelection
	default:
		panic(fmt.Sprintf("unexpected nodeSelection type %T: %+v", nodeSelection, nodeSelection))
	}

	return slice
}

func deviceAllocationResult(request, driver, pool, device string, adminAccess bool) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{
		Request: request,
		Driver:  driver,
		Pool:    pool,
		Device:  device,
	}
	if adminAccess {
		r.AdminAccess = &adminAccess
	}
	return r
}

func multipleDeviceAllocationResults(request, driver, pool string, count, startIndex int) []resourceapi.DeviceRequestAllocationResult {
	var results []resourceapi.DeviceRequestAllocationResult
	for i := startIndex; i < startIndex+count; i++ {
		results = append(results, deviceAllocationResult(request, driver, pool, fmt.Sprintf("device-%d", i), false))
	}
	return results
}

// nodeLabelSelector creates a node selector with a label match for "key" in "values".
func nodeLabelSelector(key string, values ...string) *v1.NodeSelector {
	requirements := []v1.NodeSelectorRequirement{{
		Key:      key,
		Operator: v1.NodeSelectorOpIn,
		Values:   values,
	}}
	selector := &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{MatchExpressions: requirements}}}
	return selector
}

// localNodeSelector returns a node selector for a specific node.
func localNodeSelector(nodeName string) *v1.NodeSelector {
	selector := nodeLabelSelector(fieldNameKey, nodeName)
	// Swap the requirements: we need to select by field, not label.
	selector.NodeSelectorTerms[0].MatchFields, selector.NodeSelectorTerms[0].MatchExpressions = selector.NodeSelectorTerms[0].MatchExpressions, selector.NodeSelectorTerms[0].MatchFields
	return selector
}

// allocationResult returns a matcher for one AllocationResult pointer with a list of
// embedded device allocation results. The order of those results doesn't matter.
func allocationResult(selector *v1.NodeSelector, results ...resourceapi.DeviceRequestAllocationResult) types.GomegaMatcher {
	return gstruct.MatchFields(0, gstruct.Fields{
		"Devices": gstruct.MatchFields(0, gstruct.Fields{
			"Results": gomega.ConsistOf(results), // Order is irrelevant.
			"Config":  gomega.BeNil(),
		}),
		"NodeSelector": matchNodeSelector(selector),
	})
}

// matchNodeSelector returns a matcher for a node selector. The order
// of terms, requirements, and values is irrelevant.
func matchNodeSelector(selector *v1.NodeSelector) types.GomegaMatcher {
	if selector == nil {
		return gomega.BeNil()
	}
	return gomega.HaveField("NodeSelectorTerms", matchNodeSelectorTerms(selector.NodeSelectorTerms))
}

func matchNodeSelectorTerms(terms []v1.NodeSelectorTerm) types.GomegaMatcher {
	var matchTerms []types.GomegaMatcher
	for _, term := range terms {
		matchTerms = append(matchTerms, matchNodeSelectorTerm(term))
	}
	return gomega.ConsistOf(matchTerms)
}

func matchNodeSelectorTerm(term v1.NodeSelectorTerm) types.GomegaMatcher {
	return gstruct.MatchFields(0, gstruct.Fields{
		"MatchExpressions": matchNodeSelectorRequirements(term.MatchExpressions),
		"MatchFields":      matchNodeSelectorRequirements(term.MatchFields),
	})
}

func matchNodeSelectorRequirements(requirements []v1.NodeSelectorRequirement) types.GomegaMatcher {
	var matchRequirements []types.GomegaMatcher
	for _, requirement := range requirements {
		matchRequirements = append(matchRequirements, matchNodeSelectorRequirement(requirement))
	}
	return gomega.ConsistOf(matchRequirements)
}

func matchNodeSelectorRequirement(requirement v1.NodeSelectorRequirement) types.GomegaMatcher {
	return gstruct.MatchFields(0, gstruct.Fields{
		"Key":      gomega.Equal(requirement.Key),
		"Operator": gomega.Equal(requirement.Operator),
		"Values":   gomega.ConsistOf(requirement.Values),
	})
}

func allocationResultWithConfig(selector *v1.NodeSelector, driver string, source resourceapi.AllocationConfigSource, attribute string, results ...resourceapi.DeviceRequestAllocationResult) resourceapi.AllocationResult {
	return resourceapi.AllocationResult{
		Devices: resourceapi.DeviceAllocationResult{
			Results: results,
			Config: []resourceapi.DeviceAllocationConfiguration{
				{
					Source:              source,
					DeviceConfiguration: deviceConfiguration(driver, attribute),
				},
			},
		},
		NodeSelector: selector,
	}
}

func allocationResultWithConfigs(selector *v1.NodeSelector, results []resourceapi.DeviceRequestAllocationResult, configs []resourceapi.DeviceAllocationConfiguration) resourceapi.AllocationResult {
	return resourceapi.AllocationResult{
		Devices: resourceapi.DeviceAllocationResult{
			Results: results,
			Config:  configs,
		},
		NodeSelector: selector,
	}
}

// Helpers

// convert a list of objects to a slice
func objects[T any](objs ...T) []T {
	return objs
}

// generate a ResourceSlice object with the given parameters and one device "device-1"
func sliceWithOneDevice(name string, nodeSelection any, pool, driver string) *resourceapi.ResourceSlice {
	return slice(name, nodeSelection, pool, driver, device(device1, nil, nil))
}

// generate a ResourceSclie object with the given parameters and the specified number of devices.
func sliceWithMultipleDevices(name string, nodeSelection any, pool, driver string, count int) *resourceapi.ResourceSlice {
	var devices []resourceapi.Device
	for i := 0; i < count; i++ {
		devices = append(devices, device(fmt.Sprintf("device-%d", i), nil, nil))
	}
	return slice(name, nodeSelection, pool, driver, devices...)
}

func TestAllocator(t *testing.T) {
	nonExistentAttribute := resourceapi.FullyQualifiedName(driverA + "/" + "NonExistentAttribute")
	boolAttribute := resourceapi.FullyQualifiedName(driverA + "/" + "boolAttribute")
	stringAttribute := resourceapi.FullyQualifiedName(driverA + "/" + "stringAttribute")
	versionAttribute := resourceapi.FullyQualifiedName(driverA + "/" + "driverVersion")
	intAttribute := resourceapi.FullyQualifiedName(driverA + "/" + "numa")

	testcases := map[string]struct {
		adminAccess      bool
		prioritizedList  bool
		claimsToAllocate []*resourceapi.ResourceClaim
		allocatedDevices []DeviceID
		classes          []*resourceapi.DeviceClass
		slices           []*resourceapi.ResourceSlice
		node             *v1.Node

		expectResults []any
		expectError   types.GomegaMatcher // can be used to check for no error or match specific error types
	}{

		"empty": {},
		"simple": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
			)},
		},
		"other-node": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverB),
				sliceWithOneDevice(slice2, node2, pool2, driverA),
			),
			node: node(node2, region2),

			expectResults: []any{allocationResult(
				localNodeSelector(node2),
				deviceAllocationResult(req0, driverA, pool2, device1, false),
			)},
		},
		"small-and-large": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				nil,
				request(req0, classA, 1, resourceapi.DeviceSelector{
					CEL: &resourceapi.CELDeviceSelector{
						Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("1Gi")) >= 0`, driverA),
					}}),
				request(req1, classA, 1, resourceapi.DeviceSelector{
					CEL: &resourceapi.CELDeviceSelector{
						Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("2Gi")) >= 0`, driverA),
					}}),
			)),
			classes: objects(class(classA, driverA)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("1Gi"),
				}, nil),
				device(device2, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("2Gi"),
				}, nil),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1, driverA, pool1, device2, false),
			)},
		},
		"small-and-large-backtrack-requests": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				nil,
				request(req0, classA, 1, resourceapi.DeviceSelector{
					CEL: &resourceapi.CELDeviceSelector{
						Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("1Gi")) >= 0`, driverA),
					}}),
				request(req1, classA, 1, resourceapi.DeviceSelector{
					CEL: &resourceapi.CELDeviceSelector{
						Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("2Gi")) >= 0`, driverA),
					}}),
			)),
			classes: objects(class(classA, driverA)),
			// Reversing the order in which the devices are listed causes the "large" device to
			// be allocated for the "small" request, leaving the "large" request unsatisfied.
			// The initial decision needs to be undone before a solution is found.
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device2, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("2Gi"),
				}, nil),
				device(device1, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("1Gi"),
				}, nil),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1, driverA, pool1, device2, false),
			)},
		},
		"small-and-large-backtrack-claims": {
			claimsToAllocate: objects(
				claimWithRequests(
					claim0,
					nil,
					request(req0, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("1Gi")) >= 0`, driverA),
						}})),
				claimWithRequests(
					claim1,
					nil,
					request(req1, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("2Gi")) >= 0`, driverA),
						}}),
				)),
			classes: objects(class(classA, driverA)),
			// Reversing the order in which the devices are listed causes the "large" device to
			// be allocated for the "small" request, leaving the "large" request unsatisfied.
			// The initial decision needs to be undone before a solution is found.
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device2, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("2Gi"),
				}, nil),
				device(device1, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("1Gi"),
				}, nil),
			)),
			node: node(node1, region1),

			expectResults: []any{
				allocationResult(localNodeSelector(node1), deviceAllocationResult(req0, driverA, pool1, device1, false)),
				allocationResult(localNodeSelector(node1), deviceAllocationResult(req1, driverA, pool1, device2, false)),
			},
		},
		"devices-split-across-different-slices": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
				Name:            req0,
				Count:           2,
				AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
				DeviceClassName: classA,
			})),
			classes: objects(class(classA, driverA)),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
				sliceWithOneDevice(slice2, node1, pool2, driverA),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req0, driverA, pool2, device1, false),
			)},
		},
		"obsolete-slice": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices: objects(
				sliceWithOneDevice("slice-1-obsolete", node1, pool1, driverA),
				func() *resourceapi.ResourceSlice {
					slice := sliceWithOneDevice(slice1, node1, pool1, driverA)
					// This makes the other slice obsolete.
					slice.Spec.Pool.Generation++
					return slice
				}(),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
			)},
		},
		"duplicate-slice": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices: func() []*resourceapi.ResourceSlice {
				// This simulates the problem that can
				// (theoretically) occur when the resource
				// slice controller wants to publish a pool
				// with two slices but ends up creating some
				// identical slices under different names
				// because its informer cache was out-dated on
				// another sync (see
				// resourceslicecontroller.go).
				sliceA := sliceWithOneDevice(slice1, node1, pool1, driverA)
				sliceA.Spec.Pool.ResourceSliceCount = 2
				sliceB := sliceA.DeepCopy()
				sliceB.Name += "-2"
				return []*resourceapi.ResourceSlice{sliceA, sliceB}
			}(),
			node: node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring(fmt.Sprintf("pool %s is invalid: duplicate device name %s", pool1, device1))),
		},
		"no-slices": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices:           nil,
			node:             node(node1, region1),

			expectResults: nil,
		},
		"not-enough-suitable-devices": {
			claimsToAllocate: objects(claim(claim0, req0, classA), claim(claim0, req1, classA)),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),

			node: node(node1, region1),

			expectResults: nil,
		},
		"no-classes": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          nil,
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: nil,
			expectError:   gomega.MatchError(gomega.ContainSubstring("could not retrieve device class class-a")),
		},
		"unknown-class": {
			claimsToAllocate: objects(claim(claim0, req0, "unknown-class")),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: nil,
			expectError:   gomega.MatchError(gomega.ContainSubstring("could not retrieve device class unknown-class")),
		},
		"empty-class": {
			claimsToAllocate: objects(claim(claim0, req0, "")),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: nil,
			expectError:   gomega.MatchError(gomega.ContainSubstring("claim claim-0, request req-0: missing device class name (unsupported request type?)")),
		},
		"no-claims-to-allocate": {
			claimsToAllocate: nil,
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: nil,
		},
		"all-devices-single": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
				Name:            req0,
				AllocationMode:  resourceapi.DeviceAllocationModeAll,
				DeviceClassName: classA,
			})),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
			)},
		},
		"all-devices-many": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
				Name:            req0,
				AllocationMode:  resourceapi.DeviceAllocationModeAll,
				DeviceClassName: classA,
			})),
			classes: objects(class(classA, driverA)),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
				sliceWithOneDevice(slice1, node1, pool2, driverA),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req0, driverA, pool2, device1, false),
			)},
		},
		"all-devices-of-the-incomplete-pool": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
				Name:            req0,
				AllocationMode:  resourceapi.DeviceAllocationModeAll,
				Count:           1,
				DeviceClassName: classA,
			})),
			classes: objects(class(classA, driverA)),
			slices: objects(
				func() *resourceapi.ResourceSlice {
					slice := sliceWithOneDevice(slice1, node1, pool1, driverA)
					// This makes the pool incomplete, one other slice is missing.
					slice.Spec.Pool.ResourceSliceCount++
					return slice
				}(),
			),
			node: node(node1, region1),

			expectResults: nil,
			expectError:   gomega.MatchError(gomega.ContainSubstring("claim claim-0, request req-0: asks for all devices, but resource pool driver-a/pool-1 is currently being updated")),
		},
		"all-devices-plus-another": {
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeAll,
					DeviceClassName: classA,
				}),
				claimWithRequests(claim1, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           1,
					DeviceClassName: classB,
				}),
			),
			classes: objects(
				class(classA, driverA),
				class(classB, driverB),
			),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
				sliceWithOneDevice(slice1, node1, pool1, driverB),
			),
			node: node(node1, region1),

			expectResults: []any{
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverA, pool1, device1, false),
				),
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverB, pool1, device1, false),
				),
			},
		},
		"all-devices-plus-another-reversed": {
			claimsToAllocate: objects(
				claimWithRequests(claim1, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           1,
					DeviceClassName: classB,
				}),
				claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeAll,
					DeviceClassName: classA,
				}),
			),
			classes: objects(
				class(classA, driverA),
				class(classB, driverB),
			),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
				sliceWithOneDevice(slice1, node1, pool1, driverB),
			),
			node: node(node1, region1),

			expectResults: []any{
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverB, pool1, device1, false),
				),
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverA, pool1, device1, false),
				),
			},
		},
		"all-devices-many-plus-another": {
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeAll,
					DeviceClassName: classA,
				}),
				claimWithRequests(claim1, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           1,
					DeviceClassName: classB,
				}),
			),
			classes: objects(
				class(classA, driverA),
				class(classB, driverB),
			),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1, nil, nil),
					device(device2, nil, nil),
				),
				sliceWithOneDevice(slice1, node1, pool1, driverB),
			),
			node: node(node1, region1),

			expectResults: []any{
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverA, pool1, device1, false),
					deviceAllocationResult(req0, driverA, pool1, device2, false),
				),
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverB, pool1, device1, false),
				),
			},
		},
		"all-devices-many-plus-another-reversed": {
			claimsToAllocate: objects(
				claimWithRequests(claim1, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           1,
					DeviceClassName: classB,
				}),
				claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeAll,
					DeviceClassName: classA,
				}),
			),
			classes: objects(
				class(classA, driverA),
				class(classB, driverB),
			),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1, nil, nil),
					device(device2, nil, nil),
				),
				sliceWithOneDevice(slice1, node1, pool1, driverB),
			),
			node: node(node1, region1),

			expectResults: []any{
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverB, pool1, device1, false),
				),
				allocationResult(
					localNodeSelector(node1),
					deviceAllocationResult(req0, driverA, pool1, device1, false),
					deviceAllocationResult(req0, driverA, pool1, device2, false),
				),
			},
		},
		"all-devices-no-solution": {
			// One device, two claims both trying to allocate it.
			claimsToAllocate: objects(
				claimWithRequests(claim1, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           1,
					DeviceClassName: classA,
				}),
				claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeAll,
					DeviceClassName: classA,
				}),
			),
			classes: objects(
				class(classA, driverA),
			),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
			),
			node: node(node1, region1),
		},
		"all-devices-no-solution-reversed": {
			// One device, two claims both trying to allocate it.
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeAll,
					DeviceClassName: classA,
				}),
				claimWithRequests(claim1, nil, resourceapi.DeviceRequest{
					Name:            req0,
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           1,
					DeviceClassName: classA,
				}),
			),
			classes: objects(
				class(classA, driverA),
			),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
			),
			node: node(node1, region1),
		},
		"network-attached-device": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, nodeLabelSelector(regionKey, region1), pool1, driverA)),
			node:             node(node1, region1),

			expectResults: []any{allocationResult(
				nodeLabelSelector(regionKey, region1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
			)},
		},
		"unsuccessful-allocation-network-attached-device": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, nodeLabelSelector(regionKey, region1), pool1, driverA)),
			// Wrong region, no devices available.
			node: node(node2, region2),

			expectResults: nil,
		},
		"many-network-attached-devices": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, request(req0, classA, 4))),
			classes:          objects(class(classA, driverA)),
			slices: objects(
				sliceWithOneDevice(slice1, nodeLabelSelector(regionKey, region1), pool1, driverA),
				sliceWithOneDevice(slice1, true, pool2, driverA),
				sliceWithOneDevice(slice1, nodeLabelSelector(planetKey, planetValueEarth), pool3, driverA),
				sliceWithOneDevice(slice1, localNodeSelector(node1), pool4, driverA),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				// A union of the individual selectors.
				&v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: regionKey, Operator: v1.NodeSelectorOpIn, Values: []string{region1}},
							{Key: planetKey, Operator: v1.NodeSelectorOpIn, Values: []string{planetValueEarth}},
						},
						MatchFields: []v1.NodeSelectorRequirement{
							{Key: fieldNameKey, Operator: v1.NodeSelectorOpIn, Values: []string{node1}},
						},
					}},
				},
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req0, driverA, pool2, device1, false),
				deviceAllocationResult(req0, driverA, pool3, device1, false),
				deviceAllocationResult(req0, driverA, pool4, device1, false),
			)},
		},
		"local-and-network-attached-devices": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, request(req0, classA, 2))),
			classes:          objects(class(classA, driverA)),
			slices: objects(
				sliceWithOneDevice(slice1, nodeLabelSelector(regionKey, region1), pool1, driverA),
				sliceWithOneDevice(slice1, node1, pool2, driverA),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				// Once there is any node-local device, the selector is for that node.
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req0, driverA, pool2, device1, false),
			)},
		},
		"several-different-drivers": {
			claimsToAllocate: objects(claim(claim0, req0, classA), claim(claim0, req0, classB)),
			classes:          objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1, nil, nil),
					device(device2, nil, nil),
				),
				sliceWithOneDevice(slice1, node1, pool1, driverB),
			),
			node: node(node1, region1),

			expectResults: []any{
				allocationResult(localNodeSelector(node1), deviceAllocationResult(req0, driverA, pool1, device1, false)),
				allocationResult(localNodeSelector(node1), deviceAllocationResult(req0, driverB, pool1, device1, false)),
			},
		},
		"already-allocated-devices": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			allocatedDevices: []DeviceID{
				MakeDeviceID(driverA, pool1, device1),
				MakeDeviceID(driverA, pool1, device2),
			},
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: nil,
		},
		"admin-access-disabled": {
			adminAccess: false,
			claimsToAllocate: func() []*resourceapi.ResourceClaim {
				c := claim(claim0, req0, classA)
				c.Spec.Devices.Requests[0].AdminAccess = ptr.To(true)
				return []*resourceapi.ResourceClaim{c}
			}(),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: nil,
			expectError:   gomega.MatchError(gomega.ContainSubstring("claim claim-0, request req-0: admin access is requested, but the feature is disabled")),
		},
		"admin-access-enabled": {
			adminAccess: true,
			claimsToAllocate: func() []*resourceapi.ResourceClaim {
				c := claim(claim0, req0, classA)
				c.Spec.Devices.Requests[0].AdminAccess = ptr.To(true)
				return []*resourceapi.ResourceClaim{c}
			}(),
			allocatedDevices: []DeviceID{
				MakeDeviceID(driverA, pool1, device1),
				MakeDeviceID(driverA, pool1, device2),
			},
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: []any{
				allocationResult(localNodeSelector(node1), deviceAllocationResult(req0, driverA, pool1, device1, true)),
			},
		},
		"with-constraint": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{
					{MatchAttribute: &intAttribute},
					{MatchAttribute: &versionAttribute},
					{MatchAttribute: &stringAttribute},
					{MatchAttribute: &boolAttribute},
				},
				request(req0, classA, 1),
				request(req1, classA, 1),
			),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion":   {VersionValue: ptr.To("1.0.0")},
					"numa":            {IntValue: ptr.To(int64(1))},
					"stringAttribute": {StringValue: ptr.To("stringAttributeValue")},
					"boolAttribute":   {BoolValue: ptr.To(true)},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion":   {VersionValue: ptr.To("1.0.0")},
					"numa":            {IntValue: ptr.To(int64(1))},
					"stringAttribute": {StringValue: ptr.To("stringAttributeValue")},
					"boolAttribute":   {BoolValue: ptr.To(true)},
				}),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1, driverA, pool1, device2, false),
			)},
		},
		"with-constraint-non-existent-attribute": {
			claimsToAllocate: objects(claim(claim0, req0, classA, resourceapi.DeviceConstraint{
				MatchAttribute: &nonExistentAttribute,
			})),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: nil,
		},
		"with-constraint-not-matching-int-attribute": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{{MatchAttribute: &intAttribute}},
				request(req0, classA, 2)),
			),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"numa": {IntValue: ptr.To(int64(1))},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"numa": {IntValue: ptr.To(int64(2))},
				}),
			)),
			node: node(node1, region1),

			expectResults: nil,
		},
		"with-constraint-not-matching-int-attribute-all-devices": {
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claimWithRequests(
						claim0,
						[]resourceapi.DeviceConstraint{{MatchAttribute: &intAttribute}},
						request(req0, classA, 0),
					)
					claim.Spec.Devices.Requests[0].AllocationMode = resourceapi.DeviceAllocationModeAll
					return claim
				}(),
			),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"numa": {IntValue: ptr.To(int64(1))},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"numa": {IntValue: ptr.To(int64(2))},
				}),
			)),
			node: node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("claim claim-0, request req-0: cannot add device driver-a/pool-1/device-2 because a claim constraint would not be satisfied")),
		},
		"with-constraint-not-matching-string-attribute": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{{MatchAttribute: &stringAttribute}},
				request(req0, classA, 2)),
			),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"stringAttribute": {StringValue: ptr.To("stringAttributeValue")},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"stringAttribute": {StringValue: ptr.To("stringAttributeValue2")},
				}),
			)),
			node: node(node1, region1),

			expectResults: nil,
		},
		"with-constraint-not-matching-bool-attribute": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{{MatchAttribute: &boolAttribute}},
				request(req0, classA, 2)),
			),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"boolAttribute": {BoolValue: ptr.To(true)},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"boolAttribute": {BoolValue: ptr.To(false)},
				}),
			)),
			node: node(node1, region1),

			expectResults: nil,
		},
		"with-constraint-not-matching-version-attribute": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{{MatchAttribute: &versionAttribute}},
				request(req0, classA, 2)),
			),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion": {VersionValue: ptr.To("1.0.0")},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion": {VersionValue: ptr.To("2.0.0")},
				}),
			)),
			node: node(node1, region1),

			expectResults: nil,
		},
		"with-constraint-for-request": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{
					{
						Requests:       []string{req0},
						MatchAttribute: &versionAttribute,
					},
				},
				request(req0, classA, 1),
				request(req1, classA, 1),
			)),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion": {VersionValue: ptr.To("1.0.0")},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion": {VersionValue: ptr.To("2.0.0")},
				}),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1, driverA, pool1, device2, false),
			)},
		},
		"with-constraint-for-request-retry": {
			claimsToAllocate: objects(claimWithRequests(
				claim0,
				[]resourceapi.DeviceConstraint{
					{
						Requests:       []string{req0},
						MatchAttribute: &versionAttribute,
					},
					{
						MatchAttribute: &stringAttribute,
					},
				},
				request(req0, classA, 1),
				request(req1, classA, 1),
			)),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				// This device does not satisfy the second
				// match attribute, so the allocator must
				// backtrack.
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion":   {VersionValue: ptr.To("1.0.0")},
					"stringAttribute": {StringValue: ptr.To("a")},
				}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion":   {VersionValue: ptr.To("2.0.0")},
					"stringAttribute": {StringValue: ptr.To("b")},
				}),
				device(device3, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"driverVersion":   {VersionValue: ptr.To("3.0.0")},
					"stringAttribute": {StringValue: ptr.To("b")},
				}),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device2, false),
				deviceAllocationResult(req1, driverA, pool1, device3, false),
			)},
		},
		"with-class-device-config": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(classWithConfig(classA, driverA, "classAttribute")),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: []any{
				allocationResultWithConfig(
					localNodeSelector(node1),
					driverA,
					resourceapi.AllocationConfigSourceClass,
					"classAttribute",
					deviceAllocationResult(req0, driverA, pool1, device1, false),
				),
			},
		},
		"claim-with-device-config": {
			claimsToAllocate: objects(claimWithDeviceConfig(claim0, req0, classA, driverA, "deviceAttribute")),
			classes:          objects(class(classA, driverA)),
			slices:           objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:             node(node1, region1),

			expectResults: []any{
				allocationResultWithConfig(
					localNodeSelector(node1),
					driverA,
					resourceapi.AllocationConfigSourceClaim,
					"deviceAttribute",
					deviceAllocationResult(req0, driverA, pool1, device1, false),
				),
			},
		},
		"unknown-selector": {
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claim(claim0, req0, classA)
					claim.Spec.Devices.Requests[0].Selectors = []resourceapi.DeviceSelector{
						{ /* empty = unknown future selector */ },
					}
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("CEL expression empty (unsupported selector type?)")),
		},
		"unknown-allocation-mode": {
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claim(claim0, req0, classA)
					claim.Spec.Devices.Requests[0].AllocationMode = resourceapi.DeviceAllocationMode("future-mode")
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("unsupported count mode future-mode")),
		},
		"unknown-constraint": {
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claim(claim0, req0, classA)
					claim.Spec.Devices.Constraints = []resourceapi.DeviceConstraint{
						{ /* empty = unknown */ },
					}
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("empty constraint (unsupported constraint type?)")),
		},
		"unknown-device": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes:          objects(class(classA, driverA)),
			slices: objects(
				func() *resourceapi.ResourceSlice {
					slice := sliceWithOneDevice(slice1, node1, pool1, driverA)
					slice.Spec.Devices[0].Basic = nil /* empty = unknown future extension */
					return slice
				}(),
			),
			node: node(node1, region1),
		},
		"invalid-CEL-one-device": {
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claim(claim0, req0, classA)
					claim.Spec.Devices.Requests[0].Selectors = []resourceapi.DeviceSelector{
						{CEL: &resourceapi.CELDeviceSelector{Expression: "noSuchVar"}},
					}
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("undeclared reference")),
		},
		"invalid-CEL-one-device-class": {
			claimsToAllocate: objects(claim(claim0, req0, classA)),
			classes: objects(
				func() *resourceapi.DeviceClass {
					c := class(classA, driverA)
					c.Spec.Selectors[0].CEL.Expression = "noSuchVar"
					return c
				}(),
			),
			slices: objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:   node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("undeclared reference")),
		},
		"invalid-CEL-all-devices": {
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claim(claim0, req0, classA)
					claim.Spec.Devices.Requests[0].Selectors = []resourceapi.DeviceSelector{
						{CEL: &resourceapi.CELDeviceSelector{Expression: "noSuchVar"}},
					}
					claim.Spec.Devices.Requests[0].AllocationMode = resourceapi.DeviceAllocationModeAll
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("undeclared reference")),
		},
		"too-many-devices-single-request": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, request(req0, classA, 500))),
			classes:          objects(class(classA, driverA)),

			expectError: gomega.MatchError(gomega.ContainSubstring("exceeds the claim limit")),
		},
		"many-devices-okay": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, request(req0, classA, resourceapi.AllocationResultsMaxSize))),
			classes:          objects(class(classA, driverA)),
		},
		"too-many-devices-total": {
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil,
					request(req0, classA, resourceapi.AllocationResultsMaxSize),
					request(req1, classA, 1),
				),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithMultipleDevices(slice1, node1, pool1, driverA, resourceapi.AllocationResultsMaxSize+1)),
			node:    node(node1, region1),

			expectError: gomega.MatchError(gomega.ContainSubstring("exceeds the claim limit")),
		},
		"all-devices-invalid-CEL": {
			claimsToAllocate: objects(claimWithRequests(claim0, nil, request(req0, classA, 500))),
			classes:          objects(class(classA, driverA)),

			expectError: gomega.MatchError(gomega.ContainSubstring("exceeds the claim limit")),
		},
		"prioritized-list-first-unavailable": {
			prioritizedList: true,
			claimsToAllocate: objects(claimWithRequests(claim0, nil, requestWithPrioritizedList(req0,
				subRequest(subReq0, classB, 1),
				subRequest(subReq1, classA, 1),
			))),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0SubReq1, driverA, pool1, device1, false),
			)},
		},
		"prioritized-list-non-available": {
			prioritizedList: true,
			claimsToAllocate: objects(claimWithRequests(claim0, nil, requestWithPrioritizedList(req0,
				subRequest(subReq0, classB, 2),
				subRequest(subReq1, classA, 2),
			))),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(
				sliceWithOneDevice(slice1, node1, pool1, driverA),
				sliceWithOneDevice(slice2, node1, pool2, driverB),
			),
			node: node(node1, region1),

			expectResults: nil,
		},
		"prioritized-list-device-config": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithAll(claim0,
					objects(
						requestWithPrioritizedList(req0,
							subRequest(subReq0, classA, 1),
							subRequest(subReq1, classB, 2),
						),
					),
					nil,
					objects(
						deviceClaimConfig([]string{req0SubReq0}, deviceConfiguration(driverA, "foo")),
						deviceClaimConfig([]string{req0SubReq1}, deviceConfiguration(driverB, "bar")),
					),
				),
			),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices: objects(slice(slice1, node1, pool1, driverB,
				device(device1, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}),
				device(device2, nil, map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResultWithConfigs(
				localNodeSelector(node1),
				objects(
					deviceAllocationResult(req0SubReq1, driverB, pool1, device1, false),
					deviceAllocationResult(req0SubReq1, driverB, pool1, device2, false),
				),
				[]resourceapi.DeviceAllocationConfiguration{
					{
						Source: resourceapi.AllocationConfigSourceClaim,
						Requests: []string{
							req0SubReq1,
						},
						DeviceConfiguration: deviceConfiguration(driverB, "bar"),
					},
				},
			)},
		},
		"prioritized-list-class-config": {
			prioritizedList: true,
			claimsToAllocate: objects(claimWithRequests(claim0, nil, requestWithPrioritizedList(req0,
				subRequest(subReq0, classA, 2),
				subRequest(subReq1, classB, 2),
			))),
			classes: objects(
				classWithConfig(classA, driverA, "foo"),
				classWithConfig(classB, driverB, "bar"),
			),
			slices: objects(
				slice(slice1, node1, pool1, driverB,
					device(device1, nil, nil),
					device(device2, nil, nil),
				),
				slice(slice2, node1, pool2, driverA,
					device(device3, nil, nil),
				),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResultWithConfigs(
				localNodeSelector(node1),
				objects(
					deviceAllocationResult(req0SubReq1, driverB, pool1, device1, false),
					deviceAllocationResult(req0SubReq1, driverB, pool1, device2, false),
				),
				[]resourceapi.DeviceAllocationConfiguration{
					{
						Source:              resourceapi.AllocationConfigSourceClass,
						Requests:            nil,
						DeviceConfiguration: deviceConfiguration(driverB, "bar"),
					},
				},
			)},
		},
		"prioritized-list-subrequests-with-expressions": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil,
					request(req0, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("1Gi")) >= 0`, driverA),
						}},
					),
					requestWithPrioritizedList(req1,
						subRequest(subReq0, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("4Gi")) >= 0`, driverA),
							}}),
						subRequest(subReq1, classA, 2, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("2Gi")) >= 0`, driverA),
							}}),
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(slice(slice1, node1, pool1, driverA,
				device(device1, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("2Gi"),
				}, nil),
				device(device2, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("2Gi"),
				}, nil),
				device(device3, map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("1Gi"),
				}, nil),
			)),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device3, false),
				deviceAllocationResult(req1SubReq1, driverA, pool1, device1, false),
				deviceAllocationResult(req1SubReq1, driverA, pool1, device2, false),
			)},
		},
		"prioritized-list-subrequests-with-constraints-ref-parent-request": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0,
					[]resourceapi.DeviceConstraint{
						{
							Requests:       []string{req0, req1},
							MatchAttribute: &versionAttribute,
						},
					},
					request(req0, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("8Gi")) >= 0`, driverA),
						}},
					),
					requestWithPrioritizedList(req1,
						subRequest(subReq0, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("2Gi")) >= 0`, driverA),
							}},
						),
						subRequest(subReq1, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("1Gi")) >= 0`, driverA),
							}},
						),
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("8Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
							"driverVersion": {VersionValue: ptr.To("1.0.0")},
						},
					),
				),
				slice(slice2, node1, pool2, driverA,
					device(device2,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("2Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
							"driverVersion": {VersionValue: ptr.To("2.0.0")},
						},
					),
					device(device3,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("1Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
							"driverVersion": {VersionValue: ptr.To("1.0.0")},
						},
					),
				),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1SubReq1, driverA, pool2, device3, false),
			)},
		},
		"prioritized-list-subrequests-with-constraints-ref-sub-request": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0,
					[]resourceapi.DeviceConstraint{
						{
							Requests:       []string{req0, req1SubReq0},
							MatchAttribute: &versionAttribute,
						},
					},
					request(req0, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("8Gi")) >= 0`, driverA),
						}},
					),
					requestWithPrioritizedList(req1,
						subRequest(subReq0, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("2Gi")) >= 0`, driverA),
							}},
						),
						subRequest(subReq1, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("1Gi")) >= 0`, driverA),
							}},
						),
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("8Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
							"driverVersion": {VersionValue: ptr.To("1.0.0")},
						},
					),
				),
				slice(slice2, node1, pool2, driverA,
					device(device2,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("2Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
							"driverVersion": {VersionValue: ptr.To("2.0.0")},
						},
					),
				),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1SubReq1, driverA, pool2, device2, false),
			)},
		},
		"prioritized-list-subrequests-with-allocation-mode-all": {
			prioritizedList: true,
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claimWithRequests(claim0, nil,
						requestWithPrioritizedList(req0,
							subRequest(subReq0, classA, 1),
							subRequest(subReq1, classA, 1),
						),
					)
					claim.Spec.Devices.Requests[0].FirstAvailable[0].AllocationMode = resourceapi.DeviceAllocationModeAll
					claim.Spec.Devices.Requests[0].FirstAvailable[0].Count = 0
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1, nil, nil),
					device(device2, nil, nil),
				),
			),
			allocatedDevices: []DeviceID{
				MakeDeviceID(driverA, pool1, device1),
			},
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0SubReq1, driverA, pool1, device2, false),
			)},
		},
		"prioritized-list-allocation-mode-all-multiple-requests": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil,
					request(req0, classA, 1),
					requestWithPrioritizedList(req1,
						func() resourceapi.DeviceSubRequest {
							subReq := subRequest(subReq0, classA, 1)
							subReq.AllocationMode = resourceapi.DeviceAllocationModeAll
							subReq.Count = 0
							return subReq
						}(),
						subRequest(subReq1, classA, 1),
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1, nil, nil),
					device(device2, nil, nil),
				),
			),
			node: node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0, driverA, pool1, device1, false),
				deviceAllocationResult(req1SubReq1, driverA, pool1, device2, false),
			)},
		},
		"prioritized-list-disabled": {
			prioritizedList: false,
			claimsToAllocate: objects(
				func() *resourceapi.ResourceClaim {
					claim := claimWithRequests(claim0, nil,
						requestWithPrioritizedList(req0,
							subRequest(subReq0, classA, 1),
						),
					)
					return claim
				}(),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: nil,
			expectError:   gomega.MatchError(gomega.ContainSubstring("claim claim-0, request req-0: has subrequests, but the feature is disabled")),
		},
		"prioritized-list-multi-request": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil,
					request(req1, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("8Gi")) >= 0`, driverA),
						}},
					),
					requestWithPrioritizedList(req0,
						subRequest(subReq0, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("8Gi")) >= 0`, driverA),
							}},
						),
						subRequest(subReq1, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("4Gi")) >= 0`, driverA),
							}},
						),
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("8Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
					),
				),
				slice(slice2, node1, pool2, driverA,
					device(device2,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("4Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
					),
				),
			),
			node: node(node1, region1),
			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req1, driverA, pool1, device1, false),
				deviceAllocationResult(req0SubReq1, driverA, pool2, device2, false),
			)},
		},
		"prioritized-list-with-backtracking": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil,
					requestWithPrioritizedList(req0,
						subRequest(subReq0, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("8Gi")) >= 0`, driverA),
							}},
						),
						subRequest(subReq1, classA, 1, resourceapi.DeviceSelector{
							CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("4Gi")) >= 0`, driverA),
							}},
						),
					),
					request(req1, classA, 1, resourceapi.DeviceSelector{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: fmt.Sprintf(`device.capacity["%s"].memory.compareTo(quantity("8Gi")) >= 0`, driverA),
						}},
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices: objects(
				slice(slice1, node1, pool1, driverA,
					device(device1,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("8Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
					),
				),
				slice(slice2, node1, pool2, driverA,
					device(device2,
						map[resourceapi.QualifiedName]resource.Quantity{
							"memory": resource.MustParse("4Gi"),
						},
						map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
					),
				),
			),
			node: node(node1, region1),
			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0SubReq1, driverA, pool2, device2, false),
				deviceAllocationResult(req1, driverA, pool1, device1, false),
			)},
		},
		"prioritized-list-too-many-in-first-subrequest": {
			prioritizedList: true,
			claimsToAllocate: objects(claimWithRequests(claim0, nil, requestWithPrioritizedList(req0,
				subRequest(subReq0, classB, 500),
				subRequest(subReq1, classA, 1),
			))),
			classes: objects(class(classA, driverA), class(classB, driverB)),
			slices:  objects(sliceWithOneDevice(slice1, node1, pool1, driverA)),
			node:    node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				deviceAllocationResult(req0SubReq1, driverA, pool1, device1, false),
			)},
		},
		"prioritized-list-max-allocation-limit-request": {
			prioritizedList: true,
			claimsToAllocate: objects(
				claimWithRequests(claim0, nil,
					requestWithPrioritizedList(req0,
						subRequest(subReq0, classA, resourceapi.AllocationResultsMaxSize),
						subRequest(subReq1, classA, resourceapi.AllocationResultsMaxSize/2),
					),
					requestWithPrioritizedList(req1,
						subRequest(subReq0, classA, resourceapi.AllocationResultsMaxSize),
						subRequest(subReq1, classA, resourceapi.AllocationResultsMaxSize/2),
					),
				),
			),
			classes: objects(class(classA, driverA)),
			slices:  objects(sliceWithMultipleDevices(slice1, node1, pool1, driverA, resourceapi.AllocationResultsMaxSize*2)),
			node:    node(node1, region1),

			expectResults: []any{allocationResult(
				localNodeSelector(node1),
				slices.Concat(
					multipleDeviceAllocationResults(req0SubReq1, driverA, pool1, resourceapi.AllocationResultsMaxSize/2, 0),
					multipleDeviceAllocationResults(req1SubReq1, driverA, pool1, resourceapi.AllocationResultsMaxSize/2, resourceapi.AllocationResultsMaxSize/2),
				)...,
			)},
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			g := gomega.NewWithT(t)

			// Listing objects is deterministic and returns them in the same
			// order as in the test case. That makes the allocation result
			// also deterministic.
			var classLister informerLister[resourceapi.DeviceClass]
			for _, class := range tc.classes {
				classLister.objs = append(classLister.objs, class.DeepCopy())
			}
			claimsToAllocate := slices.Clone(tc.claimsToAllocate)
			allocatedDevices := slices.Clone(tc.allocatedDevices)
			slices := slices.Clone(tc.slices)

			allocator, err := NewAllocator(ctx, tc.adminAccess, tc.prioritizedList, claimsToAllocate, sets.New(allocatedDevices...), classLister, slices, cel.NewCache(1))
			g.Expect(err).ToNot(gomega.HaveOccurred())

			results, err := allocator.Allocate(ctx, tc.node)
			matchError := tc.expectError
			if matchError == nil {
				matchError = gomega.Not(gomega.HaveOccurred())
			}
			g.Expect(err).To(matchError)
			g.Expect(results).To(gomega.ConsistOf(tc.expectResults...))

			// Objects that the allocator had access to should not have been modified.
			g.Expect(claimsToAllocate).To(gomega.HaveExactElements(tc.claimsToAllocate))
			g.Expect(allocatedDevices).To(gomega.HaveExactElements(tc.allocatedDevices))
			g.Expect(slices).To(gomega.ConsistOf(tc.slices))
			g.Expect(classLister.objs).To(gomega.ConsistOf(tc.classes))
		})
	}
}

type informerLister[T any] struct {
	objs []*T
	err  error
}

func (l informerLister[T]) List() (ret []*T, err error) {
	return l.objs, l.err
}

func (l informerLister[T]) Get(name string) (*T, error) {
	for _, obj := range l.objs {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		if accessor.GetName() == name {
			return obj, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, "not found")
}
