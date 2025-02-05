/*
Copyright 2014 The Kubernetes Authors.

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

package internalversion

import (
	"fmt"
	"math"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/certificate/csr"
	"k8s.io/kubernetes/pkg/apis/admissionregistration"
	"k8s.io/kubernetes/pkg/apis/apiserverinternal"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/certificates"
	"k8s.io/kubernetes/pkg/apis/coordination"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/discovery"
	"k8s.io/kubernetes/pkg/apis/flowcontrol"
	"k8s.io/kubernetes/pkg/apis/networking"
	nodeapi "k8s.io/kubernetes/pkg/apis/node"
	"k8s.io/kubernetes/pkg/apis/policy"
	"k8s.io/kubernetes/pkg/apis/rbac"
	resourceapis "k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/pkg/apis/storage"
	"k8s.io/kubernetes/pkg/apis/storagemigration"
	"k8s.io/kubernetes/pkg/printers"
	"k8s.io/utils/ptr"
)

var containerRestartPolicyAlways = api.ContainerRestartPolicyAlways

func TestFormatResourceName(t *testing.T) {
	tests := []struct {
		kind schema.GroupKind
		name string
		want string
	}{
		{schema.GroupKind{}, "", ""},
		{schema.GroupKind{}, "name", "name"},
		{schema.GroupKind{Kind: "Kind"}, "", "kind/"}, // should not happen in practice
		{schema.GroupKind{Kind: "Kind"}, "name", "kind/name"},
		{schema.GroupKind{Group: "group", Kind: "Kind"}, "name", "kind.group/name"},
	}
	for _, tt := range tests {
		if got := formatResourceName(tt.kind, tt.name, true); got != tt.want {
			t.Errorf("formatResourceName(%q, %q) = %q, want %q", tt.kind, tt.name, got, tt.want)
		}
	}
}

type TestPrintHandler struct {
	numCalls int
}

func (t *TestPrintHandler) TableHandler(columnDefinitions []metav1.TableColumnDefinition, printFunc interface{}) error {
	t.numCalls++
	return nil
}

func (t *TestPrintHandler) getNumCalls() int {
	return t.numCalls
}

func TestAllHandlers(t *testing.T) {
	h := &TestPrintHandler{numCalls: 0}
	AddHandlers(h)
	if h.getNumCalls() == 0 {
		t.Error("TableHandler not called in AddHandlers")
	}
}

func TestPrintEvent(t *testing.T) {
	tests := []struct {
		event    api.Event
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		// Basic event; no generate options
		{
			event: api.Event{
				Source: api.EventSource{Component: "kubelet"},
				InvolvedObject: api.ObjectReference{
					Kind:      "Pod",
					Name:      "Pod Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:         "Event Reason",
				Message:        "Message Data",
				FirstTimestamp: metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -3)},
				LastTimestamp:  metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -2)},
				Count:          6,
				Type:           api.EventTypeNormal,
				ObjectMeta:     metav1.ObjectMeta{Name: "event1"},
			},
			options: printers.GenerateOptions{},
			// Columns: Last Seen, Type, Reason, Object, Message
			expected: []metav1.TableRow{{Cells: []interface{}{"2d", "Normal", "Event Reason", "pod/Pod Name", "Message Data"}}},
		},
		// Basic event; generate options=Wide
		{
			event: api.Event{
				Source: api.EventSource{
					Component: "kubelet",
					Host:      "Node1",
				},
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:         "Event Reason",
				Message:        "Message Data",
				FirstTimestamp: metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -3)},
				LastTimestamp:  metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -2)},
				Count:          6,
				Type:           api.EventTypeWarning,
				ObjectMeta:     metav1.ObjectMeta{Name: "event2"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"2d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, Node1", "Message Data", "3d", int64(6), "event2"}}},
		},
		// Basic event, w/o FirstTimestamp set
		{
			event: api.Event{
				Source: api.EventSource{
					Component: "kubelet",
					Host:      "Node1",
				},
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:        "Event Reason",
				Message:       "Message Data",
				EventTime:     metav1.MicroTime{Time: time.Now().UTC().AddDate(0, 0, -3)},
				LastTimestamp: metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -3)},
				Count:         1,
				Type:          api.EventTypeWarning,
				ObjectMeta:    metav1.ObjectMeta{Name: "event3"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"3d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, Node1", "Message Data", "3d", int64(1), "event3"}}},
		},
		// Basic event, w/o LastTimestamp set
		{
			event: api.Event{
				Source: api.EventSource{
					Component: "kubelet",
					Host:      "Node1",
				},
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:         "Event Reason",
				Message:        "Message Data",
				EventTime:      metav1.MicroTime{Time: time.Now().UTC().AddDate(0, 0, -3)},
				FirstTimestamp: metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -3)},
				Count:          1,
				Type:           api.EventTypeWarning,
				ObjectMeta:     metav1.ObjectMeta{Name: "event4"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"3d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, Node1", "Message Data", "3d", int64(1), "event4"}}},
		},
		// Basic event, w/o FirstTimestamp and LastTimestamp set
		{
			event: api.Event{
				Source: api.EventSource{
					Component: "kubelet",
					Host:      "Node1",
				},
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:     "Event Reason",
				Message:    "Message Data",
				EventTime:  metav1.MicroTime{Time: time.Now().UTC().AddDate(0, 0, -3)},
				Count:      1,
				Type:       api.EventTypeWarning,
				ObjectMeta: metav1.ObjectMeta{Name: "event5"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"3d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, Node1", "Message Data", "3d", int64(1), "event5"}}},
		},
		// Basic event serie, w/o FirstTimestamp, LastTimestamp and Count set
		{
			event: api.Event{
				Source: api.EventSource{
					Component: "kubelet",
					Host:      "Node1",
				},
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Series: &api.EventSeries{
					Count:            2,
					LastObservedTime: metav1.MicroTime{Time: time.Now().UTC().AddDate(0, 0, -2)},
				},
				Reason:     "Event Reason",
				Message:    "Message Data",
				EventTime:  metav1.MicroTime{Time: time.Now().UTC().AddDate(0, 0, -3)},
				Type:       api.EventTypeWarning,
				ObjectMeta: metav1.ObjectMeta{Name: "event6"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"2d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, Node1", "Message Data", "3d", int64(2), "event6"}}},
		},
		// Singleton event, w/o FirstTimestamp, LastTimestamp and Count set
		{
			event: api.Event{
				Source: api.EventSource{
					Component: "kubelet",
					Host:      "Node1",
				},
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:     "Event Reason",
				Message:    "Message Data",
				EventTime:  metav1.MicroTime{Time: time.Now().UTC().AddDate(0, 0, -3)},
				Type:       api.EventTypeWarning,
				ObjectMeta: metav1.ObjectMeta{Name: "event7"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"3d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, Node1", "Message Data", "3d", int64(1), "event7"}}},
		},
		// Basic event, with empty Source; generate options=Wide
		{
			event: api.Event{
				ReportingController: "kubelet",
				ReportingInstance:   "test",
				InvolvedObject: api.ObjectReference{
					Kind:      "Deployment",
					Name:      "Deployment Name",
					FieldPath: "spec.containers{foo}",
				},
				Reason:         "Event Reason",
				Message:        "Message Data",
				FirstTimestamp: metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -3)},
				LastTimestamp:  metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -2)},
				Count:          6,
				Type:           api.EventTypeWarning,
				ObjectMeta:     metav1.ObjectMeta{Name: "event2"},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Last Seen, Type, Reason, Object, Subobject, Source, Message, First Seen, Count, Name
			expected: []metav1.TableRow{{Cells: []interface{}{"2d", "Warning", "Event Reason", "deployment/Deployment Name", "spec.containers{foo}", "kubelet, test", "Message Data", "3d", int64(6), "event2"}}},
		},
	}

	for i, test := range tests {
		rows, err := printEvent(&test.event, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintEventsResultSorted(t *testing.T) {

	eventList := api.EventList{
		Items: []api.Event{
			{
				Source:         api.EventSource{Component: "kubelet"},
				Message:        "Item 1",
				FirstTimestamp: metav1.NewTime(time.Date(2014, time.January, 15, 0, 0, 0, 0, time.UTC)),
				LastTimestamp:  metav1.NewTime(time.Date(2014, time.January, 15, 0, 0, 0, 0, time.UTC)),
				Count:          1,
				Type:           api.EventTypeNormal,
			},
			{
				Source:         api.EventSource{Component: "scheduler"},
				Message:        "Item 2",
				FirstTimestamp: metav1.NewTime(time.Date(1987, time.June, 17, 0, 0, 0, 0, time.UTC)),
				LastTimestamp:  metav1.NewTime(time.Date(1987, time.June, 17, 0, 0, 0, 0, time.UTC)),
				Count:          1,
				Type:           api.EventTypeNormal,
			},
			{
				Source:         api.EventSource{Component: "kubelet"},
				Message:        "Item 3",
				FirstTimestamp: metav1.NewTime(time.Date(2002, time.December, 25, 0, 0, 0, 0, time.UTC)),
				LastTimestamp:  metav1.NewTime(time.Date(2002, time.December, 25, 0, 0, 0, 0, time.UTC)),
				Count:          1,
				Type:           api.EventTypeNormal,
			},
		},
	}

	rows, err := printEventList(&eventList, printers.GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("Generate Event List: Wrong number of table rows returned. Expected 3, got (%d)", len(rows))
	}
	// Verify the watch event dates are in order.
	firstRow := rows[0]
	message1 := firstRow.Cells[4]
	if message1.(string) != "Item 1" {
		t.Errorf("Wrong event ordering: expecting (Item 1), got (%s)", message1)
	}
	secondRow := rows[1]
	message2 := secondRow.Cells[4]
	if message2 != "Item 2" {
		t.Errorf("Wrong event ordering: expecting (Item 2), got (%s)", message2)
	}
	thirdRow := rows[2]
	message3 := thirdRow.Cells[4]
	if message3 != "Item 3" {
		t.Errorf("Wrong event ordering: expecting (Item 3), got (%s)", message3)
	}
}

func TestPrintNamespace(t *testing.T) {
	tests := []struct {
		namespace api.Namespace
		expected  []metav1.TableRow
	}{
		// Basic namespace with status and age.
		{
			namespace: api.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "namespace1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Status: api.NamespaceStatus{
					Phase: "FooStatus",
				},
			},
			// Columns: Name, Status, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"namespace1", "FooStatus", "0s"}}},
		},
		// Basic namespace without status or age.
		{
			namespace: api.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "namespace2",
				},
			},
			// Columns: Name, Status, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"namespace2", "", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printNamespace(&test.namespace, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintSecret(t *testing.T) {
	tests := []struct {
		secret   api.Secret
		expected []metav1.TableRow
	}{
		// Basic namespace with type, data, and age.
		{
			secret: api.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "secret1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Type: "kubernetes.io/service-account-token",
				Data: map[string][]byte{
					"token": []byte("secret data"),
				},
			},
			// Columns: Name, Type, Data, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"secret1", "kubernetes.io/service-account-token", int64(1), "0s"}}},
		},
		// Basic namespace with type and age; no data.
		{
			secret: api.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "secret1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Type: "kubernetes.io/service-account-token",
			},
			// Columns: Name, Type, Data, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"secret1", "kubernetes.io/service-account-token", int64(0), "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printSecret(&test.secret, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintServiceAccount(t *testing.T) {
	tests := []struct {
		serviceAccount api.ServiceAccount
		expected       []metav1.TableRow
	}{
		// Basic service account without secrets
		{
			serviceAccount: api.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sa1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Secrets: []api.ObjectReference{},
			},
			// Columns: Name, (Num) Secrets, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"sa1", int64(0), "0s"}}},
		},
		// Basic service account with two secrets.
		{
			serviceAccount: api.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sa1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Secrets: []api.ObjectReference{
					{Name: "Secret1"},
					{Name: "Secret2"},
				},
			},
			// Columns: Name, (Num) Secrets, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"sa1", int64(2), "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printServiceAccount(&test.serviceAccount, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeStatus(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{Type: api.NodeReady, Status: api.ConditionTrue}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo1", "Ready", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
				Spec:       api.NodeSpec{Unschedulable: true},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{Type: api.NodeReady, Status: api.ConditionTrue}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo2", "Ready,SchedulingDisabled", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo3"},
				Status: api.NodeStatus{Conditions: []api.NodeCondition{
					{Type: api.NodeReady, Status: api.ConditionTrue},
					{Type: api.NodeReady, Status: api.ConditionTrue}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo3", "Ready", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo4"},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{Type: api.NodeReady, Status: api.ConditionFalse}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo4", "NotReady", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo5"},
				Spec:       api.NodeSpec{Unschedulable: true},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{Type: api.NodeReady, Status: api.ConditionFalse}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo5", "NotReady,SchedulingDisabled", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo6"},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{Type: "InvalidValue", Status: api.ConditionTrue}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo6", "Unknown", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo7"},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo7", "Unknown", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo8"},
				Spec:       api.NodeSpec{Unschedulable: true},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{Type: "InvalidValue", Status: api.ConditionTrue}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo8", "Unknown,SchedulingDisabled", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo9"},
				Spec:       api.NodeSpec{Unschedulable: true},
				Status:     api.NodeStatus{Conditions: []api.NodeCondition{{}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo9", "Unknown,SchedulingDisabled", "<none>", "<unknown>", ""}}},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{})
		if err != nil {
			t.Fatalf("Error generating table rows for Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeRole(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo9"},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo9", "Unknown", "<none>", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "foo10",
					Labels: map[string]string{"node-role.kubernetes.io/master": "", "node-role.kubernetes.io/control-plane": "", "node-role.kubernetes.io/proxy": "", "kubernetes.io/role": "node"},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo10", "Unknown", "control-plane,master,node,proxy", "<unknown>", ""}}},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "foo11",
					Labels: map[string]string{"kubernetes.io/role": "node"},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"foo11", "Unknown", "node", "<unknown>", ""}}},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{})
		if err != nil {
			t.Fatalf("An error occurred generating table rows for Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeOSImage(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
				Status: api.NodeStatus{
					NodeInfo:  api.NodeSystemInfo{OSImage: "fake-os-image"},
					Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo1", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "fake-os-image", "<unknown>", "<unknown>"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
				Status: api.NodeStatus{
					NodeInfo:  api.NodeSystemInfo{KernelVersion: "fake-kernel-version"},
					Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo2", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "<unknown>", "fake-kernel-version", "<unknown>"},
				},
			},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{Wide: true})
		if err != nil {
			t.Fatalf("An error occurred generating table for Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeKernelVersion(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
				Status: api.NodeStatus{
					NodeInfo:  api.NodeSystemInfo{KernelVersion: "fake-kernel-version"},
					Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo1", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "<unknown>", "fake-kernel-version", "<unknown>"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
				Status: api.NodeStatus{
					NodeInfo:  api.NodeSystemInfo{OSImage: "fake-os-image"},
					Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo2", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "fake-os-image", "<unknown>", "<unknown>"},
				},
			},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{Wide: true})
		if err != nil {
			t.Fatalf("An error occurred generating table rows Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeContainerRuntimeVersion(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
				Status: api.NodeStatus{
					NodeInfo:  api.NodeSystemInfo{ContainerRuntimeVersion: "foo://1.2.3"},
					Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo1", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "<unknown>", "<unknown>", "foo://1.2.3"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
				Status: api.NodeStatus{
					NodeInfo:  api.NodeSystemInfo{},
					Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}},
				},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo2", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{Wide: true})
		if err != nil {
			t.Fatalf("An error occurred generating table rows Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeName(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "127.0.0.1"},
				Status:     api.NodeStatus{},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"127.0.0.1", "Unknown", "<none>", "<unknown>", ""}}}},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: ""},
				Status:     api.NodeStatus{},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion
			expected: []metav1.TableRow{{Cells: []interface{}{"", "Unknown", "<none>", "<unknown>", ""}}},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{})
		if err != nil {
			t.Fatalf("An error occurred generating table rows Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeExternalIP(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
				Status:     api.NodeStatus{Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo1", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
				Status:     api.NodeStatus{Addresses: []api.NodeAddress{{Type: api.NodeInternalIP, Address: "1.1.1.1"}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo2", "Unknown", "<none>", "<unknown>", "", "1.1.1.1", "<none>", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo3"},
				Status: api.NodeStatus{Addresses: []api.NodeAddress{
					{Type: api.NodeExternalIP, Address: "2.2.2.2"},
					{Type: api.NodeInternalIP, Address: "3.3.3.3"},
					{Type: api.NodeExternalIP, Address: "4.4.4.4"},
				}},
			},
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo3", "Unknown", "<none>", "<unknown>", "", "3.3.3.3", "2.2.2.2", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{Wide: true})
		if err != nil {
			t.Fatalf("An error occurred generating table rows Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNodeInternalIP(t *testing.T) {

	table := []struct {
		node     api.Node
		expected []metav1.TableRow
	}{
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
				Status:     api.NodeStatus{Addresses: []api.NodeAddress{{Type: api.NodeInternalIP, Address: "1.1.1.1"}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo1", "Unknown", "<none>", "<unknown>", "", "1.1.1.1", "<none>", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
				Status:     api.NodeStatus{Addresses: []api.NodeAddress{{Type: api.NodeExternalIP, Address: "1.1.1.1"}}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo2", "Unknown", "<none>", "<unknown>", "", "<none>", "1.1.1.1", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
		{
			node: api.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "foo3"},
				Status: api.NodeStatus{Addresses: []api.NodeAddress{
					{Type: api.NodeInternalIP, Address: "2.2.2.2"},
					{Type: api.NodeExternalIP, Address: "3.3.3.3"},
					{Type: api.NodeInternalIP, Address: "4.4.4.4"},
				}},
			},
			// Columns: Name, Status, Roles, Age, KubeletVersion, NodeInternalIP, NodeExternalIP, OSImage, KernelVersion, ContainerRuntimeVersion
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"foo3", "Unknown", "<none>", "<unknown>", "", "2.2.2.2", "3.3.3.3", "<unknown>", "<unknown>", "<unknown>"},
				},
			},
		},
	}

	for i, test := range table {
		rows, err := printNode(&test.node, printers.GenerateOptions{Wide: true})
		if err != nil {
			t.Fatalf("An error occurred generating table rows Node: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintIngress(t *testing.T) {
	ingress := networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test1",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-10 * 365 * 24 * time.Hour))},
		},
		Spec: networking.IngressSpec{
			IngressClassName: ptr.To("foo"),
			DefaultBackend: &networking.IngressBackend{
				Service: &networking.IngressServiceBackend{
					Name: "default-backend",
					Port: networking.ServiceBackendPort{
						Name:   "default-backend",
						Number: 80,
					},
				},
			},
		},
		Status: networking.IngressStatus{
			LoadBalancer: networking.IngressLoadBalancerStatus{
				Ingress: []networking.IngressLoadBalancerIngress{
					{
						IP:       "2.3.4.5",
						Hostname: "localhost.localdomain",
					},
				},
			},
		},
	}
	// Columns: Name, Hosts, Address, Ports, Age
	expected := []metav1.TableRow{{Cells: []interface{}{"test1", "foo", "*", "2.3.4.5", "80", "10y"}}}

	rows, err := printIngress(&ingress, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error generating table rows for Ingress: %#v", err)
	}
	rows[0].Object.Object = nil
	if !reflect.DeepEqual(expected, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expected, rows))
	}
}

func TestPrintIngressClass(t *testing.T) {
	testCases := []struct {
		name         string
		ingressClass *networking.IngressClass
		expected     []metav1.TableRow
	}{{
		name: "example with params",
		ingressClass: &networking.IngressClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test1",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-10 * 365 * 24 * time.Hour))},
			},
			Spec: networking.IngressClassSpec{
				Controller: "example.com/controller",
				Parameters: &networking.IngressClassParametersReference{Kind: "customgroup", Name: "example"},
			},
		},
		expected: []metav1.TableRow{{Cells: []interface{}{"test1", "example.com/controller", "customgroup/example", "10y"}}},
	}, {
		name: "example with params + API Group",
		ingressClass: &networking.IngressClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test1",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-10 * 365 * 24 * time.Hour))},
			},
			Spec: networking.IngressClassSpec{
				Controller: "example.com/controller",
				Parameters: &networking.IngressClassParametersReference{
					APIGroup: ptr.To("example.com"),
					Kind:     "customgroup",
					Name:     "example",
				},
			},
		},
		expected: []metav1.TableRow{{Cells: []interface{}{"test1", "example.com/controller", "customgroup.example.com/example", "10y"}}},
	}, {
		name: "example without params",
		ingressClass: &networking.IngressClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test2",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-11 * 365 * 24 * time.Hour))},
			},
			Spec: networking.IngressClassSpec{
				Controller: "example.com/controller2",
			},
		},
		expected: []metav1.TableRow{{Cells: []interface{}{"test2", "example.com/controller2", "<none>", "11y"}}},
	}}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			rows, err := printIngressClass(testCase.ingressClass, printers.GenerateOptions{})
			if err != nil {
				t.Fatalf("Error generating table rows for Ingress: %#v", err)
			}
			for i := range rows {
				rows[i].Object.Object = nil
			}
			if !reflect.DeepEqual(testCase.expected, rows) {
				t.Errorf("mismatch: %s", cmp.Diff(testCase.expected, rows))
			}
		})
	}
}

func TestPrintServiceLoadBalancer(t *testing.T) {
	tests := []struct {
		service  api.Service
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		// Test load balancer service with multiple external IP's
		{
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "service1"},
				Spec: api.ServiceSpec{
					ClusterIPs: []string{"1.2.3.4"},
					Type:       "LoadBalancer",
					Ports:      []api.ServicePort{{Port: 80, Protocol: "TCP"}},
				},
				Status: api.ServiceStatus{
					LoadBalancer: api.LoadBalancerStatus{
						Ingress: []api.LoadBalancerIngress{{IP: "2.3.4.5"}, {IP: "3.4.5.6"}}},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"service1", "LoadBalancer", "1.2.3.4", "2.3.4.5,3.4.5.6", "80/TCP", "<unknown>"}}},
		},
		// Test load balancer service with pending external IP.
		{
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "service2"},
				Spec: api.ServiceSpec{
					ClusterIPs: []string{"1.3.4.5"},
					Type:       "LoadBalancer",
					Ports:      []api.ServicePort{{Port: 80, Protocol: "TCP"}, {Port: 8090, Protocol: "UDP"}, {Port: 8000, Protocol: "TCP"}, {Port: 7777, Protocol: "SCTP"}},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"service2", "LoadBalancer", "1.3.4.5", "<pending>", "80/TCP,8090/UDP,8000/TCP,7777/SCTP", "<unknown>"}}},
		},
		// Test load balancer service with multiple ports.
		{
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "service3"},
				Spec: api.ServiceSpec{
					ClusterIPs: []string{"1.4.5.6"},
					Type:       "LoadBalancer",
					Ports:      []api.ServicePort{{Port: 80, Protocol: "TCP"}, {Port: 8090, Protocol: "UDP"}, {Port: 8000, Protocol: "TCP"}},
				},
				Status: api.ServiceStatus{
					LoadBalancer: api.LoadBalancerStatus{
						Ingress: []api.LoadBalancerIngress{{IP: "2.3.4.5"}}},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"service3", "LoadBalancer", "1.4.5.6", "2.3.4.5", "80/TCP,8090/UDP,8000/TCP", "<unknown>"}}},
		},
		// Long external IP's list gets elided.
		{
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "service4"},
				Spec: api.ServiceSpec{
					ClusterIPs: []string{"1.5.6.7"},
					Type:       "LoadBalancer",
					Ports:      []api.ServicePort{{Port: 80, Protocol: "TCP"}, {Port: 8090, Protocol: "UDP"}, {Port: 8000, Protocol: "TCP"}},
				},
				Status: api.ServiceStatus{
					LoadBalancer: api.LoadBalancerStatus{
						Ingress: []api.LoadBalancerIngress{{IP: "2.3.4.5"}, {IP: "3.4.5.6"}, {IP: "5.6.7.8", Hostname: "host5678"}}},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"service4", "LoadBalancer", "1.5.6.7", "2.3.4.5,3.4.5...", "80/TCP,8090/UDP,8000/TCP", "<unknown>"}}},
		},
		// Generate options: Wide, includes selectors.
		{
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "service4"},
				Spec: api.ServiceSpec{
					ClusterIPs: []string{"1.5.6.7"},
					Type:       "LoadBalancer",
					Ports:      []api.ServicePort{{Port: 80, Protocol: "TCP"}, {Port: 8090, Protocol: "UDP"}, {Port: 8000, Protocol: "TCP"}},
					Selector:   map[string]string{"foo": "bar"},
				},
				Status: api.ServiceStatus{
					LoadBalancer: api.LoadBalancerStatus{
						Ingress: []api.LoadBalancerIngress{{IP: "2.3.4.5"}, {IP: "3.4.5.6"}, {IP: "5.6.7.8", Hostname: "host5678"}}},
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"service4", "LoadBalancer", "1.5.6.7", "2.3.4.5,3.4.5.6,5.6.7.8", "80/TCP,8090/UDP,8000/TCP", "<unknown>", "foo=bar"}}},
		},
	}

	for i, test := range tests {
		rows, err := printService(&test.service, test.options)
		if err != nil {
			t.Fatalf("Error printing table rows for Service: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPod(t *testing.T) {
	deleteTime := metav1.NewTime(time.Now().Add(-10 * time.Second))
	tests := []struct {
		pod    api.Pod
		expect []metav1.TableRow
	}{
		{
			// Test name, num of containers, restarts, container ready status
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "podPhase",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test1", "1/2", "podPhase", "6", "<unknown>"}}},
		},
		{
			// Test container error overwrites pod phase
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test2"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "podPhase",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{State: api.ContainerState{Waiting: &api.ContainerStateWaiting{Reason: "ContainerWaitingReason"}}, RestartCount: 3},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test2", "1/2", "ContainerWaitingReason", "6", "<unknown>"}}},
		},
		{
			// Test the same as the above but with Terminated state and the first container overwrites the rest
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test3"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "podPhase",
					ContainerStatuses: []api.ContainerStatus{
						{State: api.ContainerState{Waiting: &api.ContainerStateWaiting{Reason: "ContainerWaitingReason"}}, RestartCount: 3},
						{State: api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "ContainerTerminatedReason"}}, RestartCount: 3},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test3", "0/2", "ContainerWaitingReason", "6", "<unknown>"}}},
		},
		{
			// Test ready is not enough for reporting running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test4"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "podPhase",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{Ready: true, RestartCount: 3},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test4", "1/2", "podPhase", "6", "<unknown>"}}},
		},
		{
			// Test ready is not enough for reporting running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test5"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Reason: "podReason",
					Phase:  "podPhase",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{Ready: true, RestartCount: 3},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test5", "1/2", "podReason", "6", "<unknown>"}}},
		},
		{
			// Test pod has 2 containers, one is running and the other is completed, w/o ready condition
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test6"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase:  "Running",
					Reason: "",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}},
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test6", "1/2", "NotReady", "6", "<unknown>"}}},
		},
		{
			// Test pod has 2 containers, one is running and the other is completed, with ready condition
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test6"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase:  "Running",
					Reason: "",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}},
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
					},
					Conditions: []api.PodCondition{
						{Type: api.PodReady, Status: api.ConditionTrue, LastProbeTime: metav1.Time{Time: time.Now()}},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test6", "1/2", "Running", "6", "<unknown>"}}},
		},
		{
			// Test pod has 1 init container restarting and 1 container not running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test7"},
				Spec:       api.PodSpec{InitContainers: make([]api.Container, 1), Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "podPhase",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test7", "0/1", "Init:0/1", "3 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 init containers, one restarting and the other not running, and 1 container not running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test8"},
				Spec:       api.PodSpec{InitContainers: make([]api.Container, 2), Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "podPhase",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
						{
							Ready: false,
							State: api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready: false,
							State: api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test8", "0/1", "Init:0/2", "3 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 init containers, one completed without restarts and the other restarting, and 1 container not running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test9"},
				Spec:       api.PodSpec{InitContainers: make([]api.Container, 2), Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "podPhase",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Ready: false,
							State: api.ContainerState{Terminated: &api.ContainerStateTerminated{}},
						},
						{
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready: false,
							State: api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test9", "0/1", "Init:1/2", "3 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 init containers, one completed with restarts and the other restarting, and 1 container not running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test10"},
				Spec:       api.PodSpec{InitContainers: make([]api.Container, 2), Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "podPhase",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Ready:                false,
							RestartCount:         2,
							State:                api.ContainerState{Terminated: &api.ContainerStateTerminated{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-2 * time.Minute))}},
						},
						{
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready: false,
							State: api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test10", "0/1", "Init:1/2", "5 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 1 init container completed with restarts and one container restarting
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test11"},
				Spec:       api.PodSpec{InitContainers: make([]api.Container, 1), Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Running",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Ready:                false,
							RestartCount:         2,
							State:                api.ContainerState{Terminated: &api.ContainerStateTerminated{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-2 * time.Minute))}},
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:                false,
							RestartCount:         4,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-20 * time.Second))}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test11", "0/1", "Running", "4 (20s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 1 container that restarted 5d ago
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test12"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Running",
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:                true,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-5 * 24 * time.Hour))}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test12", "1/1", "Running", "3 (5d ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 containers, one has never restarted and the other has restarted 10d ago
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test13"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "Running",
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        true,
							RestartCount: 0,
							State:        api.ContainerState{Running: &api.ContainerStateRunning{}},
						},
						{
							Ready:                true,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test13", "2/2", "Running", "3 (10d ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 containers, one restarted 5d ago and the other restarted 20d ago
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test14"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "Running",
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:                true,
							RestartCount:         6,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-5 * 24 * time.Hour))}},
						},
						{
							Ready:                true,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-20 * 24 * time.Hour))}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test14", "2/2", "Running", "9 (5d ago)", "<unknown>"}}},
		},
		{
			// Test PodScheduled condition with reason SchedulingGated
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test15"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: "podPhase",
					Conditions: []api.PodCondition{
						{
							Type:   api.PodScheduled,
							Status: api.ConditionFalse,
							Reason: apiv1.PodReasonSchedulingGated,
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test15", "0/2", apiv1.PodReasonSchedulingGated, "0", "<unknown>"}}},
		},
		{
			// Test pod condition succeed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test16"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: api.PodSucceeded,
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}},
						},
					},
				},
			},
			[]metav1.TableRow{{Conditions: podSuccessConditions, Cells: []interface{}{"test16", "0/1", "Completed", "0", "<unknown>"}}},
		},
		{
			// Test pod condition failed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test17"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: api.PodFailed,
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
						},
					},
				},
			},
			[]metav1.TableRow{{Conditions: podFailedConditions, Cells: []interface{}{"test17", "0/1", "Error", "0", "<unknown>"}}},
		},
		{
			// Test pod condition succeed with deletion
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test18", DeletionTimestamp: &deleteTime},
				Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: api.PodSucceeded,
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}},
						},
					},
				},
			},
			[]metav1.TableRow{{Conditions: podSuccessConditions, Cells: []interface{}{"test18", "0/1", "Completed", "0", "<unknown>"}}},
		},
		{
			// Test pod condition running with deletion
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test19", DeletionTimestamp: &deleteTime},
				Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Running",
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Running: &api.ContainerStateRunning{}},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test19", "0/1", "Terminating", "0", "<unknown>"}}},
		},
		{ // Test pod condition pending with deletion
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test20", DeletionTimestamp: &deleteTime},
				Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Pending",
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test20", "0/1", "Terminating", "0", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPod(&test.pod, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expect, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expect, rows))
		}
	}
}

func TestPrintPodWithRestartableInitContainer(t *testing.T) {
	tests := []struct {
		pod    api.Pod
		expect []metav1.TableRow
	}{
		{
			// Test pod has 2 restartable init containers, the first one running but not started.
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: api.PodSpec{
					InitContainers: []api.Container{
						{Name: "restartable-init-1", RestartPolicy: &containerRestartPolicyAlways},
						{Name: "restartable-init-2", RestartPolicy: &containerRestartPolicyAlways},
					}, Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Pending",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Name:                 "restartable-init-1",
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							Started:              ptr.To(false),
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
						{
							Name:    "restartable-init-2",
							Ready:   false,
							State:   api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
							Started: ptr.To(false),
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
					Conditions: []api.PodCondition{
						{Type: api.PodInitialized, Status: api.ConditionFalse},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test1", "0/3", "Init:0/2", "3 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 restartable init containers, the first one started and the second one running but not started.
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: api.PodSpec{
					InitContainers: []api.Container{
						{Name: "restartable-init-1", RestartPolicy: &containerRestartPolicyAlways},
						{Name: "restartable-init-2", RestartPolicy: &containerRestartPolicyAlways},
					}, Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Pending",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Name:                 "restartable-init-1",
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							Started:              ptr.To(true),
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
						{
							Name:    "restartable-init-2",
							Ready:   false,
							State:   api.ContainerState{Running: &api.ContainerStateRunning{}},
							Started: ptr.To(false),
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:        false,
							RestartCount: 0,
							State:        api.ContainerState{Waiting: &api.ContainerStateWaiting{}},
						},
					},
					Conditions: []api.PodCondition{
						{Type: api.PodInitialized, Status: api.ConditionFalse},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test1", "0/3", "Init:1/2", "3 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 restartable init containers started and 1 container running
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test2"},
				Spec: api.PodSpec{
					InitContainers: []api.Container{
						{Name: "restartable-init-1", RestartPolicy: &containerRestartPolicyAlways},
						{Name: "restartable-init-2", RestartPolicy: &containerRestartPolicyAlways},
					}, Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Running",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Name:                 "restartable-init-1",
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							Started:              ptr.To(true),
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
						{
							Name:    "restartable-init-2",
							Ready:   false,
							State:   api.ContainerState{Running: &api.ContainerStateRunning{}},
							Started: ptr.To(true),
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:                true,
							RestartCount:         4,
							State:                api.ContainerState{Running: &api.ContainerStateRunning{}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-20 * time.Second))}},
						},
					},
					Conditions: []api.PodCondition{
						{Type: api.PodInitialized, Status: api.ConditionTrue},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test2", "1/3", "Running", "7 (10s ago)", "<unknown>"}}},
		},
		{
			// Test pod has 2 restartable init containers completed with non-zero and 1 container completed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test3"},
				Spec: api.PodSpec{
					InitContainers: []api.Container{
						{Name: "restartable-init-1", RestartPolicy: &containerRestartPolicyAlways},
						{Name: "restartable-init-2", RestartPolicy: &containerRestartPolicyAlways},
					}, Containers: make([]api.Container, 1)},
				Status: api.PodStatus{
					Phase: "Succeeded",
					InitContainerStatuses: []api.ContainerStatus{
						{
							Name:                 "restartable-init-1",
							Ready:                false,
							RestartCount:         3,
							State:                api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Error", ExitCode: 137}},
							Started:              ptr.To(false),
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second))}},
						},
						{
							Name:    "restartable-init-2",
							Ready:   false,
							State:   api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Error", ExitCode: 137}},
							Started: ptr.To(false),
						},
					},
					ContainerStatuses: []api.ContainerStatus{
						{
							Ready:                false,
							RestartCount:         4,
							State:                api.ContainerState{Terminated: &api.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}},
							LastTerminationState: api.ContainerState{Terminated: &api.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-20 * time.Second))}},
						},
					},
					Conditions: []api.PodCondition{
						{Type: api.PodInitialized, Status: api.ConditionTrue},
					},
				},
			},
			[]metav1.TableRow{
				{
					Cells: []interface{}{"test3", "0/3", "Completed", "7 (10s ago)", "<unknown>"},
					Conditions: []metav1.TableRowCondition{
						{Type: metav1.RowCompleted, Status: metav1.ConditionTrue, Reason: "Succeeded", Message: "The pod has completed successfully."},
					},
				},
			},
		},
		{
			// Test pod has container statuses for non-existent initContainers and containers
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test4"},
				Spec: api.PodSpec{
					InitContainers: []api.Container{
						{Name: "init1", Image: "initimage"},
						{Name: "sidecar1", Image: "sidecarimage", RestartPolicy: ptr.To(api.ContainerRestartPolicyAlways)},
					},
					Containers: []api.Container{{Name: "container1", Image: "containerimage"}},
				},
				Status: api.PodStatus{
					Phase: "Running",
					InitContainerStatuses: []api.ContainerStatus{
						{Name: "initinvalid"},
						{Name: "init1"},
						{Name: "sidecar1"},
					},
					ContainerStatuses: []api.ContainerStatus{
						{Name: "containerinvalid"},
						{Name: "container1"},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test4", "0/2", "Init:0/2", "0", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPod(&test.pod, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expect, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expect, rows))
		}
	}
}

func TestPrintPodwide(t *testing.T) {
	condition1 := "condition1"
	condition2 := "condition2"
	condition3 := "condition3"
	tests := []struct {
		pod    api.Pod
		expect []metav1.TableRow
	}{
		{
			// Test when the NodeName and PodIP are not none
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: api.PodSpec{
					Containers: make([]api.Container, 2),
					NodeName:   "test1",
					ReadinessGates: []api.PodReadinessGate{
						{
							ConditionType: api.PodConditionType(condition1),
						},
						{
							ConditionType: api.PodConditionType(condition2),
						},
						{
							ConditionType: api.PodConditionType(condition3),
						},
					},
				},
				Status: api.PodStatus{
					Conditions: []api.PodCondition{
						{
							Type:   api.PodConditionType(condition1),
							Status: api.ConditionFalse,
						},
						{
							Type:   api.PodConditionType(condition2),
							Status: api.ConditionTrue,
						},
					},
					Phase:  "podPhase",
					PodIPs: []api.PodIP{{IP: "1.1.1.1"}},
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
					NominatedNodeName: "node1",
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test1", "1/2", "podPhase", "6", "<unknown>", "1.1.1.1", "test1", "node1", "1/3"}}},
		},
		{
			// Test when the NodeName and PodIP are not none
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: api.PodSpec{
					Containers: make([]api.Container, 2),
					NodeName:   "test1",
					ReadinessGates: []api.PodReadinessGate{
						{
							ConditionType: api.PodConditionType(condition1),
						},
						{
							ConditionType: api.PodConditionType(condition2),
						},
						{
							ConditionType: api.PodConditionType(condition3),
						},
					},
				},
				Status: api.PodStatus{
					Conditions: []api.PodCondition{
						{
							Type:   api.PodConditionType(condition1),
							Status: api.ConditionFalse,
						},
						{
							Type:   api.PodConditionType(condition2),
							Status: api.ConditionTrue,
						},
					},
					Phase:  "podPhase",
					PodIPs: []api.PodIP{{IP: "1.1.1.1"}, {IP: "2001:db8::"}},
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
					NominatedNodeName: "node1",
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test1", "1/2", "podPhase", "6", "<unknown>", "1.1.1.1", "test1", "node1", "1/3"}}},
		},
		{
			// Test when the NodeName and PodIP are none
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test2"},
				Spec: api.PodSpec{
					Containers: make([]api.Container, 2),
					NodeName:   "",
				},
				Status: api.PodStatus{
					Phase: "podPhase",
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{State: api.ContainerState{Waiting: &api.ContainerStateWaiting{Reason: "ContainerWaitingReason"}}, RestartCount: 3},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test2", "1/2", "ContainerWaitingReason", "6", "<unknown>", "<none>", "<none>", "<none>", "<none>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPod(&test.pod, printers.GenerateOptions{Wide: true})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expect, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expect, rows))
		}
	}
}

func TestPrintPodConditions(t *testing.T) {
	runningPod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test1", Labels: map[string]string{"a": "1", "b": "2"}},
		Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
		Status: api.PodStatus{
			Phase: "Running",
			ContainerStatuses: []api.ContainerStatus{
				{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
				{RestartCount: 3},
			},
		},
	}
	succeededPod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test1", Labels: map[string]string{"a": "1", "b": "2"}},
		Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
		Status: api.PodStatus{
			Phase: "Succeeded",
			ContainerStatuses: []api.ContainerStatus{
				{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
				{RestartCount: 3},
			},
		},
	}
	failedPod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test2", Labels: map[string]string{"b": "2"}},
		Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
		Status: api.PodStatus{
			Phase: "Failed",
			ContainerStatuses: []api.ContainerStatus{
				{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
				{RestartCount: 3},
			},
		},
	}
	tests := []struct {
		pod    *api.Pod
		expect []metav1.TableRow
	}{
		// Should not have TableRowCondition
		{
			pod: runningPod,
			// Columns: Name, Ready, Reason, Restarts, Age
			expect: []metav1.TableRow{{Cells: []interface{}{"test1", "1/2", "Running", "6", "<unknown>"}}},
		},
		// Should have TableRowCondition: podSuccessConditions
		{
			pod: succeededPod,
			expect: []metav1.TableRow{
				{
					// Columns: Name, Ready, Reason, Restarts, Age
					Cells:      []interface{}{"test1", "1/2", "Succeeded", "6", "<unknown>"},
					Conditions: podSuccessConditions,
				},
			},
		},
		// Should have TableRowCondition: podFailedCondition
		{
			pod: failedPod,
			expect: []metav1.TableRow{
				{
					// Columns: Name, Ready, Reason, Restarts, Age
					Cells:      []interface{}{"test2", "1/2", "Failed", "6", "<unknown>"},
					Conditions: podFailedConditions,
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printPod(test.pod, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expect, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expect, rows))
		}
	}
}

func TestPrintPodList(t *testing.T) {
	tests := []struct {
		pods   api.PodList
		expect []metav1.TableRow
	}{
		// Test podList's pod: name, num of containers, restarts, container ready status
		{
			api.PodList{
				Items: []api.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "test1"},
						Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
						Status: api.PodStatus{
							Phase: "podPhase",
							ContainerStatuses: []api.ContainerStatus{
								{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
								{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "test2"},
						Spec:       api.PodSpec{Containers: make([]api.Container, 1)},
						Status: api.PodStatus{
							Phase: "podPhase",
							ContainerStatuses: []api.ContainerStatus{
								{Ready: true, RestartCount: 1, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
							},
						},
					},
				},
			},
			[]metav1.TableRow{{Cells: []interface{}{"test1", "2/2", "podPhase", "6", "<unknown>"}}, {Cells: []interface{}{"test2", "1/1", "podPhase", "1", "<unknown>"}}},
		},
	}

	for _, test := range tests {
		rows, err := printPodList(&test.pods, printers.GenerateOptions{})

		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expect, rows) {
			t.Errorf("mismatch: %s", cmp.Diff(test.expect, rows))
		}
	}
}

func TestPrintNonTerminatedPod(t *testing.T) {
	tests := []struct {
		pod    api.Pod
		expect []metav1.TableRow
	}{
		{
			// Test pod phase Running should be printed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: api.PodRunning,
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
				},
			},
			// Columns: Name, Ready, Reason, Restarts, Age
			[]metav1.TableRow{{Cells: []interface{}{"test1", "1/2", "Running", "6", "<unknown>"}}},
		},
		{
			// Test pod phase Pending should be printed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test2"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: api.PodPending,
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
				},
			},
			// Columns: Name, Ready, Reason, Restarts, Age
			[]metav1.TableRow{{Cells: []interface{}{"test2", "1/2", "Pending", "6", "<unknown>"}}},
		},
		{
			// Test pod phase Unknown should be printed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test3"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: api.PodUnknown,
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
				},
			},
			// Columns: Name, Ready, Reason, Restarts, Age
			[]metav1.TableRow{{Cells: []interface{}{"test3", "1/2", "Unknown", "6", "<unknown>"}}},
		},
		{
			// Test pod phase Succeeded should be printed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test4"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: api.PodSucceeded,
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{RestartCount: 3},
					},
				},
			},
			// Columns: Name, Ready, Reason, Restarts, Age
			[]metav1.TableRow{
				{
					Cells:      []interface{}{"test4", "1/2", "Succeeded", "6", "<unknown>"},
					Conditions: podSuccessConditions,
				},
			},
		},
		{
			// Test pod phase Failed shouldn't be printed
			api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test5"},
				Spec:       api.PodSpec{Containers: make([]api.Container, 2)},
				Status: api.PodStatus{
					Phase: api.PodFailed,
					ContainerStatuses: []api.ContainerStatus{
						{Ready: true, RestartCount: 3, State: api.ContainerState{Running: &api.ContainerStateRunning{}}},
						{Ready: true, RestartCount: 3},
					},
				},
			},
			// Columns: Name, Ready, Reason, Restarts, Age
			[]metav1.TableRow{
				{
					Cells:      []interface{}{"test5", "1/2", "Failed", "6", "<unknown>"},
					Conditions: podFailedConditions,
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printPod(&test.pod, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expect, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expect, rows))
		}
	}
}

func TestPrintPodTemplate(t *testing.T) {
	tests := []struct {
		podTemplate api.PodTemplate
		options     printers.GenerateOptions
		expected    []metav1.TableRow
	}{
		// Test basic pod template with no containers.
		{
			podTemplate: api.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-template-1"},
				Template: api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-template-1"},
					Spec: api.PodSpec{
						Containers: []api.Container{},
					},
				},
			},

			options: printers.GenerateOptions{},
			// Columns: Name, Containers, Images, Pod Labels
			expected: []metav1.TableRow{{Cells: []interface{}{"pod-template-1", "", "", "<none>"}}},
		},
		// Test basic pod template with two containers.
		{
			podTemplate: api.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-template-2"},
				Template: api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-template-2"},
					Spec: api.PodSpec{
						Containers: []api.Container{
							{
								Name:  "fake-container1",
								Image: "fake-image1",
							},
							{
								Name:  "fake-container2",
								Image: "fake-image2",
							},
						},
					},
				},
			},

			options: printers.GenerateOptions{},
			// Columns: Name, Containers, Images, Pod Labels
			expected: []metav1.TableRow{{Cells: []interface{}{"pod-template-2", "fake-container1,fake-container2", "fake-image1,fake-image2", "<none>"}}},
		},
		// Test basic pod template with pod labels
		{
			podTemplate: api.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-template-3"},
				Template: api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "pod-template-3",
						Labels: map[string]string{"foo": "bar"},
					},
					Spec: api.PodSpec{
						Containers: []api.Container{},
					},
				},
			},

			options: printers.GenerateOptions{},
			// Columns: Name, Containers, Images, Pod Labels
			expected: []metav1.TableRow{{Cells: []interface{}{"pod-template-3", "", "", "foo=bar"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPodTemplate(&test.podTemplate, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPodTemplateList(t *testing.T) {

	templateList := api.PodTemplateList{
		Items: []api.PodTemplate{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-template-1"},
				Template: api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "pod-template-2",
						Labels: map[string]string{"foo": "bar"},
					},
					Spec: api.PodSpec{
						Containers: []api.Container{},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-template-2"},
				Template: api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "pod-template-2",
						Labels: map[string]string{"a": "b"},
					},
					Spec: api.PodSpec{
						Containers: []api.Container{},
					},
				},
			},
		},
	}

	// Columns: Name, Containers, Images, Pod Labels
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"pod-template-1", "", "", "foo=bar"}},
		{Cells: []interface{}{"pod-template-2", "", "", "a=b"}},
	}

	rows, err := printPodTemplateList(&templateList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing pod template list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

type stringTestList []struct {
	name, got, exp string
}

func TestTranslateTimestampSince(t *testing.T) {
	tl := stringTestList{
		{"a while from now", translateTimestampSince(metav1.Time{Time: time.Now().Add(2.1e9)}), "<invalid>"},
		{"almost now", translateTimestampSince(metav1.Time{Time: time.Now().Add(1.9e9)}), "0s"},
		{"now", translateTimestampSince(metav1.Time{Time: time.Now()}), "0s"},
		{"unknown", translateTimestampSince(metav1.Time{}), "<unknown>"},
		{"30 seconds ago", translateTimestampSince(metav1.Time{Time: time.Now().Add(-3e10)}), "30s"},
		{"5 minutes ago", translateTimestampSince(metav1.Time{Time: time.Now().Add(-3e11)}), "5m"},
		{"an hour ago", translateTimestampSince(metav1.Time{Time: time.Now().Add(-6e12)}), "100m"},
		{"2 days ago", translateTimestampSince(metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -2)}), "2d"},
		{"months ago", translateTimestampSince(metav1.Time{Time: time.Now().UTC().AddDate(0, 0, -90)}), "90d"},
		{"10 years ago", translateTimestampSince(metav1.Time{Time: time.Now().UTC().Add(time.Duration(-10 * 365 * 24 * time.Hour))}), "10y"},
	}
	for _, test := range tl {
		if test.got != test.exp {
			t.Errorf("On %v, expected '%v', but got '%v'",
				test.name, test.exp, test.got)
		}
	}
}

func TestTranslateTimestampUntil(t *testing.T) {
	// Since this method compares the time with time.Now() internally,
	// small buffers of 0.1 seconds are added on comparing times to consider method call overhead.
	// Otherwise, the output strings become shorter than expected.
	const buf = 1e8
	tl := stringTestList{
		{"a while ago", translateTimestampUntil(metav1.Time{Time: time.Now().Add(-2.1e9)}), "<invalid>"},
		{"almost now", translateTimestampUntil(metav1.Time{Time: time.Now().Add(-1.9e9)}), "0s"},
		{"now", translateTimestampUntil(metav1.Time{Time: time.Now()}), "0s"},
		{"unknown", translateTimestampUntil(metav1.Time{}), "<unknown>"},
		{"in 30 seconds", translateTimestampUntil(metav1.Time{Time: time.Now().Add(3e10 + buf)}), "30s"},
		{"in 5 minutes", translateTimestampUntil(metav1.Time{Time: time.Now().Add(3e11 + buf)}), "5m"},
		{"in an hour", translateTimestampUntil(metav1.Time{Time: time.Now().Add(6e12 + buf)}), "100m"},
		{"in 2 days", translateTimestampUntil(metav1.Time{Time: time.Now().UTC().AddDate(0, 0, 2).Add(buf)}), "2d"},
		{"in months", translateTimestampUntil(metav1.Time{Time: time.Now().UTC().AddDate(0, 0, 90).Add(buf)}), "90d"},
		{"in 10 years", translateTimestampUntil(metav1.Time{Time: time.Now().UTC().Add(time.Duration(10 * 365 * 24 * time.Hour)).Add(buf)}), "10y"},
	}
	for _, test := range tl {
		if test.got != test.exp {
			t.Errorf("On %v, expected '%v', but got '%v'",
				test.name, test.exp, test.got)
		}
	}
}

func TestPrintDeployment(t *testing.T) {

	testDeployment := apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test1",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
		},
		Spec: apps.DeploymentSpec{
			Replicas: 5,
			Template: api.PodTemplateSpec{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "fake-container1",
							Image: "fake-image1",
						},
						{
							Name:  "fake-container2",
							Image: "fake-image2",
						},
					},
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
		},
		Status: apps.DeploymentStatus{
			Replicas:            10,
			UpdatedReplicas:     2,
			AvailableReplicas:   1,
			UnavailableReplicas: 4,
		},
	}

	tests := []struct {
		deployment apps.Deployment
		options    printers.GenerateOptions
		expected   []metav1.TableRow
	}{
		// Test Deployment with no generate options.
		{
			deployment: testDeployment,
			options:    printers.GenerateOptions{},
			// Columns: Name, ReadyReplicas, UpdatedReplicas, AvailableReplicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "0/5", int64(2), int64(1), "0s"}}},
		},
		// Test generate options: Wide.
		{
			deployment: testDeployment,
			options:    printers.GenerateOptions{Wide: true},
			// Columns: Name, ReadyReplicas, UpdatedReplicas, AvailableReplicas, Age, Containers, Images, Selectors
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "0/5", int64(2), int64(1), "0s", "fake-container1,fake-container2", "fake-image1,fake-image2", "foo=bar"}}},
		},
	}

	for i, test := range tests {
		rows, err := printDeployment(&test.deployment, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintDaemonSet(t *testing.T) {

	testDaemonSet := apps.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test1",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
		},
		Spec: apps.DaemonSetSpec{
			Template: api.PodTemplateSpec{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "fake-container1",
							Image: "fake-image1",
						},
						{
							Name:  "fake-container2",
							Image: "fake-image2",
						},
					},
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
		},
		Status: apps.DaemonSetStatus{
			CurrentNumberScheduled: 2,
			DesiredNumberScheduled: 3,
			NumberReady:            1,
			UpdatedNumberScheduled: 2,
			NumberAvailable:        0,
		},
	}

	tests := []struct {
		daemonSet apps.DaemonSet
		options   printers.GenerateOptions
		expected  []metav1.TableRow
	}{
		// Test generate daemon set with no generate options.
		{
			daemonSet: testDaemonSet,
			options:   printers.GenerateOptions{},
			// Columns: Name, Num Desired, Num Current, Num Ready, Num Updated, Num Available, Selectors, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", int64(3), int64(2), int64(1), int64(2), int64(0), "<none>", "0s"}}},
		},
		// Test generate daemon set with "Wide" generate options.
		{
			daemonSet: testDaemonSet,
			options:   printers.GenerateOptions{Wide: true},
			// Columns: Name, Num Desired, Num Current, Num Ready, Num Updated, Num Available, Node Selectors, Age, Containers, Images, Labels
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", int64(3), int64(2), int64(1), int64(2), int64(0), "<none>", "0s", "fake-container1,fake-container2", "fake-image1,fake-image2", "foo=bar"}}},
		},
	}

	for i, test := range tests {
		rows, err := printDaemonSet(&test.daemonSet, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintDaemonSetList(t *testing.T) {

	daemonSetList := apps.DaemonSetList{
		Items: []apps.DaemonSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "daemonset1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.DaemonSetSpec{
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-container1",
									Image: "fake-image1",
								},
								{
									Name:  "fake-container2",
									Image: "fake-image2",
								},
							},
						},
					},
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					UpdatedNumberScheduled: 2,
					NumberAvailable:        0,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "daemonset2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.DaemonSetSpec{
					Template: api.PodTemplateSpec{},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 4,
					DesiredNumberScheduled: 2,
					NumberReady:            9,
					UpdatedNumberScheduled: 3,
					NumberAvailable:        3,
				},
			},
		},
	}

	// Columns: Name, Num Desired, Num Current, Num Ready, Num Updated, Num Available, Selectors, Age
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"daemonset1", int64(3), int64(2), int64(1), int64(2), int64(0), "<none>", "0s"}},
		{Cells: []interface{}{"daemonset2", int64(2), int64(4), int64(9), int64(3), int64(3), "<none>", "0s"}},
	}

	rows, err := printDaemonSetList(&daemonSetList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing daemon set list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

func TestPrintJob(t *testing.T) {
	now := time.Now()
	completions := int32(2)
	tests := []struct {
		job      batch.Job
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		{
			// Generate table rows for Job with no generate options.
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: &completions,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-job-container1",
									Image: "fake-job-image1",
								},
								{
									Name:  "fake-job-container2",
									Image: "fake-job-image2",
								},
							},
						},
					},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-label": "job-lable-value"}},
				},
				Status: batch.JobStatus{
					Succeeded: 1,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job1", "Running", "1/2", "", "0s"}}},
		},
		// Generate table rows for Job with generate options "Wide".
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: &completions,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-job-container1",
									Image: "fake-job-image1",
								},
								{
									Name:  "fake-job-container2",
									Image: "fake-job-image2",
								},
							},
						},
					},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-label": "job-label-value"}},
				},
				Status: batch.JobStatus{
					Succeeded: 1,
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Status, Completions, Duration, Age, Containers, Images, Selectors
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"job1", "Running", "1/2", "", "0s", "fake-job-container1,fake-job-container2", "fake-job-image1,fake-job-image2", "job-label=job-label-value"},
				},
			},
		},
		// Job with ten-year age.
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job2",
					CreationTimestamp: metav1.Time{Time: time.Now().AddDate(-10, 0, 0)},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded: 0,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job2", "Running", "0/1", "", "10y"}}},
		},
		// Job with duration.
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-10 * 365 * 24 * time.Hour))},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded:      0,
					StartTime:      &metav1.Time{Time: now.Add(time.Minute)},
					CompletionTime: &metav1.Time{Time: now.Add(31 * time.Minute)},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job3", "Running", "0/1", "30m", "10y"}}},
		},
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job4",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-10 * 365 * 24 * time.Hour))},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded: 0,
					StartTime: &metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job4", "Running", "0/1", "20m", "10y"}}},
		},
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job5",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded: 0,
					Conditions: []batch.JobCondition{
						{
							Type:   batch.JobComplete,
							Status: api.ConditionTrue,
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job5", "Complete", "0/1", "", "0s"}}},
		},
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job6",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded: 0,
					Conditions: []batch.JobCondition{
						{
							Type:   batch.JobFailed,
							Status: api.ConditionTrue,
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job6", "Failed", "0/1", "", "0s"}}},
		},
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job7",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded: 0,
					Conditions: []batch.JobCondition{
						{
							Type:   batch.JobSuspended,
							Status: api.ConditionTrue,
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job7", "Suspended", "0/1", "", "0s"}}},
		},
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job8",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
				Status: batch.JobStatus{
					Succeeded: 0,
					Conditions: []batch.JobCondition{
						{
							Type:   batch.JobFailureTarget,
							Status: api.ConditionTrue,
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job8", "FailureTarget", "0/1", "", "0s"}}},
		},
		{
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job9",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
					DeletionTimestamp: &metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: nil,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Status, Completions, Duration, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"job9", "Terminating", "0/1", "", "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printJob(&test.job, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintJobList(t *testing.T) {
	completions := int32(2)
	jobList := batch.JobList{
		Items: []batch.Job{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: &completions,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-job-container1",
									Image: "fake-job-image1",
								},
								{
									Name:  "fake-job-container2",
									Image: "fake-job-image2",
								},
							},
						},
					},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-label": "job-lable-value"}},
				},
				Status: batch.JobStatus{
					Succeeded: 1,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "job2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.JobSpec{
					Completions: &completions,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-job-container1",
									Image: "fake-job-image1",
								},
								{
									Name:  "fake-job-container2",
									Image: "fake-job-image2",
								},
							},
						},
					},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-label": "job-lable-value"}},
				},
				Status: batch.JobStatus{
					Succeeded: 2,
					StartTime: &metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
				},
			},
		},
	}

	// Columns: Name, Status, Completions, Duration, Age
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"job1", "Running", "1/2", "", "0s"}},
		{Cells: []interface{}{"job2", "Running", "2/2", "20m", "0s"}},
	}

	rows, err := printJobList(&jobList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing job list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

func TestPrintHPA(t *testing.T) {
	minReplicasVal := int32(2)
	targetUtilizationVal := int32(80)
	currentUtilizationVal := int32(50)
	metricLabelSelector, err := metav1.ParseToLabelSelector("label=value")
	if err != nil {
		t.Errorf("unable to parse label selector: %v", err)
	}
	tests := []struct {
		hpa      autoscaling.HorizontalPodAutoscaler
		expected []metav1.TableRow
	}{
		// minReplicas unset
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MaxReplicas: 10,
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "<none>", "<unset>", int64(10), int64(4), "<unknown>"}}},
		},
		// external source type, target average value (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ExternalMetricSourceType,
							External: &autoscaling.ExternalMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name:     "some-external-metric",
									Selector: metricLabelSelector,
								},
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "<unknown>/100m (avg)", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// external source type, target average value
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ExternalMetricSourceType,
							External: &autoscaling.ExternalMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name:     "some-external-metric",
									Selector: metricLabelSelector,
								},
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ExternalMetricSourceType,
							External: &autoscaling.ExternalMetricStatus{
								Metric: autoscaling.MetricIdentifier{
									Name:     "some-external-metric",
									Selector: metricLabelSelector,
								},
								Current: autoscaling.MetricValueStatus{
									AverageValue: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "50m/100m (avg)", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// external source type, target value (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ExternalMetricSourceType,
							External: &autoscaling.ExternalMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name:     "some-service-metric",
									Selector: metricLabelSelector,
								},
								Target: autoscaling.MetricTarget{
									Type:  autoscaling.ValueMetricType,
									Value: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "<unknown>/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// external source type, target value
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ExternalMetricSourceType,
							External: &autoscaling.ExternalMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name:     "some-external-metric",
									Selector: metricLabelSelector,
								},
								Target: autoscaling.MetricTarget{
									Type:  autoscaling.ValueMetricType,
									Value: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ExternalMetricSourceType,
							External: &autoscaling.ExternalMetricStatus{
								Metric: autoscaling.MetricIdentifier{
									Name: "some-external-metric",
								},
								Current: autoscaling.MetricValueStatus{
									Value: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "50m/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// pods source type (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.PodsMetricSourceType,
							Pods: &autoscaling.PodsMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name: "some-pods-metric",
								},
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "<unknown>/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// pods source type
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.PodsMetricSourceType,
							Pods: &autoscaling.PodsMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name: "some-pods-metric",
								},
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.PodsMetricSourceType,
							Pods: &autoscaling.PodsMetricStatus{
								Metric: autoscaling.MetricIdentifier{
									Name: "some-pods-metric",
								},
								Current: autoscaling.MetricValueStatus{
									AverageValue: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "50m/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// object source type (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ObjectMetricSourceType,
							Object: &autoscaling.ObjectMetricSource{
								DescribedObject: autoscaling.CrossVersionObjectReference{
									Name: "some-service",
									Kind: "Service",
								},
								Metric: autoscaling.MetricIdentifier{
									Name: "some-service-metric",
								},
								Target: autoscaling.MetricTarget{
									Type:  autoscaling.ValueMetricType,
									Value: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "<unknown>/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// object source type
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ObjectMetricSourceType,
							Object: &autoscaling.ObjectMetricSource{
								DescribedObject: autoscaling.CrossVersionObjectReference{
									Name: "some-service",
									Kind: "Service",
								},
								Metric: autoscaling.MetricIdentifier{
									Name: "some-service-metric",
								},
								Target: autoscaling.MetricTarget{
									Type:  autoscaling.ValueMetricType,
									Value: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ObjectMetricSourceType,
							Object: &autoscaling.ObjectMetricStatus{
								DescribedObject: autoscaling.CrossVersionObjectReference{
									Name: "some-service",
									Kind: "Service",
								},
								Metric: autoscaling.MetricIdentifier{
									Name: "some-service-metric",
								},
								Current: autoscaling.MetricValueStatus{
									Value: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "50m/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// resource source type, targetVal (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: <unknown>/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// resource source type, targetVal
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricStatus{
								Name: api.ResourceCPU,
								Current: autoscaling.MetricValueStatus{
									AverageValue: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: 50m/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// resource source type, targetUtil (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: &targetUtilizationVal,
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: <unknown>/80%", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// resource source type, targetUtil
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: &targetUtilizationVal,
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricStatus{
								Name: api.ResourceCPU,
								Current: autoscaling.MetricValueStatus{
									AverageUtilization: &currentUtilizationVal,
									AverageValue:       resource.NewMilliQuantity(40, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: 50%/80%", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// container resource source type, targetVal (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ContainerResourceMetricSourceType,
							ContainerResource: &autoscaling.ContainerResourceMetricSource{
								Name:      api.ResourceCPU,
								Container: "application",
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: <unknown>/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// container resource source type, targetVal
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ContainerResourceMetricSourceType,
							ContainerResource: &autoscaling.ContainerResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ContainerResourceMetricSourceType,
							ContainerResource: &autoscaling.ContainerResourceMetricStatus{
								Name:      api.ResourceCPU,
								Container: "application",
								Current: autoscaling.MetricValueStatus{
									AverageValue: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: 50m/100m", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// container resource source type, targetUtil (no current)
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ContainerResourceMetricSourceType,
							ContainerResource: &autoscaling.ContainerResourceMetricSource{
								Name:      api.ResourceCPU,
								Container: "application",
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: &targetUtilizationVal,
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: <unknown>/80%", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// container resource source type, targetUtil
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ContainerResourceMetricSourceType,
							ContainerResource: &autoscaling.ContainerResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: &targetUtilizationVal,
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.ContainerResourceMetricSourceType,
							ContainerResource: &autoscaling.ContainerResourceMetricStatus{
								Name:      api.ResourceCPU,
								Container: "application",
								Current: autoscaling.MetricValueStatus{
									AverageUtilization: &currentUtilizationVal,
									AverageValue:       resource.NewMilliQuantity(40, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "cpu: 50%/80%", "2", int64(10), int64(4), "<unknown>"}}},
		},
		// multiple specs
		{
			hpa: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "some-hpa"},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{
						Name: "some-rc",
						Kind: "ReplicationController",
					},
					MinReplicas: &minReplicasVal,
					MaxReplicas: 10,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.PodsMetricSourceType,
							Pods: &autoscaling.PodsMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name: "some-pods-metric",
								},
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
								},
							},
						},
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: &targetUtilizationVal,
								},
							},
						},
						{
							Type: autoscaling.PodsMetricSourceType,
							Pods: &autoscaling.PodsMetricSource{
								Metric: autoscaling.MetricIdentifier{
									Name: "other-pods-metric",
								},
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(400, resource.DecimalSI),
								},
							},
						},
					},
				},
				Status: autoscaling.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 4,
					DesiredReplicas: 5,
					CurrentMetrics: []autoscaling.MetricStatus{
						{
							Type: autoscaling.PodsMetricSourceType,
							Pods: &autoscaling.PodsMetricStatus{
								Metric: autoscaling.MetricIdentifier{
									Name: "some-pods-metric",
								},
								Current: autoscaling.MetricValueStatus{
									AverageValue: resource.NewMilliQuantity(50, resource.DecimalSI),
								},
							},
						},
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricStatus{
								Name: api.ResourceCPU,
								Current: autoscaling.MetricValueStatus{
									AverageUtilization: &currentUtilizationVal,
									AverageValue:       resource.NewMilliQuantity(40, resource.DecimalSI),
								},
							},
						},
					},
				},
			},
			// Columns: Name, Reference, Targets, MinPods, MaxPods, Replicas, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"some-hpa", "ReplicationController/some-rc", "50m/100m, cpu: 50%/80% + 1 more...", "2", int64(10), int64(4), "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printHorizontalPodAutoscaler(&test.hpa, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintService(t *testing.T) {
	singleExternalIP := []string{"80.11.12.10"}
	mulExternalIP := []string{"80.11.12.10", "80.11.12.11"}
	tests := []struct {
		service  api.Service
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		{
			// Test name, cluster ip, port with protocol
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeClusterIP,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     2233,
						},
					},
					ClusterIPs: []string{"10.9.8.7"},
					Selector:   map[string]string{"foo": "bar"}, // Does NOT get printed.
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "ClusterIP", "10.9.8.7", "<none>", "2233/tcp", "<unknown>"}}},
		},
		{
			// Test generate options: Wide includes selectors.
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeClusterIP,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     2233,
						},
					},
					ClusterIPs: []string{"10.9.8.7"},
					Selector:   map[string]string{"foo": "bar"},
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age, Selector
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "ClusterIP", "10.9.8.7", "<none>", "2233/tcp", "<unknown>", "foo=bar"}}},
		},
		{
			// Test NodePort service
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test2"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeNodePort,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     8888,
							NodePort: 9999,
						},
					},
					ClusterIPs: []string{"10.9.8.7"},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test2", "NodePort", "10.9.8.7", "<none>", "8888:9999/tcp", "<unknown>"}}},
		},
		{
			// Test LoadBalancer service
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test3"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeLoadBalancer,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     8888,
						},
					},
					ClusterIPs: []string{"10.9.8.7"},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test3", "LoadBalancer", "10.9.8.7", "<pending>", "8888/tcp", "<unknown>"}}},
		},
		{
			// Test LoadBalancer service with single ExternalIP and no LoadBalancerStatus
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test4"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeLoadBalancer,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     8888,
						},
					},
					ClusterIPs:  []string{"10.9.8.7"},
					ExternalIPs: singleExternalIP,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test4", "LoadBalancer", "10.9.8.7", "80.11.12.10", "8888/tcp", "<unknown>"}}},
		},
		{
			// Test LoadBalancer service with single ExternalIP
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test5"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeLoadBalancer,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     8888,
						},
					},
					ClusterIPs:  []string{"10.9.8.7"},
					ExternalIPs: singleExternalIP,
				},
				Status: api.ServiceStatus{
					LoadBalancer: api.LoadBalancerStatus{
						Ingress: []api.LoadBalancerIngress{
							{
								IP:       "3.4.5.6",
								Hostname: "test.cluster.com",
							},
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test5", "LoadBalancer", "10.9.8.7", "3.4.5.6,80.11.12.10", "8888/tcp", "<unknown>"}}},
		},
		{
			// Test LoadBalancer service with mul ExternalIPs
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test6"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeLoadBalancer,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     8888,
						},
					},
					ClusterIPs:  []string{"10.9.8.7"},
					ExternalIPs: mulExternalIP,
				},
				Status: api.ServiceStatus{
					LoadBalancer: api.LoadBalancerStatus{
						Ingress: []api.LoadBalancerIngress{
							{
								IP:       "2.3.4.5",
								Hostname: "test.cluster.local",
							},
							{
								IP:       "3.4.5.6",
								Hostname: "test.cluster.com",
							},
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test6", "LoadBalancer", "10.9.8.7", "2.3.4.5,3.4.5.6,80.11.12.10,80.11.12.11", "8888/tcp", "<unknown>"}}},
		},
		{
			// Test ExternalName service
			service: api.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test7"},
				Spec: api.ServiceSpec{
					Type:         api.ServiceTypeExternalName,
					ExternalName: "my.database.example.com",
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test7", "ExternalName", "<none>", "my.database.example.com", "<none>", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printService(&test.service, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintServiceList(t *testing.T) {
	serviceList := api.ServiceList{
		Items: []api.Service{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "service1"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeClusterIP,
					Ports: []api.ServicePort{
						{
							Protocol: "tcp",
							Port:     2233,
						},
					},
					ClusterIPs: []string{"10.9.8.7"},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "service2"},
				Spec: api.ServiceSpec{
					Type: api.ServiceTypeNodePort,
					Ports: []api.ServicePort{
						{
							Protocol: "udp",
							Port:     5566,
						},
					},
					ClusterIPs: []string{"1.2.3.4"},
				},
			},
		},
	}

	// Columns: Name, Type, Cluster-IP, External-IP, Port(s), Age
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"service1", "ClusterIP", "10.9.8.7", "<none>", "2233/tcp", "<unknown>"}},
		{Cells: []interface{}{"service2", "NodePort", "1.2.3.4", "<none>", "5566/udp", "<unknown>"}},
	}

	rows, err := printServiceList(&serviceList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing service list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

func TestPrintPodDisruptionBudget(t *testing.T) {
	minAvailable := intstr.FromInt32(22)
	maxUnavailable := intstr.FromInt32(11)
	tests := []struct {
		pdb      policy.PodDisruptionBudget
		expected []metav1.TableRow
	}{
		// Min Available set, no Max Available.
		{
			pdb: policy.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         "ns1",
					Name:              "pdb1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: policy.PodDisruptionBudgetSpec{
					MinAvailable: &minAvailable,
				},
				Status: policy.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 5,
				},
			},
			// Columns: Name, Min Available, Max Available, Allowed Disruptions, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"pdb1", "22", "N/A", int64(5), "0s"}}},
		},
		// Max Available set, no Min Available.
		{
			pdb: policy.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         "ns2",
					Name:              "pdb2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: policy.PodDisruptionBudgetSpec{
					MaxUnavailable: &maxUnavailable,
				},
				Status: policy.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 5,
				},
			},
			// Columns: Name, Min Available, Max Available, Allowed Disruptions, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"pdb2", "N/A", "11", int64(5), "0s"}}},
		}}

	for i, test := range tests {
		rows, err := printPodDisruptionBudget(&test.pdb, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPodDisruptionBudgetList(t *testing.T) {
	minAvailable := intstr.FromInt32(22)
	maxUnavailable := intstr.FromInt32(11)

	pdbList := policy.PodDisruptionBudgetList{
		Items: []policy.PodDisruptionBudget{
			{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         "ns1",
					Name:              "pdb1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: policy.PodDisruptionBudgetSpec{
					MaxUnavailable: &maxUnavailable,
				},
				Status: policy.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 5,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         "ns2",
					Name:              "pdb2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: policy.PodDisruptionBudgetSpec{
					MinAvailable: &minAvailable,
				},
				Status: policy.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 3,
				},
			},
		},
	}

	// Columns: Name, Min Available, Max Available, Allowed Disruptions, Age
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"pdb1", "N/A", "11", int64(5), "0s"}},
		{Cells: []interface{}{"pdb2", "22", "N/A", int64(3), "0s"}},
	}

	rows, err := printPodDisruptionBudgetList(&pdbList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing pod template list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

func TestPrintControllerRevision(t *testing.T) {
	tests := []struct {
		history  apps.ControllerRevision
		expected []metav1.TableRow
	}{
		{
			history: apps.ControllerRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
					OwnerReferences: []metav1.OwnerReference{
						{
							Controller: boolP(true),
							APIVersion: "apps/v1",
							Kind:       "DaemonSet",
							Name:       "foo",
						},
					},
				},
				Revision: 1,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "daemonset.apps/foo", int64(1), "0s"}}},
		},
		{
			history: apps.ControllerRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
					OwnerReferences: []metav1.OwnerReference{
						{
							Controller: boolP(false),
							Kind:       "ABC",
							Name:       "foo",
						},
					},
				},
				Revision: 2,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test2", "<none>", int64(2), "0s"}}},
		},
		{
			history: apps.ControllerRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
					OwnerReferences:   []metav1.OwnerReference{},
				},
				Revision: 3,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test3", "<none>", int64(3), "0s"}}},
		},
		{
			history: apps.ControllerRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test4",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
					OwnerReferences:   nil,
				},
				Revision: 4,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test4", "<none>", int64(4), "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printControllerRevision(&test.history, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func boolP(b bool) *bool {
	return &b
}

func TestPrintConfigMap(t *testing.T) {
	tests := []struct {
		configMap api.ConfigMap
		expected  []metav1.TableRow
	}{
		// Basic config map with no data.
		{
			configMap: api.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "configmap1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
			},
			// Columns: Name, Data, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"configmap1", int64(0), "0s"}}},
		},
		// Basic config map with one data entry
		{
			configMap: api.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "configmap2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Data: map[string]string{
					"foo": "bar",
				},
			},
			// Columns: Name, (Num) Data, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"configmap2", int64(1), "0s"}}},
		},
		// Basic config map with one data and one binary data entry.
		{
			configMap: api.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "configmap3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Data: map[string]string{
					"foo": "bar",
				},
				BinaryData: map[string][]byte{
					"bin": []byte("binary data"),
				},
			},
			// Columns: Name, (Num) Data, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"configmap3", int64(2), "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printConfigMap(&test.configMap, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintNetworkPolicy(t *testing.T) {
	tests := []struct {
		policy   networking.NetworkPolicy
		expected []metav1.TableRow
	}{
		// Basic network policy with empty spec.
		{
			policy: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "policy1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: networking.NetworkPolicySpec{},
			},
			// Columns: Name, Pod-Selector, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"policy1", "<none>", "0s"}}},
		},
		// Basic network policy with pod selector.
		{
			policy: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "policy2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				},
			},
			// Columns: Name, Pod-Selector, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"policy2", "foo=bar", "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printNetworkPolicy(&test.policy, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintRoleBinding(t *testing.T) {
	tests := []struct {
		binding  rbac.RoleBinding
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		// Basic role binding
		{
			binding: rbac.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "binding1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Subjects: []rbac.Subject{
					{
						Kind: "User",
						Name: "system:kube-controller-manager",
					},
				},
				RoleRef: rbac.RoleRef{
					Kind: "Role",
					Name: "extension-apiserver-authentication-reader",
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"binding1", "Role/extension-apiserver-authentication-reader", "0s"}}},
		},
		// Generate options=Wide; print subject and roles.
		{
			binding: rbac.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "binding2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Subjects: []rbac.Subject{
					{
						Kind: "User",
						Name: "user-name",
					},
					{
						Kind: "Group",
						Name: "group-name",
					},
					{
						Kind:      "ServiceAccount",
						Name:      "service-account-name",
						Namespace: "service-account-namespace",
					},
				},
				RoleRef: rbac.RoleRef{
					Kind: "Role",
					Name: "role-name",
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Age, Role, Users, Groups, ServiceAccounts
			expected: []metav1.TableRow{{Cells: []interface{}{"binding2", "Role/role-name", "0s", "user-name", "group-name", "service-account-namespace/service-account-name"}}},
		},
	}

	for i, test := range tests {
		rows, err := printRoleBinding(&test.binding, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintClusterRoleBinding(t *testing.T) {
	tests := []struct {
		binding  rbac.ClusterRoleBinding
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		// Basic cluster role binding
		{
			binding: rbac.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "binding1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Subjects: []rbac.Subject{
					{
						Kind: "User",
						Name: "system:kube-controller-manager",
					},
				},
				RoleRef: rbac.RoleRef{
					Kind: "Role",
					Name: "extension-apiserver-authentication-reader",
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"binding1", "Role/extension-apiserver-authentication-reader", "0s"}}},
		},
		// Generate options=Wide; print subject and roles.
		{
			binding: rbac.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "binding2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Subjects: []rbac.Subject{
					{
						Kind: "User",
						Name: "user-name",
					},
					{
						Kind: "Group",
						Name: "group-name",
					},
					{
						Kind:      "ServiceAccount",
						Name:      "service-account-name",
						Namespace: "service-account-namespace",
					},
				},
				RoleRef: rbac.RoleRef{
					Kind: "Role",
					Name: "role-name",
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Age, Role, Users, Groups, ServiceAccounts
			expected: []metav1.TableRow{{Cells: []interface{}{"binding2", "Role/role-name", "0s", "user-name", "group-name", "service-account-namespace/service-account-name"}}},
		},
	}

	for i, test := range tests {
		rows, err := printClusterRoleBinding(&test.binding, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}
func TestPrintCertificateSigningRequest(t *testing.T) {
	tests := []struct {
		csr      certificates.CertificateSigningRequest
		expected []metav1.TableRow
	}{
		// Basic CSR with no spec or status; defaults to status: Pending.
		{
			csr: certificates.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "csr1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec:   certificates.CertificateSigningRequestSpec{},
				Status: certificates.CertificateSigningRequestStatus{},
			},
			// Columns: Name, Age, SignerName, Requestor, RequestedDuration, Condition
			expected: []metav1.TableRow{{Cells: []interface{}{"csr1", "0s", "<none>", "", "<none>", "Pending"}}},
		},
		// Basic CSR with Spec and Status=Approved.
		{
			csr: certificates.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "csr2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: certificates.CertificateSigningRequestSpec{
					Username: "CSR Requestor",
				},
				Status: certificates.CertificateSigningRequestStatus{
					Conditions: []certificates.CertificateSigningRequestCondition{
						{
							Type: certificates.CertificateApproved,
						},
					},
				},
			},
			// Columns: Name, Age, SignerName, Requestor, RequestedDuration, Condition
			expected: []metav1.TableRow{{Cells: []interface{}{"csr2", "0s", "<none>", "CSR Requestor", "<none>", "Approved"}}},
		},
		// Basic CSR with Spec and SignerName set
		{
			csr: certificates.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "csr2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: certificates.CertificateSigningRequestSpec{
					Username:   "CSR Requestor",
					SignerName: "example.com/test-signer",
				},
				Status: certificates.CertificateSigningRequestStatus{
					Conditions: []certificates.CertificateSigningRequestCondition{
						{
							Type: certificates.CertificateApproved,
						},
					},
				},
			},
			// Columns: Name, Age, SignerName, Requestor, RequestedDuration, Condition
			expected: []metav1.TableRow{{Cells: []interface{}{"csr2", "0s", "example.com/test-signer", "CSR Requestor", "<none>", "Approved"}}},
		},
		// Basic CSR with Spec, SignerName and ExpirationSeconds set
		{
			csr: certificates.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "csr2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: certificates.CertificateSigningRequestSpec{
					Username:          "CSR Requestor",
					SignerName:        "example.com/test-signer",
					ExpirationSeconds: csr.DurationToExpirationSeconds(7*24*time.Hour + time.Hour), // a little bit more than a week
				},
				Status: certificates.CertificateSigningRequestStatus{
					Conditions: []certificates.CertificateSigningRequestCondition{
						{
							Type: certificates.CertificateApproved,
						},
					},
				},
			},
			// Columns: Name, Age, SignerName, Requestor, RequestedDuration, Condition
			expected: []metav1.TableRow{{Cells: []interface{}{"csr2", "0s", "example.com/test-signer", "CSR Requestor", "7d1h", "Approved"}}},
		},
		// Basic CSR with Spec and Status=Approved; certificate issued.
		{
			csr: certificates.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "csr2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: certificates.CertificateSigningRequestSpec{
					Username: "CSR Requestor",
				},
				Status: certificates.CertificateSigningRequestStatus{
					Conditions: []certificates.CertificateSigningRequestCondition{
						{
							Type: certificates.CertificateApproved,
						},
					},
					Certificate: []byte("cert data"),
				},
			},
			// Columns: Name, Age, SignerName, Requestor, RequestedDuration, Condition
			expected: []metav1.TableRow{{Cells: []interface{}{"csr2", "0s", "<none>", "CSR Requestor", "<none>", "Approved,Issued"}}},
		},
		// Basic CSR with Spec and Status=Denied.
		{
			csr: certificates.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "csr3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: certificates.CertificateSigningRequestSpec{
					Username: "CSR Requestor",
				},
				Status: certificates.CertificateSigningRequestStatus{
					Conditions: []certificates.CertificateSigningRequestCondition{
						{
							Type: certificates.CertificateDenied,
						},
					},
				},
			},
			// Columns: Name, Age, SignerName, Requestor, RequestedDuration, Condition
			expected: []metav1.TableRow{{Cells: []interface{}{"csr3", "0s", "<none>", "CSR Requestor", "<none>", "Denied"}}},
		},
	}

	for i, test := range tests {
		rows, err := printCertificateSigningRequest(&test.csr, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintReplicationController(t *testing.T) {
	tests := []struct {
		rc       api.ReplicationController
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		// Basic print replication controller without replicas or status.
		{
			rc: api.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rc1",
					Namespace: "test-namespace",
				},
				Spec: api.ReplicationControllerSpec{
					Selector: map[string]string{"a": "b"},
					Template: &api.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"a": "b"},
						},
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:                     "test",
									Image:                    "test_image",
									ImagePullPolicy:          api.PullIfNotPresent,
									TerminationMessagePolicy: api.TerminationMessageReadFile,
								},
							},
							RestartPolicy: api.RestartPolicyAlways,
							DNSPolicy:     api.DNSClusterFirst,
						},
					},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Desired, Current, Ready, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"rc1", int64(0), int64(0), int64(0), "<unknown>"}}},
		},
		// Basic print replication controller with replicas; does not print containers or labels
		{
			rc: api.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rc1",
					Namespace: "test-namespace",
				},
				Spec: api.ReplicationControllerSpec{
					Replicas: 5,
					Selector: map[string]string{"a": "b"},
					Template: &api.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"a": "b"},
						},
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:                     "test",
									Image:                    "test_image",
									ImagePullPolicy:          api.PullIfNotPresent,
									TerminationMessagePolicy: api.TerminationMessageReadFile,
								},
							},
							RestartPolicy: api.RestartPolicyAlways,
							DNSPolicy:     api.DNSClusterFirst,
						},
					},
				},
				Status: api.ReplicationControllerStatus{
					Replicas:      3,
					ReadyReplicas: 1,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Desired, Current, Ready, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"rc1", int64(5), int64(3), int64(1), "<unknown>"}}},
		},
		// Generate options: Wide; print containers and labels.
		{
			rc: api.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{
					Name: "rc1",
				},
				Spec: api.ReplicationControllerSpec{
					Replicas: 5,
					Selector: map[string]string{"a": "b"},
					Template: &api.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"a": "b"},
						},
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:                     "test",
									Image:                    "test_image",
									ImagePullPolicy:          api.PullIfNotPresent,
									TerminationMessagePolicy: api.TerminationMessageReadFile,
								},
							},
							RestartPolicy: api.RestartPolicyAlways,
							DNSPolicy:     api.DNSClusterFirst,
						},
					},
				},
				Status: api.ReplicationControllerStatus{
					Replicas:      3,
					ReadyReplicas: 1,
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Desired, Current, Ready, Age, Containers, Images, Selector
			expected: []metav1.TableRow{{Cells: []interface{}{"rc1", int64(5), int64(3), int64(1), "<unknown>", "test", "test_image", "a=b"}}},
		},
		{
			// make sure Bookmark event will not lead a panic
			rc: api.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						metav1.InitialEventsAnnotationKey: "true",
					},
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Desired, Current, Ready, Age, Containers, Images, Selector
			expected: []metav1.TableRow{{Cells: []interface{}{"", int64(0), int64(0), int64(0), "<unknown>", "", "", "<none>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printReplicationController(&test.rc, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintReplicaSet(t *testing.T) {
	tests := []struct {
		replicaSet apps.ReplicaSet
		options    printers.GenerateOptions
		expected   []metav1.TableRow
	}{
		// Generate options empty
		{
			replicaSet: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.ReplicaSetSpec{
					Replicas: 5,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-container1",
									Image: "fake-image1",
								},
								{
									Name:  "fake-container2",
									Image: "fake-image2",
								},
							},
						},
					},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				},
				Status: apps.ReplicaSetStatus{
					Replicas:      5,
					ReadyReplicas: 2,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Desired, Current, Ready, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", int64(5), int64(5), int64(2), "0s"}}},
		},
		// Generate options "Wide"
		{
			replicaSet: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.ReplicaSetSpec{
					Replicas: 5,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-container1",
									Image: "fake-image1",
								},
								{
									Name:  "fake-container2",
									Image: "fake-image2",
								},
							},
						},
					},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				},
				Status: apps.ReplicaSetStatus{
					Replicas:      5,
					ReadyReplicas: 2,
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Desired, Current, Ready, Age, Containers, Images, Selector
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", int64(5), int64(5), int64(2), "0s", "fake-container1,fake-container2", "fake-image1,fake-image2", "foo=bar"}}},
		},
	}

	for i, test := range tests {
		rows, err := printReplicaSet(&test.replicaSet, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintReplicaSetList(t *testing.T) {

	replicaSetList := apps.ReplicaSetList{
		Items: []apps.ReplicaSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "replicaset1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.ReplicaSetSpec{
					Replicas: 5,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-container1",
									Image: "fake-image1",
								},
								{
									Name:  "fake-container2",
									Image: "fake-image2",
								},
							},
						},
					},
				},
				Status: apps.ReplicaSetStatus{
					Replicas:      5,
					ReadyReplicas: 2,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "replicaset2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.ReplicaSetSpec{
					Replicas: 4,
					Template: api.PodTemplateSpec{},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				},
				Status: apps.ReplicaSetStatus{
					Replicas:      3,
					ReadyReplicas: 1,
				},
			},
		},
	}

	// Columns: Name, Desired, Current, Ready, Age
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"replicaset1", int64(5), int64(5), int64(2), "0s"}},
		{Cells: []interface{}{"replicaset2", int64(4), int64(3), int64(1), "0s"}},
	}

	rows, err := printReplicaSetList(&replicaSetList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing replica set list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

func TestPrintStatefulSet(t *testing.T) {
	tests := []struct {
		statefulSet apps.StatefulSet
		options     printers.GenerateOptions
		expected    []metav1.TableRow
	}{
		// Basic stateful set; no generate options.
		{
			statefulSet: apps.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.StatefulSetSpec{
					Replicas: 5,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-container1",
									Image: "fake-image1",
								},
								{
									Name:  "fake-container2",
									Image: "fake-image2",
								},
							},
						},
					},
				},
				Status: apps.StatefulSetStatus{
					Replicas:      5,
					ReadyReplicas: 2,
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Ready, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "2/5", "0s"}}},
		},
		// Generate options "Wide"; includes containers and images.
		{
			statefulSet: apps.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: apps.StatefulSetSpec{
					Replicas: 5,
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:  "fake-container1",
									Image: "fake-image1",
								},
								{
									Name:  "fake-container2",
									Image: "fake-image2",
								},
							},
						},
					},
				},
				Status: apps.StatefulSetStatus{
					Replicas:      5,
					ReadyReplicas: 2,
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Ready, Age, Containers, Images
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "2/5", "0s", "fake-container1,fake-container2", "fake-image1,fake-image2"}}},
		},
	}

	for i, test := range tests {
		rows, err := printStatefulSet(&test.statefulSet, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPersistentVolume(t *testing.T) {
	myScn := "my-scn"
	myVacn := "my-vacn"

	claimRef := api.ObjectReference{
		Name:      "test",
		Namespace: "default",
	}
	tests := []struct {
		pv       api.PersistentVolume
		expected []metav1.TableRow
	}{
		{
			// Test bound
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test1",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:    &claimRef,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadOnlyMany},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("4Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumeBound,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "4Gi", "ROX", "", "Bound", "default/test", "", "<unset>", "", "<unknown>", "<unset>"}}},
		},
		{
			// Test failed
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test2",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:    &claimRef,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadOnlyMany},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("4Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumeFailed,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test2", "4Gi", "ROX", "", "Failed", "default/test", "", "<unset>", "", "<unknown>", "<unset>"}}},
		},
		{
			// Test pending
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test3",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:    &claimRef,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteMany},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumePending,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test3", "10Gi", "RWX", "", "Pending", "default/test", "", "<unset>", "", "<unknown>", "<unset>"}}},
		},
		{
			// Test pending, storageClass
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test4",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:         &claimRef,
					StorageClassName: myScn,
					AccessModes:      []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumePending,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test4", "10Gi", "RWO", "", "Pending", "default/test", "my-scn", "<unset>", "", "<unknown>", "<unset>"}}},
		},
		{
			// Test pending, storageClass, volumeAttributesClass
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test4",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:                  &claimRef,
					StorageClassName:          myScn,
					VolumeAttributesClassName: &myVacn,
					AccessModes:               []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumePending,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test4", "10Gi", "RWO", "", "Pending", "default/test", "my-scn", "my-vacn", "", "<unknown>", "<unset>"}}},
		},
		{
			// Test available
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test5",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:         &claimRef,
					StorageClassName: myScn,
					AccessModes:      []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumeAvailable,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test5", "10Gi", "RWO", "", "Available", "default/test", "my-scn", "<unset>", "", "<unknown>", "<unset>"}}},
		},
		{
			// Test released
			pv: api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test6",
				},
				Spec: api.PersistentVolumeSpec{
					ClaimRef:         &claimRef,
					StorageClassName: myScn,
					AccessModes:      []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
				Status: api.PersistentVolumeStatus{
					Phase: api.VolumeReleased,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test6", "10Gi", "RWO", "", "Released", "default/test", "my-scn", "<unset>", "", "<unknown>", "<unset>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPersistentVolume(&test.pv, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPersistentVolumeClaim(t *testing.T) {
	volumeMode := api.PersistentVolumeFilesystem
	myVacn := "my-vacn"
	myScn := "my-scn"
	tests := []struct {
		pvc      api.PersistentVolumeClaim
		expected []metav1.TableRow
	}{
		{
			// Test name, num of containers, restarts, container ready status
			pvc: api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test1",
				},
				Spec: api.PersistentVolumeClaimSpec{
					VolumeName: "my-volume",
					VolumeMode: &volumeMode,
				},
				Status: api.PersistentVolumeClaimStatus{
					Phase:       api.ClaimBound,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadOnlyMany},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("4Gi"),
					},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", "Bound", "my-volume", "4Gi", "ROX", "", "<unset>", "<unknown>", "Filesystem"}}},
		},
		{
			// Test name, num of containers, restarts, container ready status
			pvc: api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test2",
				},
				Spec: api.PersistentVolumeClaimSpec{
					VolumeMode: &volumeMode,
				},
				Status: api.PersistentVolumeClaimStatus{
					Phase:       api.ClaimLost,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadOnlyMany},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("4Gi"),
					},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test2", "Lost", "", "", "", "", "<unset>", "<unknown>", "Filesystem"}}},
		},
		{
			// Test name, num of containers, restarts, container ready status
			pvc: api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test3",
				},
				Spec: api.PersistentVolumeClaimSpec{
					VolumeName: "my-volume",
					VolumeMode: &volumeMode,
				},
				Status: api.PersistentVolumeClaimStatus{
					Phase:       api.ClaimPending,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteMany},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test3", "Pending", "my-volume", "10Gi", "RWX", "", "<unset>", "<unknown>", "Filesystem"}}},
		},
		{
			// Test name, num of containers, restarts, container ready status
			pvc: api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test4",
				},
				Spec: api.PersistentVolumeClaimSpec{
					VolumeName:       "my-volume",
					StorageClassName: &myScn,
					VolumeMode:       &volumeMode,
				},
				Status: api.PersistentVolumeClaimStatus{
					Phase:       api.ClaimPending,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test4", "Pending", "my-volume", "10Gi", "RWO", "my-scn", "<unset>", "<unknown>", "Filesystem"}}},
		},
		{
			// Test name, num of containers, restarts, container ready status
			pvc: api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test5",
				},
				Spec: api.PersistentVolumeClaimSpec{
					VolumeName:       "my-volume",
					StorageClassName: &myScn,
				},
				Status: api.PersistentVolumeClaimStatus{
					Phase:       api.ClaimPending,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test5", "Pending", "my-volume", "10Gi", "RWO", "my-scn", "<unset>", "<unknown>", "<unset>"}}},
		},
		{
			// Test name, num of containers, restarts, container ready status
			pvc: api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test5",
				},
				Spec: api.PersistentVolumeClaimSpec{
					VolumeName:                "my-volume",
					StorageClassName:          &myScn,
					VolumeAttributesClassName: &myVacn,
				},
				Status: api.PersistentVolumeClaimStatus{
					Phase:       api.ClaimPending,
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					Capacity: map[api.ResourceName]resource.Quantity{
						api.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"test5", "Pending", "my-volume", "10Gi", "RWO", "my-scn", "my-vacn", "<unknown>", "<unset>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPersistentVolumeClaim(&test.pvc, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintComponentStatus(t *testing.T) {
	tests := []struct {
		componentStatus api.ComponentStatus
		expected        []metav1.TableRow
	}{
		// Basic component status without conditions
		{
			componentStatus: api.ComponentStatus{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cs1",
				},
				Conditions: []api.ComponentCondition{},
			},
			// Columns: Name, Status, Message, Error
			expected: []metav1.TableRow{{Cells: []interface{}{"cs1", "Unknown", "", ""}}},
		},
		// Basic component status with healthy condition.
		{
			componentStatus: api.ComponentStatus{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cs2",
				},
				Conditions: []api.ComponentCondition{
					{
						Type:    "Healthy",
						Status:  api.ConditionTrue,
						Message: "test message",
						Error:   "test error",
					},
				},
			},
			// Columns: Name, Status, Message, Error
			expected: []metav1.TableRow{{Cells: []interface{}{"cs2", "Healthy", "test message", "test error"}}},
		},
		// Basic component status with healthy condition.
		{
			componentStatus: api.ComponentStatus{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cs3",
				},
				Conditions: []api.ComponentCondition{
					{
						Type:    "Healthy",
						Status:  api.ConditionFalse,
						Message: "test message",
						Error:   "test error",
					},
				},
			},
			// Columns: Name, Status, Message, Error
			expected: []metav1.TableRow{{Cells: []interface{}{"cs3", "Unhealthy", "test message", "test error"}}},
		},
	}

	for i, test := range tests {
		rows, err := printComponentStatus(&test.componentStatus, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintCronJob(t *testing.T) {
	timeZone := "LOCAL"
	completions := int32(2)
	suspend := false
	tests := []struct {
		cronjob  batch.CronJob
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		// Basic cron job; does not print containers, images, or labels.
		{
			cronjob: batch.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "0/5 * * * ?",
					Suspend:  &suspend,
					JobTemplate: batch.JobTemplateSpec{
						Spec: batch.JobSpec{
							Completions: &completions,
							Template: api.PodTemplateSpec{
								Spec: api.PodSpec{
									Containers: []api.Container{
										{
											Name:  "fake-job-container1",
											Image: "fake-job-image1",
										},
										{
											Name:  "fake-job-container2",
											Image: "fake-job-image2",
										},
									},
								},
							},
							Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
						},
					},
				},
				Status: batch.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: time.Now().Add(1.9e9)},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Schedule, Suspend, Active, Last Schedule, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"cronjob1", "0/5 * * * ?", "<none>", "False", int64(0), "0s", "0s"}}},
		},
		// Basic cron job; does not print containers, images, or labels.
		{
			cronjob: batch.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "0/5 * * * ?",
					TimeZone: &timeZone,
					Suspend:  &suspend,
					JobTemplate: batch.JobTemplateSpec{
						Spec: batch.JobSpec{
							Completions: &completions,
							Template: api.PodTemplateSpec{
								Spec: api.PodSpec{
									Containers: []api.Container{
										{
											Name:  "fake-job-container1",
											Image: "fake-job-image1",
										},
										{
											Name:  "fake-job-container2",
											Image: "fake-job-image2",
										},
									},
								},
							},
							Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
						},
					},
				},
				Status: batch.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: time.Now().Add(1.9e9)},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Schedule, Suspend, Active, Last Schedule, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"cronjob1", "0/5 * * * ?", "LOCAL", "False", int64(0), "0s", "0s"}}},
		},
		// Generate options: Wide; prints containers, images, and labels.
		{
			cronjob: batch.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "0/5 * * * ?",
					Suspend:  &suspend,
					JobTemplate: batch.JobTemplateSpec{
						Spec: batch.JobSpec{
							Completions: &completions,
							Template: api.PodTemplateSpec{
								Spec: api.PodSpec{
									Containers: []api.Container{
										{
											Name:  "fake-job-container1",
											Image: "fake-job-image1",
										},
										{
											Name:  "fake-job-container2",
											Image: "fake-job-image2",
										},
									},
								},
							},
							Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
						},
					},
				},
				Status: batch.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: time.Now().Add(1.9e9)},
				},
			},
			options: printers.GenerateOptions{Wide: true},
			// Columns: Name, Schedule, Suspend, Active, Last Schedule, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"cronjob1", "0/5 * * * ?", "<none>", "False", int64(0), "0s", "0s", "fake-job-container1,fake-job-container2", "fake-job-image1,fake-job-image2", "a=b"}}},
		},
		// CronJob with Last Schedule and Age
		{
			cronjob: batch.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "0/5 * * * ?",
					Suspend:  &suspend,
				},
				Status: batch.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: time.Now().Add(-3e10)},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Schedule, Suspend, Active, Last Schedule, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"cronjob2", "0/5 * * * ?", "<none>", "False", int64(0), "30s", "5m"}}},
		},
		// CronJob without Last Schedule
		{
			cronjob: batch.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "0/5 * * * ?",
					Suspend:  &suspend,
				},
				Status: batch.CronJobStatus{},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, Schedule, Suspend, Active, Last Schedule, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"cronjob3", "0/5 * * * ?", "<none>", "False", int64(0), "<none>", "5m"}}},
		},
	}

	for i, test := range tests {
		rows, err := printCronJob(&test.cronjob, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintCronJobList(t *testing.T) {
	timeZone := "LOCAL"
	completions := int32(2)
	suspend := false

	cronJobList := batch.CronJobList{
		Items: []batch.CronJob{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "0/5 * * * ?",
					TimeZone: &timeZone,
					Suspend:  &suspend,
					JobTemplate: batch.JobTemplateSpec{
						Spec: batch.JobSpec{
							Completions: &completions,
							Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
						},
					},
				},
				Status: batch.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: time.Now().Add(1.9e9)},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cronjob2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: batch.CronJobSpec{
					Schedule: "4/5 1 1 1 ?",
					Suspend:  &suspend,
					JobTemplate: batch.JobTemplateSpec{
						Spec: batch.JobSpec{
							Completions: &completions,
							Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
						},
					},
				},
				Status: batch.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
				},
			},
		},
	}

	// Columns: Name, Schedule, Suspend, Active, Last Schedule, Age
	expectedRows := []metav1.TableRow{
		{Cells: []interface{}{"cronjob1", "0/5 * * * ?", "LOCAL", "False", int64(0), "0s", "0s"}},
		{Cells: []interface{}{"cronjob2", "4/5 1 1 1 ?", "<none>", "False", int64(0), "20m", "0s"}},
	}

	rows, err := printCronJobList(&cronJobList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error printing job list: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil
	}
	if !reflect.DeepEqual(expectedRows, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expectedRows, rows))
	}
}

func TestPrintStorageClass(t *testing.T) {
	policyDelte := api.PersistentVolumeReclaimDelete
	policyRetain := api.PersistentVolumeReclaimRetain
	bindModeImmediate := storage.VolumeBindingImmediate
	bindModeWait := storage.VolumeBindingWaitForFirstConsumer
	tests := []struct {
		sc       storage.StorageClass
		expected []metav1.TableRow
	}{
		{
			sc: storage.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sc1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Provisioner: "kubernetes.io/glusterfs",
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"sc1", "kubernetes.io/glusterfs", "Delete",
				"Immediate", false, "0s"}}},
		},
		{
			sc: storage.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sc2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Provisioner: "kubernetes.io/nfs",
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"sc2", "kubernetes.io/nfs", "Delete",
				"Immediate", false, "5m"}}},
		},
		{
			sc: storage.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sc3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Provisioner:   "kubernetes.io/nfs",
				ReclaimPolicy: &policyDelte,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"sc3", "kubernetes.io/nfs", "Delete",
				"Immediate", false, "5m"}}},
		},
		{
			sc: storage.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sc4",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Provisioner:       "kubernetes.io/nfs",
				ReclaimPolicy:     &policyRetain,
				VolumeBindingMode: &bindModeImmediate,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"sc4", "kubernetes.io/nfs", "Retain",
				"Immediate", false, "5m"}}},
		},
		{
			sc: storage.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sc5",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Provisioner:       "kubernetes.io/nfs",
				ReclaimPolicy:     &policyRetain,
				VolumeBindingMode: &bindModeWait,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"sc5", "kubernetes.io/nfs", "Retain",
				"WaitForFirstConsumer", false, "5m"}}},
		},
		{
			sc: storage.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sc6",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Provisioner:          "kubernetes.io/nfs",
				ReclaimPolicy:        &policyRetain,
				AllowVolumeExpansion: boolP(true),
				VolumeBindingMode:    &bindModeWait,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"sc6", "kubernetes.io/nfs", "Retain",
				"WaitForFirstConsumer", true, "5m"}}},
		},
	}

	for i, test := range tests {
		rows, err := printStorageClass(&test.sc, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintVolumeAttributesClass(t *testing.T) {
	tests := []struct {
		vac      storage.VolumeAttributesClass
		expected []metav1.TableRow
	}{
		{
			vac: storage.VolumeAttributesClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "vac1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				DriverName: "fake",
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"vac1", "fake", "0s"}}},
		},
		{
			vac: storage.VolumeAttributesClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "vac2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				DriverName: "fake",
				Parameters: map[string]string{
					"iops":       "500",
					"throughput": "50MiB/s",
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"vac2", "fake", "5m"}}},
		},
	}

	for i, test := range tests {
		rows, err := printVolumeAttributesClass(&test.vac, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintLease(t *testing.T) {
	holder1 := "holder1"
	holder2 := "holder2"
	fixedTimeStrAndLayout := "2006-01-02 15:04:05"
	fixedTime, _ := time.Parse(fixedTimeStrAndLayout, fixedTimeStrAndLayout)
	tests := []struct {
		options  printers.GenerateOptions
		lease    coordination.Lease
		expected []metav1.TableRow
	}{
		{
			lease: coordination.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "lease1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: coordination.LeaseSpec{
					HolderIdentity: &holder1,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"lease1", "holder1", "0s"}}},
		},
		{
			lease: coordination.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "lease2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: coordination.LeaseSpec{
					HolderIdentity: &holder2,
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"lease2", "holder2", "5m"}}},
		},
		{
			options: printers.GenerateOptions{Wide: true},
			lease: coordination.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "lease3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: coordination.LeaseSpec{
					HolderIdentity: &holder2,
					RenewTime:      &metav1.MicroTime{Time: fixedTime},
				},
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"lease3", "holder2", "5m", "2006-01-02 15:04:05"}}},
		},
	}

	for i, test := range tests {
		rows, err := printLease(&test.lease, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPriorityClass(t *testing.T) {
	preemptNever := api.PreemptNever
	preemptLowerPriority := api.PreemptLowerPriority
	tests := []struct {
		name     string
		options  printers.GenerateOptions
		pc       scheduling.PriorityClass
		expected []metav1.TableRow
	}{
		{
			name: "Test case with PreemptNever policy",
			pc: scheduling.PriorityClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pc1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Value:            1,
				PreemptionPolicy: &preemptNever,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"pc1", int64(1), bool(false), "0s"}}},
		},
		{
			name: "Test case with PreemptLowerPriority policy",
			pc: scheduling.PriorityClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pc2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Value:            1000000000,
				GlobalDefault:    true,
				PreemptionPolicy: &preemptLowerPriority,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"pc2", int64(1000000000), bool(true), "5m"}}},
		},
		{
			name:    "Test case with PreemptLowerPriority policy and wide output",
			options: printers.GenerateOptions{Wide: true},
			pc: scheduling.PriorityClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pc2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Value:            1000000000,
				GlobalDefault:    true,
				PreemptionPolicy: &preemptLowerPriority,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"pc2", int64(1000000000), bool(true), "5m", string(api.PreemptLowerPriority)}}},
		},
		{
			name:    "Test case without set PreemptLowerPriority policy",
			options: printers.GenerateOptions{Wide: true},
			pc: scheduling.PriorityClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pc2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Value:            1000000000,
				GlobalDefault:    true,
				PreemptionPolicy: nil,
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"pc2", int64(1000000000), bool(true), "5m", string("")}}},
		},
	}

	for i, test := range tests {
		rows, err := printPriorityClass(&test.pc, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("test case %s: %d mismatch: %s", test.name, i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintRuntimeClass(t *testing.T) {
	tests := []struct {
		rc       nodeapi.RuntimeClass
		expected []metav1.TableRow
	}{
		{
			rc: nodeapi.RuntimeClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "rc1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Handler: "h1",
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"rc1", "h1", "0s"}}},
		},
		{
			rc: nodeapi.RuntimeClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "rc2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Handler: "h2",
			},
			expected: []metav1.TableRow{{Cells: []interface{}{"rc2", "h2", "5m"}}},
		},
	}

	for i, test := range tests {
		rows, err := printRuntimeClass(&test.rc, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintEndpoint(t *testing.T) {

	tests := []struct {
		endpoint api.Endpoints
		expected []metav1.TableRow
	}{
		// Basic endpoint with no IP's
		{
			endpoint: api.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "endpoint1",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
			},
			// Columns: Name, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"endpoint1", "<none>", "0s"}}},
		},
		// Endpoint with no ports
		{
			endpoint: api.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "endpoint3",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Subsets: []api.EndpointSubset{
					{
						Addresses: []api.EndpointAddress{
							{
								IP: "1.2.3.4",
							},
							{
								IP: "5.6.7.8",
							},
						},
					},
				},
			},
			// Columns: Name, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"endpoint3", "1.2.3.4,5.6.7.8", "5m"}}},
		},
		// Basic endpoint with two IP's and one port
		{
			endpoint: api.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "endpoint2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Subsets: []api.EndpointSubset{
					{
						Addresses: []api.EndpointAddress{
							{
								IP: "1.2.3.4",
							},
							{
								IP: "5.6.7.8",
							},
						},
						Ports: []api.EndpointPort{
							{
								Port:     8001,
								Protocol: "tcp",
							},
						},
					},
				},
			},
			// Columns: Name, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"endpoint2", "1.2.3.4:8001,5.6.7.8:8001", "0s"}}},
		},
		// Basic endpoint with greater than three IP's triggering "more" string
		{
			endpoint: api.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "endpoint2",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Subsets: []api.EndpointSubset{
					{
						Addresses: []api.EndpointAddress{
							{
								IP: "1.2.3.4",
							},
							{
								IP: "5.6.7.8",
							},
							{
								IP: "9.8.7.6",
							},
							{
								IP: "6.6.6.6",
							},
						},
						Ports: []api.EndpointPort{
							{
								Port:     8001,
								Protocol: "tcp",
							},
						},
					},
				},
			},
			// Columns: Name, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"endpoint2", "1.2.3.4:8001,5.6.7.8:8001,9.8.7.6:8001 + 1 more...", "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printEndpoints(&test.endpoint, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}

}

func TestPrintEndpointSlice(t *testing.T) {
	tcpProtocol := api.ProtocolTCP

	tests := []struct {
		endpointSlice discovery.EndpointSlice
		expected      []metav1.TableRow
	}{
		{
			endpointSlice: discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "abcslice.123",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				AddressType: discovery.AddressTypeIPv4,
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To("http"),
					Port:     ptr.To[int32](80),
					Protocol: &tcpProtocol,
				}},
				Endpoints: []discovery.Endpoint{{
					Addresses: []string{"10.1.2.3", "2001:db8::1234:5678"},
				}},
			},
			// Columns: Name, AddressType, Ports, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"abcslice.123", "IPv4", "80", "10.1.2.3,2001:db8::1234:5678", "0s"}}},
		}, {
			endpointSlice: discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "longerslicename.123",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				AddressType: discovery.AddressTypeIPv6,
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To("http"),
					Port:     ptr.To[int32](80),
					Protocol: &tcpProtocol,
				}, {
					Name:     ptr.To("https"),
					Port:     ptr.To[int32](443),
					Protocol: &tcpProtocol,
				}},
				Endpoints: []discovery.Endpoint{{
					Addresses: []string{"10.1.2.3", "2001:db8::1234:5678"},
				}, {
					Addresses: []string{"10.2.3.4", "2001:db8::2345:6789"},
				}},
			},
			// Columns: Name, AddressType, Ports, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"longerslicename.123", "IPv6", "80,443", "10.1.2.3,2001:db8::1234:5678,10.2.3.4 + 1 more...", "5m"}}},
		}, {
			endpointSlice: discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "multiportslice.123",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				AddressType: discovery.AddressTypeIPv4,
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To("http"),
					Port:     ptr.To[int32](80),
					Protocol: &tcpProtocol,
				}, {
					Name:     ptr.To("https"),
					Port:     ptr.To[int32](443),
					Protocol: &tcpProtocol,
				}, {
					Name:     ptr.To("extra1"),
					Port:     ptr.To[int32](3000),
					Protocol: &tcpProtocol,
				}, {
					Name:     ptr.To("extra2"),
					Port:     ptr.To[int32](3001),
					Protocol: &tcpProtocol,
				}},
				Endpoints: []discovery.Endpoint{{
					Addresses: []string{"10.1.2.3", "2001:db8::1234:5678"},
				}, {
					Addresses: []string{"10.2.3.4", "2001:db8::2345:6789"},
				}},
			},
			// Columns: Name, AddressType, Ports, Endpoints, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"multiportslice.123", "IPv4", "80,443,3000 + 1 more...", "10.1.2.3,2001:db8::1234:5678,10.2.3.4 + 1 more...", "5m"}}},
		},
	}

	for i, test := range tests {
		rows, err := printEndpointSlice(&test.endpointSlice, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintFlowSchema(t *testing.T) {
	all := []string{"*"}

	tests := []struct {
		fs       flowcontrol.FlowSchema
		expected []metav1.TableRow
	}{
		{
			fs: flowcontrol.FlowSchema{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "all-matcher",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: flowcontrol.FlowSchemaSpec{
					PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{Name: "allee"},
					MatchingPrecedence:         math.MaxInt32,
					DistinguisherMethod:        &flowcontrol.FlowDistinguisherMethod{Type: flowcontrol.FlowDistinguisherMethodByUserType},
					Rules: []flowcontrol.PolicyRulesWithSubjects{{
						Subjects: []flowcontrol.Subject{{
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:authenticated"},
						}},
						ResourceRules: []flowcontrol.ResourcePolicyRule{{
							Verbs:        all,
							APIGroups:    all,
							Resources:    all,
							ClusterScope: true,
							Namespaces:   all,
						}},
					}, {
						Subjects: []flowcontrol.Subject{{
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:unauthenticated"},
						}},
						ResourceRules: []flowcontrol.ResourcePolicyRule{{
							Verbs:        all,
							APIGroups:    all,
							Resources:    all,
							ClusterScope: true,
							Namespaces:   all,
						}},
					}, {
						Subjects: []flowcontrol.Subject{{
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:authenticated"},
						}, {
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:unauthenticated"},
						}},
						NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
							Verbs:           all,
							NonResourceURLs: all,
						}},
					}},
				},
			},
			// Columns: Name, PriorityLevelName, MatchingPrecedence, DistinguisherMethod, Age, MissingPL
			expected: []metav1.TableRow{{Cells: []interface{}{"all-matcher", "allee", int64(math.MaxInt32), "ByUser", "0s", "?"}}},
		}, {
			fs: flowcontrol.FlowSchema{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "some-matcher",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: flowcontrol.FlowSchemaSpec{
					PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{Name: "allee"},
					MatchingPrecedence:         0,
					DistinguisherMethod:        &flowcontrol.FlowDistinguisherMethod{Type: flowcontrol.FlowDistinguisherMethodByNamespaceType},
					Rules: []flowcontrol.PolicyRulesWithSubjects{{
						Subjects: []flowcontrol.Subject{{
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:unauthenticated"},
						}},
						ResourceRules: []flowcontrol.ResourcePolicyRule{{
							Verbs:        all,
							APIGroups:    all,
							Resources:    all,
							ClusterScope: true,
							Namespaces:   all,
						}},
					}, {
						Subjects: []flowcontrol.Subject{{
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:authenticated"},
						}, {
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:unauthenticated"},
						}},
						NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
							Verbs:           all,
							NonResourceURLs: all,
						}},
					}},
				},
				Status: flowcontrol.FlowSchemaStatus{
					Conditions: []flowcontrol.FlowSchemaCondition{{
						Type:               flowcontrol.FlowSchemaConditionDangling,
						Status:             "True",
						LastTransitionTime: metav1.Time{Time: time.Now().Add(-time.Hour)},
					}},
				},
			},
			// Columns: Name, PriorityLevelName, MatchingPrecedence, DistinguisherMethod, Age, MissingPL
			expected: []metav1.TableRow{{Cells: []interface{}{"some-matcher", "allee", int64(0), "ByNamespace", "5m", "True"}}},
		}, {
			fs: flowcontrol.FlowSchema{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "exempt",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: flowcontrol.FlowSchemaSpec{
					PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{Name: "allee"},
					MatchingPrecedence:         0,
					DistinguisherMethod:        nil,
					Rules: []flowcontrol.PolicyRulesWithSubjects{{
						Subjects: []flowcontrol.Subject{{
							Kind:  flowcontrol.SubjectKindGroup,
							Group: &flowcontrol.GroupSubject{Name: "system:masters"},
						}},
						ResourceRules: []flowcontrol.ResourcePolicyRule{{
							Verbs:        all,
							APIGroups:    all,
							Resources:    all,
							ClusterScope: true,
							Namespaces:   all,
						}},
					}},
				},
			},
			// Columns: Name, PriorityLevelName, MatchingPrecedence, DistinguisherMethod, Age, MissingPL
			expected: []metav1.TableRow{{Cells: []interface{}{"exempt", "allee", int64(0), "<none>", "5m", "?"}}},
		},
	}

	for i, test := range tests {
		rows, err := printFlowSchema(&test.fs, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintDeviceClass(t *testing.T) {

	tests := []struct {
		deviceClass resourceapis.DeviceClass
		expected    []metav1.TableRow
	}{
		{
			deviceClass: resourceapis.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-deviceclass",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
			},
			// Columns: Name, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-deviceclass", "5m"}}},
		},
		{
			// test case for Empty creation timestamp that is translated to <unknown>
			deviceClass: resourceapis.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-deviceclass",
					CreationTimestamp: metav1.Time{},
				},
			},
			// Columns: Name, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-deviceclass", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printDeviceClass(&test.deviceClass, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintResourceClaim(t *testing.T) {

	tests := []struct {
		name          string
		resourceClaim resourceapis.ResourceClaim
		expected      []metav1.TableRow
	}{
		{
			name: "ResourceClaim with Pending State",
			resourceClaim: resourceapis.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceclaim",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: resourceapis.ResourceClaimSpec{
					Devices: resourceapis.DeviceClaim{
						Requests: []resourceapis.DeviceRequest{
							{
								Name:            "deviceRequest",
								DeviceClassName: "deviceClass",
								AllocationMode:  resourceapis.DeviceAllocationModeExactCount,
								Count:           1,
							},
						},
					},
				},
			},
			// Columns: Name, State, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceclaim", "pending", "5m"}}},
		},
		{
			// test case for Empty creation timestamp that is translated to <unknown>
			name: "ResourceClaim with Pending State and Empty creation timestamp",
			resourceClaim: resourceapis.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceclaim",
					CreationTimestamp: metav1.Time{},
				},
				Spec: resourceapis.ResourceClaimSpec{
					Devices: resourceapis.DeviceClaim{
						Requests: []resourceapis.DeviceRequest{
							{
								Name:            "deviceRequest",
								DeviceClassName: "deviceClass",
								AllocationMode:  resourceapis.DeviceAllocationModeExactCount,
								Count:           1,
							},
						},
					},
				},
			},
			// Columns: Name, State, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceclaim", "pending", "<unknown>"}}},
		},
		{
			name: "ResourceClaim with Allocated and Reserved State",
			resourceClaim: resourceapis.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceclaim",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: resourceapis.ResourceClaimSpec{
					Devices: resourceapis.DeviceClaim{
						Requests: []resourceapis.DeviceRequest{
							{
								Name:            "deviceRequest",
								DeviceClassName: "deviceClass",
								AllocationMode:  resourceapis.DeviceAllocationModeExactCount,
								Count:           1,
							},
						},
					},
				},
				Status: resourceapis.ResourceClaimStatus{
					Allocation: &resourceapis.AllocationResult{},
					ReservedFor: []resourceapis.ResourceClaimConsumerReference{
						{
							Resource: "pods",
							Name:     "pod-test",
							UID:      types.UID("pod-test"),
						},
					},
				},
			},
			// Columns: Name, State, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceclaim", "allocated,reserved", "5m"}}},
		},
		{
			name: "ResourceClaim with Deleted State",
			resourceClaim: resourceapis.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceclaim",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Spec: resourceapis.ResourceClaimSpec{
					Devices: resourceapis.DeviceClaim{
						Requests: []resourceapis.DeviceRequest{
							{
								Name:            "deviceRequest",
								DeviceClassName: "deviceClass",
								AllocationMode:  resourceapis.DeviceAllocationModeExactCount,
								Count:           1,
							},
						},
					},
				},
			},
			// Columns: Name, State, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceclaim", "deleted", "5m"}}},
		},
	}

	for i, test := range tests {
		rows, err := printResourceClaim(&test.resourceClaim, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintResourceClaimTemplate(t *testing.T) {

	tests := []struct {
		resourceClaimTemplate resourceapis.ResourceClaimTemplate
		expected              []metav1.TableRow
	}{
		{
			resourceClaimTemplate: resourceapis.ResourceClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceclaimtemplate",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: resourceapis.ResourceClaimTemplateSpec{
					Spec: resourceapis.ResourceClaimSpec{
						Devices: resourceapis.DeviceClaim{
							Requests: []resourceapis.DeviceRequest{{
								Name:            "test-deviceRequest",
								DeviceClassName: "deviceClassName",
								AllocationMode:  resourceapis.DeviceAllocationModeExactCount,
								Count:           1,
							}},
						},
					},
				},
			},
			// Columns: Name, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceclaimtemplate", "5m"}}},
		},
		{
			// test case for Empty creation timestamp that is translated to <unknown>
			resourceClaimTemplate: resourceapis.ResourceClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceclaimtemplate",
					CreationTimestamp: metav1.Time{},
				},
				Spec: resourceapis.ResourceClaimTemplateSpec{
					Spec: resourceapis.ResourceClaimSpec{
						Devices: resourceapis.DeviceClaim{
							Requests: []resourceapis.DeviceRequest{{
								Name:            "test-deviceRequest",
								DeviceClassName: "deviceClassName",
								AllocationMode:  resourceapis.DeviceAllocationModeExactCount,
								Count:           1,
							}},
						},
					},
				},
			},
			// Columns: Name, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceclaimtemplate", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printResourceClaimTemplate(&test.resourceClaimTemplate, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintResourceSlice(t *testing.T) {

	tests := []struct {
		resourceSlice resourceapis.ResourceSlice
		expected      []metav1.TableRow
	}{
		{
			resourceSlice: resourceapis.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceslice",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: resourceapis.ResourceSliceSpec{
					NodeName: "nodeName",
					Driver:   "driverName",
					Pool: resourceapis.ResourcePool{
						Name:               "poolName",
						ResourceSliceCount: 1,
					},
				},
			},
			// Columns: Name, Node, Driver, Pool, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceslice", "nodeName", "driverName", "poolName", "5m"}}},
		},
		{
			resourceSlice: resourceapis.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourceslice",
					CreationTimestamp: metav1.Time{},
				},
				Spec: resourceapis.ResourceSliceSpec{
					NodeName: "nodeName",
					Driver:   "driverName",
					Pool: resourceapis.ResourcePool{
						Name:               "poolName",
						ResourceSliceCount: 1,
					},
				},
			},
			// Columns: Name, Node, Driver, Pool, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test-resourceslice", "nodeName", "driverName", "poolName", "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printResourceSlice(&test.resourceSlice, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintPriorityLevelConfiguration(t *testing.T) {
	tests := []struct {
		pl       flowcontrol.PriorityLevelConfiguration
		expected []metav1.TableRow
	}{
		{
			pl: flowcontrol.PriorityLevelConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "unlimited",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: flowcontrol.PriorityLevelConfigurationSpec{
					Type: flowcontrol.PriorityLevelEnablementExempt,
				},
			},
			// Columns: Name, Type, NominalConcurrencyShares, Queues, HandSize, QueueLengthLimit, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"unlimited", "Exempt", "<none>", "<none>", "<none>", "<none>", "0s"}}},
		},
		{
			pl: flowcontrol.PriorityLevelConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "unqueued",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: flowcontrol.PriorityLevelConfigurationSpec{
					Type: flowcontrol.PriorityLevelEnablementLimited,
					Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: 47,
						LimitResponse: flowcontrol.LimitResponse{
							Type: flowcontrol.LimitResponseTypeReject,
						},
					},
				},
			},
			// Columns: Name, Type, NominalConcurrencyShares, Queues, HandSize, QueueLengthLimit, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"unqueued", "Limited", int32(47), "<none>", "<none>", "<none>", "0s"}}},
		},
		{
			pl: flowcontrol.PriorityLevelConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "queued",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec: flowcontrol.PriorityLevelConfigurationSpec{
					Type: flowcontrol.PriorityLevelEnablementLimited,
					Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: 42,
						LimitResponse: flowcontrol.LimitResponse{
							Type: flowcontrol.LimitResponseTypeQueue,
							Queuing: &flowcontrol.QueuingConfiguration{
								Queues:           8,
								HandSize:         3,
								QueueLengthLimit: 4,
							},
						},
					},
				},
			},
			// Columns: Name, Type, NominalConcurrencyShares, Queues, HandSize, QueueLengthLimit, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"queued", "Limited", int32(42), int32(8), int32(3), int32(4), "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printPriorityLevelConfiguration(&test.pl, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintStorageVersion(t *testing.T) {
	commonEncodingVersion := "v1"
	tests := []struct {
		sv       apiserverinternal.StorageVersion
		expected []metav1.TableRow
	}{
		{
			sv: apiserverinternal.StorageVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "empty",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Status: apiserverinternal.StorageVersionStatus{},
			},
			// Columns: Name, CommonEncodingVersion, StorageVersions, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"empty", "<unset>", "<unset>", "0s"}}},
		},
		{
			sv: apiserverinternal.StorageVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "valid",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Status: apiserverinternal.StorageVersionStatus{
					StorageVersions: []apiserverinternal.ServerStorageVersion{
						{
							APIServerID:       "1",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1"},
						},
						{
							APIServerID:       "2",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1", "v2"},
						},
					},
					CommonEncodingVersion: &commonEncodingVersion,
				},
			},
			// Columns: Name, CommonEncodingVersion, StorageVersions, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"valid", "v1", "1=v1,2=v1", "0s"}}},
		},
		{
			sv: apiserverinternal.StorageVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "disagree",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Status: apiserverinternal.StorageVersionStatus{
					StorageVersions: []apiserverinternal.ServerStorageVersion{
						{
							APIServerID:       "1",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1"},
						},
						{
							APIServerID:       "2",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1", "v2"},
						},
						{
							APIServerID:       "3",
							EncodingVersion:   "v2",
							DecodableVersions: []string{"v2"},
						},
					},
				},
			},
			// Columns: Name, CommonEncodingVersion, StorageVersions, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"disagree", "<unset>", "1=v1,2=v1,3=v2", "0s"}}},
		},
		{
			sv: apiserverinternal.StorageVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "agreeWithMore",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Status: apiserverinternal.StorageVersionStatus{
					StorageVersions: []apiserverinternal.ServerStorageVersion{
						{
							APIServerID:       "1",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1"},
						},
						{
							APIServerID:       "2",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1", "v2"},
						},
						{
							APIServerID:       "3",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1", "v2"},
						},
						{
							APIServerID:       "4",
							EncodingVersion:   "v1",
							DecodableVersions: []string{"v1", "v2", "v3alpha1"},
						},
					},
					CommonEncodingVersion: &commonEncodingVersion,
				},
			},
			// Columns: Name, CommonEncodingVersion, StorageVersions, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"agreeWithMore", "v1", "1=v1,2=v1,3=v1 + 1 more...", "0s"}}},
		},
	}

	for i, test := range tests {
		rows, err := printStorageVersion(&test.sv, printers.GenerateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintScale(t *testing.T) {
	tests := []struct {
		scale    autoscaling.Scale
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		{
			scale: autoscaling.Scale{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-autoscaling",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(1.9e9)},
				},
				Spec:   autoscaling.ScaleSpec{Replicas: 2},
				Status: autoscaling.ScaleStatus{Replicas: 1},
			},
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"test-autoscaling", int64(2), int64(1), string("0s")},
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printScale(&test.scale, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintClusterTrustBundle(t *testing.T) {
	tests := []struct {
		bundle   certificates.ClusterTrustBundle
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		{
			bundle: certificates.ClusterTrustBundle{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-cluster-trust-bundle",
				},
				Spec: certificates.ClusterTrustBundleSpec{
					SignerName: "test-signer-name",
				},
			},
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"test-cluster-trust-bundle", "test-signer-name"},
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printClusterTrustBundle(&test.bundle, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintValidatingAdmissionPolicyBinding(t *testing.T) {
	tests := []struct {
		validatingAdmissionPolicyBinding admissionregistration.ValidatingAdmissionPolicyBinding
		options                          printers.GenerateOptions
		expected                         []metav1.TableRow
	}{
		{
			validatingAdmissionPolicyBinding: admissionregistration.ValidatingAdmissionPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "config",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: admissionregistration.ValidatingAdmissionPolicyBindingSpec{
					PolicyName: "test-scale.example.com",
					ParamRef: &admissionregistration.ParamRef{
						Name:                    "test-scale-setting.example.com",
						ParameterNotFoundAction: ptr.To(admissionregistration.DenyAction),
					},
				},
			},
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"config", "test-scale.example.com", "*/test-scale-setting.example.com", "5m"},
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printValidatingAdmissionPolicyBinding(&test.validatingAdmissionPolicyBinding, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintLeaseCandidate(t *testing.T) {
	tests := []struct {
		leaseCandidate coordination.LeaseCandidate
		options        printers.GenerateOptions
		expected       []metav1.TableRow
	}{
		{
			leaseCandidate: coordination.LeaseCandidate{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-lease",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: coordination.LeaseCandidateSpec{
					BinaryVersion:    "test-binary-version",
					EmulationVersion: "test-emulation-version",
					LeaseName:        "test-lease-name",
				},
			},
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"test-lease", "test-lease-name", "test-binary-version", "test-emulation-version", "5m"},
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printLeaseCandidate(&test.leaseCandidate, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintResourceQuota(t *testing.T) {
	tests := []struct {
		resourceQuota api.ResourceQuota
		options       printers.GenerateOptions
		expected      []metav1.TableRow
	}{
		{
			resourceQuota: api.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-resourcequota",
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-3e11)},
				},
				Spec: api.ResourceQuotaSpec{},
				Status: api.ResourceQuotaStatus{
					Used: api.ResourceList{
						api.ResourceCPU:                    resource.MustParse("1"),
						api.ResourceMemory:                 resource.MustParse("1Gi"),
						api.ResourcePods:                   resource.MustParse("1"),
						api.ResourceServices:               resource.MustParse("1"),
						api.ResourceReplicationControllers: resource.MustParse("1"),
						api.ResourceQuotas:                 resource.MustParse("1"),
						api.ResourceLimitsCPU:              resource.MustParse("2"),
						api.ResourceLimitsMemory:           resource.MustParse("2Gi"),
					},
					Hard: api.ResourceList{
						api.ResourceCPU:                    resource.MustParse("100"),
						api.ResourceMemory:                 resource.MustParse("4Gi"),
						api.ResourcePods:                   resource.MustParse("10"),
						api.ResourceServices:               resource.MustParse("10"),
						api.ResourceReplicationControllers: resource.MustParse("10"),
						api.ResourceQuotas:                 resource.MustParse("1"),
						api.ResourceLimitsCPU:              resource.MustParse("200"),
						api.ResourceLimitsMemory:           resource.MustParse("8Gi"),
					},
				},
			},
			expected: []metav1.TableRow{
				{
					Cells: []interface{}{"test-resourcequota", "cpu: 1/100, memory: 1Gi/4Gi, pods: 1/10, replicationcontrollers: 1/10, resourcequotas: 1/1, services: 1/10", "limits.cpu: 2/200, limits.memory: 2Gi/8Gi", "5m"},
				},
			},
		},
	}

	for i, test := range tests {
		rows, err := printResourceQuota(&test.resourceQuota, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestTableRowDeepCopyShouldNotPanic(t *testing.T) {
	tests := []struct {
		name    string
		printer func() ([]metav1.TableRow, error)
	}{
		{
			name: "Pod",
			printer: func() ([]metav1.TableRow, error) {
				return printPod(&api.Pod{}, printers.GenerateOptions{})
			},
		},
		{
			name: "PodTemplate",
			printer: func() ([]metav1.TableRow, error) {
				return printPodTemplate(&api.PodTemplate{}, printers.GenerateOptions{})
			},
		},
		{
			name: "PodDisruptionBudget",
			printer: func() ([]metav1.TableRow, error) {
				return printPodDisruptionBudget(&policy.PodDisruptionBudget{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ReplicationController",
			printer: func() ([]metav1.TableRow, error) {
				return printReplicationController(&api.ReplicationController{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ReplicaSet",
			printer: func() ([]metav1.TableRow, error) {
				return printReplicaSet(&apps.ReplicaSet{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Job",
			printer: func() ([]metav1.TableRow, error) {
				return printJob(&batch.Job{}, printers.GenerateOptions{})
			},
		},
		{
			name: "CronJob",
			printer: func() ([]metav1.TableRow, error) {
				return printCronJob(&batch.CronJob{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Service",
			printer: func() ([]metav1.TableRow, error) {
				return printService(&api.Service{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Ingress",
			printer: func() ([]metav1.TableRow, error) {
				return printIngress(&networking.Ingress{}, printers.GenerateOptions{})
			},
		},
		{
			name: "IngressClass",
			printer: func() ([]metav1.TableRow, error) {
				return printIngressClass(&networking.IngressClass{}, printers.GenerateOptions{})
			},
		},
		{
			name: "StatefulSet",
			printer: func() ([]metav1.TableRow, error) {
				return printStatefulSet(&apps.StatefulSet{}, printers.GenerateOptions{})
			},
		},
		{
			name: "DaemonSet",
			printer: func() ([]metav1.TableRow, error) {
				return printDaemonSet(&apps.DaemonSet{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Endpoints",
			printer: func() ([]metav1.TableRow, error) {
				return printEndpoints(&api.Endpoints{}, printers.GenerateOptions{})
			},
		},
		{
			name: "EndpointSlice",
			printer: func() ([]metav1.TableRow, error) {
				return printEndpointSlice(&discovery.EndpointSlice{}, printers.GenerateOptions{})
			},
		},
		{
			name: "CSINode",
			printer: func() ([]metav1.TableRow, error) {
				return printCSINode(&storage.CSINode{}, printers.GenerateOptions{})
			},
		},
		{
			name: "CSIDriver",
			printer: func() ([]metav1.TableRow, error) {
				return printCSIDriver(&storage.CSIDriver{}, printers.GenerateOptions{})
			},
		},
		{
			name: "CSIStorageCapacity",
			printer: func() ([]metav1.TableRow, error) {
				return printCSIStorageCapacity(&storage.CSIStorageCapacity{}, printers.GenerateOptions{})
			},
		},
		{
			name: "MutatingWebhookConfiguration",
			printer: func() ([]metav1.TableRow, error) {
				return printMutatingWebhook(&admissionregistration.MutatingWebhookConfiguration{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ValidatingWebhookConfiguration",
			printer: func() ([]metav1.TableRow, error) {
				return printValidatingWebhook(&admissionregistration.ValidatingWebhookConfiguration{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ValidatingAdmissionPolicy",
			printer: func() ([]metav1.TableRow, error) {
				return printValidatingAdmissionPolicy(&admissionregistration.ValidatingAdmissionPolicy{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ValidatingAdmissionPolicyBinding",
			printer: func() ([]metav1.TableRow, error) {
				return printValidatingAdmissionPolicyBinding(&admissionregistration.ValidatingAdmissionPolicyBinding{}, printers.GenerateOptions{})
			},
		},
		{
			name: "MutatingAdmissionPolicy",
			printer: func() ([]metav1.TableRow, error) {
				return printMutatingAdmissionPolicy(&admissionregistration.MutatingAdmissionPolicy{}, printers.GenerateOptions{})
			},
		},
		{
			name: "MutatingAdmissionPolicyBinding",
			printer: func() ([]metav1.TableRow, error) {
				return printMutatingAdmissionPolicyBinding(&admissionregistration.MutatingAdmissionPolicyBinding{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Namespace",
			printer: func() ([]metav1.TableRow, error) {
				return printNamespace(&api.Namespace{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Secret",
			printer: func() ([]metav1.TableRow, error) {
				return printSecret(&api.Secret{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ServiceAccount",
			printer: func() ([]metav1.TableRow, error) {
				return printServiceAccount(&api.ServiceAccount{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Node",
			printer: func() ([]metav1.TableRow, error) {
				return printNode(&api.Node{}, printers.GenerateOptions{})
			},
		},
		{
			name: "PersistentVolume",
			printer: func() ([]metav1.TableRow, error) {
				return printPersistentVolume(&api.PersistentVolume{}, printers.GenerateOptions{})
			},
		},
		{
			name: "PersistentVolumeClaim",
			printer: func() ([]metav1.TableRow, error) {
				return printPersistentVolumeClaim(&api.PersistentVolumeClaim{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Event",
			printer: func() ([]metav1.TableRow, error) {
				return printEvent(&api.Event{}, printers.GenerateOptions{})
			},
		},
		{
			name: "RoleBinding",
			printer: func() ([]metav1.TableRow, error) {
				return printRoleBinding(&rbac.RoleBinding{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ClusterRoleBinding",
			printer: func() ([]metav1.TableRow, error) {
				return printClusterRoleBinding(&rbac.ClusterRoleBinding{}, printers.GenerateOptions{})
			},
		},
		{
			name: "CertificateSigningRequest",
			printer: func() ([]metav1.TableRow, error) {
				return printCertificateSigningRequest(&certificates.CertificateSigningRequest{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ComponentStatus",
			printer: func() ([]metav1.TableRow, error) {
				return printComponentStatus(&api.ComponentStatus{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Deployment",
			printer: func() ([]metav1.TableRow, error) {
				return printDeployment(&apps.Deployment{}, printers.GenerateOptions{})
			},
		},
		{
			name: "HorizontalPodAutoscaler",
			printer: func() ([]metav1.TableRow, error) {
				return printHorizontalPodAutoscaler(&autoscaling.HorizontalPodAutoscaler{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ConfigMap",
			printer: func() ([]metav1.TableRow, error) {
				return printConfigMap(&api.ConfigMap{}, printers.GenerateOptions{})
			},
		},
		{
			name: "NetworkPolicy",
			printer: func() ([]metav1.TableRow, error) {
				return printNetworkPolicy(&networking.NetworkPolicy{}, printers.GenerateOptions{})
			},
		},
		{
			name: "StorageClass",
			printer: func() ([]metav1.TableRow, error) {
				return printStorageClass(&storage.StorageClass{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Lease",
			printer: func() ([]metav1.TableRow, error) {
				return printLease(&coordination.Lease{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ControllerRevision",
			printer: func() ([]metav1.TableRow, error) {
				return printControllerRevision(&apps.ControllerRevision{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ResourceQuota",
			printer: func() ([]metav1.TableRow, error) {
				return printResourceQuota(&api.ResourceQuota{}, printers.GenerateOptions{})
			},
		},
		{
			name: "PriorityClass",
			printer: func() ([]metav1.TableRow, error) {
				return printPriorityClass(&scheduling.PriorityClass{}, printers.GenerateOptions{})
			},
		},
		{
			name: "RuntimeClass",
			printer: func() ([]metav1.TableRow, error) {
				return printRuntimeClass(&nodeapi.RuntimeClass{}, printers.GenerateOptions{})
			},
		},
		{
			name: "VolumeAttachment",
			printer: func() ([]metav1.TableRow, error) {
				return printVolumeAttachment(&storage.VolumeAttachment{}, printers.GenerateOptions{})
			},
		},
		{
			name: "FlowSchema",
			printer: func() ([]metav1.TableRow, error) {
				return printFlowSchema(&flowcontrol.FlowSchema{}, printers.GenerateOptions{})
			},
		},
		{
			name: "StorageVersion",
			printer: func() ([]metav1.TableRow, error) {
				return printStorageVersion(&apiserverinternal.StorageVersion{}, printers.GenerateOptions{})
			},
		},
		{
			name: "PriorityLevelConfiguration",
			printer: func() ([]metav1.TableRow, error) {
				return printPriorityLevelConfiguration(&flowcontrol.PriorityLevelConfiguration{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Scale",
			printer: func() ([]metav1.TableRow, error) {
				return printScale(&autoscaling.Scale{}, printers.GenerateOptions{})
			},
		},
		{
			name: "Status",
			printer: func() ([]metav1.TableRow, error) {
				return printStatus(&metav1.Status{}, printers.GenerateOptions{})
			},
		},
		{
			name: "DeviceClass",
			printer: func() ([]metav1.TableRow, error) {
				return printDeviceClass(&resourceapis.DeviceClass{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ResourceClaim",
			printer: func() ([]metav1.TableRow, error) {
				return printResourceClaim(&resourceapis.ResourceClaim{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ResourceClaimTemplate",
			printer: func() ([]metav1.TableRow, error) {
				return printResourceClaimTemplate(&resourceapis.ResourceClaimTemplate{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ResourceSlice",
			printer: func() ([]metav1.TableRow, error) {
				return printResourceSlice(&resourceapis.ResourceSlice{}, printers.GenerateOptions{})
			},
		},
		{
			name: "LeaseCandidate",
			printer: func() ([]metav1.TableRow, error) {
				return printLeaseCandidate(&coordination.LeaseCandidate{}, printers.GenerateOptions{})
			},
		},
		{
			name: "ClusterTrustBundle",
			printer: func() ([]metav1.TableRow, error) {
				return printClusterTrustBundle(&certificates.ClusterTrustBundle{}, printers.GenerateOptions{})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := test.printer()
			if err != nil {
				t.Fatalf("expected no error, but got: %#v", err)
			}
			if len(rows) <= 0 {
				t.Fatalf("expected to have at least one TableRow, but got: %d", len(rows))
			}

			func() {
				defer func() {
					if err := recover(); err != nil {
						// Same as stdlib http server code. Manually allocate stack
						// trace buffer size to prevent excessively large logs
						const size = 64 << 10
						buf := make([]byte, size)
						buf = buf[:runtime.Stack(buf, false)]
						err = fmt.Errorf("%q stack:\n%s", err, buf)

						t.Errorf("Expected no panic, but got: %v", err)
					}
				}()

				// should not panic
				rows[0].DeepCopy()
			}()

		})
	}
}

func TestPrintIPAddress(t *testing.T) {
	ip := networking.IPAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "192.168.2.2",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Duration(-10 * 365 * 24 * time.Hour))},
		},
		Spec: networking.IPAddressSpec{
			ParentRef: &networking.ParentReference{
				Group:     "mygroup",
				Resource:  "myresource",
				Namespace: "mynamespace",
				Name:      "myname",
			},
		},
	}
	// Columns: Name, ParentRef, Age
	expected := []metav1.TableRow{{Cells: []interface{}{"192.168.2.2", "myresource.mygroup/mynamespace/myname", "10y"}}}

	rows, err := printIPAddress(&ip, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error generating table rows for IPAddress: %#v", err)
	}
	rows[0].Object.Object = nil
	if !reflect.DeepEqual(expected, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expected, rows))
	}
}

func TestPrintIPAddressList(t *testing.T) {
	ipList := networking.IPAddressList{
		Items: []networking.IPAddress{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "192.168.2.2",
					CreationTimestamp: metav1.Time{},
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "mygroup",
						Resource:  "myresource",
						Namespace: "mynamespace",
						Name:      "myname",
					},
				},
			}, {
				ObjectMeta: metav1.ObjectMeta{
					Name:              "2001:db8::2",
					CreationTimestamp: metav1.Time{},
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "mygroup2",
						Resource:  "myresource2",
						Namespace: "mynamespace2",
						Name:      "myname2",
					},
				},
			},
		},
	}
	// Columns: Name, ParentRef, Age
	expected := []metav1.TableRow{
		{Cells: []interface{}{"192.168.2.2", "myresource.mygroup/mynamespace/myname", "<unknown>"}},
		{Cells: []interface{}{"2001:db8::2", "myresource2.mygroup2/mynamespace2/myname2", "<unknown>"}},
	}

	rows, err := printIPAddressList(&ipList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error generating table rows for IPAddress: %#v", err)
	}
	for i := range rows {
		rows[i].Object.Object = nil

	}
	if !reflect.DeepEqual(expected, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expected, rows))
	}

}

func TestPrintServiceCIDR(t *testing.T) {
	ipv4CIDR := "10.1.0.0/16"
	ipv6CIDR := "fd00:1:1::/64"

	tests := []struct {
		ccc      networking.ServiceCIDR
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		{
			// Test name, IPv4 only.
			ccc: networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{Name: "test1"},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{ipv4CIDR},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, IPv4, IPv6, Age.
			expected: []metav1.TableRow{{Cells: []interface{}{"test1", ipv4CIDR, "<unknown>"}}},
		},
		{
			// Test name, IPv6 only.
			ccc: networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{Name: "test5"},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{ipv6CIDR},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, PerNodeHostBits, IPv4, IPv6, Age
			expected: []metav1.TableRow{{Cells: []interface{}{"test5", ipv6CIDR, "<unknown>"}}},
		},
		{
			// Test name, DualStack.
			ccc: networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{Name: "test9"},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{ipv4CIDR, ipv6CIDR},
				},
			},
			options: printers.GenerateOptions{},
			// Columns: Name, PerNodeHostBits, IPv4, IPv6, Age.
			expected: []metav1.TableRow{{Cells: []interface{}{"test9", ipv4CIDR + "," + ipv6CIDR, "<unknown>"}}},
		},
	}

	for i, test := range tests {
		rows, err := printServiceCIDR(&test.ccc, test.options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("%d mismatch: %s", i, cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintServiceCIDRList(t *testing.T) {
	cccList := networking.ServiceCIDRList{
		Items: []networking.ServiceCIDR{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ccc1"},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"10.1.0.0/16", "fd00:1:1::/64"},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ccc2"},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"10.2.0.0/16", "fd00:2:1::/64"},
				},
			},
		},
	}

	tests := []struct {
		options  printers.GenerateOptions
		expected []metav1.TableRow
	}{
		{
			// Test name, DualStack with node selector, wide.
			options: printers.GenerateOptions{Wide: false},
			expected: []metav1.TableRow{
				// Columns: Name, IPv4, IPv6, Age.
				{Cells: []interface{}{"ccc1", "10.1.0.0/16,fd00:1:1::/64", "<unknown>"}},
				{Cells: []interface{}{"ccc2", "10.2.0.0/16,fd00:2:1::/64", "<unknown>"}},
			},
		},
		{
			// Test name, DualStack with node selector, wide.
			options: printers.GenerateOptions{Wide: true},
			expected: []metav1.TableRow{
				// Columns: Name, CIDRs, Age.
				{Cells: []interface{}{"ccc1", "10.1.0.0/16,fd00:1:1::/64", "<unknown>"}},
				{Cells: []interface{}{"ccc2", "10.2.0.0/16,fd00:2:1::/64", "<unknown>"}},
			},
		},
	}

	for _, test := range tests {
		rows, err := printServiceCIDRList(&cccList, test.options)
		if err != nil {
			t.Fatalf("Error printing service list: %#v", err)
		}
		for i := range rows {
			rows[i].Object.Object = nil
		}
		if !reflect.DeepEqual(test.expected, rows) {
			t.Errorf("mismatch: %s", cmp.Diff(test.expected, rows))
		}
	}
}

func TestPrintStorageVersionMigration(t *testing.T) {
	storageVersionMigration := storagemigration.StorageVersionMigration{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StorageVersionMigration",
			APIVersion: "storagemigration.k8s.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "print-test",
		},
		Spec: storagemigration.StorageVersionMigrationSpec{
			Resource: storagemigration.GroupVersionResource{
				Group:    "test-group",
				Version:  "test-version",
				Resource: "test-resource",
			},
		},
	}

	// Columns: Name, GVRTOMIGRATE
	expected := []metav1.TableRow{{Cells: []interface{}{"print-test", "test-resource.test-version.test-group"}}}

	rows, err := printStorageVersionMigration(&storageVersionMigration, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error generating table rows for StorageVersionMigration: %#v", err)
	}
	rows[0].Object.Object = nil
	if !reflect.DeepEqual(expected, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expected, rows))
	}
}

func TestPrintStorageVersionMigrationList(t *testing.T) {
	storageVersionMigrationList := storagemigration.StorageVersionMigrationList{
		Items: []storagemigration.StorageVersionMigration{
			{
				TypeMeta: metav1.TypeMeta{
					Kind:       "StorageVersionMigration",
					APIVersion: "storagemigration.k8s.io/v1alpha1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "print-test",
				},
				Spec: storagemigration.StorageVersionMigrationSpec{
					Resource: storagemigration.GroupVersionResource{
						Group:    "test-group",
						Version:  "test-version",
						Resource: "test-resource",
					},
				},
			},
			{
				TypeMeta: metav1.TypeMeta{
					Kind:       "StorageVersionMigration",
					APIVersion: "storagemigration.k8s.io/v1alpha1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "print-test2",
				},
				Spec: storagemigration.StorageVersionMigrationSpec{
					Resource: storagemigration.GroupVersionResource{
						Group:    "test-group2",
						Version:  "test-version2",
						Resource: "test-resource2",
					},
				},
			},
		},
	}

	// Columns: Name, GVRTOMIGRATE
	expected := []metav1.TableRow{
		{Cells: []interface{}{"print-test", "test-resource.test-version.test-group"}},
		{Cells: []interface{}{"print-test2", "test-resource2.test-version2.test-group2"}},
	}

	rows, err := printStorageVersionMigrationList(&storageVersionMigrationList, printers.GenerateOptions{})
	if err != nil {
		t.Fatalf("Error generating table rows for StorageVersionMigration: %#v", err)
	}

	for i := range rows {
		rows[i].Object.Object = nil
	}

	if !reflect.DeepEqual(expected, rows) {
		t.Errorf("mismatch: %s", cmp.Diff(expected, rows))
	}
}
