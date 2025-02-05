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

package get

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/rest/fake"
	restclientwatch "k8s.io/client-go/rest/watch"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	"k8s.io/kubectl/pkg/scheme"
)

var (
	grace              = int64(30)
	enableServiceLinks = corev1.DefaultEnableServiceLinks
)

func testComponentStatusData() *corev1.ComponentStatusList {
	good := corev1.ComponentStatus{
		Conditions: []corev1.ComponentCondition{
			{Type: corev1.ComponentHealthy, Status: corev1.ConditionTrue, Message: "ok"},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "servergood"},
	}

	bad := corev1.ComponentStatus{
		Conditions: []corev1.ComponentCondition{
			{Type: corev1.ComponentHealthy, Status: corev1.ConditionFalse, Message: "", Error: "bad status: 500"},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "serverbad"},
	}

	unknown := corev1.ComponentStatus{
		Conditions: []corev1.ComponentCondition{
			{Type: corev1.ComponentHealthy, Status: corev1.ConditionUnknown, Message: "", Error: "fizzbuzz error"},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "serverunknown"},
	}

	return &corev1.ComponentStatusList{
		Items: []corev1.ComponentStatus{good, bad, unknown},
	}
}

// Verifies that schemas that are not in the master tree of Kubernetes can be retrieved via Get.
func TestGetUnknownSchemaObject(t *testing.T) {
	t.Skip("This test is completely broken.  The first thing it does is add the object to the scheme!")
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	_, _, codec := cmdtesting.NewExternalScheme()

	obj := &cmdtesting.ExternalType{
		Kind:       "Type",
		APIVersion: "apitest/unlikelyversion",
		Name:       "foo",
	}

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp: &http.Response{
			StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(),
			Body: cmdtesting.ObjBody(codec, obj),
		},
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"type", "foo"})

	expected := []runtime.Object{cmdtesting.NewInternalType("", "", "foo")}
	actual := []runtime.Object{}
	if len(actual) != len(expected) {
		t.Fatalf("expected: %#v, but actual: %#v", expected, actual)
	}
	t.Logf("actual: %#v", actual[0])
	for i, obj := range actual {
		expectedJSON := runtime.EncodeOrDie(codec, expected[i])
		expectedMap := map[string]interface{}{}
		if err := json.Unmarshal([]byte(expectedJSON), &expectedMap); err != nil {
			t.Fatal(err)
		}

		actualJSON := runtime.EncodeOrDie(codec, obj)
		actualMap := map[string]interface{}{}
		if err := json.Unmarshal([]byte(actualJSON), &actualMap); err != nil {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(expectedMap, actualMap) {
			t.Errorf("expectedMap: %#v, but actualMap: %#v", expectedMap, actualMap)
		}
	}
}

// Verifies that schemas that are not in the master tree of Kubernetes can be retrieved via Get.
func TestGetSchemaObject(t *testing.T) {
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(corev1.SchemeGroupVersion)
	t.Logf("%v", string(runtime.EncodeOrDie(codec, &corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Name: "foo"}})))

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Name: "foo"}})},
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.Run(cmd, []string{"replicationcontrollers", "foo"})

	if !strings.Contains(buf.String(), "foo") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestGetObjects(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   AGE
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetObjectSubresourceStatus(t *testing.T) {
	_, _, replicationcontrollers := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &replicationcontrollers.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("subresource", "status")
	cmd.Run(cmd, []string{"replicationcontrollers", "rc1"})

	expected := `NAME   AGE
rc1    <unknown>
`

	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetObjectSubresourceScale(t *testing.T) {
	_, _, replicationcontrollers := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: replicationControllersScaleSubresourceTableObjBody(codec, replicationcontrollers.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("subresource", "scale")
	cmd.Run(cmd, []string{"replicationcontrollers", "rc1"})

	expected := `NAME   DESIRED   AVAILABLE
rc1    1         0
`

	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetTableObjects(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetV1TableObjects(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podV1TableObjBody(codec, pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetObjectsShowKind(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("show-kind", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME      AGE
pod/foo   <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetTableObjectsShowKind(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("show-kind", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME      READY   STATUS   RESTARTS   AGE
pod/foo   0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleResourceTypesShowKinds(t *testing.T) {
	pods, svcs, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)}, nil
			case p == "/namespaces/test/replicationcontrollers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.ReplicationControllerList{})}, nil
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svcs)}, nil
			case p == "/namespaces/test/statefulsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &appsv1.StatefulSetList{})}, nil
			case p == "/namespaces/test/horizontalpodautoscalers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &autoscalingv1.HorizontalPodAutoscalerList{})}, nil
			case p == "/namespaces/test/jobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &batchv1.JobList{})}, nil
			case p == "/namespaces/test/cronjobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &batchv1beta1.CronJobList{})}, nil
			case p == "/namespaces/test/daemonsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &appsv1.DaemonSetList{})}, nil
			case p == "/namespaces/test/deployments" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &extensionsv1beta1.DeploymentList{})}, nil
			case p == "/namespaces/test/replicasets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &extensionsv1beta1.ReplicaSetList{})}, nil

			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, bufErr := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"all"})

	expected := `NAME      AGE
pod/foo   <unknown>
pod/bar   <unknown>

NAME          AGE
service/baz   <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}

	// The error out should be empty
	if e, a := "", bufErr.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTableResourceTypesShowKinds(t *testing.T) {
	pods, svcs, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items...)}, nil
			case p == "/namespaces/test/replicationcontrollers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.ReplicationControllerList{})}, nil
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: serviceTableObjBody(codec, svcs.Items...)}, nil
			case p == "/namespaces/test/statefulsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &appsv1.StatefulSetList{})}, nil
			case p == "/namespaces/test/horizontalpodautoscalers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &autoscalingv1.HorizontalPodAutoscalerList{})}, nil
			case p == "/namespaces/test/jobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &batchv1.JobList{})}, nil
			case p == "/namespaces/test/cronjobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &batchv1beta1.CronJobList{})}, nil
			case p == "/namespaces/test/daemonsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &appsv1.DaemonSetList{})}, nil
			case p == "/namespaces/test/deployments" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &extensionsv1beta1.DeploymentList{})}, nil
			case p == "/namespaces/test/replicasets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &extensionsv1beta1.ReplicaSetList{})}, nil

			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, bufErr := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"all"})

	expected := `NAME      READY   STATUS   RESTARTS   AGE
pod/foo   0/0              0          <unknown>
pod/bar   0/0              0          <unknown>

NAME          TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
service/baz   ClusterIP   <none>       <none>        <none>    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}

	// The error out should be empty
	if e, a := "", bufErr.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestNoBlankLinesForGetMultipleTableResource(t *testing.T) {
	pods, svcs, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items...)}, nil
			case p == "/namespaces/test/replicationcontrollers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: serviceTableObjBody(codec, svcs.Items...)}, nil
			case p == "/namespaces/test/statefulsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/horizontalpodautoscalers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/jobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/cronjobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/daemonsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/deployments" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/replicasets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil

			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, bufErr := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	expected := `NAME      READY   STATUS   RESTARTS   AGE
pod/foo   0/0              0          <unknown>
pod/bar   0/0              0          <unknown>

NAME          TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
service/baz   ClusterIP   <none>       <none>        <none>    <unknown>
`
	for _, cmdArgs := range [][]string{
		{"pods,services,jobs"},
		{"deployments,pods,statefulsets,services,jobs"},
		{"all"},
	} {
		cmd.Run(cmd, cmdArgs)

		if e, a := expected, buf.String(); e != a {
			t.Errorf("[kubectl get %v] expected\n%v\ngot\n%v", cmdArgs, e, a)
		}

		// The error out should be empty
		if e, a := "", bufErr.String(); e != a {
			t.Errorf("[kubectl get %v] expected\n%v\ngot\n%v", cmdArgs, e, a)
		}

		buf.Reset()
		bufErr.Reset()
	}
}

func TestNoBlankLinesForGetAll(t *testing.T) {
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/replicationcontrollers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/statefulsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/horizontalpodautoscalers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/jobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/cronjobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/daemonsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/deployments" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/replicasets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil

			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, errbuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"all"})

	expected := ``
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
	expectedErr := `No resources found in test namespace.
`
	if e, a := expectedErr, errbuf.String(); e != a {
		t.Errorf("expectedErr\n%v\ngot\n%v", e, a)
	}
}

