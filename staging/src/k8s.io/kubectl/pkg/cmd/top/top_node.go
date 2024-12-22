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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/discovery"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/metricsutil"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
	metricsV1beta1api "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

// TopNodeOptions contains all the options for running the top-node cli command.
type TopNodeOptions struct {
	ResourceName       string
	Selector           string
	SortBy             string
	NoHeaders          bool
	UseProtocolBuffers bool
	ShowCapacity       bool
	ShowSwap           bool

	NodeClient      corev1client.CoreV1Interface
	Printer         *metricsutil.TopCmdPrinter
	DiscoveryClient discovery.DiscoveryInterface
	MetricsClient   metricsclientset.Interface

	genericiooptions.IOStreams
}

var (
	topNodeLong = templates.LongDesc(i18n.T(`
		Display resource (CPU/memory) usage of nodes.

		The top-node command allows you to see the resource consumption of nodes.`))

	topNodeExample = templates.Examples(i18n.T(`
		  # Show metrics for all nodes
		  kubectl top node

		  # Show metrics for a given node
		  kubectl top node NODE_NAME`))
)

func NewCmdTopNode(f cmdutil.Factory, o *TopNodeOptions, streams genericiooptions.IOStreams) *cobra.Command {
	if o == nil {
		o = &TopNodeOptions{
			IOStreams:          streams,
			UseProtocolBuffers: true,
		}
	}

	cmd := &cobra.Command{
		Use:                   "node [NAME | -l label]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Display resource (CPU/memory) usage of nodes"),
		Long:                  topNodeLong,
		Example:               topNodeExample,
		ValidArgsFunction:     completion.ResourceNameCompletionFunc(f, "node"),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunTopNode())
		},
		Aliases: []string{"nodes", "no"},
	}
	cmdutil.AddLabelSelectorFlagVar(cmd, &o.Selector)
	cmd.Flags().StringVar(&o.SortBy, "sort-by", o.SortBy, "If non-empty, sort nodes list using specified field. The field can be either 'cpu' or 'memory'.")
	cmd.Flags().BoolVar(&o.NoHeaders, "no-headers", o.NoHeaders, "If present, print output without headers")
	cmd.Flags().BoolVar(&o.UseProtocolBuffers, "use-protocol-buffers", o.UseProtocolBuffers, "Enables using protocol-buffers to access Metrics API.")
	cmd.Flags().BoolVar(&o.ShowCapacity, "show-capacity", o.ShowCapacity, "Print node resources based on Capacity instead of Allocatable(default) of the nodes.")
	cmd.Flags().BoolVar(&o.ShowSwap, "show-swap", o.ShowSwap, "Print node resources related to swap memory.")

	return cmd
}

func (o *TopNodeOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		o.ResourceName = args[0]
	} else if len(args) > 1 {
		return cmdutil.UsageErrorf(cmd, "%s", cmd.Use)
	}

	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}

	o.DiscoveryClient = clientset.DiscoveryClient

	config, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	if o.UseProtocolBuffers {
		config.ContentType = "application/vnd.kubernetes.protobuf"
	}
	o.MetricsClient, err = metricsclientset.NewForConfig(config)
	if err != nil {
		return err
	}

	o.NodeClient = clientset.CoreV1()

	o.Printer = metricsutil.NewTopCmdPrinter(o.Out, o.ShowSwap)
	return nil
}

func (o *TopNodeOptions) Validate() error {
	if len(o.SortBy) > 0 {
		if o.SortBy != sortByCPU && o.SortBy != sortByMemory {
			return errors.New("--sort-by accepts only cpu or memory")
		}
	}
	if len(o.ResourceName) > 0 && len(o.Selector) > 0 {
		return errors.New("only one of NAME or --selector can be provided")
	}
	return nil
}

