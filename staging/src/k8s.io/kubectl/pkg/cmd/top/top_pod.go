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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
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
	metricsv1beta1api "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

type TopPodOptions struct {
	ResourceName       string
	Namespace          string
	LabelSelector      string
	FieldSelector      string
	SortBy             string
	AllNamespaces      bool
	PrintContainers    bool
	NoHeaders          bool
	UseProtocolBuffers bool
	Sum                bool
	ShowSwap           bool

	PodClient       corev1client.PodsGetter
	Printer         *metricsutil.TopCmdPrinter
	DiscoveryClient discovery.DiscoveryInterface
	MetricsClient   metricsclientset.Interface

	genericiooptions.IOStreams
}

const metricsCreationDelay = 2 * time.Minute

var (
	topPodLong = templates.LongDesc(i18n.T(`
		Display resource (CPU/memory) usage of pods.

		The 'top pod' command allows you to see the resource consumption of pods.

		Due to the metrics pipeline delay, they may be unavailable for a few minutes
		since pod creation.`))

	topPodExample = templates.Examples(i18n.T(`
		# Show metrics for all pods in the default namespace
		kubectl top pod

		# Show metrics for all pods in the given namespace
		kubectl top pod --namespace=NAMESPACE

		# Show metrics for a given pod and its containers
		kubectl top pod POD_NAME --containers

		# Show metrics for the pods defined by label name=myLabel
		kubectl top pod -l name=myLabel`))
)

func NewCmdTopPod(f cmdutil.Factory, o *TopPodOptions, streams genericiooptions.IOStreams) *cobra.Command {
	if o == nil {
		o = &TopPodOptions{
			IOStreams:          streams,
			UseProtocolBuffers: true,
		}
	}

	cmd := &cobra.Command{
		Use:                   "pod [NAME | -l label]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Display resource (CPU/memory) usage of pods"),
		Long:                  topPodLong,
		Example:               topPodExample,
		ValidArgsFunction:     completion.ResourceNameCompletionFunc(f, "pod"),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunTopPod())
		},
		Aliases: []string{"pods", "po"},
	}
	cmdutil.AddLabelSelectorFlagVar(cmd, &o.LabelSelector)
	cmd.Flags().StringVar(&o.FieldSelector, "field-selector", o.FieldSelector, "Selector (field query) to filter on, supports '=', '==', and '!='.(e.g. --field-selector key1=value1,key2=value2). The server only supports a limited number of field queries per type.")
	cmd.Flags().StringVar(&o.SortBy, "sort-by", o.SortBy, "If non-empty, sort pods list using specified field. The field can be either 'cpu' or 'memory'.")
	cmd.Flags().BoolVar(&o.PrintContainers, "containers", o.PrintContainers, "If present, print usage of containers within a pod.")
	cmd.Flags().BoolVarP(&o.AllNamespaces, "all-namespaces", "A", o.AllNamespaces, "If present, list the requested object(s) across all namespaces. Namespace in current context is ignored even if specified with --namespace.")
	cmd.Flags().BoolVar(&o.NoHeaders, "no-headers", o.NoHeaders, "If present, print output without headers.")
	cmd.Flags().BoolVar(&o.UseProtocolBuffers, "use-protocol-buffers", o.UseProtocolBuffers, "Enables using protocol-buffers to access Metrics API.")
	cmd.Flags().BoolVar(&o.Sum, "sum", o.Sum, "Print the sum of the resource usage")
	cmd.Flags().BoolVar(&o.ShowSwap, "show-swap", o.ShowSwap, "Print pod resources related to swap memory.")
	return cmd
}

func (o *TopPodOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	if len(args) == 1 {
		o.ResourceName = args[0]
	} else if len(args) > 1 {
		return cmdutil.UsageErrorf(cmd, "%s", cmd.Use)
	}

	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
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

	o.PodClient = clientset.CoreV1()

	o.Printer = metricsutil.NewTopCmdPrinter(o.Out, o.ShowSwap)
	return nil
}

