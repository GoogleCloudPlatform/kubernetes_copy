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

package drain

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest/fake"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/drain"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/utils/ptr"
)

const (
	EvictionMethod = "Eviction"
	DeleteMethod   = "Delete"
)

var node *corev1.Node
var cordonedNode *corev1.Node

func TestMain(m *testing.M) {
	// Create a node.
	node = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Status: corev1.NodeStatus{},
	}

	// A copy of the same node, but cordoned.
	cordonedNode = node.DeepCopy()
	cordonedNode.Spec.Unschedulable = true
	os.Exit(m.Run())
}

func TestCordon(t *testing.T) {
	tests := []struct {
		description string
		node        *corev1.Node
		expected    *corev1.Node
		cmd         func(cmdutil.Factory, genericiooptions.IOStreams) *cobra.Command
		arg         string
		expectFatal bool
	}{
		{
			description: "node/node syntax",
			node:        cordonedNode,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node/node",
			expectFatal: false,
		},
		{
			description: "uncordon for real",
			node:        cordonedNode,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "uncordon does nothing",
			node:        node,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "cordon does nothing",
			node:        cordonedNode,
			expected:    cordonedNode,
			cmd:         NewCmdCordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "cordon for real",
			node:        node,
			expected:    cordonedNode,
			cmd:         NewCmdCordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "cordon missing node",
			node:        node,
			expected:    node,
			cmd:         NewCmdCordon,
			arg:         "bar",
			expectFatal: true,
		},
		{
			description: "uncordon missing node",
			node:        node,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "bar",
			expectFatal: true,
		},
		{
			description: "cordon for multiple nodes",
			node:        node,
			expected:    cordonedNode,
			cmd:         NewCmdCordon,
			arg:         "node node1 node2",
			expectFatal: false,
		},
		{
			description: "uncordon for multiple nodes",
			node:        cordonedNode,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node node1 node2",
			expectFatal: false,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory()
			defer tf.Cleanup()

			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
			ns := scheme.Codecs.WithoutConversion()

			newNode := &corev1.Node{}
			updated := false
			tf.Client = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
				NegotiatedSerializer: ns,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					m := &MyReq{req}
					switch {
					case m.isFor("GET", "/nodes/node1"):
						fallthrough
					case m.isFor("GET", "/nodes/node2"):
						fallthrough
					case m.isFor("GET", "/nodes/node"):
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, test.node)}, nil
					case m.isFor("GET", "/nodes/bar"):
						return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.StringBody("nope")}, nil
					case m.isFor("PATCH", "/nodes/node1"):
						fallthrough
					case m.isFor("PATCH", "/nodes/node2"):
						fallthrough
					case m.isFor("PATCH", "/nodes/node"):
						data, err := io.ReadAll(req.Body)
						if err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						defer req.Body.Close()
						oldJSON, err := runtime.Encode(codec, node)
						if err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						appliedPatch, err := strategicpatch.StrategicMergePatch(oldJSON, data, &corev1.Node{})
						if err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						if err := runtime.DecodeInto(codec, appliedPatch, newNode); err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						if !reflect.DeepEqual(test.expected.Spec, newNode.Spec) {
							t.Fatalf("%s: expected:\n%v\nsaw:\n%v\n", test.description, test.expected.Spec.Unschedulable, newNode.Spec.Unschedulable)
						}
						updated = true
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, newNode)}, nil
					default:
						t.Fatalf("%s: unexpected request: %v %#v\n%#v", test.description, req.Method, req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			ioStreams, _, _, _ := genericiooptions.NewTestIOStreams()
			cmd := test.cmd(tf, ioStreams)

			var recovered interface{}
			sawFatal := false
			func() {
				defer func() {
					// Recover from the panic below.
					recovered = recover()
					// Restore cmdutil behavior
					cmdutil.DefaultBehaviorOnFatal()
				}()
				cmdutil.BehaviorOnFatal(func(e string, code int) {
					sawFatal = true
					panic(e)
				})
				cmd.SetArgs(strings.Split(test.arg, " "))
				cmd.Execute()
			}()

			switch {
			case recovered != nil && !sawFatal:
				t.Fatalf("got panic: %v", recovered)
			case test.expectFatal:
				if !sawFatal {
					t.Fatalf("%s: unexpected non-error", test.description)
				}
				if updated {
					t.Fatalf("%s: unexpected update", test.description)
				}
			case !test.expectFatal && sawFatal:
				t.Fatalf("%s: unexpected error", test.description)
			case !reflect.DeepEqual(test.expected.Spec, test.node.Spec) && !updated:
				t.Fatalf("%s: node never updated", test.description)
			}
		})
	}
}