func TestNoErrorLinesForGetAll(t *testing.T) {
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/replicationcontrollers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/statefulsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/horizontalpodautoscalers" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/jobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/cronjobs" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/daemonsets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil
			case p == "/namespaces/test/deployments" && m == "GET":
				return &http.Response{StatusCode: http.StatusForbidden, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &metav1.Status{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Status",
						APIVersion: "v1",
					},
					Status:  metav1.StatusFailure,
					Code:    http.StatusForbidden,
					Reason:  metav1.StatusReasonForbidden,
					Message: "Not allowed",
				})}, nil
			case p == "/namespaces/test/replicasets" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)}, nil

			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, errbuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"all"})

	expected := ``
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
	expectedErr := `No resources found in test namespace.
`
	if e, a := expectedErr, errbuf.String(); e != a {
		t.Errorf("expectedErr\n%v\ngot\n%v", e, a)
	}
}

func TestNotFoundMessageForGetNonNamespacedResources(t *testing.T) {
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTableObjBody(codec)},
	}

	streams, _, buf, errbuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"persistentvolumes"})

	expected := ``
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
	expectedErr := `No resources found
`
	if e, a := expectedErr, errbuf.String(); e != a {
		t.Errorf("expectedErr\n%v\ngot\n%v", e, a)
	}
}

func TestGetObjectsShowLabels(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("show-labels", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   AGE         LABELS
foo    <unknown>   <none>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetTableObjectsShowLabels(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("show-labels", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE         LABELS
foo    0/0              0          <unknown>   <none>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetEmptyTable(t *testing.T) {
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	emptyTable := io.NopCloser(bytes.NewBufferString(`{
"kind":"Table",
"apiVersion":"meta.k8s.io/v1beta1",
"metadata":{
	"resourceVersion":"346"
},
"columnDefinitions":[
	{"name":"Name","type":"string","format":"name","description":"the name","priority":0}
],
"rows":[]
}`))

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: emptyTable},
	}

	streams, _, buf, errbuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods"})

	expected := ``
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
	expectedErr := `No resources found in test namespace.
`
	if e, a := expectedErr, errbuf.String(); e != a {
		t.Errorf("expectedErr\n%v\ngot\n%v", e, a)
	}
}

func TestGetObjectIgnoreNotFound(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	ns := &corev1.NamespaceList{
		ListMeta: metav1.ListMeta{
			ResourceVersion: "1",
		},
		Items: []corev1.Namespace{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "testns", Namespace: "test", ResourceVersion: "11"},
				Spec:       corev1.NamespaceSpec{},
			},
		},
	}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods/nonexistentpod" && m == "GET":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.StringBody("")}, nil
			case p == "/api/v1/namespaces/test" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &ns.Items[0])}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("ignore-not-found", "true")
	cmd.Flags().Set("output", "yaml")
	cmd.Run(cmd, []string{"pods", "nonexistentpod"})

	if buf.String() != "" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestEmptyResult(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.PodList{})}, nil
		}),
	}

	streams, _, _, errbuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	// we're assuming that an empty file is being passed from stdin
	cmd.Flags().Set("filename", "-")
	cmd.Run(cmd, []string{})

	if !strings.Contains(errbuf.String(), "No resources found") {
		t.Errorf("unexpected output: %q", errbuf.String())
	}
}

