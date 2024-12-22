/*
Copyright 2016 The Kubernetes Authors.

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

package top

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest/fake"
	core "k8s.io/client-go/testing"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	"k8s.io/kubectl/pkg/scheme"
	metricsv1beta1api "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	pointer "k8s.io/utils/ptr"
)

const (
	apiPrefix  = "api"
	apiVersion = "v1"
)

func TestTopNodeAllMetricsFrom(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	expectedMetrics, nodes := testNodeV1beta1MetricsData()
	expectedNodePath := fmt.Sprintf("/%s/%s/nodes", apiPrefix, apiVersion)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	ns := scheme.Codecs.WithoutConversion()

	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: ns,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apibody)))}, nil
			case p == "/apis":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apisbodyWithMetrics)))}, nil
			case p == expectedNodePath && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, nodes)}, nil
			default:
				t.Fatalf("unexpected request: %#v\nGot URL: %#v\n", req, req.URL)
				return nil, nil
			}
		}),
	}
	fakemetricsClientset := &metricsfake.Clientset{}
	fakemetricsClientset.AddReactor("list", "nodes", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		return true, expectedMetrics, nil
	})
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdTopNode(tf, nil, streams)

	// TODO in the long run, we want to test most of our commands like this. Wire the options struct with specific mocks
	// TODO then check the particular Run functionality and harvest results from fake clients
	cmdOptions := &TopNodeOptions{
		IOStreams: streams,
	}
	if err := cmdOptions.Complete(tf, cmd, []string{}); err != nil {
		t.Fatal(err)
	}
	cmdOptions.MetricsClient = fakemetricsClientset
	if err := cmdOptions.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cmdOptions.RunTopNode(); err != nil {
		t.Fatal(err)
	}

	// Check the presence of node names in the output.
	result := buf.String()
	for _, m := range expectedMetrics.Items {
		if !strings.Contains(result, m.Name) {
			t.Errorf("missing metrics for %s: \n%s", m.Name, result)
		}
	}
}

func TestTopNodeWithNameMetricsFrom(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	metrics, nodes := testNodeV1beta1MetricsData()
	expectedMetrics := metrics.Items[0]
	expectedNode := nodes.Items[0]
	nonExpectedMetrics := metricsv1beta1api.NodeMetricsList{
		ListMeta: metrics.ListMeta,
		Items:    metrics.Items[1:],
	}
	expectedNodePath := fmt.Sprintf("/%s/%s/nodes/%s", apiPrefix, apiVersion, expectedMetrics.Name)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	ns := scheme.Codecs.WithoutConversion()

	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: ns,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apibody)))}, nil
			case p == "/apis":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apisbodyWithMetrics)))}, nil
			case p == expectedNodePath && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &expectedNode)}, nil
			default:
				t.Fatalf("unexpected request: %#v\nGot URL: %#v\n", req, req.URL)
				return nil, nil
			}
		}),
	}
	fakemetricsClientset := &metricsfake.Clientset{}
	fakemetricsClientset.AddReactor("get", "nodes", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		return true, &expectedMetrics, nil
	})
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdTopNode(tf, nil, streams)

	// TODO in the long run, we want to test most of our commands like this. Wire the options struct with specific mocks
	// TODO then check the particular Run functionality and harvest results from fake clients
	cmdOptions := &TopNodeOptions{
		IOStreams: streams,
	}
	if err := cmdOptions.Complete(tf, cmd, []string{expectedMetrics.Name}); err != nil {
		t.Fatal(err)
	}
	cmdOptions.MetricsClient = fakemetricsClientset
	if err := cmdOptions.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cmdOptions.RunTopNode(); err != nil {
		t.Fatal(err)
	}

	// Check the presence of node names in the output.
	result := buf.String()
	if !strings.Contains(result, expectedMetrics.Name) {
		t.Errorf("missing metrics for %s: \n%s", expectedMetrics.Name, result)
	}
	for _, m := range nonExpectedMetrics.Items {
		if strings.Contains(result, m.Name) {
			t.Errorf("unexpected metrics for %s: \n%s", m.Name, result)
		}
	}
}

func TestTopNodeWithLabelSelectorMetricsFrom(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	metrics, nodes := testNodeV1beta1MetricsData()
	expectedMetrics := &metricsv1beta1api.NodeMetricsList{
		ListMeta: metrics.ListMeta,
		Items:    metrics.Items[0:1],
	}
	expectedNodes := v1.NodeList{
		ListMeta: nodes.ListMeta,
		Items:    nodes.Items[0:1],
	}
	nonExpectedMetrics := &metricsv1beta1api.NodeMetricsList{
		ListMeta: metrics.ListMeta,
		Items:    metrics.Items[1:],
	}
	label := "key=value"
	expectedNodePath := fmt.Sprintf("/%s/%s/nodes", apiPrefix, apiVersion)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	ns := scheme.Codecs.WithoutConversion()

	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: ns,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m, _ := req.URL.Path, req.Method, req.URL.RawQuery; {
			case p == "/api":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apibody)))}, nil
			case p == "/apis":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apisbodyWithMetrics)))}, nil
			case p == expectedNodePath && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &expectedNodes)}, nil
			default:
				t.Fatalf("unexpected request: %#v\nGot URL: %#v\n", req, req.URL)
				return nil, nil
			}
		}),
	}

	fakemetricsClientset := &metricsfake.Clientset{}
	fakemetricsClientset.AddReactor("list", "nodes", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		return true, expectedMetrics, nil
	})
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdTopNode(tf, nil, streams)
	cmd.Flags().Set("selector", label)

	// TODO in the long run, we want to test most of our commands like this. Wire the options struct with specific mocks
	// TODO then check the particular Run functionality and harvest results from fake clients
	cmdOptions := &TopNodeOptions{
		IOStreams: streams,
	}
	if err := cmdOptions.Complete(tf, cmd, []string{}); err != nil {
		t.Fatal(err)
	}
	cmdOptions.MetricsClient = fakemetricsClientset
	if err := cmdOptions.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cmdOptions.RunTopNode(); err != nil {
		t.Fatal(err)
	}

	// Check the presence of node names in the output.
	result := buf.String()
	for _, m := range expectedMetrics.Items {
		if !strings.Contains(result, m.Name) {
			t.Errorf("missing metrics for %s: \n%s", m.Name, result)
		}
	}
	for _, m := range nonExpectedMetrics.Items {
		if strings.Contains(result, m.Name) {
			t.Errorf("unexpected metrics for %s: \n%s", m.Name, result)
		}
	}
}

func TestTopNodeWithSortByCpuMetricsFrom(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	metrics, nodes := testNodeV1beta1MetricsData()
	expectedMetrics := &metricsv1beta1api.NodeMetricsList{
		ListMeta: metrics.ListMeta,
		Items:    metrics.Items[:],
	}
	expectedNodes := v1.NodeList{
		ListMeta: nodes.ListMeta,
		Items:    nodes.Items[:],
	}
	expectedNodePath := fmt.Sprintf("/%s/%s/nodes", apiPrefix, apiVersion)
	expectedNodesNames := []string{"node2", "node3", "node1"}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	ns := scheme.Codecs

	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: ns,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api":
				return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apibody)))}, nil
			case p == "/apis":
				return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apisbodyWithMetrics)))}, nil
			case p == expectedNodePath && m == "GET":
				return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &expectedNodes)}, nil
			default:
				t.Fatalf("unexpected request: %#v\nGot URL: %#v\n", req, req.URL)
				return nil, nil
			}
		}),
	}
	fakemetricsClientset := &metricsfake.Clientset{}
	fakemetricsClientset.AddReactor("list", "nodes", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		return true, expectedMetrics, nil
	})
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdTopNode(tf, nil, streams)
	cmd.Flags().Set("sort-by", "cpu")

	// TODO in the long run, we want to test most of our commands like this. Wire the options struct with specific mocks
	// TODO then check the particular Run functionality and harvest results from fake clients
	cmdOptions := &TopNodeOptions{
		IOStreams: streams,
		SortBy:    "cpu",
	}
	if err := cmdOptions.Complete(tf, cmd, []string{}); err != nil {
		t.Fatal(err)
	}
	cmdOptions.MetricsClient = fakemetricsClientset
	if err := cmdOptions.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cmdOptions.RunTopNode(); err != nil {
		t.Fatal(err)
	}

	// Check the presence of node names in the output.
	result := buf.String()

	for _, m := range expectedMetrics.Items {
		if !strings.Contains(result, m.Name) {
			t.Errorf("missing metrics for %s: \n%s", m.Name, result)
		}
	}

	resultLines := strings.Split(result, "\n")
	resultNodes := make([]string, len(resultLines)-2) // don't process first (header) and last (empty) line

	for i, line := range resultLines[1 : len(resultLines)-1] { // don't process first (header) and last (empty) line
		lineFirstColumn := strings.Split(line, " ")[0]
		resultNodes[i] = lineFirstColumn
	}

	if !reflect.DeepEqual(resultNodes, expectedNodesNames) {
		t.Errorf("kinds not matching:\n\texpectedKinds: %v\n\tgotKinds: %v\n", expectedNodesNames, resultNodes)
	}

}

func TestTopNodeWithSortByMemoryMetricsFrom(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	metrics, nodes := testNodeV1beta1MetricsData()
	expectedMetrics := &metricsv1beta1api.NodeMetricsList{
		ListMeta: metrics.ListMeta,
		Items:    metrics.Items[:],
	}
	expectedNodes := v1.NodeList{
		ListMeta: nodes.ListMeta,
		Items:    nodes.Items[:],
	}
	expectedNodePath := fmt.Sprintf("/%s/%s/nodes", apiPrefix, apiVersion)
	expectedNodesNames := []string{"node2", "node3", "node1"}

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	ns := scheme.Codecs

	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: ns,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api":
				return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apibody)))}, nil
			case p == "/apis":
				return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apisbodyWithMetrics)))}, nil
			case p == expectedNodePath && m == "GET":
				return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &expectedNodes)}, nil
			default:
				t.Fatalf("unexpected request: %#v\nGot URL: %#v\n", req, req.URL)
				return nil, nil
			}
		}),
	}
	fakemetricsClientset := &metricsfake.Clientset{}
	fakemetricsClientset.AddReactor("list", "nodes", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		return true, expectedMetrics, nil
	})
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdTopNode(tf, nil, streams)
	cmd.Flags().Set("sort-by", "memory")

	// TODO in the long run, we want to test most of our commands like this. Wire the options struct with specific mocks
	// TODO then check the particular Run functionality and harvest results from fake clients
	cmdOptions := &TopNodeOptions{
		IOStreams: streams,
		SortBy:    "memory",
	}
	if err := cmdOptions.Complete(tf, cmd, []string{}); err != nil {
		t.Fatal(err)
	}
	cmdOptions.MetricsClient = fakemetricsClientset
	if err := cmdOptions.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cmdOptions.RunTopNode(); err != nil {
		t.Fatal(err)
	}

	// Check the presence of node names in the output.
	result := buf.String()

	for _, m := range expectedMetrics.Items {
		if !strings.Contains(result, m.Name) {
			t.Errorf("missing metrics for %s: \n%s", m.Name, result)
		}
	}

	resultLines := strings.Split(result, "\n")
	resultNodes := make([]string, len(resultLines)-2) // don't process first (header) and last (empty) line

	for i, line := range resultLines[1 : len(resultLines)-1] { // don't process first (header) and last (empty) line
		lineFirstColumn := strings.Split(line, " ")[0]
		resultNodes[i] = lineFirstColumn
	}

	if !reflect.DeepEqual(resultNodes, expectedNodesNames) {
		t.Errorf("kinds not matching:\n\texpectedKinds: %v\n\tgotKinds: %v\n", expectedNodesNames, resultNodes)
	}

}

func TestTopNodeWithSwap(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	expectedMetrics, nodes := testNodeV1beta1MetricsData()
	expectedNodePath := fmt.Sprintf("/%s/%s/nodes", apiPrefix, apiVersion)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	ns := scheme.Codecs.WithoutConversion()

	getStubSummary := func(gigsOfSwapCapacity int64) []byte {
		stubSummary := Summary{
			Node: NodeStats{
				Swap: &SwapStats{
					SwapAvailableBytes: pointer.To(uint64(gigsOfSwapCapacity * (1024 * 1024))),
					SwapUsageBytes:     pointer.To(uint64(0)),
				},
			},
		}

		summaryBytes, err := json.Marshal(stubSummary)
		if err != nil {
			t.Fatal(err)
		}

		return summaryBytes
	}

	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: ns,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apibody)))}, nil
			case p == "/apis":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte(apisbodyWithMetrics)))}, nil
			case p == expectedNodePath && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, nodes)}, nil
			case strings.Contains(p, "proxy/stats/summary") && m == "GET":
				summaryBytes := getStubSummary(5)
				if strings.Contains(p, "node3") {
					summaryBytes = getStubSummary(0)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.BytesBody(summaryBytes)}, nil
			default:
				t.Fatalf("unexpected request: %#v\nGot URL: %#v\n", req, req.URL)
				return nil, nil
			}
		}),
	}
	fakemetricsClientset := &metricsfake.Clientset{}
	fakemetricsClientset.AddReactor("list", "nodes", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		return true, expectedMetrics, nil
	})
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdTopNode(tf, nil, streams)

	// TODO in the long run, we want to test most of our commands like this. Wire the options struct with specific mocks
	// TODO then check the particular Run functionality and harvest results from fake clients
	cmdOptions := &TopNodeOptions{
		IOStreams: streams,
		ShowSwap:  true,
	}
	if err := cmdOptions.Complete(tf, cmd, []string{}); err != nil {
		t.Fatal(err)
	}
	cmdOptions.MetricsClient = fakemetricsClientset
	if err := cmdOptions.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cmdOptions.RunTopNode(); err != nil {
		t.Fatal(err)
	}

	// Check the presence of node names in the output.
	result := buf.String()
	if !strings.Contains(result, "SWAP(bytes)") {
		t.Errorf("missing SWAP(bytes) header: \n%s", result)
	}
	if !strings.Contains(result, "SWAP(%)") {
		t.Errorf("missing SWAP(%%) header: \n%s", result)
	}
	if !strings.Contains(result, "SWAP CAPACITY(bytes)") {
		t.Errorf("missing SWAP(%%) header: \n%s", result)
	}

	for _, m := range expectedMetrics.Items {
		if !strings.Contains(result, m.Name) {
			t.Errorf("missing metrics for %s: \n%s", m.Name, result)
		}
		if _, foundSwapMetric := m.Usage["swap"]; !foundSwapMetric {
			t.Errorf("missing swap metric for %s: \n%s", m.Name, result)
		}
	}
}
