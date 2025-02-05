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

package resourceclaim

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/integration/framework"
)

// TestEnableDisableDRAResourceClaimDeviceStatus first test the feature gate disabled
// by creating a ResourceClaim with an invalid device (not allocated device) and checks
// the object is not validated.
// Then the feature gate is created, and an attempt to create similar invalid ResourceClaim
// is done with no success.
func TestEnableDisableDRAResourceClaimDeviceStatus(t *testing.T) {
	// start etcd instance
	etcdOptions := framework.SharedEtcd()
	apiServerOptions := kubeapiservertesting.NewDefaultTestServerOptions()
	// apiserver with the feature disabled
	server1 := kubeapiservertesting.StartTestServerOrDie(t, apiServerOptions,
		[]string{
			fmt.Sprintf("--feature-gates=%s=true,%s=false", features.DynamicResourceAllocation, features.DRAResourceClaimDeviceStatus),
		},
		etcdOptions)
	client1, err := clientset.NewForConfig(server1.ClientConfig)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	ns := framework.CreateNamespaceOrDie(client1, "test-enable-dra-resourceclaim-device-status", t)

	rcDisabledName := "test-enable-dra-resourceclaim-device-status-rc-disabled"
	rcDisabled := &v1beta1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: rcDisabledName,
		},
		Spec: v1beta1.ResourceClaimSpec{
			Devices: v1beta1.DeviceClaim{
				Requests: []v1beta1.DeviceRequest{
					{
						Name:            "foo",
						DeviceClassName: "foo",
						Count:           1,
						AllocationMode:  v1beta1.DeviceAllocationModeExactCount,
					},
				},
			},
		},
	}

	if _, err := client1.ResourceV1beta1().ResourceClaims(ns.Name).Create(context.TODO(), rcDisabled, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	rcDisabled.Status = v1beta1.ResourceClaimStatus{
		Devices: []v1beta1.AllocatedDeviceStatus{
			{
				Driver: "foo",
				Pool:   "foo",
				Device: "foo",
				Data: &runtime.RawExtension{
					Raw: []byte(`{"kind": "foo", "apiVersion": "dra.example.com/v1"}`),
				},
				NetworkData: &v1beta1.NetworkDeviceData{
					InterfaceName: "net-1",
					IPs: []string{
						"10.9.8.0/24",
						"2001:db8::/64",
					},
					HardwareAddress: "ea:9f:cb:40:b1:7b",
				},
			},
		},
	}
	if _, err := client1.ResourceV1beta1().ResourceClaims(ns.Name).UpdateStatus(context.TODO(), rcDisabled, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	rcDisabled, err = client1.ResourceV1beta1().ResourceClaims(ns.Name).Get(context.TODO(), rcDisabledName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// No devices as the Kubernetes api-server dropped these fields since the feature is disabled.
	if len(rcDisabled.Status.Devices) != 0 {
		t.Fatalf("expected 0 Device in status got %d", len(rcDisabled.Status.Devices))
	}

	// shutdown apiserver with the feature disabled
	server1.TearDownFn()

	// apiserver with the feature enabled
	server2 := kubeapiservertesting.StartTestServerOrDie(t, apiServerOptions,
		[]string{
			fmt.Sprintf("--feature-gates=%s=true,%s=true", features.DynamicResourceAllocation, features.DRAResourceClaimDeviceStatus),
		},
		etcdOptions)
	client2, err := clientset.NewForConfig(server2.ClientConfig)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	rcEnabledName := "test-enable-dra-resourceclaim-device-status-rc-enabled"
	rcEnabled := &v1beta1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: rcEnabledName,
		},
		Spec: v1beta1.ResourceClaimSpec{
			Devices: v1beta1.DeviceClaim{
				Requests: []v1beta1.DeviceRequest{
					{
						Name:            "bar",
						DeviceClassName: "bar",
						Count:           1,
						AllocationMode:  v1beta1.DeviceAllocationModeExactCount,
					},
				},
			},
		},
	}

	if _, err := client2.ResourceV1beta1().ResourceClaims(ns.Name).Create(context.TODO(), rcEnabled, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Tests the validation is enabled.
	// validation will refuse this update as the device is not allocated.
	rcEnabled.Status = v1beta1.ResourceClaimStatus{
		Devices: []v1beta1.AllocatedDeviceStatus{
			{
				Driver: "bar",
				Pool:   "bar",
				Device: "bar",
				Data: &runtime.RawExtension{
					Raw: []byte(`{"kind": "foo", "apiVersion": "dra.example.com/v1"}`),
				},
				NetworkData: &v1beta1.NetworkDeviceData{
					InterfaceName: "net-1",
					IPs: []string{
						"10.9.8.0/24",
						"2001:db8::/64",
					},
					HardwareAddress: "ea:9f:cb:40:b1:7b",
				},
			},
		},
	}
	if _, err := client2.ResourceV1beta1().ResourceClaims(ns.Name).UpdateStatus(context.TODO(), rcEnabled, metav1.UpdateOptions{}); err == nil {
		t.Fatalf("Expected error (must be an allocated device in the claim)")
	}

	rcEnabled.Status = v1beta1.ResourceClaimStatus{
		Allocation: &v1beta1.AllocationResult{
			Devices: v1beta1.DeviceAllocationResult{
				Results: []v1beta1.DeviceRequestAllocationResult{
					{
						Request: "bar",
						Driver:  "bar",
						Pool:    "bar",
						Device:  "bar",
					},
				},
			},
		},
		Devices: []v1beta1.AllocatedDeviceStatus{
			{
				Driver: "bar",
				Pool:   "bar",
				Device: "bar",
				Data: &runtime.RawExtension{
					Raw: []byte(`{"kind": "foo", "apiVersion": "dra.example.com/v1"}`),
				},
				NetworkData: &v1beta1.NetworkDeviceData{
					InterfaceName: "net-1",
					IPs: []string{
						"10.9.8.0/24",
						"2001:db8::/64",
					},
					HardwareAddress: "ea:9f:cb:40:b1:7b",
				},
			},
		},
	}
	if _, err := client2.ResourceV1beta1().ResourceClaims(ns.Name).UpdateStatus(context.TODO(), rcEnabled, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Tests the field is enabled.
	rcEnabled, err = client2.ResourceV1beta1().ResourceClaims(ns.Name).Get(context.TODO(), rcEnabledName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rcEnabled.Status.Devices) != 1 {
		t.Fatalf("expected 1 Device in status got %d", len(rcEnabled.Status.Devices))
	}

	// shutdown apiserver with the feature enabled
	server2.TearDownFn()
}