func TestEmptyResultJSON(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.PodList{})}, nil
		}),
	}

	streams, _, outbuf, errbuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	// we're assuming that an empty file is being passed from stdin
	cmd.Flags().Set("filename", "-")
	cmd.Flags().Set("output", "json")
	cmd.Run(cmd, []string{})

	if errbuf.Len() > 0 {
		t.Errorf("unexpected error: %q", errbuf.String())
	}
	if !strings.Contains(outbuf.String(), `"items": []`) {
		t.Errorf("unexpected output: %q", outbuf.String())
	}
}

func TestGetSortedObjects(t *testing.T) {
	pods := &corev1.PodList{
		ListMeta: metav1.ListMeta{
			ResourceVersion: "15",
		},
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "test", ResourceVersion: "10"},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "test", ResourceVersion: "11"},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test", ResourceVersion: "9"},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
		},
	}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)},
	}
	tf.ClientConfigVal = &restclient.Config{ContentConfig: restclient.ContentConfig{GroupVersion: &corev1.SchemeGroupVersion}}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	// sorting with metadata.name
	cmd.Flags().Set("sort-by", ".metadata.name")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   AGE
a      <unknown>
b      <unknown>
c      <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetSortedObjectsUnstructuredTable(t *testing.T) {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sortTestTableData()[0])
	if err != nil {
		t.Fatal(err)
	}
	unstructuredBytes, err := json.MarshalIndent(unstructuredMap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// t.Log(string(unstructuredBytes))
	body := io.NopCloser(bytes.NewReader(unstructuredBytes))

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body},
	}
	tf.ClientConfigVal = &restclient.Config{ContentConfig: restclient.ContentConfig{GroupVersion: &corev1.SchemeGroupVersion}}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	// sorting with metadata.name
	cmd.Flags().Set("sort-by", ".metadata.name")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   CUSTOM
a      custom-a
b      custom-b
c      custom-c
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func sortTestData() []runtime.Object {
	return []runtime.Object{
		&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "test", ResourceVersion: "10"},
			Spec: corev1.PodSpec{
				RestartPolicy:                 corev1.RestartPolicyAlways,
				DNSPolicy:                     corev1.DNSClusterFirst,
				TerminationGracePeriodSeconds: &grace,
				SecurityContext:               &corev1.PodSecurityContext{},
				EnableServiceLinks:            &enableServiceLinks,
			},
		},
		&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "test", ResourceVersion: "11"},
			Spec: corev1.PodSpec{
				RestartPolicy:                 corev1.RestartPolicyAlways,
				DNSPolicy:                     corev1.DNSClusterFirst,
				TerminationGracePeriodSeconds: &grace,
				SecurityContext:               &corev1.PodSecurityContext{},
				EnableServiceLinks:            &enableServiceLinks,
			},
		},
		&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test", ResourceVersion: "9"},
			Spec: corev1.PodSpec{
				RestartPolicy:                 corev1.RestartPolicyAlways,
				DNSPolicy:                     corev1.DNSClusterFirst,
				TerminationGracePeriodSeconds: &grace,
				SecurityContext:               &corev1.PodSecurityContext{},
				EnableServiceLinks:            &enableServiceLinks,
			},
		},
	}
}

func sortTestTableData() []runtime.Object {
	return []runtime.Object{
		&metav1beta1.Table{
			TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1beta1", Kind: "Table"},
			ColumnDefinitions: []metav1beta1.TableColumnDefinition{
				{Name: "NAME", Type: "string", Format: "name"},
				{Name: "CUSTOM", Type: "string", Format: ""},
			},
			Rows: []metav1beta1.TableRow{
				{
					Cells: []interface{}{"c", "custom-c"},
					Object: runtime.RawExtension{
						Object: &corev1.Pod{
							TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
							ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "test", ResourceVersion: "10"},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: &grace,
								SecurityContext:               &corev1.PodSecurityContext{},
								EnableServiceLinks:            &enableServiceLinks,
							},
						},
					},
				},
				{
					Cells: []interface{}{"b", "custom-b"},
					Object: runtime.RawExtension{
						Object: &corev1.Pod{
							TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
							ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "test", ResourceVersion: "11"},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: &grace,
								SecurityContext:               &corev1.PodSecurityContext{},
								EnableServiceLinks:            &enableServiceLinks,
							},
						},
					},
				},
				{
					Cells: []interface{}{"a", "custom-a"},
					Object: runtime.RawExtension{
						Object: &corev1.Pod{
							TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
							ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test", ResourceVersion: "9"},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: &grace,
								SecurityContext:               &corev1.PodSecurityContext{},
								EnableServiceLinks:            &enableServiceLinks,
							},
						},
					},
				},
			},
		},
	}
}