func (o TopNodeOptions) RunTopNode() error {
	var err error
	selector := labels.Everything()
	if len(o.Selector) > 0 {
		selector, err = labels.Parse(o.Selector)
		if err != nil {
			return err
		}
	}

	apiGroups, err := o.DiscoveryClient.ServerGroups()
	if err != nil {
		return err
	}

	metricsAPIAvailable := SupportedMetricsAPIVersionAvailable(apiGroups)

	if !metricsAPIAvailable {
		return errors.New("Metrics API not available")
	}

	metrics, err := getNodeMetricsFromMetricsAPI(o.MetricsClient, o.ResourceName, selector)
	if err != nil {
		return err
	}

	if len(metrics.Items) == 0 {
		return errors.New("metrics not available yet")
	}

	var nodes []v1.Node
	if len(o.ResourceName) > 0 {
		node, err := o.NodeClient.Nodes().Get(context.TODO(), o.ResourceName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		nodes = append(nodes, *node)
	} else {
		nodeList, err := o.NodeClient.Nodes().List(context.TODO(), metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			return err
		}
		nodes = append(nodes, nodeList.Items...)
	}

	availableResources := make(map[string]v1.ResourceList)

	for _, n := range nodes {
		if !o.ShowCapacity {
			availableResources[n.Name] = n.Status.Allocatable
		} else {
			availableResources[n.Name] = n.Status.Capacity
		}

		if o.ShowSwap {
			availableSwap, err := getNodeSwapCapacity(o.NodeClient, n.Name)
			if err != nil {
				return fmt.Errorf("failed to get swap memory capacity for node %s: %w", n.Name, err)
			}
			availableResources[n.Name]["swap"] = *availableSwap
		}
	}

	return o.Printer.PrintNodeMetrics(metrics.Items, availableResources, o.NoHeaders, o.SortBy)
}

func getNodeMetricsFromMetricsAPI(metricsClient metricsclientset.Interface, resourceName string, selector labels.Selector) (*metricsapi.NodeMetricsList, error) {
	var err error
	versionedMetrics := &metricsV1beta1api.NodeMetricsList{}
	mc := metricsClient.MetricsV1beta1()
	nm := mc.NodeMetricses()
	if resourceName != "" {
		m, err := nm.Get(context.TODO(), resourceName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		versionedMetrics.Items = []metricsV1beta1api.NodeMetrics{*m}
	} else {
		versionedMetrics, err = nm.List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, err
		}
	}
	metrics := &metricsapi.NodeMetricsList{}
	err = metricsV1beta1api.Convert_v1beta1_NodeMetricsList_To_metrics_NodeMetricsList(versionedMetrics, metrics, nil)
	if err != nil {
		return nil, err
	}
	return metrics, nil
}

func getNodeSwapCapacity(nodeClient corev1client.CoreV1Interface, nodeName string) (*resource.Quantity, error) {
	summaryBytes, err := nodeClient.RESTClient().Get().
		AbsPath("/api/v1/nodes", nodeName, "proxy/stats/summary").
		DoRaw(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("failed to get node summary: %w", err)
	}

	summary := Summary{}
	err = json.Unmarshal(summaryBytes, &summary)
	if err != nil {
		return nil, err
	}

	if summary.Node.Swap == nil || summary.Node.Swap.SwapAvailableBytes == nil || summary.Node.Swap.SwapUsageBytes == nil {
		return nil, fmt.Errorf("failed to get swap memory information for node %s", nodeName)
	}

	availableSwap := *summary.Node.Swap.SwapAvailableBytes + *summary.Node.Swap.SwapUsageBytes
	return resource.NewQuantity(int64(availableSwap), resource.DecimalSI), nil
}

type Summary struct {
	Node NodeStats `json:"node"`
}

type NodeStats struct {
	Swap *SwapStats `json:"swap,omitempty"`
}

type SwapStats struct {
	SwapAvailableBytes *uint64 `json:"swapAvailableBytes,omitempty"`
	SwapUsageBytes     *uint64 `json:"swapUsageBytes,omitempty"`
}