func (o *TopPodOptions) Validate() error {
	if len(o.SortBy) > 0 {
		if o.SortBy != sortByCPU && o.SortBy != sortByMemory {
			return errors.New("--sort-by accepts only cpu or memory")
		}
	}
	if len(o.ResourceName) > 0 && (len(o.LabelSelector) > 0 || len(o.FieldSelector) > 0) {
		return errors.New("only one of NAME or selector can be provided")
	}
	return nil
}

func (o TopPodOptions) RunTopPod() error {
	var err error
	labelSelector := labels.Everything()
	if len(o.LabelSelector) > 0 {
		labelSelector, err = labels.Parse(o.LabelSelector)
		if err != nil {
			return err
		}
	}
	fieldSelector := fields.Everything()
	if len(o.FieldSelector) > 0 {
		fieldSelector, err = fields.ParseSelector(o.FieldSelector)
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
	metrics, err := getMetricsFromMetricsAPI(o.MetricsClient, o.Namespace, o.ResourceName, o.AllNamespaces, labelSelector, fieldSelector)
	if err != nil {
		return err
	}

	// First we check why no metrics have been received.
	if len(metrics.Items) == 0 {
		// If the API server query is successful but all the pods are newly created,
		// the metrics are probably not ready yet, so we return the error here in the first place.
		err := verifyEmptyMetrics(o, labelSelector, fieldSelector)
		if err != nil {
			return err
		}

		// if we had no errors, be sure we output something.
		if o.AllNamespaces {
			fmt.Fprintln(o.ErrOut, "No resources found")
		} else {
			fmt.Fprintf(o.ErrOut, "No resources found in %s namespace.\n", o.Namespace)
		}
	}

	return o.Printer.PrintPodMetrics(metrics.Items, o.PrintContainers, o.AllNamespaces, o.NoHeaders, o.SortBy, o.Sum)
}

func getMetricsFromMetricsAPI(metricsClient metricsclientset.Interface, namespace, resourceName string, allNamespaces bool, labelSelector labels.Selector, fieldSelector fields.Selector) (*metricsapi.PodMetricsList, error) {
	var err error
	ns := metav1.NamespaceAll
	if !allNamespaces {
		ns = namespace
	}
	versionedMetrics := &metricsv1beta1api.PodMetricsList{}
	if resourceName != "" {
		m, err := metricsClient.MetricsV1beta1().PodMetricses(ns).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		versionedMetrics.Items = []metricsv1beta1api.PodMetrics{*m}
	} else {
		versionedMetrics, err = metricsClient.MetricsV1beta1().PodMetricses(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String(), FieldSelector: fieldSelector.String()})
		if err != nil {
			return nil, err
		}
	}
	metrics := &metricsapi.PodMetricsList{}
	err = metricsv1beta1api.Convert_v1beta1_PodMetricsList_To_metrics_PodMetricsList(versionedMetrics, metrics, nil)
	if err != nil {
		return nil, err
	}
	return metrics, nil
}

func verifyEmptyMetrics(o TopPodOptions, labelSelector labels.Selector, fieldSelector fields.Selector) error {
	if len(o.ResourceName) > 0 {
		pod, err := o.PodClient.Pods(o.Namespace).Get(context.TODO(), o.ResourceName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := checkPodAge(pod); err != nil {
			return err
		}
	} else {
		pods, err := o.PodClient.Pods(o.Namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: labelSelector.String(),
			FieldSelector: fieldSelector.String(),
		})
		if err != nil {
			return err
		}
		if len(pods.Items) == 0 {
			return nil
		}
		for _, pod := range pods.Items {
			if err := checkPodAge(&pod); err != nil {
				return err
			}
		}
	}
	return errors.New("metrics not available yet")
}

func checkPodAge(pod *corev1.Pod) error {
	age := time.Since(pod.CreationTimestamp.Time)
	if age > metricsCreationDelay {
		message := fmt.Sprintf("Metrics not available for pod %s/%s, age: %s", pod.Namespace, pod.Name, age.String())
		return errors.New(message)
	} else {
		klog.V(2).Infof("Metrics not yet available for pod %s/%s, age: %s", pod.Namespace, pod.Name, age.String())
		return nil
	}
}