func TestRuntimeSorter(t *testing.T) {
	tests := []struct {
		name        string
		field       string
		objs        []runtime.Object
		op          func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error
		expect      string
		expectError string
	}{
		{
			name:  "ensure sorter works with an empty object list",
			field: "metadata.name",
			objs:  []runtime.Object{},
			op: func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error {
				return nil
			},
			expect: "",
		},
		{
			name:  "ensure sorter returns original position",
			field: "metadata.name",
			objs:  sortTestData(),
			op: func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error {
				for idx := range objs {
					p := sorter.OriginalPosition(idx)
					fmt.Fprintf(out, "%v,", p)
				}
				return nil
			},
			expect: "2,1,0,",
		},
		{
			name:  "ensure sorter handles table object position",
			field: "metadata.name",
			objs:  sortTestTableData(),
			op: func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error {
				for idx := range objs {
					p := sorter.OriginalPosition(idx)
					fmt.Fprintf(out, "%v,", p)
				}
				return nil
			},
			expect: "0,",
		},
		{
			name:  "ensure sorter sorts table objects",
			field: "metadata.name",
			objs:  sortTestData(),
			op: func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error {
				for _, o := range objs {
					fmt.Fprintf(out, "%s,", o.(*corev1.Pod).Name)
				}
				return nil
			},
			expect: "a,b,c,",
		},
		{
			name:        "ensure sorter rejects mixed Table + non-Table object lists",
			field:       "metadata.name",
			objs:        append(sortTestData(), sortTestTableData()...),
			op:          func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error { return nil },
			expectError: "sorting is not supported on mixed Table",
		},
		{
			name:        "ensure sorter errors out on invalid jsonpath",
			field:       "metadata.unknown",
			objs:        sortTestData(),
			op:          func(sorter *RuntimeSorter, objs []runtime.Object, out io.Writer) error { return nil },
			expectError: "couldn't find any field with path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sorter := NewRuntimeSorter(tc.objs, tc.field)
			if err := sorter.Sort(); err != nil {
				if len(tc.expectError) > 0 && strings.Contains(err.Error(), tc.expectError) {
					return
				}

				if len(tc.expectError) > 0 {
					t.Fatalf("unexpected error: expecting %s, but got %s", tc.expectError, err)
				}

				t.Fatalf("unexpected error: %v", err)
			}

			out := bytes.NewBuffer([]byte{})
			err := tc.op(sorter, tc.objs, out)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.expect != out.String() {
				t.Fatalf("unexpected output: expecting %s, but got %s", tc.expect, out.String())
			}

		})
	}

}

func TestGetObjectsIdentifiedByFile(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("filename", "../../../testdata/controller.yaml")
	cmd.Run(cmd, []string{})

	expected := `NAME   AGE
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetTableObjectsIdentifiedByFile(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items[0])},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("filename", "../../../testdata/controller.yaml")
	cmd.Run(cmd, []string{})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetListObjects(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   AGE
foo    <unknown>
bar    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetListTableObjects(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items...)},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
bar    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetListComponentStatus(t *testing.T) {
	statuses := testComponentStatusData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Resp:                 &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: componentStatusTableObjBody(codec, (*statuses).Items...)},
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"componentstatuses"})

	expected := `NAME            STATUS      MESSAGE   ERROR
servergood      Healthy     ok        
serverbad       Unhealthy             bad status: 500
serverunknown   Unhealthy             fizzbuzz error
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMixedGenericObjects(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	// ensure that a runtime.Object without
	// an ObjectMeta field is handled properly
	structuredObj := &metav1.Status{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Status",
			APIVersion: "v1",
		},
		Status:  "Success",
		Message: "",
		Reason:  "",
		Code:    0,
	}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, structuredObj)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Flags().Set("output", "json")
	cmd.Run(cmd, []string{"pods"})

	expected := `{
    "apiVersion": "v1",
    "items": [
        {
            "apiVersion": "v1",
            "kind": "Status",
            "metadata": {},
            "status": "Success"
        }
    ],
    "kind": "List",
    "metadata": {
        "resourceVersion": ""
    }
}
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeObjects(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods,services"})

	expected := `NAME      AGE
pod/foo   <unknown>
pod/bar   <unknown>

NAME          AGE
service/baz   <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeTableObjects(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items...)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: serviceTableObjBody(codec, svc.Items...)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.Run(cmd, []string{"pods,services"})

	expected := `NAME      READY   STATUS   RESTARTS   AGE
pod/foo   0/0              0          <unknown>
pod/bar   0/0              0          <unknown>

NAME          TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
service/baz   ClusterIP   <none>       <none>        <none>    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeObjectsAsList(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("output", "json")
	cmd.Run(cmd, []string{"pods,services"})

	expected := `{
    "apiVersion": "v1",
    "items": [
        {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {
                "creationTimestamp": null,
                "name": "foo",
                "namespace": "test",
                "resourceVersion": "10"
            },
            "spec": {
                "containers": null,
                "dnsPolicy": "ClusterFirst",
                "enableServiceLinks": true,
                "restartPolicy": "Always",
                "securityContext": {},
                "terminationGracePeriodSeconds": 30
            },
            "status": {}
        },
        {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {
                "creationTimestamp": null,
                "name": "bar",
                "namespace": "test",
                "resourceVersion": "11"
            },
            "spec": {
                "containers": null,
                "dnsPolicy": "ClusterFirst",
                "enableServiceLinks": true,
                "restartPolicy": "Always",
                "securityContext": {},
                "terminationGracePeriodSeconds": 30
            },
            "status": {}
        },
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "creationTimestamp": null,
                "name": "baz",
                "namespace": "test",
                "resourceVersion": "12"
            },
            "spec": {
                "sessionAffinity": "None",
                "type": "ClusterIP"
            },
            "status": {
                "loadBalancer": {}
            }
        }
    ],
    "kind": "List",
    "metadata": {
        "resourceVersion": ""
    }
}
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("did not match:\n%v", cmp.Diff(e, a))
	}
}

