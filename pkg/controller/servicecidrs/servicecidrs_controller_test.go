/*
Copyright 2023 The Kubernetes Authors.

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

package servicecidrs

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	networkingapiv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controlplane/controller/defaultservicecidr"
	"k8s.io/kubernetes/pkg/registry/core/service/ipallocator"
	"k8s.io/kubernetes/test/utils/ktesting"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

type testController struct {
	*Controller
	servicecidrsStore cache.Store
	ipaddressesStore  cache.Store
}

func newController(ctx context.Context, t *testing.T, cidrs []*networkingapiv1.ServiceCIDR, ips []*networkingapiv1.IPAddress) (*fake.Clientset, *testController) {
	client := fake.NewSimpleClientset()

	informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())

	serviceCIDRInformer := informerFactory.Networking().V1().ServiceCIDRs()
	cidrStore := serviceCIDRInformer.Informer().GetStore()
	for _, obj := range cidrs {
		err := cidrStore.Add(obj)
		if err != nil {
			t.Fatal(err)
		}
	}
	ipAddressInformer := informerFactory.Networking().V1().IPAddresses()
	ipStore := ipAddressInformer.Informer().GetStore()
	for _, obj := range ips {
		err := ipStore.Add(obj)
		if err != nil {
			t.Fatal(err)
		}
	}
	controller := NewController(
		ctx,
		serviceCIDRInformer,
		ipAddressInformer,
		client)

	var alwaysReady = func() bool { return true }
	controller.serviceCIDRsSynced = alwaysReady
	controller.ipAddressSynced = alwaysReady

	return client, &testController{
		controller,
		cidrStore,
		ipStore,
	}
}

func TestControllerSync(t *testing.T) {
	now := time.Now()

	// ServiceCIDR that is just being deleted
	deletingServiceCIDR := makeServiceCIDR("deleting-cidr", "192.168.0.0/24", "2001:db2::/64")
	deletingServiceCIDR.Finalizers = []string{ServiceCIDRProtectionFinalizer}
	deletingServiceCIDR.DeletionTimestamp = ptr.To[metav1.Time](metav1.Now())

	// ServiceCIDR that has been deleted for longer than the deletionGracePeriod
	deletedServiceCIDR := makeServiceCIDR("deleted-cidr", "192.168.0.0/24", "2001:db2::/64")
	deletedServiceCIDR.Finalizers = []string{ServiceCIDRProtectionFinalizer}
	deletedServiceCIDR.DeletionTimestamp = ptr.To[metav1.Time](metav1.NewTime(now.Add(-deletionGracePeriod - 1*time.Second)))

	testCases := []struct {
		name       string
		cidrs      []*networkingapiv1.ServiceCIDR
		ips        []*networkingapiv1.IPAddress
		cidrSynced string
		actions    [][]string // verb and resource and subresource
	}{
		{
			name: "no existing service CIDRs",
		},
		{
			name: "default service CIDR must have finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			cidrSynced: defaultservicecidr.DefaultServiceCIDRName,
			actions:    [][]string{{"patch", "servicecidrs", ""}, {"patch", "servicecidrs", "status"}},
		},
		{
			name: "service CIDR must have finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR("no-finalizer", "192.168.0.0/24", "2001:db2::/64"),
			},
			cidrSynced: "no-finalizer",
			actions:    [][]string{{"patch", "servicecidrs", ""}, {"patch", "servicecidrs", "status"}},
		},
		{
			name: "service CIDR being deleted must remove the finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", ""}},
		},
		{
			name: "service CIDR being deleted but within the grace period must be requeued not remove the finalizer", // TODO: assert is actually requeued
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletingServiceCIDR,
			},
			cidrSynced: deletingServiceCIDR.Name,
			actions:    [][]string{},
		},
		{
			name: "service CIDR being deleted with IPv4 addresses should update the status",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", "status"}},
		},
		{
			name: "service CIDR being deleted and overlapping same range and IPv4 addresses should remove the finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", ""}},
		},
		{
			name: "service CIDR being deleted and overlapping and IPv4 addresses should remove the finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
				makeServiceCIDR("overlapping", "192.168.0.0/16", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", ""}},
		},
		{
			name: "service CIDR being deleted and not overlapping and IPv4 addresses should update the status",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
				makeServiceCIDR("overlapping", "192.168.255.0/26", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", "status"}},
		},
		{
			name: "service CIDR being deleted with IPv6 addresses should update the status",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("2001:db2::1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", "status"}},
		},
		{
			name: "service CIDR being deleted and overlapping same range and IPv6 addresses should remove the finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("2001:db2::1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", ""}},
		},
		{
			name: "service CIDR being deleted and overlapping and IPv6 addresses should remove the finalizer",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
				makeServiceCIDR("overlapping", "192.168.0.0/16", "2001:db2::/48"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("2001:db2::1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", ""}},
		},
		{
			name: "service CIDR being deleted and not overlapping and IPv6 addresses should update the status",
			cidrs: []*networkingapiv1.ServiceCIDR{
				deletedServiceCIDR,
				makeServiceCIDR("overlapping", "192.168.255.0/26", "2001:db2:a:b::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("2001:db2::1"),
			},
			cidrSynced: deletedServiceCIDR.Name,
			actions:    [][]string{{"patch", "servicecidrs", "status"}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			client, controller := newController(tCtx, t, tc.cidrs, tc.ips)
			// server side apply does not play well with fake client go
			// so we skip the errors and only assert on the actions
			// https://github.com/kubernetes/kubernetes/issues/99953
			_ = controller.sync(tCtx, tc.cidrSynced)
			expectAction(t, client.Actions(), tc.actions)

		})
	}
}

func makeServiceCIDR(name, primary, secondary string) *networkingapiv1.ServiceCIDR {
	serviceCIDR := &networkingapiv1.ServiceCIDR{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: networkingapiv1.ServiceCIDRSpec{},
	}
	serviceCIDR.Spec.CIDRs = append(serviceCIDR.Spec.CIDRs, primary)
	if secondary != "" {
		serviceCIDR.Spec.CIDRs = append(serviceCIDR.Spec.CIDRs, secondary)
	}
	return serviceCIDR
}

func makeIPAddress(name string) *networkingapiv1.IPAddress {
	family := string(v1.IPv4Protocol)
	if netutils.IsIPv6String(name) {
		family = string(v1.IPv6Protocol)
	}
	return &networkingapiv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				networkingapiv1.LabelIPAddressFamily: family,
				networkingapiv1.LabelManagedBy:       ipallocator.ControllerName,
			},
		},
	}
}

func expectAction(t *testing.T, actions []k8stesting.Action, expected [][]string) {
	t.Helper()
	if len(actions) != len(expected) {
		t.Fatalf("Expected at least %d actions, got %d \ndiff: %v", len(expected), len(actions), cmp.Diff(expected, actions))
	}

	for i, action := range actions {
		verb := expected[i][0]
		if action.GetVerb() != verb {
			t.Errorf("Expected action %d verb to be %s, got %s", i, verb, action.GetVerb())
		}
		resource := expected[i][1]
		if action.GetResource().Resource != resource {
			t.Errorf("Expected action %d resource to be %s, got %s", i, resource, action.GetResource().Resource)
		}
		subresource := expected[i][2]
		if action.GetSubresource() != subresource {
			t.Errorf("Expected action %d subresource to be %s, got %s", i, subresource, action.GetSubresource())
		}
	}
}

func TestController_canDeleteCIDR(t *testing.T) {
	tests := []struct {
		name       string
		cidrs      []*networkingapiv1.ServiceCIDR
		ips        []*networkingapiv1.IPAddress
		cidrSynced *networkingapiv1.ServiceCIDR
		want       bool
	}{
		{
			name:       "empty",
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR and no IPs",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR with IPs",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.24"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       false,
		},
		{
			name: "CIDR without IPs",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.1.24"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR with IPv4 address referencing the subnet address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.0"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR with IPv4 address referencing the broadcast address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.255"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR with IPv6 address referencing the broadcast address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("2001:0db2::ffff:ffff:ffff:ffff"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       false,
		},
		{
			name: "CIDR with same range overlapping and IPs",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.23"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR with smaller range overlapping and IPs",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/26", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.23"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
		{
			name: "CIDR with smaller range overlapping but IPs orphan",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/28", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.23"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       false,
		},
		{
			name: "CIDR with larger range overlapping and IPs",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/16", "2001:db2::/64"),
			},
			ips: []*networkingapiv1.IPAddress{
				makeIPAddress("192.168.0.23"),
			},
			cidrSynced: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want:       true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			_, controller := newController(tCtx, t, tc.cidrs, tc.ips)
			got, err := controller.canDeleteCIDR(tCtx, tc.cidrSynced)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Controller.canDeleteCIDR() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestController_ipToCidrs(t *testing.T) {
	tests := []struct {
		name  string
		cidrs []*networkingapiv1.ServiceCIDR
		ip    *networkingapiv1.IPAddress
		want  []string
	}{
		{
			name: "empty",
			ip:   makeIPAddress("192.168.0.23"),
			want: []string{},
		}, {
			name: "one CIDR",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("192.168.0.23"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName},
		}, {
			name: "two equal CIDR",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("192.168.0.23"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName, "overlapping"},
		}, {
			name: "three CIDR - two same and one larger",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping2", "192.168.0.0/26", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("192.168.0.23"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName, "overlapping", "overlapping2"},
		}, {
			name: "three CIDR - two same and one larger - IPv4 subnet address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping2", "192.168.0.0/26", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("192.168.0.0"),
			want: []string{},
		}, {
			name: "three CIDR - two same and one larger - IPv4 broadcast address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping2", "192.168.0.0/26", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("192.168.0.63"), // broadcast for 192.168.0.0/26
			want: []string{defaultservicecidr.DefaultServiceCIDRName, "overlapping"},
		}, {
			name: "three CIDR - two same and one larger - IPv6 subnet address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping2", "192.168.0.0/26", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("2001:db2::"),
			want: []string{},
		}, {
			name: "three CIDR - two same and one larger - IPv6 broadcast address",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping2", "192.168.0.0/26", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			ip:   makeIPAddress("2001:0db2::ffff:ffff:ffff:ffff"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName, "overlapping"},
		}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			_, controller := newController(tCtx, t, tt.cidrs, nil)
			if got := controller.containingServiceCIDRs(tt.ip); !cmp.Equal(got, tt.want, cmpopts.SortSlices(func(a, b string) bool { return a < b })) {
				t.Errorf("Controller.ipToCidrs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestController_cidrToCidrs(t *testing.T) {
	tests := []struct {
		name  string
		cidrs []*networkingapiv1.ServiceCIDR
		cidr  *networkingapiv1.ServiceCIDR
		want  []string
	}{
		{
			name: "empty",
			cidr: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want: []string{},
		}, {
			name: "one CIDR",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			cidr: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName},
		}, {
			name: "two equal CIDR",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			cidr: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName, "overlapping"},
		}, {
			name: "three CIDR - two same and one larger",
			cidrs: []*networkingapiv1.ServiceCIDR{
				makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping", "192.168.0.0/24", "2001:db2::/64"),
				makeServiceCIDR("overlapping2", "192.168.0.0/26", "2001:db2::/96"),
				makeServiceCIDR("unrelated", "10.0.0.0/24", ""),
				makeServiceCIDR("unrelated2", "10.0.0.0/16", ""),
			},
			cidr: makeServiceCIDR(defaultservicecidr.DefaultServiceCIDRName, "192.168.0.0/24", "2001:db2::/64"),
			want: []string{defaultservicecidr.DefaultServiceCIDRName, "overlapping", "overlapping2"},
		}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			_, controller := newController(tCtx, t, tt.cidrs, nil)
			if got := controller.overlappingServiceCIDRs(tt.cidr); !cmp.Equal(got, tt.want, cmpopts.SortSlices(func(a, b string) bool { return a < b })) {
				t.Errorf("Controller.cidrToCidrs() = %v, want %v", got, tt.want)
			}
		})
	}
}