func TestDrain(t *testing.T) {
	labels := make(map[string]string)
	labels["my_key"] = "my_value"

	rc := corev1.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rc",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.ReplicationControllerSpec{
			Selector: labels,
		},
	}

	rcPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "ReplicationController",
					Name:               "rc",
					UID:                "123",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ds",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	dsPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "DaemonSet",
					Name:               "ds",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	dsTerminatedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "DaemonSet",
					Name:               "ds",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}

	dsPodWithEmptyDir := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "DaemonSet",
					Name:               "ds",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
	}

	orphanedDsPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "job",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: batchv1.JobSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	jobPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Job",
					Name:               "job",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
	}

	terminatedJobPodWithLocalStorage := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Job",
					Name:               "job",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}

	rs := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rs",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	rsPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "ReplicaSet",
					Name:               "rs",
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	nakedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	emptydirPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
	}
	emptydirTerminatedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}

	tests := []struct {
		description                string
		node                       *corev1.Node
		expected                   *corev1.Node
		pods                       []corev1.Pod
		rcs                        []corev1.ReplicationController
		replicaSets                []appsv1.ReplicaSet
		args                       []string
		failUponEvictionOrDeletion bool
		expectWarning              string
		expectFatal                bool
		expectDelete               bool
		expectOutputToContain      string
	}{
		{
			description:           "RC-managed pod",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{rcPod},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:  "DS-managed pod",
			node:         node,
			expected:     cordonedNode,
			pods:         []corev1.Pod{dsPod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:           "DS-managed terminated pod",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{dsTerminatedPod},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:  "orphaned DS-managed pod",
			node:         node,
			expected:     cordonedNode,
			pods:         []corev1.Pod{orphanedDsPod},
			rcs:          []corev1.ReplicationController{},
			args:         []string{"node"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:           "orphaned DS-managed pod with --force",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{orphanedDsPod},
			rcs:                   []corev1.ReplicationController{},
			args:                  []string{"node", "--force"},
			expectFatal:           false,
			expectDelete:          true,
			expectWarning:         "Warning: deleting Pods that declare no controller: default/bar",
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "DS-managed pod with --ignore-daemonsets",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{dsPod},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node", "--ignore-daemonsets"},
			expectFatal:           false,
			expectDelete:          false,
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "DS-managed pod with emptyDir with --ignore-daemonsets",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{dsPodWithEmptyDir},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node", "--ignore-daemonsets"},
			expectWarning:         "Warning: ignoring DaemonSet-managed Pods: default/bar",
			expectFatal:           false,
			expectDelete:          false,
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "Job-managed pod with local storage",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{jobPod},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node", "--force", "--delete-emptydir-data=true"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "Job-managed terminated pod",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{terminatedJobPodWithLocalStorage},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "RS-managed pod",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{rsPod},
			replicaSets:           []appsv1.ReplicaSet{rs},
			args:                  []string{"node"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:  "naked pod",
			node:         node,
			expected:     cordonedNode,
			pods:         []corev1.Pod{nakedPod},
			rcs:          []corev1.ReplicationController{},
			args:         []string{"node"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:           "naked pod with --force",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{nakedPod},
			rcs:                   []corev1.ReplicationController{},
			args:                  []string{"node", "--force"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:  "pod with EmptyDir",
			node:         node,
			expected:     cordonedNode,
			pods:         []corev1.Pod{emptydirPod},
			args:         []string{"node", "--force"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:           "terminated pod with emptyDir",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{emptydirTerminatedPod},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "pod with EmptyDir and --delete-emptydir-data",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{emptydirPod},
			args:                  []string{"node", "--force", "--delete-emptydir-data=true"},
			expectFatal:           false,
			expectDelete:          true,
			expectOutputToContain: "node/node drained",
		},
		{
			description:           "empty node",
			node:                  node,
			expected:              cordonedNode,
			pods:                  []corev1.Pod{},
			rcs:                   []corev1.ReplicationController{rc},
			args:                  []string{"node"},
			expectFatal:           false,
			expectDelete:          false,
			expectOutputToContain: "node/node drained",
		},
		{
			description:                "fail to list pods",
			node:                       node,
			expected:                   cordonedNode,
			pods:                       []corev1.Pod{rsPod},
			replicaSets:                []appsv1.ReplicaSet{rs},
			args:                       []string{"node"},
			expectFatal:                true,
			expectDelete:               true,
			failUponEvictionOrDeletion: true,
		},
	}

	testEviction := false
	for i := 0; i < 2; i++ {
		testEviction = !testEviction
		var currMethod string
		if testEviction {
			currMethod = EvictionMethod
		} else {
			currMethod = DeleteMethod
		}
		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				newNode := &corev1.Node{}
				var deletions, evictions int32
				tf := cmdtesting.NewTestFactory()
				defer tf.Cleanup()

				codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
				ns := scheme.Codecs.WithoutConversion()

				tf.Client = &fake.RESTClient{
					GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
					NegotiatedSerializer: ns,
					Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
						m := &MyReq{req}
						switch {
						case req.Method == "GET" && req.URL.Path == "/api":
							apiVersions := metav1.APIVersions{
								Versions: []string{"v1"},
							}
							return cmdtesting.GenResponseWithJsonEncodedBody(apiVersions)
						case req.Method == "GET" && req.URL.Path == "/apis":
							groupList := metav1.APIGroupList{
								Groups: []metav1.APIGroup{
									{
										Name: "policy",
										PreferredVersion: metav1.GroupVersionForDiscovery{
											GroupVersion: "policy/v1",
										},
									},
								},
							}
							return cmdtesting.GenResponseWithJsonEncodedBody(groupList)
						case req.Method == "GET" && req.URL.Path == "/api/v1":
							resourceList := metav1.APIResourceList{
								GroupVersion: "v1",
							}
							if testEviction {
								resourceList.APIResources = []metav1.APIResource{
									{
										Name:    drain.EvictionSubresource,
										Kind:    drain.EvictionKind,
										Group:   "policy",
										Version: "v1",
									},
								}
							}
							return cmdtesting.GenResponseWithJsonEncodedBody(resourceList)
						case m.isFor("GET", "/nodes/node"):
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, test.node)}, nil
						case m.isFor("GET", "/namespaces/default/replicationcontrollers/rc"):
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &test.rcs[0])}, nil
						case m.isFor("GET", "/namespaces/default/daemonsets/ds"):
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &ds)}, nil
						case m.isFor("GET", "/namespaces/default/daemonsets/missing-ds"):
							return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &appsv1.DaemonSet{})}, nil
						case m.isFor("GET", "/namespaces/default/jobs/job"):
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &job)}, nil
						case m.isFor("GET", "/namespaces/default/replicasets/rs"):
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &test.replicaSets[0])}, nil
						case m.isFor("GET", "/namespaces/default/pods/bar"):
							return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.Pod{})}, nil
						case m.isFor("GET", "/pods"):
							if test.failUponEvictionOrDeletion && atomic.LoadInt32(&evictions) > 0 || atomic.LoadInt32(&deletions) > 0 {
								return nil, errors.New("request failed")
							}
							values, err := url.ParseQuery(req.URL.RawQuery)
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							getParams := make(url.Values)
							getParams["fieldSelector"] = []string{"spec.nodeName=node"}
							getParams["limit"] = []string{"500"}
							if !reflect.DeepEqual(getParams, values) {
								t.Fatalf("%s: expected:\n%v\nsaw:\n%v\n", test.description, getParams, values)
							}
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.PodList{Items: test.pods})}, nil
						case m.isFor("GET", "/replicationcontrollers"):
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.ReplicationControllerList{Items: test.rcs})}, nil
						case m.isFor("PATCH", "/nodes/node"):
							data, err := io.ReadAll(req.Body)
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							defer req.Body.Close()
							oldJSON, err := runtime.Encode(codec, node)
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							appliedPatch, err := strategicpatch.StrategicMergePatch(oldJSON, data, &corev1.Node{})
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							if err := runtime.DecodeInto(codec, appliedPatch, newNode); err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							if !reflect.DeepEqual(test.expected.Spec, newNode.Spec) {
								t.Fatalf("%s: expected:\n%v\nsaw:\n%v\n", test.description, test.expected.Spec, newNode.Spec)
							}
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, newNode)}, nil
						case m.isFor("DELETE", "/namespaces/default/pods/bar"):
							atomic.AddInt32(&deletions, 1)
							if test.failUponEvictionOrDeletion {
								return nil, errors.New("request failed")
							}
							return &http.Response{StatusCode: http.StatusNoContent, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &test.pods[0])}, nil
						case m.isFor("POST", "/namespaces/default/pods/bar/eviction"):
							atomic.AddInt32(&evictions, 1)
							if test.failUponEvictionOrDeletion {
								return nil, errors.New("request failed")
							}
							return &http.Response{StatusCode: http.StatusCreated, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &metav1.Status{})}, nil
						default:
							t.Fatalf("%s: unexpected request: %v %#v\n%#v", test.description, req.Method, req.URL, req)
							return nil, nil
						}
					}),
				}
				tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

				ioStreams, _, outBuf, errBuf := genericiooptions.NewTestIOStreams()
				cmd := NewCmdDrain(tf, ioStreams)

				var recovered interface{}
				sawFatal := false
				fatalMsg := ""
				func() {
					defer func() {
						// Recover from the panic below.
						recovered = recover()
						// Restore cmdutil behavior
						cmdutil.DefaultBehaviorOnFatal()
					}()
					cmdutil.BehaviorOnFatal(func(e string, code int) { sawFatal = true; fatalMsg = e; panic(e) })
					cmd.SetArgs(test.args)
					cmd.Execute()
				}()
				switch {
				case recovered != nil && !sawFatal:
					t.Fatalf("got panic: %v", recovered)
				case test.expectFatal && !sawFatal:
					t.Fatalf("%s: unexpected non-error when using %s", test.description, currMethod)
				case !test.expectFatal && sawFatal:
					t.Fatalf("%s: unexpected error when using %s: %s", test.description, currMethod, fatalMsg)
				}

				deleted := deletions > 0
				evicted := evictions > 0

				if test.expectDelete {
					// Test Delete
					if !testEviction && !deleted {
						t.Fatalf("%s: pod never deleted", test.description)
					}
					// Test Eviction
					if testEviction {
						if !evicted {
							t.Fatalf("%s: pod never evicted", test.description)
						}
						if evictions > 1 {
							t.Fatalf("%s: asked to evict same pod %d too many times", test.description, evictions-1)
						}
					}
				}
				if !test.expectDelete {
					if deleted {
						t.Fatalf("%s: unexpected delete when using %s", test.description, currMethod)
					}
					if deletions > 1 {
						t.Fatalf("%s: asked to deleted same pod %d too many times", test.description, deletions-1)
					}
				}
				if deleted && evicted {
					t.Fatalf("%s: same pod deleted %d times and evicted %d times", test.description, deletions, evictions)
				}

				if len(test.expectWarning) > 0 {
					if len(errBuf.String()) == 0 {
						t.Fatalf("%s: expected warning, but found no stderr output", test.description)
					}

					// Mac and Bazel on Linux behave differently when returning newlines
					if a, e := errBuf.String(), test.expectWarning; !strings.Contains(a, e) {
						t.Fatalf("%s: actual warning message did not match expected warning message.\n Expecting:\n%v\n  Got:\n%v", test.description, e, a)
					}
				}

				if len(test.expectOutputToContain) > 0 {
					out := outBuf.String()
					if !strings.Contains(out, test.expectOutputToContain) {
						t.Fatalf("%s: expected output to contain: %s\nGot:\n%s", test.description, test.expectOutputToContain, out)
					}
				}
			})
		}
	}
}
func TestDrainWithDelay(t *testing.T) {
	tests := []struct {
		description              string
		args                     []string
		expectOutputNotToContain string
		expectOutputToContain    string
	}{
		{
			description:              "no delay",
			args:                     []string{"-l", "name in (node1,node2)"},
			expectOutputNotToContain: "Waiting",
		},
		{
			description:           "with delay",
			args:                  []string{"-l", "name in (node1,node2)", "--delay-between-nodes", "1"},
			expectOutputToContain: "1 seconds",
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory()
			defer tf.Cleanup()
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
			tf.Client = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
				NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					m := &MyReq{req}
					switch {
					case m.isFor("GET", "/pods"):
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.PodList{Items: []corev1.Pod{}})}, nil
					case m.isFor("GET", "/nodes"):
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.NodeList{Items: []corev1.Node{{}, {}}})}, nil
					default:
						t.Fatalf("%s: unexpected request: %v %#v\n%#v", test.description, req.Method, req.URL, req)
						return nil, nil
					}
				}),
			}
			ioStreams, _, outBuf, _ := genericiooptions.NewTestIOStreams()
			cmd := NewCmdDrain(tf, ioStreams)
			cmd.SetArgs(test.args)
			cmd.Execute()
			out := outBuf.String()
			if len(test.expectOutputToContain) > 0 {
				if !strings.Contains(out, test.expectOutputToContain) {
					t.Fatalf("%s: expected output to contain: %s\nGot:\n%s", test.description, test.expectOutputToContain, out)
				}
			}
			if len(test.expectOutputNotToContain) > 0 {
				if strings.Contains(out, test.expectOutputNotToContain) {
					t.Fatalf("%s: expected output not to contain: %s\nGot:\n%s", test.description, test.expectOutputToContain, out)
				}
			}
		})
	}
}

type MyReq struct {
	Request *http.Request
}

func (m *MyReq) isFor(method string, path string) bool {
	req := m.Request

	return method == req.Method && (req.URL.Path == path ||
		req.URL.Path == strings.Join([]string{"/api/v1", path}, "") ||
		req.URL.Path == strings.Join([]string{"/apis/apps/v1", path}, "") ||
		req.URL.Path == strings.Join([]string{"/apis/batch/v1", path}, ""))
}