func TestGetMultipleTypeObjectsWithLabelSelector(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.LabelSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("selector", "a=b")
	cmd.Run(cmd, []string{"pods,services"})

	expected := `NAME      AGE
pod/foo   <unknown>
pod/bar   <unknown>

NAME          AGE
service/baz   <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeTableObjectsWithLabelSelector(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.LabelSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items...)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: serviceTableObjBody(codec, svc.Items...)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("selector", "a=b")
	cmd.Run(cmd, []string{"pods,services"})

	expected := `NAME      READY   STATUS   RESTARTS   AGE
pod/foo   0/0              0          <unknown>
pod/bar   0/0              0          <unknown>

NAME          TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
service/baz   ClusterIP   <none>       <none>        <none>    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeObjectsWithFieldSelector(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.FieldSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("field-selector", "a=b")
	cmd.Run(cmd, []string{"pods,services"})

	expected := `NAME      AGE
pod/foo   <unknown>
pod/bar   <unknown>

NAME          AGE
service/baz   <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeTableObjectsWithFieldSelector(t *testing.T) {
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.FieldSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods.Items...)}, nil
			case "/namespaces/test/services":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: serviceTableObjBody(codec, svc.Items...)}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("field-selector", "a=b")
	cmd.Run(cmd, []string{"pods,services"})

	expected := `NAME      READY   STATUS   RESTARTS   AGE
pod/foo   0/0              0          <unknown>
pod/bar   0/0              0          <unknown>

NAME          TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
service/baz   ClusterIP   <none>       <none>        <none>    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeObjectsWithDirectReference(t *testing.T) {
	_, svc, _ := cmdtesting.TestData()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
	}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/nodes/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, node)}, nil
			case "/namespaces/test/services/bar":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Run(cmd, []string{"services/bar", "node/foo"})

	expected := `NAME          AGE
service/baz   <unknown>

NAME       AGE
node/foo   <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestGetMultipleTypeTableObjectsWithDirectReference(t *testing.T) {
	_, svc, _ := cmdtesting.TestData()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
	}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/nodes/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: nodeTableObjBody(codec, *node)}, nil
			case "/namespaces/test/services/bar":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: serviceTableObjBody(codec, svc.Items[0])}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Run(cmd, []string{"services/bar", "node/foo"})

	expected := `NAME          TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
service/baz   ClusterIP   <none>       <none>        <none>    <unknown>

NAME       STATUS    ROLES    AGE         VERSION
node/foo   Unknown   <none>   <unknown>   
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func watchTestData() ([]corev1.Pod, []watch.Event) {
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "bar",
				Namespace:       "test",
				ResourceVersion: "9",
			},
			Spec: corev1.PodSpec{
				RestartPolicy:                 corev1.RestartPolicyAlways,
				DNSPolicy:                     corev1.DNSClusterFirst,
				TerminationGracePeriodSeconds: &grace,
				SecurityContext:               &corev1.PodSecurityContext{},
				EnableServiceLinks:            &enableServiceLinks,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "foo",
				Namespace:       "test",
				ResourceVersion: "10",
			},
			Spec: corev1.PodSpec{
				RestartPolicy:                 corev1.RestartPolicyAlways,
				DNSPolicy:                     corev1.DNSClusterFirst,
				TerminationGracePeriodSeconds: &grace,
				SecurityContext:               &corev1.PodSecurityContext{},
				EnableServiceLinks:            &enableServiceLinks,
			},
		},
	}
	events := []watch.Event{
		// current state events
		{
			Type: watch.Added,
			Object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "bar",
					Namespace:       "test",
					ResourceVersion: "9",
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
		},
		{
			Type: watch.Added,
			Object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "foo",
					Namespace:       "test",
					ResourceVersion: "10",
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
		},
		// resource events
		{
			Type: watch.Modified,
			Object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "foo",
					Namespace:       "test",
					ResourceVersion: "11",
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
		},
		{
			Type: watch.Deleted,
			Object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "foo",
					Namespace:       "test",
					ResourceVersion: "12",
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext:               &corev1.PodSecurityContext{},
					EnableServiceLinks:            &enableServiceLinks,
				},
			},
		},
	}
	return pods, events
}

func TestWatchLabelSelector(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	podList := &corev1.PodList{
		Items: pods,
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
	}
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.LabelSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[2:])}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, podList)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Flags().Set("selector", "a=b")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   AGE
bar    <unknown>
foo    <unknown>
foo    <unknown>
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchTableLabelSelector(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	podList := &corev1.PodList{
		Items: pods,
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
	}
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.LabelSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableWatchBody(codec, events[2:])}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, podList.Items...)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Flags().Set("selector", "a=b")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
bar    0/0              0          <unknown>
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchFieldSelector(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	podList := &corev1.PodList{
		Items: pods,
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
	}
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.FieldSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[2:])}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, podList)}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Flags().Set("field-selector", "a=b")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   AGE
bar    <unknown>
foo    <unknown>
foo    <unknown>
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchTableFieldSelector(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	podList := &corev1.PodList{
		Items: pods,
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
	}
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get(metav1.FieldSelectorQueryParam("v1")) != "a=b" {
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
			}
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableWatchBody(codec, events[2:])}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, podList.Items...)}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Flags().Set("field-selector", "a=b")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
bar    0/0              0          <unknown>
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchResource(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods[1])}, nil
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "metadata.name=foo" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[1:])}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   AGE
foo    <unknown>
foo    <unknown>
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchStatus(t *testing.T) {
	pods, events := watchTestData()
	events = append(events, watch.Event{Type: "ERROR", Object: &metav1.Status{Status: "Failure", Reason: "InternalServerError", Message: "Something happened"}})

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods[1])}, nil
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "metadata.name=foo" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[1:])}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   AGE
foo    <unknown>
foo    <unknown>
foo    <unknown>

