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

package set

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"strings"
	"testing"

	"fmt"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest/fake"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/testapi"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/extensions"
	cmdtesting "k8s.io/kubernetes/pkg/kubectl/cmd/testing"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/printers"
)

const serviceAccount = "serviceaccount1"

func TestServiceAccountLocal(t *testing.T) {
	inputs := []struct {
		yaml     string
		apiGroup string
	}{
		{yaml: "../../../../test/fixtures/doc-yaml/user-guide/replication.yaml", apiGroup: api.GroupName},
		{yaml: "../../../../test/fixtures/doc-yaml/admin/daemon.yaml", apiGroup: extensions.GroupName},
		{yaml: "../../../../test/fixtures/doc-yaml/user-guide/replicaset/redis-slave.yaml", apiGroup: extensions.GroupName},
		{yaml: "../../../../test/fixtures/doc-yaml/user-guide/job.yaml", apiGroup: batch.GroupName},
		{yaml: "../../../../test/fixtures/doc-yaml/user-guide/deployment.yaml", apiGroup: extensions.GroupName},
		{yaml: "../../../../examples/storage/cassandra/cassandra-statefulset.yaml", apiGroup: apps.GroupName},
	}

	f, tf, _, _ := cmdtesting.NewAPIFactory()
	tf.Client = &fake.RESTClient{
		APIRegistry: api.Registry,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s %#v\n%#v", req.Method, req.URL, req)
			return nil, nil
		}),
	}
	tf.Namespace = "test"
	out := bytes.NewBuffer([]byte{})
	cmd := NewCmdServiceAccount(f, out, out)
	cmd.SetOutput(out)
	cmd.Flags().Set("output", "yaml")
	cmd.Flags().Set("local", "true")
	for _, input := range inputs {
		testapi.Default = testapi.Groups[input.apiGroup]
		tf.Printer = printers.NewVersionedPrinter(&printers.YAMLPrinter{}, testapi.Default.Converter(), *testapi.Default.GroupVersion())
		saConfig := ServiceAccountConfig{fileNameOptions: resource.FilenameOptions{
			Filenames: []string{input.yaml}},
			out:   out,
			local: true}
		err := saConfig.Complete(f, cmd, []string{serviceAccount})
		if err == nil {
			err = saConfig.Validate()
		}
		if err == nil {
			err = saConfig.Run()
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assert.True(t, strings.Contains(out.String(), "serviceAccountName: "+serviceAccount))
	}
}

func TestServiceAccountRemote(t *testing.T) {
	inputs := []struct {
		object              runtime.Object
		apiPrefix, apiGroup string
		args                []string
	}{
		{
			object: &extensions.ReplicaSet{
				TypeMeta:   metav1.TypeMeta{Kind: "ReplicaSet", APIVersion: api.Registry.GroupOrDie(extensions.GroupName).GroupVersion.String()},
				ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
			},
			apiPrefix: "/apis", apiGroup: extensions.GroupName,
			args: []string{"replicaset", "nginx", serviceAccount},
		},
		{
			object: &extensions.DaemonSet{
				TypeMeta:   metav1.TypeMeta{Kind: "DaemonSet", APIVersion: api.Registry.GroupOrDie(extensions.GroupName).GroupVersion.String()},
				ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
			},
			apiPrefix: "/apis", apiGroup: extensions.GroupName,
			args: []string{"daemonset", "nginx", serviceAccount},
		},
		{
			object: &api.ReplicationController{
				TypeMeta:   metav1.TypeMeta{Kind: "ReplicationController", APIVersion: api.Registry.GroupOrDie(api.GroupName).GroupVersion.String()},
				ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
			},
			apiPrefix: "/api", apiGroup: api.GroupName,
			args: []string{"replicationcontroller", "nginx", serviceAccount}},
		{
			object: &extensions.Deployment{
				TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: api.Registry.GroupOrDie(extensions.GroupName).GroupVersion.String()},
				ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
			},
			apiPrefix: "/apis", apiGroup: extensions.GroupName,
			args: []string{"deployment", "nginx", serviceAccount},
		},
		{
			object: &batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job", APIVersion: api.Registry.GroupOrDie(batch.GroupName).GroupVersion.String()},
				ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
			},
			apiPrefix: "/apis", apiGroup: batch.GroupName,
			args: []string{"job", "nginx", serviceAccount},
		},
		{
			object: &apps.StatefulSet{
				TypeMeta:   metav1.TypeMeta{Kind: "StatefulSet", APIVersion: api.Registry.GroupOrDie(apps.GroupName).GroupVersion.String()},
				ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
			},
			apiPrefix: "/apis", apiGroup: apps.GroupName,
			args: []string{"statefulset", "nginx", serviceAccount},
		},
	}
	for _, input := range inputs {

		groupVersion := api.Registry.GroupOrDie(input.apiGroup).GroupVersion
		//input.object.GetObjectKind().SetGroupVersionKind(groupVersion.WithKind(input.args[0]))
		testapi.Default = testapi.Groups[input.apiGroup]
		f, tf, codec, _ := cmdtesting.NewAPIFactory()
		tf.Printer = printers.NewVersionedPrinter(&printers.YAMLPrinter{}, testapi.Default.Converter(), *testapi.Default.GroupVersion())
		tf.Namespace = "test"
		tf.Client = &fake.RESTClient{
			APIRegistry:          api.Registry,
			NegotiatedSerializer: testapi.Default.NegotiatedSerializer(),
			Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
				resourcePath := testapi.Default.ResourcePath(input.args[0]+"s", tf.Namespace, input.args[1])
				switch p, m := req.URL.Path, req.Method; {
				case p == resourcePath && m == http.MethodGet:
					return &http.Response{StatusCode: http.StatusOK, Header: defaultHeader(), Body: objBody(codec, input.object)}, nil
				case p == resourcePath && m == http.MethodPatch:
					stream, err := req.GetBody()
					if err != nil {
						return nil, err
					}
					bytes, err := ioutil.ReadAll(stream)
					if err != nil {
						return nil, err
					}
					assert.True(t, strings.Contains(string(bytes), `"serviceAccountName":`+`"`+serviceAccount+`"`))
					return &http.Response{StatusCode: http.StatusOK, Header: defaultHeader(), Body: objBody(codec, input.object)}, nil
				default:
					t.Errorf("%s: unexpected request: %s %#v\n%#v", "serviceaccount", req.Method, req.URL, req)
					return nil, fmt.Errorf("unexpected request")
				}
			}),
			VersionedAPIPath: path.Join(input.apiPrefix, groupVersion.String()),
			GroupName:        input.apiGroup,
		}
		out := bytes.NewBuffer([]byte{})
		cmd := NewCmdServiceAccount(f, out, out)
		cmd.SetOutput(out)
		cmd.Flags().Set("output", "yaml")

		saConfig := ServiceAccountConfig{
			out:   out,
			local: false}
		err := saConfig.Complete(f, cmd, input.args)
		if err == nil {
			saConfig.categoryExpander = resource.LegacyCategoryExpander
			err = saConfig.Validate()
		}
		if err == nil {
			err = saConfig.Run()
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

}

func objBody(codec runtime.Codec, obj runtime.Object) io.ReadCloser {
	return ioutil.NopCloser(bytes.NewReader([]byte(runtime.EncodeOrDie(codec, obj))))
}

func defaultHeader() http.Header {
	header := http.Header{}
	header.Set("Content-Type", runtime.ContentTypeJSON)
	return header
}

func bytesBody(bodyBytes []byte) io.ReadCloser {
	return ioutil.NopCloser(bytes.NewReader(bodyBytes))
}
