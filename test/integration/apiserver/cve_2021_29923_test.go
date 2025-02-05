/*
Copyright 2022 The Kubernetes Authors.

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

package apiserver

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

// TestCanaryCVE_2021_29923 tests to make sure that objects that use the golang IP parsers allow IPv4 addresses with leading zeros.
// Is it possible that exist more fields that can contain IPs, the test consider the most significant.
// xref: https://issues.k8s.io/100895
func TestCanaryCVE_2021_29923(t *testing.T) {
	// Disable ServiceAccount admission plugin as we don't have serviceaccount controller running.
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	client, err := clientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}

	ns := framework.CreateNamespaceOrDie(client, "test-cve-2021-29923", t)
	defer framework.DeleteNamespaceOrDie(client, ns, t)

	dynamicClient, err := dynamic.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("unexpected error creating dynamic client: %v", err)
	}

	objects := map[schema.GroupVersionResource]string{
		// k8s.io/kubernetes/pkg/api/v1
		gvr("", "v1", "nodes"):     `{"kind": "Node", "apiVersion": "v1", "metadata": {"name": "node1"}, "spec": {"unschedulable": true}, "status": {"addresses":[{"address":"172.18.0.012","type":"InternalIP"}]}}`,
		gvr("", "v1", "pods"):      `{"kind": "Pod", "apiVersion": "v1", "metadata": {"name": "pod1", "namespace": "test-cve-2021-29923"}, "spec": {"containers": [{"image": "` + "image" + `", "name": "container7", "resources": {"limits": {"cpu": "1M"}, "requests": {"cpu": "1M"}}}]}, "status": {"podIP":"10.244.0.05","podIPs":[{"ip":"10.244.0.05"}]}}`,
		gvr("", "v1", "services"):  `{"kind": "Service", "apiVersion": "v1", "metadata": {"name": "service1", "namespace": "test-cve-2021-29923"}, "spec": {"clusterIP": "10.0.0.011", "externalIP": "192.168.0.012", "externalName": "service1name", "ports": [{"port": 10000, "targetPort": 11000}], "selector": {"test": "data"}}}`,
		gvr("", "v1", "endpoints"): `{"kind": "Endpoints", "apiVersion": "v1", "metadata": {"name": "ep1name", "namespace": "test-cve-2021-29923"}, "subsets": [{"addresses": [{"hostname": "bar-001", "ip": "192.168.3.011"}], "ports": [{"port": 8000}]}]}`,
		// k8s.io/kubernetes/pkg/apis/discovery/v1
		gvr("discovery.k8s.io", "v1", "endpointslices"): `{"kind": "EndpointSlice", "apiVersion": "discovery.k8s.io/v1", "metadata": {"name": "slicev1", "namespace": "test-cve-2021-29923"}, "addressType": "IPv4", "protocol": "TCP", "ports": [], "endpoints": [{"addresses": ["10.244.0.011"], "conditions": {"ready": true, "serving": true, "terminating": false}, "nodeName": "control-plane"}]}`,
		// k8s.io/kubernetes/pkg/apis/networking/v1
		gvr("networking.k8s.io", "v1", "ingresses"):       `{"kind": "Ingress", "apiVersion": "networking.k8s.io/v1", "metadata": {"name": "ingress3", "namespace": "test-cve-2021-29923"}, "spec": {"defaultBackend": {"service":{"name":"service", "port":{"number": 5000}}}}, "status":{"loadBalancer":{"ingress": [{"ip":"10.0.0.013"}]}}}`,
		gvr("networking.k8s.io", "v1", "networkpolicies"): `{"kind": "NetworkPolicy", "apiVersion": "networking.k8s.io/v1", "metadata": {"name": "np2", "namespace": "test-cve-2021-29923"}, "spec": {"egress":[{"ports":[{"port":5978,"protocol":"TCP"}],"to":[{"ipBlock":{"cidr":"10.0.012.0/24"}}]}],"ingress":[{"from":[{"ipBlock":{"cidr":"172.017.0.0/16","except":["172.17.001.0/24"]}},{"podSelector":{"matchLabels":{"role":"frontend"}}}],"ports":[{"port":6379,"protocol":"TCP"}]}],"podSelector":{"matchLabels":{"role":"db"}},"policyTypes":["Ingress","Egress"]}}`,
	}

	for gvr, data := range objects {
		t.Run(gvr.String(), func(t *testing.T) {
			obj := map[string]interface{}{}
			if err := json.Unmarshal([]byte(data), &obj); err != nil {
				t.Fatal(err)
			}

			cr := &unstructured.Unstructured{Object: obj}

			_, err := dynamicClient.Resource(gvr).Namespace(cr.GetNamespace()).Create(context.TODO(), cr, metav1.CreateOptions{})
			if err != nil {
				t.Errorf("error creating resource %s with IPs with leading zeros %v", gvr.String(), err)
			}

		})
	}

}