STATUS    REASON                MESSAGE
Failure   InternalServerError   Something happened
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchTableResource(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods[1])}, nil
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "metadata.name=foo" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableWatchBody(codec, events[1:])}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchResourceTable(t *testing.T) {
	columns := []metav1beta1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: "the name", Priority: 0},
		{Name: "Active", Type: "boolean", Description: "active", Priority: 0},
	}

	listTable := &metav1beta1.Table{
		TypeMeta:          metav1.TypeMeta{APIVersion: "meta.k8s.io/v1beta1", Kind: "Table"},
		ColumnDefinitions: columns,
		Rows: []metav1beta1.TableRow{
			{
				Cells: []interface{}{"a", true},
				Object: runtime.RawExtension{
					Object: &corev1.Pod{
						TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
						ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test", ResourceVersion: "10"},
					},
				},
			},
			{
				Cells: []interface{}{"b", true},
				Object: runtime.RawExtension{
					Object: &corev1.Pod{
						TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
						ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "test", ResourceVersion: "20"},
					},
				},
			},
		},
	}

	events := []watch.Event{
		{
			Type: watch.Added,
			Object: &metav1beta1.Table{
				TypeMeta:          metav1.TypeMeta{APIVersion: "meta.k8s.io/v1beta1", Kind: "Table"},
				ColumnDefinitions: columns, // first event includes the columns
				Rows: []metav1beta1.TableRow{{
					Cells: []interface{}{"a", false},
					Object: runtime.RawExtension{
						Object: &corev1.Pod{
							TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
							ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test", ResourceVersion: "30"},
						},
					},
				}},
			},
		},
		{
			Type: watch.Deleted,
			Object: &metav1beta1.Table{
				ColumnDefinitions: []metav1beta1.TableColumnDefinition{},
				Rows: []metav1beta1.TableRow{{
					Cells: []interface{}{"b", false},
					Object: runtime.RawExtension{
						Object: &corev1.Pod{
							TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
							ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "test", ResourceVersion: "40"},
						},
					},
				}},
			},
		},
	}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") != "true" && req.URL.Query().Get("fieldSelector") == "" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, listTable)}, nil
				}
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events)}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   ACTIVE
a      true
b      true
a      false
b      false
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchResourceWatchEvents(t *testing.T) {

	testcases := []struct {
		format   string
		table    bool
		expected string
	}{
		{
			format: "",
			expected: `EVENT      NAMESPACE   NAME      AGE
ADDED      test        pod/bar   <unknown>
ADDED      test        pod/foo   <unknown>
MODIFIED   test        pod/foo   <unknown>
DELETED    test        pod/foo   <unknown>
`,
		},
		{
			format: "",
			table:  true,
			expected: `EVENT      NAMESPACE   NAME      READY   STATUS   RESTARTS   AGE
ADDED      test        pod/bar   0/0              0          <unknown>
ADDED      test        pod/foo   0/0              0          <unknown>
MODIFIED   test        pod/foo   0/0              0          <unknown>
DELETED    test        pod/foo   0/0              0          <unknown>
`,
		},
		{
			format: "wide",
			table:  true,
			expected: `EVENT      NAMESPACE   NAME      READY   STATUS   RESTARTS   AGE         IP       NODE     NOMINATED NODE   READINESS GATES
ADDED      test        pod/bar   0/0              0          <unknown>   <none>   <none>   <none>           <none>
ADDED      test        pod/foo   0/0              0          <unknown>   <none>   <none>   <none>           <none>
MODIFIED   test        pod/foo   0/0              0          <unknown>   <none>   <none>   <none>           <none>
DELETED    test        pod/foo   0/0              0          <unknown>   <none>   <none>   <none>           <none>
`,
		},
		{
			format: "json",
			expected: `{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"creationTimestamp":null,"name":"bar","namespace":"test","resourceVersion":"9"},"spec":{"containers":null,"dnsPolicy":"ClusterFirst","enableServiceLinks":true,"restartPolicy":"Always","securityContext":{},"terminationGracePeriodSeconds":30},"status":{}}}
{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"creationTimestamp":null,"name":"foo","namespace":"test","resourceVersion":"10"},"spec":{"containers":null,"dnsPolicy":"ClusterFirst","enableServiceLinks":true,"restartPolicy":"Always","securityContext":{},"terminationGracePeriodSeconds":30},"status":{}}}
{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"creationTimestamp":null,"name":"foo","namespace":"test","resourceVersion":"11"},"spec":{"containers":null,"dnsPolicy":"ClusterFirst","enableServiceLinks":true,"restartPolicy":"Always","securityContext":{},"terminationGracePeriodSeconds":30},"status":{}}}
{"type":"DELETED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"creationTimestamp":null,"name":"foo","namespace":"test","resourceVersion":"12"},"spec":{"containers":null,"dnsPolicy":"ClusterFirst","enableServiceLinks":true,"restartPolicy":"Always","securityContext":{},"terminationGracePeriodSeconds":30},"status":{}}}
`,
		},
		{
			format: "yaml",
			expected: `object:
  apiVersion: v1
  kind: Pod
  metadata:
    creationTimestamp: null
    name: bar
    namespace: test
    resourceVersion: "9"
  spec:
    containers: null
    dnsPolicy: ClusterFirst
    enableServiceLinks: true
    restartPolicy: Always
    securityContext: {}
    terminationGracePeriodSeconds: 30
  status: {}
type: ADDED
---
object:
  apiVersion: v1
  kind: Pod
  metadata:
    creationTimestamp: null
    name: foo
    namespace: test
    resourceVersion: "10"
  spec:
    containers: null
    dnsPolicy: ClusterFirst
    enableServiceLinks: true
    restartPolicy: Always
    securityContext: {}
    terminationGracePeriodSeconds: 30
  status: {}
type: ADDED
---
object:
  apiVersion: v1
  kind: Pod
  metadata:
    creationTimestamp: null
    name: foo
    namespace: test
    resourceVersion: "11"
  spec:
    containers: null
    dnsPolicy: ClusterFirst
    enableServiceLinks: true
    restartPolicy: Always
    securityContext: {}
    terminationGracePeriodSeconds: 30
  status: {}
type: MODIFIED
---
object:
  apiVersion: v1
  kind: Pod
  metadata:
    creationTimestamp: null
    name: foo
    namespace: test
    resourceVersion: "12"
  spec:
    containers: null
    dnsPolicy: ClusterFirst
    enableServiceLinks: true
    restartPolicy: Always
    securityContext: {}
    terminationGracePeriodSeconds: 30
  status: {}
type: DELETED
`,
		},
		{
			format: `jsonpath={.type},{.object.metadata.name},{.object.metadata.resourceVersion}{"\n"}`,
			expected: `ADDED,bar,9
ADDED,foo,10
MODIFIED,foo,11
DELETED,foo,12
`,
		},
		{
			format: `go-template={{.type}},{{.object.metadata.name}},{{.object.metadata.resourceVersion}}{{"\n"}}`,
			expected: `ADDED,bar,9
ADDED,foo,10
MODIFIED,foo,11
DELETED,foo,12
`,
		},
		{
			format: `custom-columns=TYPE:.type,NAME:.object.metadata.name,RSRC:.object.metadata.resourceVersion`,
			expected: `TYPE    NAME   RSRC
ADDED   bar    9
ADDED   foo    10
MODIFIED   foo    11
DELETED    foo    12
`,
		},
		{
			format: `name`,
			expected: `pod/bar
pod/foo
pod/foo
pod/foo
`,
		},
	}

	for _, tc := range testcases {
		t.Run(fmt.Sprintf("%s, table=%v", tc.format, tc.table), func(t *testing.T) {
			pods, events := watchTestData()

			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()
			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

			podList := &corev1.PodList{
				Items: pods,
				ListMeta: metav1.ListMeta{
					ResourceVersion: "10",
				},
			}

			tf.UnstructuredClient = &fake.RESTClient{
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch req.URL.Path {
					case "/pods":
						if req.URL.Query().Get("watch") == "true" {
							if tc.table {
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableWatchBody(codec, events[2:])}, nil
							} else {
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[2:])}, nil
							}
						}

						if tc.table {
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, podList.Items...)}, nil
						} else {
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, podList)}, nil
						}
					default:
						t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
						return nil, nil
					}
				}),
			}

			streams, _, buf, _ := genericiooptions.NewTestIOStreams()
			cmd := NewCmdGet("kubectl", tf, streams)
			cmd.SetOut(buf)
			cmd.SetErr(buf)

			cmd.Flags().Set("watch", "true")
			cmd.Flags().Set("all-namespaces", "true")
			cmd.Flags().Set("show-kind", "true")
			cmd.Flags().Set("output-watch-events", "true")
			if len(tc.format) > 0 {
				cmd.Flags().Set("output", tc.format)
			}

			cmd.Run(cmd, []string{"pods"})
			if e, a := tc.expected, buf.String(); e != a {
				t.Errorf("expected\n%v\ngot\n%v", e, a)
			}
		})
	}
}

