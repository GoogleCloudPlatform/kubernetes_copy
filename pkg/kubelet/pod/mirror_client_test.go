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

package pod

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

func TestParsePodFullName(t *testing.T) {
	type nameTuple struct {
		Name      string
		Namespace string
	}
	successfulCases := map[string]nameTuple{
		"bar_foo":         {Name: "bar", Namespace: "foo"},
		"bar.org_foo.com": {Name: "bar.org", Namespace: "foo.com"},
		"bar-bar_foo":     {Name: "bar-bar", Namespace: "foo"},
	}
	failedCases := []string{"barfoo", "bar_foo_foo", "", "bar_", "_foo"}

	for podFullName, expected := range successfulCases {
		name, namespace, err := kubecontainer.ParsePodFullName(podFullName)
		if err != nil {
			t.Errorf("unexpected error when parsing the full name: %v", err)
			continue
		}
		if name != expected.Name || namespace != expected.Namespace {
			t.Errorf("expected name %q, namespace %q; got name %q, namespace %q",
				expected.Name, expected.Namespace, name, namespace)
		}
	}
	for _, podFullName := range failedCases {
		_, _, err := kubecontainer.ParsePodFullName(podFullName)
		if err == nil {
			t.Errorf("expected error when parsing the full name, got none")
		}
	}
}

func TestCreateMirrorPod(t *testing.T) {
	tCtx := ktesting.Init(t)
	const (
		testNodeName = "test-node-name"
		testNodeUID  = types.UID("test-node-uid-1234")
		testPodName  = "test-pod-name"
		testPodNS    = "test-pod-ns"
		testPodHash  = "123456789"
	)
	testcases := []struct {
		desc          string
		node          *v1.Node
		nodeErr       error
		expectSuccess bool
	}{{
		desc:          "cannot get node",
		nodeErr:       errors.New("expected: cannot get node"),
		expectSuccess: false,
	}, {
		desc: "node missing UID",
		node: &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNodeName,
			},
		},
		expectSuccess: false,
	}, {
		desc: "successfully fetched node",
		node: &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNodeName,
				UID:  testNodeUID,
			},
		},
		expectSuccess: true,
	}}

	for _, test := range testcases {
		t.Run(test.desc, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			nodeGetter := &fakeNodeGetter{
				t:              t,
				expectNodeName: testNodeName,
				node:           test.node,
				err:            test.nodeErr,
			}
			mc := NewBasicMirrorClient(clientset, testNodeName, nodeGetter)

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPodName,
					Namespace: testPodNS,
					Annotations: map[string]string{
						kubetypes.ConfigHashAnnotationKey: testPodHash,
					},
				},
			}

			err := mc.CreateMirrorPod(tCtx, pod)
			if !test.expectSuccess {
				assert.Error(t, err)
				return
			}

			createdPod, err := clientset.CoreV1().Pods(testPodNS).Get(tCtx, testPodName, metav1.GetOptions{})
			require.NoError(t, err)

			// Validate created pod
			assert.Equal(t, testPodHash, createdPod.Annotations[kubetypes.ConfigMirrorAnnotationKey])
			assert.Len(t, createdPod.OwnerReferences, 1)
			expectedOwnerRef := metav1.OwnerReference{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       testNodeName,
				UID:        testNodeUID,
				Controller: ptr.To(true),
			}
			assert.Equal(t, expectedOwnerRef, createdPod.OwnerReferences[0])
		})
	}
}

type fakeNodeGetter struct {
	t              *testing.T
	expectNodeName string

	node *v1.Node
	err  error
}

func (f *fakeNodeGetter) Get(nodeName string) (*v1.Node, error) {
	require.Equal(f.t, f.expectNodeName, nodeName)
	return f.node, f.err
}
