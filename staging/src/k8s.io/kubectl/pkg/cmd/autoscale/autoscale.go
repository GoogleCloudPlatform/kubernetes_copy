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

package autoscale

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/spf13/cobra"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	autoscalingv1client "k8s.io/client-go/kubernetes/typed/autoscaling/v1"
	autoscalingv2client "k8s.io/client-go/kubernetes/typed/autoscaling/v2"
	"k8s.io/client-go/scale"
	"k8s.io/klog/v2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	autoscaleLong = templates.LongDesc(i18n.T(`
		Creates an autoscaler that automatically chooses and sets the number of pods that run in a Kubernetes cluster.
		The command will attempt to use the autoscaling/v2 API first, in case of an error, it will fall back to autoscaling/v1 API.

		Looks up a deployment, replica set, stateful set, or replication controller by name and creates an autoscaler that uses the given resource as a reference.
		An autoscaler can automatically increase or decrease number of pods deployed within the system as needed.`))

	autoscaleExample = templates.Examples(i18n.T(`
		# Auto scale a deployment "foo", with the number of pods between 2 and 10, no target CPU utilization specified so a default autoscaling policy will be used
		kubectl autoscale deployment foo --min=2 --max=10

		# Auto scale a replication controller "foo", with the number of pods between 1 and 5, target CPU utilization at 80%
		kubectl autoscale rc foo --max=5 --cpu-percent=80`))
)

// AutoscaleOptions declares the arguments accepted by the Autoscale command
type AutoscaleOptions struct {
	FilenameOptions *resource.FilenameOptions

	RecordFlags *genericclioptions.RecordFlags
	Recorder    genericclioptions.Recorder

	PrintFlags *genericclioptions.PrintFlags
	ToPrinter  func(string) (printers.ResourcePrinter, error)

	Name       string
	Min        int32
	Max        int32
	CPUPercent int32
	CPU        string
	Memory     string

	createAnnotation bool
	args             []string
	enforceNamespace bool
	namespace        string
	dryRunStrategy   cmdutil.DryRunStrategy
	builder          *resource.Builder
	fieldManager     string

	HPAClientV1       autoscalingv1client.HorizontalPodAutoscalersGetter
	HPAClientV2       autoscalingv2client.HorizontalPodAutoscalersGetter
	scaleKindResolver scale.ScaleKindResolver

	genericiooptions.IOStreams
}

// NewAutoscaleOptions creates the options for autoscale
func NewAutoscaleOptions(ioStreams genericiooptions.IOStreams) *AutoscaleOptions {
	return &AutoscaleOptions{
		PrintFlags:      genericclioptions.NewPrintFlags("autoscaled").WithTypeSetter(scheme.Scheme),
		FilenameOptions: &resource.FilenameOptions{},
		RecordFlags:     genericclioptions.NewRecordFlags(),
		Recorder:        genericclioptions.NoopRecorder{},

		IOStreams: ioStreams,
	}
}

// NewCmdAutoscale returns the autoscale Cobra command
func NewCmdAutoscale(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewAutoscaleOptions(ioStreams)

	validArgs := []string{"deployment", "replicaset", "replicationcontroller", "statefulset"}

	cmd := &cobra.Command{
		Use:                   "autoscale (-f FILENAME | TYPE NAME | TYPE/NAME) [--min=MINPODS] --max=MAXPODS [--cpu-percent=CPU]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Auto-scale a deployment, replica set, stateful set, or replication controller"),
		Long:                  autoscaleLong,
		Example:               autoscaleExample,
		ValidArgsFunction:     completion.SpecifiedResourceTypeAndNameCompletionFunc(f, validArgs),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}

	// bind flag structs
	o.RecordFlags.AddFlags(cmd)
	o.PrintFlags.AddFlags(cmd)
	cmd.Flags().Int32Var(&o.Min, "min", -1, "The lower limit for the number of pods that can be set by the autoscaler. If it's not specified or negative, the server will apply a default value.")
	cmd.Flags().Int32Var(&o.Max, "max", -1, "The upper limit for the number of pods that can be set by the autoscaler. Required.")
	cmd.MarkFlagRequired("max")
	cmd.Flags().Int32Var(&o.CPUPercent, "cpu-percent", -1, "The target average CPU utilization (represented as a percent of requested CPU) over all the pods. If it's not specified or negative, a default autoscaling policy will be used.")
	cmd.Flags().StringVar(&o.CPU, "cpu", "", `Target CPU. Specify as a percentage (e.g."70%" for 70% of requested CPU), an absolute value with units (e.g."500m" for 500 milliCPU), or an absolute value without units which defaults to milliCPU (e.g."500" is interpreted as "500m").`)
	cmd.Flags().StringVar(&o.Memory, "memory", "", `Target memory. Specify as a percentage (e.g."60%" for 60% of requested memory), an absolute value in mebibytes (e.g."200Mi" for 200 MiB), or gibibytes (e.g."1Gi" for 1 GiB). If an absolute value is specified without a unit, it defaults to mebibytes (e.g."200" is interpreted as "200Mi").`)
	cmd.Flags().StringVar(&o.Name, "name", "", i18n.T("The name for the newly created object. If not specified, the name of the input resource will be used."))
	cmdutil.AddDryRunFlag(cmd)
	cmdutil.AddFilenameOptionFlags(cmd, o.FilenameOptions, "identifying the resource to autoscale.")
	cmdutil.AddApplyAnnotationFlags(cmd)
	cmdutil.AddFieldManagerFlagVar(cmd, &o.fieldManager, "kubectl-autoscale")
	return cmd
}

// Complete verifies command line arguments and loads data from the command environment
func (o *AutoscaleOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	o.dryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}
	discoveryClient, err := f.ToDiscoveryClient()
	if err != nil {
		return err
	}
	o.createAnnotation = cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)
	o.builder = f.NewBuilder()
	o.scaleKindResolver = scale.NewDiscoveryScaleKindResolver(discoveryClient)
	o.args = args
	o.RecordFlags.Complete(cmd)

	o.Recorder, err = o.RecordFlags.ToRecorder()
	if err != nil {
		return err
	}

	kubeClient, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	o.HPAClientV2 = kubeClient.AutoscalingV2()
	o.HPAClientV1 = kubeClient.AutoscalingV1()

	o.namespace, o.enforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.ToPrinter = func(operation string) (printers.ResourcePrinter, error) {
		o.PrintFlags.NamePrintFlags.Operation = operation
		cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.dryRunStrategy)

		return o.PrintFlags.ToPrinter()
	}

	return nil
}

// Validate checks that the provided attach options are specified.
func (o *AutoscaleOptions) Validate() error {
	if o.Max < 1 {
		return fmt.Errorf("--max=MAXPODS is required and must be at least 1, max: %d", o.Max)
	}
	if o.Max < o.Min {
		return fmt.Errorf("--max=MAXPODS must be larger or equal to --min=MINPODS, max: %d, min: %d", o.Max, o.Min)
	}
	// only one of the CPUPercent or CPU param is allowed
	if o.CPUPercent != 0 && o.CPU != "" {
		return fmt.Errorf("--cpu-percent and --cpu are mutually exclusive, CPUPercent: %v, CPU: %v", o.CPUPercent, o.CPU)
	}
	return nil
}

func (o *AutoscaleOptions) Run() error {
	r := o.builder.
		Unstructured().
		ContinueOnError().
		NamespaceParam(o.namespace).DefaultNamespace().
		FilenameParam(o.enforceNamespace, o.FilenameOptions).
		ResourceTypeOrNameArgs(false, o.args...).
		Flatten().
		Do()
	if err := r.Err(); err != nil {
		return err
	}

	count := 0
	err := r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		mapping := info.ResourceMapping()
		gvr := mapping.GroupVersionKind.GroupVersion().WithResource(mapping.Resource.Resource)
		if _, err := o.scaleKindResolver.ScaleForResource(gvr); err != nil {
			return fmt.Errorf("cannot autoscale a %v: %v", mapping.GroupVersionKind.Kind, err)
		}

		// handles the creation of HorizontalPodAutoscaler objects for both v2 and v1 APIs.
		// If v2 API fails, try to create and handle HorizontalPodAutoscaler using v1 API
		hpaV2, err := o.createHorizontalPodAutoscalerV2(info.Name, mapping)
		if err != nil {
			return fmt.Errorf("failed to create v2 HorizontalPodAutoscaler: %w", err)
		}
		if err := o.handleHPA(hpaV2); err != nil {
			klog.V(1).Infof("Encountered an error with the v2 HorizontalPodAutoscaler: %v. "+
				"Falling back to try the v1 HorizontalPodAutoscaler", err)
			// check if the HPA can be created using v1 API.
			if ok, err := o.canCreateHPAV1(); !ok {
				klog.V(1).Infof("The current configuration cannot be represented using v1 HorizontalPodAutoscaler.")
				return fmt.Errorf("failed to create v2 HPA and the configuration is incompatible with v1: %w", err)
			}
			hpaV1 := o.createHorizontalPodAutoscalerV1(info.Name, mapping)
			if err := o.handleHPA(hpaV1); err != nil {
				return err
			}
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no objects passed to autoscale")
	}
	return nil
}

func (o *AutoscaleOptions) canCreateHPAV1() (bool, error) {
	// Allow fallback to v1 HPA only if:
	// 1. CPUPercent is set and Memory is not set.
	// 2. Or, Memory is not set and the metric type is UtilizationMetricType.
	var isUtilizationMetricType bool
	_, _, metricsType, err := parseResourceInput(o.CPU, corev1.ResourceCPU)
	if err != nil {
		return false, err
	}
	if metricsType == autoscalingv2.UtilizationMetricType {
		isUtilizationMetricType = true
	}
	return (o.CPUPercent >= 0 && o.Memory == "") || (o.Memory == "" && isUtilizationMetricType), nil
}

// handleHPA handles the creation and management of a single HPA object.
func (o *AutoscaleOptions) handleHPA(hpa runtime.Object) error {
	if err := o.Recorder.Record(hpa); err != nil {
		return fmt.Errorf("error recording current command: %w", err)
	}

	if o.dryRunStrategy == cmdutil.DryRunClient {
		printer, err := o.ToPrinter("created")
		if err != nil {
			return err
		}
		return printer.PrintObj(hpa, o.Out)
	}

	if err := util.CreateOrUpdateAnnotation(o.createAnnotation, hpa, scheme.DefaultJSONEncoder()); err != nil {
		return err
	}

	createOptions := metav1.CreateOptions{}
	if o.fieldManager != "" {
		createOptions.FieldManager = o.fieldManager
	}
	if o.dryRunStrategy == cmdutil.DryRunServer {
		createOptions.DryRun = []string{metav1.DryRunAll}
	}

	var actualHPA runtime.Object
	var err error
	switch typedHPA := hpa.(type) {
	case *autoscalingv2.HorizontalPodAutoscaler:
		actualHPA, err = o.HPAClientV2.HorizontalPodAutoscalers(o.namespace).Create(context.TODO(), typedHPA, createOptions)
	case *autoscalingv1.HorizontalPodAutoscaler:
		actualHPA, err = o.HPAClientV1.HorizontalPodAutoscalers(o.namespace).Create(context.TODO(), typedHPA, createOptions)
	default:
		return fmt.Errorf("unsupported HorizontalPodAutoscaler type %T", hpa)
	}
	if err != nil {
		return err
	}

	printer, err := o.ToPrinter("autoscaled")
	if err != nil {
		return err
	}
	return printer.PrintObj(actualHPA, o.Out)
}

func (o *AutoscaleOptions) createHorizontalPodAutoscalerV2(refName string, mapping *meta.RESTMapping) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	name := o.Name
	if len(name) == 0 {
		name = refName
	}

	scaler := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: mapping.GroupVersionKind.GroupVersion().String(),
				Kind:       mapping.GroupVersionKind.Kind,
				Name:       refName,
			},
			MaxReplicas: o.Max,
		},
	}

	if o.Min > 0 {
		scaler.Spec.MinReplicas = &o.Min
	}

	metrics := []autoscalingv2.MetricSpec{}

	// add CPU metric if any of the CPU targets are specified
	if o.CPUPercent > 0 {
		cpuMetric := autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name:   corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{},
			},
		}
		cpuMetric.Resource.Target.Type = autoscalingv2.UtilizationMetricType
		cpuMetric.Resource.Target.AverageUtilization = &o.CPUPercent
		metrics = append(metrics, cpuMetric)
	}

	// add Cpu metric if any of the cpu targets are specified
	if o.CPU != "" {
		cpuMetric := autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name:   corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{},
			},
		}

		quantity, value, metricsType, err := parseResourceInput(o.CPU, corev1.ResourceCPU)
		if err != nil {
			return nil, err
		}
		if metricsType == autoscalingv2.UtilizationMetricType {
			cpuMetric.Resource.Target.Type = autoscalingv2.UtilizationMetricType
			cpuMetric.Resource.Target.AverageUtilization = &value
		} else if metricsType == autoscalingv2.ValueMetricType {
			cpuMetric.Resource.Target.Type = autoscalingv2.ValueMetricType
			cpuMetric.Resource.Target.Value = &quantity
		}
		metrics = append(metrics, cpuMetric)
	}

	// add Memory metric if any of the memory targets are specified
	if o.Memory != "" {
		memoryMetric := autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name:   corev1.ResourceMemory,
				Target: autoscalingv2.MetricTarget{},
			},
		}
		quantity, value, metricsType, err := parseResourceInput(o.Memory, corev1.ResourceMemory)
		if err != nil {
			return nil, err
		}
		if metricsType == autoscalingv2.UtilizationMetricType {
			memoryMetric.Resource.Target.Type = autoscalingv2.UtilizationMetricType
			memoryMetric.Resource.Target.AverageUtilization = &value
		} else if metricsType == autoscalingv2.ValueMetricType {
			memoryMetric.Resource.Target.Type = autoscalingv2.ValueMetricType
			memoryMetric.Resource.Target.Value = &quantity
		}
		metrics = append(metrics, memoryMetric)
	}

	// Only set Metrics if there are any defined
	if len(metrics) > 0 {
		scaler.Spec.Metrics = metrics
	} else {
		scaler.Spec.Metrics = nil
	}

	return &scaler, nil
}

func (o *AutoscaleOptions) createHorizontalPodAutoscalerV1(refName string, mapping *meta.RESTMapping) *autoscalingv1.HorizontalPodAutoscaler {
	name := o.Name
	if len(name) == 0 {
		name = refName
	}

	scaler := autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				APIVersion: mapping.GroupVersionKind.GroupVersion().String(),
				Kind:       mapping.GroupVersionKind.Kind,
				Name:       refName,
			},
			MaxReplicas: o.Max,
		},
	}

	if o.Min > 0 {
		v := int32(o.Min)
		scaler.Spec.MinReplicas = &v
	}
	if o.CPUPercent >= 0 {
		c := int32(o.CPUPercent)
		scaler.Spec.TargetCPUUtilizationPercentage = &c
	}

	return &scaler
}

// parseResourceInput parses the input string and resource type
// to return a Quantity, value, MetricTargetType, and error.
// The input string should be in the format of "<value>[<unit>]".
func parseResourceInput(input string, resourceType corev1.ResourceName) (apiresource.Quantity, int32, autoscalingv2.MetricTargetType, error) {
	var quantity apiresource.Quantity
	var metricType autoscalingv2.MetricTargetType
	var unit string
	var value int32

	// Use regex to match the input value and its unit
	re := regexp.MustCompile(`^(\d+\.?\d*)([a-zA-Z%]*)$`)
	matches := re.FindStringSubmatch(input)
	if len(matches) != 3 {
		return quantity, value, metricType, errors.New("invalid input format")
	}

	valueInt64, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return quantity, value, metricType, err
	}
	value = int32(valueInt64)

	unit = matches[2]

	switch {
	case unit == "%":
		// If it's a percentage, set metricTypeInt to UtilizationMetricType
		metricType = autoscalingv2.UtilizationMetricType
		return quantity, value, metricType, nil
	case unit == "m" && resourceType == corev1.ResourceCPU: // CPU unit
		metricType = autoscalingv2.ValueMetricType
		quantity = apiresource.MustParse(input)
	case unit == "M" || unit == "Mi" || unit == "Gi" || unit == "Ki": // Memory units
		metricType = autoscalingv2.ValueMetricType
		quantity = apiresource.MustParse(input)
	default:
		// Default case, no unit specified
		metricType = autoscalingv2.ValueMetricType
		if resourceType == corev1.ResourceCPU {
			// For CPU, use 'm' as the default unit
			quantity = apiresource.MustParse(fmt.Sprintf("%vm", value))
		} else if resourceType == corev1.ResourceMemory {
			// For memory, use 'Mi' as the default unit
			quantity = apiresource.MustParse(fmt.Sprintf("%vMi", value))
		} else {
			return quantity, value, metricType, errors.New("unsupported resource type")
		}
	}
	return quantity, value, metricType, nil
}