func TestWatchResourceIdentifiedByFile(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/replicationcontrollers/cassandra":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods[1])}, nil
			case "/namespaces/test/replicationcontrollers":
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "metadata.name=cassandra" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[1:])}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch", "true")
	cmd.Flags().Set("filename", "../../../testdata/controller.yaml")
	cmd.Run(cmd, []string{})

	expected := `NAME   AGE
foo    <unknown>
foo    <unknown>
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchOnlyResource(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods[1])}, nil
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "metadata.name=foo" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[1:])}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch-only", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   AGE
foo    <unknown>
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchOnlyTableResource(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods/foo":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, pods[1])}, nil
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" && req.URL.Query().Get("fieldSelector") == "metadata.name=foo" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableWatchBody(codec, events[1:])}, nil
				}
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch-only", "true")
	cmd.Run(cmd, []string{"pods", "foo"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchOnlyList(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	podList := &corev1.PodList{
		Items: pods,
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
	}
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: watchBody(codec, events[2:])}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, podList)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch-only", "true")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   AGE
foo    <unknown>
foo    <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func TestWatchOnlyTableList(t *testing.T) {
	pods, events := watchTestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	podList := &corev1.PodList{
		Items: pods,
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
	}
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/namespaces/test/pods":
				if req.URL.Query().Get("watch") == "true" {
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableWatchBody(codec, events[2:])}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: podTableObjBody(codec, podList.Items...)}, nil
			default:
				t.Fatalf("request url: %#v,and request: %#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdGet("kubectl", tf, streams)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.Flags().Set("watch-only", "true")
	cmd.Run(cmd, []string{"pods"})

	expected := `NAME   READY   STATUS   RESTARTS   AGE
