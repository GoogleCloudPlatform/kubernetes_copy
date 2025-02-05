/*
Copyright 2015 The Kubernetes Authors.

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

package types

const (
	// ResolvConfDefault is the system default DNS resolver configuration.
	ResolvConfDefault = "/etc/resolv.conf"
)

// User visible keys for managing node allocatable enforcement on the node.
const (
	NodeAllocatableEnforcementKey            = "pods"
	SystemReservedEnforcementKey             = "system-reserved"
	SystemReservedCompressibleEnforcementKey = "system-reserved-compressible"
	KubeReservedEnforcementKey               = "kube-reserved"
	KubeReservedCompressibleEnforcementKey   = "kube-reserved-compressible"
	NodeAllocatableNoneKey                   = "none"
)

// SwapBehavior types
const (
	LimitedSwap = "LimitedSwap"
	NoSwap      = "NoSwap"
)

// InPlacePodVerticalScaling types
const (
	// ErrorInconsistentCPUAllocation represent the type of an inconsistentCPUAllocationError
	ErrorInconsistentCPUAllocation = "inconsistentCPUAllocationError"
	ErrorGetCPUSet                 = "getCPUSetError"
)