foo    0/0              0          <unknown>
foo    0/0              0          <unknown>
`
	if e, a := expected, buf.String(); e != a {
		t.Errorf("expected\n%v\ngot\n%v", e, a)
	}
}

func watchBody(codec runtime.Codec, events []watch.Event) io.ReadCloser {
	buf := bytes.NewBuffer([]byte{})
	enc := restclientwatch.NewEncoder(streaming.NewEncoder(buf, codec), codec)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			panic(err)
		}
	}
	return io.NopCloser(buf)
}

var podColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name"},
	{Name: "Ready", Type: "string", Format: ""},
	{Name: "Status", Type: "string", Format: ""},
	{Name: "Restarts", Type: "integer", Format: ""},
	{Name: "Age", Type: "string", Format: ""},
	{Name: "IP", Type: "string", Format: "", Priority: 1},
	{Name: "Node", Type: "string", Format: "", Priority: 1},
	{Name: "Nominated Node", Type: "string", Format: "", Priority: 1},
	{Name: "Readiness Gates", Type: "string", Format: "", Priority: 1},
}

// build a meta table response from a pod list
func podTableObjBody(codec runtime.Codec, pods ...corev1.Pod) io.ReadCloser {
	table := &metav1beta1.Table{
		TypeMeta:          metav1.TypeMeta{APIVersion: "meta.k8s.io/v1beta1", Kind: "Table"},
		ColumnDefinitions: podColumns,
	}
	for i := range pods {
		b := bytes.NewBuffer(nil)
		codec.Encode(&pods[i], b)
		table.Rows = append(table.Rows, metav1beta1.TableRow{
			Object: runtime.RawExtension{Raw: b.Bytes()},
			Cells:  []interface{}{pods[i].Name, "0/0", "", int64(0), "<unknown>", "<none>", "<none>", "<none>", "<none>"},
		})
	}
	data, err := json.Marshal(table)
	if err != nil {
		panic(err)
	}
	if !strings.Contains(string(data), `"meta.k8s.io/v1beta1"`) {
		panic("expected v1beta1, got " + string(data))
	}
	return cmdtesting.BytesBody(data)
}

// build a meta table response from a pod list
func podV1TableObjBody(codec runtime.Codec, pods ...corev1.Pod) io.ReadCloser {
	table := &metav1.Table{
		TypeMeta:          metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: podColumns,
	}
	for i := range pods {
		b := bytes.NewBuffer(nil)
		codec.Encode(&pods[i], b)
		table.Rows = append(table.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Raw: b.Bytes()},
			Cells:  []interface{}{pods[i].Name, "0/0", "", int64(0), "<unknown>", "<none>", "<none>", "<none>", "<none>"},
		})
	}
	data, err := json.Marshal(table)
	if err != nil {
		panic(err)
	}
	if !strings.Contains(string(data), `"meta.k8s.io/v1"`) {
		panic("expected v1, got " + string(data))
	}
	return cmdtesting.BytesBody(data)
}

// build meta table watch events from pod watch events
func podTableWatchBody(codec runtime.Codec, events []watch.Event) io.ReadCloser {
	tableEvents := []watch.Event{}
	for i, e := range events {
		b := bytes.NewBuffer(nil)
		codec.Encode(e.Object, b)
		var columns []metav1.TableColumnDefinition
		if i == 0 {
			columns = podColumns
		}
		tableEvents = append(tableEvents, watch.Event{
			Type: e.Type,
			Object: &metav1.Table{
				ColumnDefinitions: columns,
				Rows: []metav1.TableRow{{
					Object: runtime.RawExtension{Raw: b.Bytes()},
					Cells:  []interface{}{e.Object.(*corev1.Pod).Name, "0/0", "", int64(0), "<unknown>", "<none>", "<none>", "<none>", "<none>"},
				}}},
		})
	}
	return watchBody(codec, tableEvents)
}

// build a meta table response from a service list
func serviceTableObjBody(codec runtime.Codec, services ...corev1.Service) io.ReadCloser {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Type", Type: "string", Format: ""},
			{Name: "Cluster-IP", Type: "string", Format: ""},
			{Name: "External-IP", Type: "string", Format: ""},
			{Name: "Port(s)", Type: "string", Format: ""},
			{Name: "Age", Type: "string", Format: ""},
		},
	}
	for i := range services {
		b := bytes.NewBuffer(nil)
		codec.Encode(&services[i], b)
		table.Rows = append(table.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Raw: b.Bytes()},
			Cells:  []interface{}{services[i].Name, "ClusterIP", "<none>", "<none>", "<none>", "<unknown>"},
		})
	}
	return cmdtesting.ObjBody(codec, table)
}

// build a meta table response from a node list
func nodeTableObjBody(codec runtime.Codec, nodes ...corev1.Node) io.ReadCloser {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string", Format: ""},
			{Name: "Roles", Type: "string", Format: ""},
			{Name: "Age", Type: "string", Format: ""},
			{Name: "Version", Type: "string", Format: ""},
		},
	}
	for i := range nodes {
		b := bytes.NewBuffer(nil)
		codec.Encode(&nodes[i], b)
		table.Rows = append(table.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Raw: b.Bytes()},
			Cells:  []interface{}{nodes[i].Name, "Unknown", "<none>", "<unknown>", ""},
		})
	}
	return cmdtesting.ObjBody(codec, table)
}

// build a meta table response from a componentStatus list
func componentStatusTableObjBody(codec runtime.Codec, componentStatuses ...corev1.ComponentStatus) io.ReadCloser {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string", Format: ""},
			{Name: "Message", Type: "string", Format: ""},
			{Name: "Error", Type: "string", Format: ""},
		},
	}
	for _, v := range componentStatuses {
		b := bytes.NewBuffer(nil)
		codec.Encode(&v, b)
		var status string
		if v.Conditions[0].Status == corev1.ConditionTrue {
			status = "Healthy"
		} else {
			status = "Unhealthy"
		}
		table.Rows = append(table.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Raw: b.Bytes()},
			Cells:  []interface{}{v.Name, status, v.Conditions[0].Message, v.Conditions[0].Error},
		})
	}
	return cmdtesting.ObjBody(codec, table)
}

// build an empty table response
func emptyTableObjBody(codec runtime.Codec) io.ReadCloser {
	table := &metav1.Table{
		ColumnDefinitions: podColumns,
	}
	return cmdtesting.ObjBody(codec, table)
}

func replicationControllersScaleSubresourceTableObjBody(codec runtime.Codec, replicationControllers ...corev1.ReplicationController) io.ReadCloser {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
			{Name: "Desired", Type: "integer", Description: autoscalingv1.ScaleSpec{}.SwaggerDoc()["replicas"]},
			{Name: "Available", Type: "integer", Description: autoscalingv1.ScaleStatus{}.SwaggerDoc()["replicas"]},
		},
	}

	for i := range replicationControllers {
		b := bytes.NewBuffer(nil)
		codec.Encode(&replicationControllers[i], b)
		table.Rows = append(table.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Raw: b.Bytes()},
			Cells:  []interface{}{replicationControllers[i].Name, replicationControllers[i].Spec.Replicas, replicationControllers[i].Status.Replicas},
		})
	}
	return cmdtesting.ObjBody(codec, table)
}
