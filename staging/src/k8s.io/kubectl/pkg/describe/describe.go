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

package describe

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/fatih/camelcase"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	discoveryv1beta1 "k8s.io/api/discovery/v1beta1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/reference"
	utilcsr "k8s.io/client-go/util/certificate/csr"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/certificate"
	deploymentutil "k8s.io/kubectl/pkg/util/deployment"
	"k8s.io/kubectl/pkg/util/event"
	"k8s.io/kubectl/pkg/util/fieldpath"
	"k8s.io/kubectl/pkg/util/qos"
	"k8s.io/kubectl/pkg/util/rbac"
	resourcehelper "k8s.io/kubectl/pkg/util/resource"
	"k8s.io/kubectl/pkg/util/slice"
	storageutil "k8s.io/kubectl/pkg/util/storage"
)

// Each level has 2 spaces for PrefixWriter
const (
	LEVEL_0 = iota
	LEVEL_1
	LEVEL_2
	LEVEL_3
	LEVEL_4
)

var (
	// globally skipped annotations
	skipAnnotations = sets.New[string](corev1.LastAppliedConfigAnnotation)

	// DescriberFn gives a way to easily override the function for unit testing if needed
	DescriberFn DescriberFunc = Describer
)

// Describer returns a Describer for displaying the specified RESTMapping type or an error.
func Describer(restClientGetter genericclioptions.RESTClientGetter, mapping *meta.RESTMapping) (ResourceDescriber, error) {
	clientConfig, err := restClientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	// try to get a describer
	if describer, ok := DescriberFor(mapping.GroupVersionKind.GroupKind(), clientConfig); ok {
		return describer, nil
	}
	// if this is a kind we don't have a describer for yet, go generic if possible
	if genericDescriber, ok := GenericDescriberFor(mapping, clientConfig); ok {
		return genericDescriber, nil
	}
	// otherwise return an unregistered error
	return nil, fmt.Errorf("no description has been implemented for %s", mapping.GroupVersionKind.String())
}

// PrefixWriter can write text at various indentation levels.
type PrefixWriter interface {
	// Write writes text with the specified indentation level.
	Write(level int, format string, a ...interface{})
	// WriteLine writes an entire line with no indentation level.
	WriteLine(a ...interface{})
	// Flush forces indentation to be reset.
	Flush()
}

// prefixWriter implements PrefixWriter
type prefixWriter struct {
	out io.Writer
}

var _ PrefixWriter = &prefixWriter{}

// NewPrefixWriter creates a new PrefixWriter.
func NewPrefixWriter(out io.Writer) PrefixWriter {
	return &prefixWriter{out: out}
}

func (pw *prefixWriter) Write(level int, format string, a ...interface{}) {
	levelSpace := "  "
	prefix := ""
	for i := 0; i < level; i++ {
		prefix += levelSpace
	}
	output := fmt.Sprintf(prefix+format, a...)
	printers.WriteEscaped(pw.out, output)
}

func (pw *prefixWriter) WriteLine(a ...interface{}) {
	output := fmt.Sprintln(a...)
	printers.WriteEscaped(pw.out, output)
}

func (pw *prefixWriter) Flush() {
	if f, ok := pw.out.(flusher); ok {
		f.Flush()
	}
}

// nestedPrefixWriter implements PrefixWriter by increasing the level
// before passing text on to some other writer.
type nestedPrefixWriter struct {
	PrefixWriter
	indent int
}

var _ PrefixWriter = &prefixWriter{}

// NewPrefixWriter creates a new PrefixWriter.
func NewNestedPrefixWriter(out PrefixWriter, indent int) PrefixWriter {
	return &nestedPrefixWriter{PrefixWriter: out, indent: indent}
}

func (npw *nestedPrefixWriter) Write(level int, format string, a ...interface{}) {
	npw.PrefixWriter.Write(level+npw.indent, format, a...)
}

func (npw *nestedPrefixWriter) WriteLine(a ...interface{}) {
	npw.PrefixWriter.Write(npw.indent, "%s", fmt.Sprintln(a...))
}

func describerMap(clientConfig *rest.Config) (map[schema.GroupKind]ResourceDescriber, error) {
	c, err := clientset.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}

	m := map[schema.GroupKind]ResourceDescriber{
		{Group: corev1.GroupName, Kind: "Pod"}:                                    &PodDescriber{c},
		{Group: corev1.GroupName, Kind: "ReplicationController"}:                  &ReplicationControllerDescriber{c},
		{Group: corev1.GroupName, Kind: "Secret"}:                                 &SecretDescriber{c},
		{Group: corev1.GroupName, Kind: "Service"}:                                &ServiceDescriber{c},
		{Group: corev1.GroupName, Kind: "ServiceAccount"}:                         &ServiceAccountDescriber{c},
		{Group: corev1.GroupName, Kind: "Node"}:                                   &NodeDescriber{c},
		{Group: corev1.GroupName, Kind: "LimitRange"}:                             &LimitRangeDescriber{c},
		{Group: corev1.GroupName, Kind: "ResourceQuota"}:                          &ResourceQuotaDescriber{c},
		{Group: corev1.GroupName, Kind: "PersistentVolume"}:                       &PersistentVolumeDescriber{c},
		{Group: corev1.GroupName, Kind: "PersistentVolumeClaim"}:                  &PersistentVolumeClaimDescriber{c},
		{Group: corev1.GroupName, Kind: "Namespace"}:                              &NamespaceDescriber{c},
		{Group: corev1.GroupName, Kind: "Endpoints"}:                              &EndpointsDescriber{c},
		{Group: corev1.GroupName, Kind: "ConfigMap"}:                              &ConfigMapDescriber{c},
		{Group: corev1.GroupName, Kind: "PriorityClass"}:                          &PriorityClassDescriber{c},
		{Group: discoveryv1beta1.GroupName, Kind: "EndpointSlice"}:                &EndpointSliceDescriber{c},
		{Group: discoveryv1.GroupName, Kind: "EndpointSlice"}:                     &EndpointSliceDescriber{c},
		{Group: autoscalingv2.GroupName, Kind: "HorizontalPodAutoscaler"}:         &HorizontalPodAutoscalerDescriber{c},
		{Group: extensionsv1beta1.GroupName, Kind: "Ingress"}:                     &IngressDescriber{c},
		{Group: networkingv1beta1.GroupName, Kind: "Ingress"}:                     &IngressDescriber{c},
		{Group: networkingv1beta1.GroupName, Kind: "IngressClass"}:                &IngressClassDescriber{c},
		{Group: networkingv1.GroupName, Kind: "Ingress"}:                          &IngressDescriber{c},
		{Group: networkingv1.GroupName, Kind: "IngressClass"}:                     &IngressClassDescriber{c},
		{Group: networkingv1beta1.GroupName, Kind: "ServiceCIDR"}:                 &ServiceCIDRDescriber{c},
		{Group: networkingv1beta1.GroupName, Kind: "IPAddress"}:                   &IPAddressDescriber{c},
		{Group: networkingv1.GroupName, Kind: "ServiceCIDR"}:                      &ServiceCIDRDescriber{c},
		{Group: networkingv1.GroupName, Kind: "IPAddress"}:                        &IPAddressDescriber{c},
		{Group: batchv1.GroupName, Kind: "Job"}:                                   &JobDescriber{c},
		{Group: batchv1.GroupName, Kind: "CronJob"}:                               &CronJobDescriber{c},
		{Group: batchv1beta1.GroupName, Kind: "CronJob"}:                          &CronJobDescriber{c},
		{Group: appsv1.GroupName, Kind: "StatefulSet"}:                            &StatefulSetDescriber{c},
		{Group: appsv1.GroupName, Kind: "Deployment"}:                             &DeploymentDescriber{c},
		{Group: appsv1.GroupName, Kind: "DaemonSet"}:                              &DaemonSetDescriber{c},
		{Group: appsv1.GroupName, Kind: "ReplicaSet"}:                             &ReplicaSetDescriber{c},
		{Group: certificatesv1beta1.GroupName, Kind: "CertificateSigningRequest"}: &CertificateSigningRequestDescriber{c},
		{Group: storagev1.GroupName, Kind: "StorageClass"}:                        &StorageClassDescriber{c},
		{Group: storagev1.GroupName, Kind: "CSINode"}:                             &CSINodeDescriber{c},
		{Group: storagev1beta1.GroupName, Kind: "VolumeAttributesClass"}:          &VolumeAttributesClassDescriber{c},
		{Group: policyv1beta1.GroupName, Kind: "PodDisruptionBudget"}:             &PodDisruptionBudgetDescriber{c},
		{Group: policyv1.GroupName, Kind: "PodDisruptionBudget"}:                  &PodDisruptionBudgetDescriber{c},
		{Group: rbacv1.GroupName, Kind: "Role"}:                                   &RoleDescriber{c},
		{Group: rbacv1.GroupName, Kind: "ClusterRole"}:                            &ClusterRoleDescriber{c},
		{Group: rbacv1.GroupName, Kind: "RoleBinding"}:                            &RoleBindingDescriber{c},
		{Group: rbacv1.GroupName, Kind: "ClusterRoleBinding"}:                     &ClusterRoleBindingDescriber{c},
		{Group: networkingv1.GroupName, Kind: "NetworkPolicy"}:                    &NetworkPolicyDescriber{c},
		{Group: schedulingv1.GroupName, Kind: "PriorityClass"}:                    &PriorityClassDescriber{c},
	}

	return m, nil
}

// DescriberFor returns the default describe functions for each of the standard
// Kubernetes types.
func DescriberFor(kind schema.GroupKind, clientConfig *rest.Config) (ResourceDescriber, bool) {
	describers, err := describerMap(clientConfig)
	if err != nil {
		klog.V(1).Info(err)
		return nil, false
	}

	f, ok := describers[kind]
	return f, ok
}

// GenericDescriberFor returns a generic describer for the specified mapping
// that uses only information available from runtime.Unstructured
func GenericDescriberFor(mapping *meta.RESTMapping, clientConfig *rest.Config) (ResourceDescriber, bool) {
	// used to fetch the resource
	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		return nil, false
	}

	// used to get events for the resource
	clientSet, err := clientset.NewForConfig(clientConfig)
	if err != nil {
		return nil, false
	}
	eventsClient := clientSet.CoreV1()

	return &genericDescriber{mapping, dynamicClient, eventsClient}, true
}

type genericDescriber struct {
	mapping *meta.RESTMapping
	dynamic dynamic.Interface
	events  corev1client.EventsGetter
}

func (g *genericDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (output string, err error) {
	obj, err := g.dynamic.Resource(g.mapping.Resource).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(g.events, obj, describerSettings.ChunkSize)
	}

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", obj.GetName())
		w.Write(LEVEL_0, "Namespace:\t%s\n", obj.GetNamespace())
		printLabelsMultiline(w, "Labels", obj.GetLabels())
		printAnnotationsMultiline(w, "Annotations", obj.GetAnnotations())
		printUnstructuredContent(w, LEVEL_0, obj.UnstructuredContent(), "", ".metadata.managedFields", ".metadata.name",
			".metadata.namespace", ".metadata.labels", ".metadata.annotations")
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func printUnstructuredContent(w PrefixWriter, level int, content map[string]interface{}, skipPrefix string, skip ...string) {
	fields := []string{}
	for field := range content {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	for _, field := range fields {
		value := content[field]
		switch typedValue := value.(type) {
		case map[string]interface{}:
			skipExpr := fmt.Sprintf("%s.%s", skipPrefix, field)
			if slice.Contains[string](skip, skipExpr, nil) {
				continue
			}
			w.Write(level, "%s:\n", smartLabelFor(field))
			printUnstructuredContent(w, level+1, typedValue, skipExpr, skip...)

		case []interface{}:
			skipExpr := fmt.Sprintf("%s.%s", skipPrefix, field)
			if slice.Contains[string](skip, skipExpr, nil) {
				continue
			}
			w.Write(level, "%s:\n", smartLabelFor(field))
			for _, child := range typedValue {
				switch typedChild := child.(type) {
				case map[string]interface{}:
					printUnstructuredContent(w, level+1, typedChild, skipExpr, skip...)
				default:
					w.Write(level+1, "%v\n", typedChild)
				}
			}

		default:
			skipExpr := fmt.Sprintf("%s.%s", skipPrefix, field)
			if slice.Contains[string](skip, skipExpr, nil) {
				continue
			}
			w.Write(level, "%s:\t%v\n", smartLabelFor(field), typedValue)
		}
	}
}

func smartLabelFor(field string) string {
	// skip creating smart label if field name contains
	// special characters other than '-'
	if strings.IndexFunc(field, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '-'
	}) != -1 {
		return field
	}

	commonAcronyms := []string{"API", "URL", "UID", "OSB", "GUID"}
	parts := camelcase.Split(field)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "_" {
			continue
		}

		if slice.Contains[string](commonAcronyms, strings.ToUpper(part), nil) {
			part = strings.ToUpper(part)
		} else {
			part = strings.Title(part)
		}
		result = append(result, part)
	}

	return strings.Join(result, " ")
}

// DefaultObjectDescriber can describe the default Kubernetes objects.
var DefaultObjectDescriber ObjectDescriber

func init() {
	d := &Describers{}
	err := d.Add(
		describeCertificateSigningRequest,
		describeCronJob,
		describeCSINode,
		describeDaemonSet,
		describeDeployment,
		describeEndpoints,
		describeEndpointSliceV1,
		describeEndpointSliceV1beta1,
		describeHorizontalPodAutoscalerV1,
		describeHorizontalPodAutoscalerV2,
		describeJob,
		describeLimitRange,
		describeNamespace,
		describeNetworkPolicy,
		describeNode,
		describePersistentVolume,
		describePersistentVolumeClaim,
		describePod,
		describePodDisruptionBudgetV1,
		describePodDisruptionBudgetV1beta1,
		describePriorityClass,
		describeQuota,
		describeReplicaSet,
		describeReplicationController,
		describeSecret,
		describeService,
		describeServiceAccount,
		describeStatefulSet,
		describeStorageClass,
		describeVolumeAttributesClass,
	)
	if err != nil {
		klog.Fatalf("Cannot register describers: %v", err)
	}
	DefaultObjectDescriber = d
}

// NamespaceDescriber generates information about a namespace
type NamespaceDescriber struct {
	clientset.Interface
}

func (d *NamespaceDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	ns, err := d.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	resourceQuotaList := &corev1.ResourceQuotaList{}
	err = runtimeresource.FollowContinue(&metav1.ListOptions{Limit: describerSettings.ChunkSize},
		func(options metav1.ListOptions) (runtime.Object, error) {
			newList, err := d.CoreV1().ResourceQuotas(name).List(context.TODO(), options)
			if err != nil {
				return nil, runtimeresource.EnhanceListError(err, options, corev1.ResourceQuotas.String())
			}
			resourceQuotaList.Items = append(resourceQuotaList.Items, newList.Items...)
			return newList, nil
		})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Server does not support resource quotas.
			// Not an error, will not show resource quotas information.
			resourceQuotaList = nil
		} else {
			return "", err
		}
	}

	limitRangeList := &corev1.LimitRangeList{}
	err = runtimeresource.FollowContinue(&metav1.ListOptions{Limit: describerSettings.ChunkSize},
		func(options metav1.ListOptions) (runtime.Object, error) {
			newList, err := d.CoreV1().LimitRanges(name).List(context.TODO(), options)
			if err != nil {
				return nil, runtimeresource.EnhanceListError(err, options, "limitranges")
			}
			limitRangeList.Items = append(limitRangeList.Items, newList.Items...)
			return newList, nil
		})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Server does not support limit ranges.
			// Not an error, will not show limit ranges information.
			limitRangeList = nil
		} else {
			return "", err
		}
	}
	return describeNamespace(ns, resourceQuotaList, limitRangeList)
}

func describeNamespace(namespace *corev1.Namespace, resourceQuotaList *corev1.ResourceQuotaList, limitRangeList *corev1.LimitRangeList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", namespace.Name)
		printLabelsMultiline(w, "Labels", namespace.Labels)
		printAnnotationsMultiline(w, "Annotations", namespace.Annotations)
		w.Write(LEVEL_0, "Status:\t%s\n", string(namespace.Status.Phase))

		if len(namespace.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n")
			w.Write(LEVEL_1, "Type\tStatus\tLastTransitionTime\tReason\tMessage\n")
			w.Write(LEVEL_1, "----\t------\t------------------\t------\t-------\n")
			for _, c := range namespace.Status.Conditions {
				w.Write(LEVEL_1, "%v\t%v\t%s\t%v\t%v\n",
					c.Type,
					c.Status,
					c.LastTransitionTime.Time.Format(time.RFC1123Z),
					c.Reason,
					c.Message)
			}
		}

		if resourceQuotaList != nil {
			w.Write(LEVEL_0, "\n")
			DescribeResourceQuotas(resourceQuotaList, w)
		}

		if limitRangeList != nil {
			w.Write(LEVEL_0, "\n")
			DescribeLimitRanges(limitRangeList, w)
		}

		return nil
	})
}

func describeLimitRangeSpec(spec corev1.LimitRangeSpec, prefix string, w PrefixWriter) {
	for i := range spec.Limits {
		item := spec.Limits[i]
		maxResources := item.Max
		minResources := item.Min
		defaultLimitResources := item.Default
		defaultRequestResources := item.DefaultRequest
		ratio := item.MaxLimitRequestRatio

		set := map[corev1.ResourceName]bool{}
		for k := range maxResources {
			set[k] = true
		}
		for k := range minResources {
			set[k] = true
		}
		for k := range defaultLimitResources {
			set[k] = true
		}
		for k := range defaultRequestResources {
			set[k] = true
		}
		for k := range ratio {
			set[k] = true
		}

		for k := range set {
			// if no value is set, we output -
			maxValue := "-"
			minValue := "-"
			defaultLimitValue := "-"
			defaultRequestValue := "-"
			ratioValue := "-"

			maxQuantity, maxQuantityFound := maxResources[k]
			if maxQuantityFound {
				maxValue = maxQuantity.String()
			}

			minQuantity, minQuantityFound := minResources[k]
			if minQuantityFound {
				minValue = minQuantity.String()
			}

			defaultLimitQuantity, defaultLimitQuantityFound := defaultLimitResources[k]
			if defaultLimitQuantityFound {
				defaultLimitValue = defaultLimitQuantity.String()
			}

			defaultRequestQuantity, defaultRequestQuantityFound := defaultRequestResources[k]
			if defaultRequestQuantityFound {
				defaultRequestValue = defaultRequestQuantity.String()
			}

			ratioQuantity, ratioQuantityFound := ratio[k]
			if ratioQuantityFound {
				ratioValue = ratioQuantity.String()
			}

			msg := "%s%s\t%v\t%v\t%v\t%v\t%v\t%v\n"
			w.Write(LEVEL_0, msg, prefix, item.Type, k, minValue, maxValue, defaultRequestValue, defaultLimitValue, ratioValue)
		}
	}
}

// DescribeLimitRanges merges a set of limit range items into a single tabular description
func DescribeLimitRanges(limitRanges *corev1.LimitRangeList, w PrefixWriter) {
	if len(limitRanges.Items) == 0 {
		w.Write(LEVEL_0, "No LimitRange resource.\n")
		return
	}
	w.Write(LEVEL_0, "Resource Limits\n Type\tResource\tMin\tMax\tDefault Request\tDefault Limit\tMax Limit/Request Ratio\n")
	w.Write(LEVEL_0, " ----\t--------\t---\t---\t---------------\t-------------\t-----------------------\n")
	for _, limitRange := range limitRanges.Items {
		describeLimitRangeSpec(limitRange.Spec, " ", w)
	}
}

// DescribeResourceQuotas merges a set of quota items into a single tabular description of all quotas
func DescribeResourceQuotas(quotas *corev1.ResourceQuotaList, w PrefixWriter) {
	if len(quotas.Items) == 0 {
		w.Write(LEVEL_0, "No resource quota.\n")
		return
	}
	sort.Sort(SortableResourceQuotas(quotas.Items))

	w.Write(LEVEL_0, "Resource Quotas\n")
	for _, q := range quotas.Items {
		w.Write(LEVEL_1, "Name:\t%s\n", q.Name)
		if len(q.Spec.Scopes) > 0 {
			scopes := make([]string, 0, len(q.Spec.Scopes))
			for _, scope := range q.Spec.Scopes {
				scopes = append(scopes, string(scope))
			}
			sort.Strings(scopes)
			w.Write(LEVEL_1, "Scopes:\t%s\n", strings.Join(scopes, ", "))
			for _, scope := range scopes {
				helpText := helpTextForResourceQuotaScope(corev1.ResourceQuotaScope(scope))
				if len(helpText) > 0 {
					w.Write(LEVEL_1, "* %s\n", helpText)
				}
			}
		}

		w.Write(LEVEL_1, "Resource\tUsed\tHard\n")
		w.Write(LEVEL_1, "--------\t---\t---\n")

		resources := make([]corev1.ResourceName, 0, len(q.Status.Hard))
		for resource := range q.Status.Hard {
			resources = append(resources, resource)
		}
		sort.Sort(SortableResourceNames(resources))

		for _, resource := range resources {
			hardQuantity := q.Status.Hard[resource]
			usedQuantity := q.Status.Used[resource]
			w.Write(LEVEL_1, "%s\t%s\t%s\n", string(resource), usedQuantity.String(), hardQuantity.String())
		}
	}
}

// LimitRangeDescriber generates information about a limit range
type LimitRangeDescriber struct {
	clientset.Interface
}

func (d *LimitRangeDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	lr := d.CoreV1().LimitRanges(namespace)

	limitRange, err := lr.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return describeLimitRange(limitRange)
}

func describeLimitRange(limitRange *corev1.LimitRange) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", limitRange.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", limitRange.Namespace)
		w.Write(LEVEL_0, "Type\tResource\tMin\tMax\tDefault Request\tDefault Limit\tMax Limit/Request Ratio\n")
		w.Write(LEVEL_0, "----\t--------\t---\t---\t---------------\t-------------\t-----------------------\n")
		describeLimitRangeSpec(limitRange.Spec, "", w)
		return nil
	})
}

// ResourceQuotaDescriber generates information about a resource quota
type ResourceQuotaDescriber struct {
	clientset.Interface
}

func (d *ResourceQuotaDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	rq := d.CoreV1().ResourceQuotas(namespace)

	resourceQuota, err := rq.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return describeQuota(resourceQuota)
}

func helpTextForResourceQuotaScope(scope corev1.ResourceQuotaScope) string {
	switch scope {
	case corev1.ResourceQuotaScopeTerminating:
		return "Matches all pods that have an active deadline. These pods have a limited lifespan on a node before being actively terminated by the system."
	case corev1.ResourceQuotaScopeNotTerminating:
		return "Matches all pods that do not have an active deadline. These pods usually include long running pods whose container command is not expected to terminate."
	case corev1.ResourceQuotaScopeBestEffort:
		return "Matches all pods that do not have resource requirements set. These pods have a best effort quality of service."
	case corev1.ResourceQuotaScopeNotBestEffort:
		return "Matches all pods that have at least one resource requirement set. These pods have a burstable or guaranteed quality of service."
	default:
		return ""
	}
}
func describeQuota(resourceQuota *corev1.ResourceQuota) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", resourceQuota.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", resourceQuota.Namespace)
		if len(resourceQuota.Spec.Scopes) > 0 {
			scopes := make([]string, 0, len(resourceQuota.Spec.Scopes))
			for _, scope := range resourceQuota.Spec.Scopes {
				scopes = append(scopes, string(scope))
			}
			sort.Strings(scopes)
			w.Write(LEVEL_0, "Scopes:\t%s\n", strings.Join(scopes, ", "))
			for _, scope := range scopes {
				helpText := helpTextForResourceQuotaScope(corev1.ResourceQuotaScope(scope))
				if len(helpText) > 0 {
					w.Write(LEVEL_0, " * %s\n", helpText)
				}
			}
		}
		w.Write(LEVEL_0, "Resource\tUsed\tHard\n")
		w.Write(LEVEL_0, "--------\t----\t----\n")

		resources := make([]corev1.ResourceName, 0, len(resourceQuota.Status.Hard))
		for resource := range resourceQuota.Status.Hard {
			resources = append(resources, resource)
		}
		sort.Sort(SortableResourceNames(resources))

		msg := "%v\t%v\t%v\n"
		for i := range resources {
			resourceName := resources[i]
			hardQuantity := resourceQuota.Status.Hard[resourceName]
			usedQuantity := resourceQuota.Status.Used[resourceName]
			if hardQuantity.Format != usedQuantity.Format {
				usedQuantity = *resource.NewQuantity(usedQuantity.Value(), hardQuantity.Format)
			}
			w.Write(LEVEL_0, msg, resourceName, usedQuantity.String(), hardQuantity.String())
		}
		return nil
	})
}

// PodDescriber generates information about a pod and the replication controllers that
// create it.
type PodDescriber struct {
	clientset.Interface
}

func (d *PodDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	pod, err := d.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if describerSettings.ShowEvents {
			eventsInterface := d.CoreV1().Events(namespace)
			selector := eventsInterface.GetFieldSelector(&name, &namespace, nil, nil)
			initialOpts := metav1.ListOptions{
				FieldSelector: selector.String(),
				Limit:         describerSettings.ChunkSize,
			}
			events := &corev1.EventList{}
			err2 := runtimeresource.FollowContinue(&initialOpts,
				func(options metav1.ListOptions) (runtime.Object, error) {
					newList, err := eventsInterface.List(context.TODO(), options)
					if err != nil {
						return nil, runtimeresource.EnhanceListError(err, options, "events")
					}
					events.Items = append(events.Items, newList.Items...)
					return newList, nil
				})

			if err2 == nil && len(events.Items) > 0 {
				return tabbedString(func(out io.Writer) error {
					w := NewPrefixWriter(out)
					w.Write(LEVEL_0, "Pod '%v': error '%v', but found events.\n", name, err)
					DescribeEvents(events, w)
					return nil
				})
			}
		}
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		if ref, err := reference.GetReference(scheme.Scheme, pod); err != nil {
			klog.Errorf("Unable to construct reference to '%#v': %v", pod, err)
		} else {
			ref.Kind = ""
			if _, isMirrorPod := pod.Annotations[corev1.MirrorPodAnnotationKey]; isMirrorPod {
				ref.UID = types.UID(pod.Annotations[corev1.MirrorPodAnnotationKey])
			}
			events, _ = searchEvents(d.CoreV1(), ref, describerSettings.ChunkSize)
		}
	}

	return describePod(pod, events)
}

func describePod(pod *corev1.Pod, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", pod.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", pod.Namespace)
		if pod.Spec.Priority != nil {
			w.Write(LEVEL_0, "Priority:\t%d\n", *pod.Spec.Priority)
		}
		if len(pod.Spec.PriorityClassName) > 0 {
			w.Write(LEVEL_0, "Priority Class Name:\t%s\n", pod.Spec.PriorityClassName)
		}
		if pod.Spec.RuntimeClassName != nil && len(*pod.Spec.RuntimeClassName) > 0 {
			w.Write(LEVEL_0, "Runtime Class Name:\t%s\n", *pod.Spec.RuntimeClassName)
		}
		if len(pod.Spec.ServiceAccountName) > 0 {
			w.Write(LEVEL_0, "Service Account:\t%s\n", pod.Spec.ServiceAccountName)
		}
		if pod.Spec.NodeName == "" {
			w.Write(LEVEL_0, "Node:\t<none>\n")
		} else {
			w.Write(LEVEL_0, "Node:\t%s\n", pod.Spec.NodeName+"/"+pod.Status.HostIP)
		}
		if pod.Status.StartTime != nil {
			w.Write(LEVEL_0, "Start Time:\t%s\n", pod.Status.StartTime.Time.Format(time.RFC1123Z))
		}
		printLabelsMultiline(w, "Labels", pod.Labels)
		printAnnotationsMultiline(w, "Annotations", pod.Annotations)
		if pod.DeletionTimestamp != nil && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			w.Write(LEVEL_0, "Status:\tTerminating (lasts %s)\n", translateTimestampSince(*pod.DeletionTimestamp))
			w.Write(LEVEL_0, "Termination Grace Period:\t%ds\n", *pod.DeletionGracePeriodSeconds)
		} else {
			w.Write(LEVEL_0, "Status:\t%s\n", string(pod.Status.Phase))
		}
		if len(pod.Status.Reason) > 0 {
			w.Write(LEVEL_0, "Reason:\t%s\n", pod.Status.Reason)
		}
		if len(pod.Status.Message) > 0 {
			w.Write(LEVEL_0, "Message:\t%s\n", pod.Status.Message)
		}
		if pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.SeccompProfile != nil {
			w.Write(LEVEL_0, "SeccompProfile:\t%s\n", pod.Spec.SecurityContext.SeccompProfile.Type)
			if pod.Spec.SecurityContext.SeccompProfile.Type == corev1.SeccompProfileTypeLocalhost {
				w.Write(LEVEL_0, "LocalhostProfile:\t%s\n", *pod.Spec.SecurityContext.SeccompProfile.LocalhostProfile)
			}
		}
		// remove when .IP field is deprecated
		w.Write(LEVEL_0, "IP:\t%s\n", pod.Status.PodIP)
		describePodIPs(pod, w, "")
		if controlledBy := printController(pod); len(controlledBy) > 0 {
			w.Write(LEVEL_0, "Controlled By:\t%s\n", controlledBy)
		}
		if len(pod.Status.NominatedNodeName) > 0 {
			w.Write(LEVEL_0, "NominatedNodeName:\t%s\n", pod.Status.NominatedNodeName)
		}

		if pod.Spec.Resources != nil {
			w.Write(LEVEL_0, "Resources:\n")
			describeResources(pod.Spec.Resources, w, LEVEL_1)
		}

		if len(pod.Spec.InitContainers) > 0 {
			describeContainers("Init Containers", pod.Spec.InitContainers, pod.Status.InitContainerStatuses, EnvValueRetriever(pod), w, "")
		}
		describeContainers("Containers", pod.Spec.Containers, pod.Status.ContainerStatuses, EnvValueRetriever(pod), w, "")
		if len(pod.Spec.EphemeralContainers) > 0 {
			var ec []corev1.Container
			for i := range pod.Spec.EphemeralContainers {
				ec = append(ec, corev1.Container(pod.Spec.EphemeralContainers[i].EphemeralContainerCommon))
			}
			describeContainers("Ephemeral Containers", ec, pod.Status.EphemeralContainerStatuses, EnvValueRetriever(pod), w, "")
		}
		if len(pod.Spec.ReadinessGates) > 0 {
			w.Write(LEVEL_0, "Readiness Gates:\n  Type\tStatus\n")
			for _, g := range pod.Spec.ReadinessGates {
				status := "<none>"
				for _, c := range pod.Status.Conditions {
					if c.Type == g.ConditionType {
						status = fmt.Sprintf("%v", c.Status)
						break
					}
				}
				w.Write(LEVEL_1, "%v \t%v \n",
					g.ConditionType,
					status)
			}
		}
		if len(pod.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n  Type\tStatus\n")
			for _, c := range pod.Status.Conditions {
				w.Write(LEVEL_1, "%v \t%v \n",
					c.Type,
					c.Status)
			}
		}
		describeVolumes(pod.Spec.Volumes, w, "")
		w.Write(LEVEL_0, "QoS Class:\t%s\n", qos.GetPodQOS(pod))
		printLabelsMultiline(w, "Node-Selectors", pod.Spec.NodeSelector)
		printPodTolerationsMultiline(w, "Tolerations", pod.Spec.Tolerations)
		describeTopologySpreadConstraints(pod.Spec.TopologySpreadConstraints, w, "")
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func printController(controllee metav1.Object) string {
	if controllerRef := metav1.GetControllerOf(controllee); controllerRef != nil {
		return fmt.Sprintf("%s/%s", controllerRef.Kind, controllerRef.Name)
	}
	return ""
}

func describePodIPs(pod *corev1.Pod, w PrefixWriter, space string) {
	if len(pod.Status.PodIPs) == 0 {
		w.Write(LEVEL_0, "%sIPs:\t<none>\n", space)
		return
	}
	w.Write(LEVEL_0, "%sIPs:\n", space)
	for _, ipInfo := range pod.Status.PodIPs {
		w.Write(LEVEL_1, "IP:\t%s\n", ipInfo.IP)
	}
}

func describeTopologySpreadConstraints(tscs []corev1.TopologySpreadConstraint, w PrefixWriter, space string) {
	if len(tscs) == 0 {
		return
	}

	sort.Slice(tscs, func(i, j int) bool {
		return tscs[i].TopologyKey < tscs[j].TopologyKey
	})

	w.Write(LEVEL_0, "%sTopology Spread Constraints:\t", space)
	for i, tsc := range tscs {
		if i != 0 {
			w.Write(LEVEL_0, "%s", space)
			w.Write(LEVEL_0, "%s", "\t")
		}

		w.Write(LEVEL_0, "%s:", tsc.TopologyKey)
		w.Write(LEVEL_0, "%v", tsc.WhenUnsatisfiable)
		w.Write(LEVEL_0, " when max skew %d is exceeded", tsc.MaxSkew)
		if tsc.LabelSelector != nil {
			w.Write(LEVEL_0, " for selector %s", metav1.FormatLabelSelector(tsc.LabelSelector))
		}
		w.Write(LEVEL_0, "\n")
	}
}

func describeVolumes(volumes []corev1.Volume, w PrefixWriter, space string) {
	if len(volumes) == 0 {
		w.Write(LEVEL_0, "%sVolumes:\t<none>\n", space)
		return
	}

	w.Write(LEVEL_0, "%sVolumes:\n", space)
	for _, volume := range volumes {
		nameIndent := ""
		if len(space) > 0 {
			nameIndent = " "
		}
		w.Write(LEVEL_1, "%s%v:\n", nameIndent, volume.Name)
		switch {
		case volume.VolumeSource.HostPath != nil:
			printHostPathVolumeSource(volume.VolumeSource.HostPath, w)
		case volume.VolumeSource.EmptyDir != nil:
			printEmptyDirVolumeSource(volume.VolumeSource.EmptyDir, w)
		case volume.VolumeSource.GCEPersistentDisk != nil:
			printGCEPersistentDiskVolumeSource(volume.VolumeSource.GCEPersistentDisk, w)
		case volume.VolumeSource.AWSElasticBlockStore != nil:
			printAWSElasticBlockStoreVolumeSource(volume.VolumeSource.AWSElasticBlockStore, w)
		case volume.VolumeSource.GitRepo != nil:
			printGitRepoVolumeSource(volume.VolumeSource.GitRepo, w)
		case volume.VolumeSource.Secret != nil:
			printSecretVolumeSource(volume.VolumeSource.Secret, w)
		case volume.VolumeSource.ConfigMap != nil:
			printConfigMapVolumeSource(volume.VolumeSource.ConfigMap, w)
		case volume.VolumeSource.NFS != nil:
			printNFSVolumeSource(volume.VolumeSource.NFS, w)
		case volume.VolumeSource.ISCSI != nil:
			printISCSIVolumeSource(volume.VolumeSource.ISCSI, w)
		case volume.VolumeSource.Glusterfs != nil:
			printGlusterfsVolumeSource(volume.VolumeSource.Glusterfs, w)
		case volume.VolumeSource.PersistentVolumeClaim != nil:
			printPersistentVolumeClaimVolumeSource(volume.VolumeSource.PersistentVolumeClaim, w)
		case volume.VolumeSource.Ephemeral != nil:
			printEphemeralVolumeSource(volume.VolumeSource.Ephemeral, w)
		case volume.VolumeSource.RBD != nil:
			printRBDVolumeSource(volume.VolumeSource.RBD, w)
		case volume.VolumeSource.Quobyte != nil:
			printQuobyteVolumeSource(volume.VolumeSource.Quobyte, w)
		case volume.VolumeSource.DownwardAPI != nil:
			printDownwardAPIVolumeSource(volume.VolumeSource.DownwardAPI, w)
		case volume.VolumeSource.AzureDisk != nil:
			printAzureDiskVolumeSource(volume.VolumeSource.AzureDisk, w)
		case volume.VolumeSource.VsphereVolume != nil:
			printVsphereVolumeSource(volume.VolumeSource.VsphereVolume, w)
		case volume.VolumeSource.Cinder != nil:
			printCinderVolumeSource(volume.VolumeSource.Cinder, w)
		case volume.VolumeSource.PhotonPersistentDisk != nil:
			printPhotonPersistentDiskVolumeSource(volume.VolumeSource.PhotonPersistentDisk, w)
		case volume.VolumeSource.PortworxVolume != nil:
			printPortworxVolumeSource(volume.VolumeSource.PortworxVolume, w)
		case volume.VolumeSource.ScaleIO != nil:
			printScaleIOVolumeSource(volume.VolumeSource.ScaleIO, w)
		case volume.VolumeSource.CephFS != nil:
			printCephFSVolumeSource(volume.VolumeSource.CephFS, w)
		case volume.VolumeSource.StorageOS != nil:
			printStorageOSVolumeSource(volume.VolumeSource.StorageOS, w)
		case volume.VolumeSource.FC != nil:
			printFCVolumeSource(volume.VolumeSource.FC, w)
		case volume.VolumeSource.AzureFile != nil:
			printAzureFileVolumeSource(volume.VolumeSource.AzureFile, w)
		case volume.VolumeSource.FlexVolume != nil:
			printFlexVolumeSource(volume.VolumeSource.FlexVolume, w)
		case volume.VolumeSource.Flocker != nil:
			printFlockerVolumeSource(volume.VolumeSource.Flocker, w)
		case volume.VolumeSource.Projected != nil:
			printProjectedVolumeSource(volume.VolumeSource.Projected, w)
		case volume.VolumeSource.CSI != nil:
			printCSIVolumeSource(volume.VolumeSource.CSI, w)
		case volume.VolumeSource.Image != nil:
			printImageVolumeSource(volume.VolumeSource.Image, w)
		default:
			w.Write(LEVEL_1, "<unknown>\n")
		}
	}
}

func printHostPathVolumeSource(hostPath *corev1.HostPathVolumeSource, w PrefixWriter) {
	hostPathType := "<none>"
	if hostPath.Type != nil {
		hostPathType = string(*hostPath.Type)
	}
	w.Write(LEVEL_2, "Type:\tHostPath (bare host directory volume)\n"+
		"    Path:\t%v\n"+
		"    HostPathType:\t%v\n",
		hostPath.Path, hostPathType)
}

func printEmptyDirVolumeSource(emptyDir *corev1.EmptyDirVolumeSource, w PrefixWriter) {
	var sizeLimit string
	if emptyDir.SizeLimit != nil && emptyDir.SizeLimit.Cmp(resource.Quantity{}) > 0 {
		sizeLimit = fmt.Sprintf("%v", emptyDir.SizeLimit)
	} else {
		sizeLimit = "<unset>"
	}
	w.Write(LEVEL_2, "Type:\tEmptyDir (a temporary directory that shares a pod's lifetime)\n"+
		"    Medium:\t%v\n"+
		"    SizeLimit:\t%v\n",
		emptyDir.Medium, sizeLimit)
}

func printGCEPersistentDiskVolumeSource(gce *corev1.GCEPersistentDiskVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tGCEPersistentDisk (a Persistent Disk resource in Google Compute Engine)\n"+
		"    PDName:\t%v\n"+
		"    FSType:\t%v\n"+
		"    Partition:\t%v\n"+
		"    ReadOnly:\t%v\n",
		gce.PDName, gce.FSType, gce.Partition, gce.ReadOnly)
}

func printAWSElasticBlockStoreVolumeSource(aws *corev1.AWSElasticBlockStoreVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tAWSElasticBlockStore (a Persistent Disk resource in AWS)\n"+
		"    VolumeID:\t%v\n"+
		"    FSType:\t%v\n"+
		"    Partition:\t%v\n"+
		"    ReadOnly:\t%v\n",
		aws.VolumeID, aws.FSType, aws.Partition, aws.ReadOnly)
}

func printGitRepoVolumeSource(git *corev1.GitRepoVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tGitRepo (a volume that is pulled from git when the pod is created)\n"+
		"    Repository:\t%v\n"+
		"    Revision:\t%v\n",
		git.Repository, git.Revision)
}

func printSecretVolumeSource(secret *corev1.SecretVolumeSource, w PrefixWriter) {
	optional := secret.Optional != nil && *secret.Optional
	w.Write(LEVEL_2, "Type:\tSecret (a volume populated by a Secret)\n"+
		"    SecretName:\t%v\n"+
		"    Optional:\t%v\n",
		secret.SecretName, optional)
}

func printConfigMapVolumeSource(configMap *corev1.ConfigMapVolumeSource, w PrefixWriter) {
	optional := configMap.Optional != nil && *configMap.Optional
	w.Write(LEVEL_2, "Type:\tConfigMap (a volume populated by a ConfigMap)\n"+
		"    Name:\t%v\n"+
		"    Optional:\t%v\n",
		configMap.Name, optional)
}

func printProjectedVolumeSource(projected *corev1.ProjectedVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tProjected (a volume that contains injected data from multiple sources)\n")
	for _, source := range projected.Sources {
		if source.Secret != nil {
			optional := source.Secret.Optional != nil && *source.Secret.Optional
			w.Write(LEVEL_2, "SecretName:\t%v\n"+
				"    Optional:\t%v\n",
				source.Secret.Name, optional)
		} else if source.DownwardAPI != nil {
			w.Write(LEVEL_2, "DownwardAPI:\ttrue\n")
		} else if source.ConfigMap != nil {
			optional := source.ConfigMap.Optional != nil && *source.ConfigMap.Optional
			w.Write(LEVEL_2, "ConfigMapName:\t%v\n"+
				"    Optional:\t%v\n",
				source.ConfigMap.Name, optional)
		} else if source.ServiceAccountToken != nil {
			w.Write(LEVEL_2, "TokenExpirationSeconds:\t%d\n",
				*source.ServiceAccountToken.ExpirationSeconds)
		}
	}
}

func printNFSVolumeSource(nfs *corev1.NFSVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tNFS (an NFS mount that lasts the lifetime of a pod)\n"+
		"    Server:\t%v\n"+
		"    Path:\t%v\n"+
		"    ReadOnly:\t%v\n",
		nfs.Server, nfs.Path, nfs.ReadOnly)
}

func printQuobyteVolumeSource(quobyte *corev1.QuobyteVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tQuobyte (a Quobyte mount on the host that shares a pod's lifetime)\n"+
		"    Registry:\t%v\n"+
		"    Volume:\t%v\n"+
		"    ReadOnly:\t%v\n",
		quobyte.Registry, quobyte.Volume, quobyte.ReadOnly)
}

func printPortworxVolumeSource(pwxVolume *corev1.PortworxVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tPortworxVolume (a Portworx Volume resource)\n"+
		"    VolumeID:\t%v\n",
		pwxVolume.VolumeID)
}

func printISCSIVolumeSource(iscsi *corev1.ISCSIVolumeSource, w PrefixWriter) {
	initiator := "<none>"
	if iscsi.InitiatorName != nil {
		initiator = *iscsi.InitiatorName
	}
	w.Write(LEVEL_2, "Type:\tISCSI (an ISCSI Disk resource that is attached to a kubelet's host machine and then exposed to the pod)\n"+
		"    TargetPortal:\t%v\n"+
		"    IQN:\t%v\n"+
		"    Lun:\t%v\n"+
		"    ISCSIInterface\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n"+
		"    Portals:\t%v\n"+
		"    DiscoveryCHAPAuth:\t%v\n"+
		"    SessionCHAPAuth:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    InitiatorName:\t%v\n",
		iscsi.TargetPortal, iscsi.IQN, iscsi.Lun, iscsi.ISCSIInterface, iscsi.FSType, iscsi.ReadOnly, iscsi.Portals, iscsi.DiscoveryCHAPAuth, iscsi.SessionCHAPAuth, iscsi.SecretRef, initiator)
}

func printISCSIPersistentVolumeSource(iscsi *corev1.ISCSIPersistentVolumeSource, w PrefixWriter) {
	initiatorName := "<none>"
	if iscsi.InitiatorName != nil {
		initiatorName = *iscsi.InitiatorName
	}
	w.Write(LEVEL_2, "Type:\tISCSI (an ISCSI Disk resource that is attached to a kubelet's host machine and then exposed to the pod)\n"+
		"    TargetPortal:\t%v\n"+
		"    IQN:\t%v\n"+
		"    Lun:\t%v\n"+
		"    ISCSIInterface\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n"+
		"    Portals:\t%v\n"+
		"    DiscoveryCHAPAuth:\t%v\n"+
		"    SessionCHAPAuth:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    InitiatorName:\t%v\n",
		iscsi.TargetPortal, iscsi.IQN, iscsi.Lun, iscsi.ISCSIInterface, iscsi.FSType, iscsi.ReadOnly, iscsi.Portals, iscsi.DiscoveryCHAPAuth, iscsi.SessionCHAPAuth, iscsi.SecretRef, initiatorName)
}

func printGlusterfsVolumeSource(glusterfs *corev1.GlusterfsVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tGlusterfs (a Glusterfs mount on the host that shares a pod's lifetime)\n"+
		"    EndpointsName:\t%v\n"+
		"    Path:\t%v\n"+
		"    ReadOnly:\t%v\n",
		glusterfs.EndpointsName, glusterfs.Path, glusterfs.ReadOnly)
}

func printGlusterfsPersistentVolumeSource(glusterfs *corev1.GlusterfsPersistentVolumeSource, w PrefixWriter) {
	endpointsNamespace := "<unset>"
	if glusterfs.EndpointsNamespace != nil {
		endpointsNamespace = *glusterfs.EndpointsNamespace
	}
	w.Write(LEVEL_2, "Type:\tGlusterfs (a Glusterfs mount on the host that shares a pod's lifetime)\n"+
		"    EndpointsName:\t%v\n"+
		"    EndpointsNamespace:\t%v\n"+
		"    Path:\t%v\n"+
		"    ReadOnly:\t%v\n",
		glusterfs.EndpointsName, endpointsNamespace, glusterfs.Path, glusterfs.ReadOnly)
}

func printPersistentVolumeClaimVolumeSource(claim *corev1.PersistentVolumeClaimVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tPersistentVolumeClaim (a reference to a PersistentVolumeClaim in the same namespace)\n"+
		"    ClaimName:\t%v\n"+
		"    ReadOnly:\t%v\n",
		claim.ClaimName, claim.ReadOnly)
}

func printEphemeralVolumeSource(ephemeral *corev1.EphemeralVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tEphemeralVolume (an inline specification for a volume that gets created and deleted with the pod)\n")
	if ephemeral.VolumeClaimTemplate != nil {
		printPersistentVolumeClaim(NewNestedPrefixWriter(w, LEVEL_2),
			&corev1.PersistentVolumeClaim{
				ObjectMeta: ephemeral.VolumeClaimTemplate.ObjectMeta,
				Spec:       ephemeral.VolumeClaimTemplate.Spec,
			}, false /* not a full PVC */)
	}
}

func printRBDVolumeSource(rbd *corev1.RBDVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tRBD (a Rados Block Device mount on the host that shares a pod's lifetime)\n"+
		"    CephMonitors:\t%v\n"+
		"    RBDImage:\t%v\n"+
		"    FSType:\t%v\n"+
		"    RBDPool:\t%v\n"+
		"    RadosUser:\t%v\n"+
		"    Keyring:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    ReadOnly:\t%v\n",
		rbd.CephMonitors, rbd.RBDImage, rbd.FSType, rbd.RBDPool, rbd.RadosUser, rbd.Keyring, rbd.SecretRef, rbd.ReadOnly)
}

func printRBDPersistentVolumeSource(rbd *corev1.RBDPersistentVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tRBD (a Rados Block Device mount on the host that shares a pod's lifetime)\n"+
		"    CephMonitors:\t%v\n"+
		"    RBDImage:\t%v\n"+
		"    FSType:\t%v\n"+
		"    RBDPool:\t%v\n"+
		"    RadosUser:\t%v\n"+
		"    Keyring:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    ReadOnly:\t%v\n",
		rbd.CephMonitors, rbd.RBDImage, rbd.FSType, rbd.RBDPool, rbd.RadosUser, rbd.Keyring, rbd.SecretRef, rbd.ReadOnly)
}

func printDownwardAPIVolumeSource(d *corev1.DownwardAPIVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tDownwardAPI (a volume populated by information about the pod)\n    Items:\n")
	for _, mapping := range d.Items {
		if mapping.FieldRef != nil {
			w.Write(LEVEL_3, "%v -> %v\n", mapping.FieldRef.FieldPath, mapping.Path)
		}
		if mapping.ResourceFieldRef != nil {
			w.Write(LEVEL_3, "%v -> %v\n", mapping.ResourceFieldRef.Resource, mapping.Path)
		}
	}
}

func printAzureDiskVolumeSource(d *corev1.AzureDiskVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tAzureDisk (an Azure Data Disk mount on the host and bind mount to the pod)\n"+
		"    DiskName:\t%v\n"+
		"    DiskURI:\t%v\n"+
		"    Kind: \t%v\n"+
		"    FSType:\t%v\n"+
		"    CachingMode:\t%v\n"+
		"    ReadOnly:\t%v\n",
		d.DiskName, d.DataDiskURI, *d.Kind, *d.FSType, *d.CachingMode, *d.ReadOnly)
}

func printVsphereVolumeSource(vsphere *corev1.VsphereVirtualDiskVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tvSphereVolume (a Persistent Disk resource in vSphere)\n"+
		"    VolumePath:\t%v\n"+
		"    FSType:\t%v\n"+
		"    StoragePolicyName:\t%v\n",
		vsphere.VolumePath, vsphere.FSType, vsphere.StoragePolicyName)
}

func printPhotonPersistentDiskVolumeSource(photon *corev1.PhotonPersistentDiskVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tPhotonPersistentDisk (a Persistent Disk resource in photon platform)\n"+
		"    PdID:\t%v\n"+
		"    FSType:\t%v\n",
		photon.PdID, photon.FSType)
}

func printCinderVolumeSource(cinder *corev1.CinderVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tCinder (a Persistent Disk resource in OpenStack)\n"+
		"    VolumeID:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n"+
		"    SecretRef:\t%v\n",
		cinder.VolumeID, cinder.FSType, cinder.ReadOnly, cinder.SecretRef)
}

func printCinderPersistentVolumeSource(cinder *corev1.CinderPersistentVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tCinder (a Persistent Disk resource in OpenStack)\n"+
		"    VolumeID:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n"+
		"    SecretRef:\t%v\n",
		cinder.VolumeID, cinder.FSType, cinder.ReadOnly, cinder.SecretRef)
}

func printScaleIOVolumeSource(sio *corev1.ScaleIOVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tScaleIO (a persistent volume backed by a block device in ScaleIO)\n"+
		"    Gateway:\t%v\n"+
		"    System:\t%v\n"+
		"    Protection Domain:\t%v\n"+
		"    Storage Pool:\t%v\n"+
		"    Storage Mode:\t%v\n"+
		"    VolumeName:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n",
		sio.Gateway, sio.System, sio.ProtectionDomain, sio.StoragePool, sio.StorageMode, sio.VolumeName, sio.FSType, sio.ReadOnly)
}

func printScaleIOPersistentVolumeSource(sio *corev1.ScaleIOPersistentVolumeSource, w PrefixWriter) {
	var secretNS, secretName string
	if sio.SecretRef != nil {
		secretName = sio.SecretRef.Name
		secretNS = sio.SecretRef.Namespace
	}
	w.Write(LEVEL_2, "Type:\tScaleIO (a persistent volume backed by a block device in ScaleIO)\n"+
		"    Gateway:\t%v\n"+
		"    System:\t%v\n"+
		"    Protection Domain:\t%v\n"+
		"    Storage Pool:\t%v\n"+
		"    Storage Mode:\t%v\n"+
		"    VolumeName:\t%v\n"+
		"    SecretName:\t%v\n"+
		"    SecretNamespace:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n",
		sio.Gateway, sio.System, sio.ProtectionDomain, sio.StoragePool, sio.StorageMode, sio.VolumeName, secretName, secretNS, sio.FSType, sio.ReadOnly)
}

func printLocalVolumeSource(ls *corev1.LocalVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tLocalVolume (a persistent volume backed by local storage on a node)\n"+
		"    Path:\t%v\n",
		ls.Path)
}

func printCephFSVolumeSource(cephfs *corev1.CephFSVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tCephFS (a CephFS mount on the host that shares a pod's lifetime)\n"+
		"    Monitors:\t%v\n"+
		"    Path:\t%v\n"+
		"    User:\t%v\n"+
		"    SecretFile:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    ReadOnly:\t%v\n",
		cephfs.Monitors, cephfs.Path, cephfs.User, cephfs.SecretFile, cephfs.SecretRef, cephfs.ReadOnly)
}

func printCephFSPersistentVolumeSource(cephfs *corev1.CephFSPersistentVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tCephFS (a CephFS mount on the host that shares a pod's lifetime)\n"+
		"    Monitors:\t%v\n"+
		"    Path:\t%v\n"+
		"    User:\t%v\n"+
		"    SecretFile:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    ReadOnly:\t%v\n",
		cephfs.Monitors, cephfs.Path, cephfs.User, cephfs.SecretFile, cephfs.SecretRef, cephfs.ReadOnly)
}

func printStorageOSVolumeSource(storageos *corev1.StorageOSVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tStorageOS (a StorageOS Persistent Disk resource)\n"+
		"    VolumeName:\t%v\n"+
		"    VolumeNamespace:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n",
		storageos.VolumeName, storageos.VolumeNamespace, storageos.FSType, storageos.ReadOnly)
}

func printStorageOSPersistentVolumeSource(storageos *corev1.StorageOSPersistentVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tStorageOS (a StorageOS Persistent Disk resource)\n"+
		"    VolumeName:\t%v\n"+
		"    VolumeNamespace:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n",
		storageos.VolumeName, storageos.VolumeNamespace, storageos.FSType, storageos.ReadOnly)
}

func printFCVolumeSource(fc *corev1.FCVolumeSource, w PrefixWriter) {
	lun := "<none>"
	if fc.Lun != nil {
		lun = strconv.Itoa(int(*fc.Lun))
	}
	w.Write(LEVEL_2, "Type:\tFC (a Fibre Channel disk)\n"+
		"    TargetWWNs:\t%v\n"+
		"    LUN:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n",
		strings.Join(fc.TargetWWNs, ", "), lun, fc.FSType, fc.ReadOnly)
}

func printAzureFileVolumeSource(azureFile *corev1.AzureFileVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tAzureFile (an Azure File Service mount on the host and bind mount to the pod)\n"+
		"    SecretName:\t%v\n"+
		"    ShareName:\t%v\n"+
		"    ReadOnly:\t%v\n",
		azureFile.SecretName, azureFile.ShareName, azureFile.ReadOnly)
}

func printAzureFilePersistentVolumeSource(azureFile *corev1.AzureFilePersistentVolumeSource, w PrefixWriter) {
	ns := ""
	if azureFile.SecretNamespace != nil {
		ns = *azureFile.SecretNamespace
	}
	w.Write(LEVEL_2, "Type:\tAzureFile (an Azure File Service mount on the host and bind mount to the pod)\n"+
		"    SecretName:\t%v\n"+
		"    SecretNamespace:\t%v\n"+
		"    ShareName:\t%v\n"+
		"    ReadOnly:\t%v\n",
		azureFile.SecretName, ns, azureFile.ShareName, azureFile.ReadOnly)
}

func printFlexPersistentVolumeSource(flex *corev1.FlexPersistentVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tFlexVolume (a generic volume resource that is provisioned/attached using an exec based plugin)\n"+
		"    Driver:\t%v\n"+
		"    FSType:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    ReadOnly:\t%v\n"+
		"    Options:\t%v\n",
		flex.Driver, flex.FSType, flex.SecretRef, flex.ReadOnly, flex.Options)
}

func printFlexVolumeSource(flex *corev1.FlexVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tFlexVolume (a generic volume resource that is provisioned/attached using an exec based plugin)\n"+
		"    Driver:\t%v\n"+
		"    FSType:\t%v\n"+
		"    SecretRef:\t%v\n"+
		"    ReadOnly:\t%v\n"+
		"    Options:\t%v\n",
		flex.Driver, flex.FSType, flex.SecretRef, flex.ReadOnly, flex.Options)
}

func printFlockerVolumeSource(flocker *corev1.FlockerVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tFlocker (a Flocker volume mounted by the Flocker agent)\n"+
		"    DatasetName:\t%v\n"+
		"    DatasetUUID:\t%v\n",
		flocker.DatasetName, flocker.DatasetUUID)
}

func printCSIVolumeSource(csi *corev1.CSIVolumeSource, w PrefixWriter) {
	var readOnly bool
	var fsType string
	if csi.ReadOnly != nil && *csi.ReadOnly {
		readOnly = true
	}
	if csi.FSType != nil {
		fsType = *csi.FSType
	}
	w.Write(LEVEL_2, "Type:\tCSI (a Container Storage Interface (CSI) volume source)\n"+
		"    Driver:\t%v\n"+
		"    FSType:\t%v\n"+
		"    ReadOnly:\t%v\n",
		csi.Driver, fsType, readOnly)
	printCSIPersistentVolumeAttributesMultiline(w, "VolumeAttributes", csi.VolumeAttributes)
}

func printCSIPersistentVolumeSource(csi *corev1.CSIPersistentVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tCSI (a Container Storage Interface (CSI) volume source)\n"+
		"    Driver:\t%v\n"+
		"    FSType:\t%v\n"+
		"    VolumeHandle:\t%v\n"+
		"    ReadOnly:\t%v\n",
		csi.Driver, csi.FSType, csi.VolumeHandle, csi.ReadOnly)
	printCSIPersistentVolumeAttributesMultiline(w, "VolumeAttributes", csi.VolumeAttributes)
}

func printCSIPersistentVolumeAttributesMultiline(w PrefixWriter, title string, annotations map[string]string) {
	printCSIPersistentVolumeAttributesMultilineIndent(w, "", title, "\t", annotations, sets.New[string]())
}

func printCSIPersistentVolumeAttributesMultilineIndent(w PrefixWriter, initialIndent, title, innerIndent string, attributes map[string]string, skip sets.Set[string]) {
	w.Write(LEVEL_2, "%s%s:%s", initialIndent, title, innerIndent)

	if len(attributes) == 0 {
		w.WriteLine("<none>")
		return
	}

	// to print labels in the sorted order
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		if skip.Has(key) {
			continue
		}
		keys = append(keys, key)
	}
	if len(attributes) == 0 {
		w.WriteLine("<none>")
		return
	}
	sort.Strings(keys)

	for i, key := range keys {
		if i != 0 {
			w.Write(LEVEL_2, initialIndent)
			w.Write(LEVEL_2, innerIndent)
		}
		line := fmt.Sprintf("%s=%s", key, attributes[key])
		if len(line) > maxAnnotationLen {
			w.Write(LEVEL_2, "%s...\n", line[:maxAnnotationLen])
		} else {
			w.Write(LEVEL_2, "%s\n", line)
		}
	}
}

func printImageVolumeSource(image *corev1.ImageVolumeSource, w PrefixWriter) {
	w.Write(LEVEL_2, "Type:\tImage (a container image or OCI artifact)\n"+
		"    Reference:\t%v\n"+
		"    PullPolicy:\t%v\n",
		image.Reference, image.PullPolicy)
}

type PersistentVolumeDescriber struct {
	clientset.Interface
}

func (d *PersistentVolumeDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().PersistentVolumes()

	pv, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), pv, describerSettings.ChunkSize)
	}

	return describePersistentVolume(pv, events)
}

func printVolumeNodeAffinity(w PrefixWriter, affinity *corev1.VolumeNodeAffinity) {
	w.Write(LEVEL_0, "Node Affinity:\t")
	if affinity == nil || affinity.Required == nil {
		w.WriteLine("<none>")
		return
	}
	w.WriteLine("")

	if affinity.Required != nil {
		w.Write(LEVEL_1, "Required Terms:\t")
		if len(affinity.Required.NodeSelectorTerms) == 0 {
			w.WriteLine("<none>")
		} else {
			w.WriteLine("")
			for i, term := range affinity.Required.NodeSelectorTerms {
				printNodeSelectorTermsMultilineWithIndent(w, LEVEL_2, fmt.Sprintf("Term %v", i), "\t", term.MatchExpressions)
			}
		}
	}
}

// printLabelsMultiline prints multiple labels with a user-defined alignment.
func printNodeSelectorTermsMultilineWithIndent(w PrefixWriter, indentLevel int, title, innerIndent string, reqs []corev1.NodeSelectorRequirement) {
	w.Write(indentLevel, "%s:%s", title, innerIndent)

	if len(reqs) == 0 {
		w.WriteLine("<none>")
		return
	}

	for i, req := range reqs {
		if i != 0 {
			w.Write(indentLevel, "%s", innerIndent)
		}
		exprStr := fmt.Sprintf("%s %s", req.Key, strings.ToLower(string(req.Operator)))
		if len(req.Values) > 0 {
			exprStr = fmt.Sprintf("%s [%s]", exprStr, strings.Join(req.Values, ", "))
		}
		w.Write(LEVEL_0, "%s\n", exprStr)
	}
}

func describePersistentVolume(pv *corev1.PersistentVolume, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", pv.Name)
		printLabelsMultiline(w, "Labels", pv.ObjectMeta.Labels)
		printAnnotationsMultiline(w, "Annotations", pv.ObjectMeta.Annotations)
		w.Write(LEVEL_0, "Finalizers:\t%v\n", pv.ObjectMeta.Finalizers)
		w.Write(LEVEL_0, "StorageClass:\t%s\n", storageutil.GetPersistentVolumeClass(pv))
		if pv.ObjectMeta.DeletionTimestamp != nil {
			w.Write(LEVEL_0, "Status:\tTerminating (lasts %s)\n", translateTimestampSince(*pv.ObjectMeta.DeletionTimestamp))
		} else {
			w.Write(LEVEL_0, "Status:\t%v\n", pv.Status.Phase)
		}
		if pv.Spec.ClaimRef != nil {
			w.Write(LEVEL_0, "Claim:\t%s\n", pv.Spec.ClaimRef.Namespace+"/"+pv.Spec.ClaimRef.Name)
		} else {
			w.Write(LEVEL_0, "Claim:\t%s\n", "")
		}
		w.Write(LEVEL_0, "Reclaim Policy:\t%v\n", pv.Spec.PersistentVolumeReclaimPolicy)
		w.Write(LEVEL_0, "Access Modes:\t%s\n", storageutil.GetAccessModesAsString(pv.Spec.AccessModes))
		if pv.Spec.VolumeMode != nil {
			w.Write(LEVEL_0, "VolumeMode:\t%v\n", *pv.Spec.VolumeMode)
		}
		storage := pv.Spec.Capacity[corev1.ResourceStorage]
		w.Write(LEVEL_0, "Capacity:\t%s\n", storage.String())
		printVolumeNodeAffinity(w, pv.Spec.NodeAffinity)
		w.Write(LEVEL_0, "Message:\t%s\n", pv.Status.Message)
		w.Write(LEVEL_0, "Source:\n")

		switch {
		case pv.Spec.HostPath != nil:
			printHostPathVolumeSource(pv.Spec.HostPath, w)
		case pv.Spec.GCEPersistentDisk != nil:
			printGCEPersistentDiskVolumeSource(pv.Spec.GCEPersistentDisk, w)
		case pv.Spec.AWSElasticBlockStore != nil:
			printAWSElasticBlockStoreVolumeSource(pv.Spec.AWSElasticBlockStore, w)
		case pv.Spec.NFS != nil:
			printNFSVolumeSource(pv.Spec.NFS, w)
		case pv.Spec.ISCSI != nil:
			printISCSIPersistentVolumeSource(pv.Spec.ISCSI, w)
		case pv.Spec.Glusterfs != nil:
			printGlusterfsPersistentVolumeSource(pv.Spec.Glusterfs, w)
		case pv.Spec.RBD != nil:
			printRBDPersistentVolumeSource(pv.Spec.RBD, w)
		case pv.Spec.Quobyte != nil:
			printQuobyteVolumeSource(pv.Spec.Quobyte, w)
		case pv.Spec.VsphereVolume != nil:
			printVsphereVolumeSource(pv.Spec.VsphereVolume, w)
		case pv.Spec.Cinder != nil:
			printCinderPersistentVolumeSource(pv.Spec.Cinder, w)
		case pv.Spec.AzureDisk != nil:
			printAzureDiskVolumeSource(pv.Spec.AzureDisk, w)
		case pv.Spec.PhotonPersistentDisk != nil:
			printPhotonPersistentDiskVolumeSource(pv.Spec.PhotonPersistentDisk, w)
		case pv.Spec.PortworxVolume != nil:
			printPortworxVolumeSource(pv.Spec.PortworxVolume, w)
		case pv.Spec.ScaleIO != nil:
			printScaleIOPersistentVolumeSource(pv.Spec.ScaleIO, w)
		case pv.Spec.Local != nil:
			printLocalVolumeSource(pv.Spec.Local, w)
		case pv.Spec.CephFS != nil:
			printCephFSPersistentVolumeSource(pv.Spec.CephFS, w)
		case pv.Spec.StorageOS != nil:
			printStorageOSPersistentVolumeSource(pv.Spec.StorageOS, w)
		case pv.Spec.FC != nil:
			printFCVolumeSource(pv.Spec.FC, w)
		case pv.Spec.AzureFile != nil:
			printAzureFilePersistentVolumeSource(pv.Spec.AzureFile, w)
		case pv.Spec.FlexVolume != nil:
			printFlexPersistentVolumeSource(pv.Spec.FlexVolume, w)
		case pv.Spec.Flocker != nil:
			printFlockerVolumeSource(pv.Spec.Flocker, w)
		case pv.Spec.CSI != nil:
			printCSIPersistentVolumeSource(pv.Spec.CSI, w)
		default:
			w.Write(LEVEL_1, "<unknown>\n")
		}

		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

type PersistentVolumeClaimDescriber struct {
	clientset.Interface
}

func (d *PersistentVolumeClaimDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().PersistentVolumeClaims(namespace)

	pvc, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	pc := d.CoreV1().Pods(namespace)

	pods, err := getPodsForPVC(pc, pvc, describerSettings)
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), pvc, describerSettings.ChunkSize)
	}

	return describePersistentVolumeClaim(pvc, events, pods)
}

func getPodsForPVC(c corev1client.PodInterface, pvc *corev1.PersistentVolumeClaim, settings DescriberSettings) ([]corev1.Pod, error) {
	nsPods, err := getPodsInChunks(c, metav1.ListOptions{Limit: settings.ChunkSize})
	if err != nil {
		return []corev1.Pod{}, err
	}

	var pods []corev1.Pod

	for _, pod := range nsPods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.VolumeSource.PersistentVolumeClaim != nil && volume.VolumeSource.PersistentVolumeClaim.ClaimName == pvc.Name {
				pods = append(pods, pod)
			}
		}
	}

ownersLoop:
	for _, ownerRef := range pvc.ObjectMeta.OwnerReferences {
		if ownerRef.Kind != "Pod" {
			continue
		}

		podIndex := -1
		for i, pod := range nsPods.Items {
			if pod.UID == ownerRef.UID {
				podIndex = i
				break
			}
		}
		if podIndex == -1 {
			// Maybe the pod has been deleted
			continue
		}

		for _, pod := range pods {
			if pod.UID == nsPods.Items[podIndex].UID {
				// This owner pod is already recorded, look for pods between other owners
				continue ownersLoop
			}
		}

		pods = append(pods, nsPods.Items[podIndex])
	}

	return pods, nil
}

func describePersistentVolumeClaim(pvc *corev1.PersistentVolumeClaim, events *corev1.EventList, pods []corev1.Pod) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		printPersistentVolumeClaim(w, pvc, true)
		printPodsMultiline(w, "Used By", pods)

		if len(pvc.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n")
			w.Write(LEVEL_1, "Type\tStatus\tLastProbeTime\tLastTransitionTime\tReason\tMessage\n")
			w.Write(LEVEL_1, "----\t------\t-----------------\t------------------\t------\t-------\n")
			for _, c := range pvc.Status.Conditions {
				w.Write(LEVEL_1, "%v \t%v \t%s \t%s \t%v \t%v\n",
					c.Type,
					c.Status,
					c.LastProbeTime.Time.Format(time.RFC1123Z),
					c.LastTransitionTime.Time.Format(time.RFC1123Z),
					c.Reason,
					c.Message)
			}
		}
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

// printPersistentVolumeClaim is used for both PVCs and PersistentVolumeClaimTemplate. For the latter,
// we need to skip some fields which have no meaning.
func printPersistentVolumeClaim(w PrefixWriter, pvc *corev1.PersistentVolumeClaim, isFullPVC bool) {
	if isFullPVC {
		w.Write(LEVEL_0, "Name:\t%s\n", pvc.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", pvc.Namespace)
	}
	w.Write(LEVEL_0, "StorageClass:\t%s\n", storageutil.GetPersistentVolumeClaimClass(pvc))
	if isFullPVC {
		if pvc.ObjectMeta.DeletionTimestamp != nil {
			w.Write(LEVEL_0, "Status:\tTerminating (lasts %s)\n", translateTimestampSince(*pvc.ObjectMeta.DeletionTimestamp))
		} else {
			w.Write(LEVEL_0, "Status:\t%v\n", pvc.Status.Phase)
		}
	}
	w.Write(LEVEL_0, "Volume:\t%s\n", pvc.Spec.VolumeName)
	printLabelsMultiline(w, "Labels", pvc.Labels)
	printAnnotationsMultiline(w, "Annotations", pvc.Annotations)
	if isFullPVC {
		w.Write(LEVEL_0, "Finalizers:\t%v\n", pvc.ObjectMeta.Finalizers)
	}
	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	capacity := ""
	accessModes := ""
	if pvc.Spec.VolumeName != "" {
		accessModes = storageutil.GetAccessModesAsString(pvc.Status.AccessModes)
		storage = pvc.Status.Capacity[corev1.ResourceStorage]
		capacity = storage.String()
	}
	w.Write(LEVEL_0, "Capacity:\t%s\n", capacity)
	w.Write(LEVEL_0, "Access Modes:\t%s\n", accessModes)
	if pvc.Spec.VolumeMode != nil {
		w.Write(LEVEL_0, "VolumeMode:\t%v\n", *pvc.Spec.VolumeMode)
	}
	if pvc.Spec.DataSource != nil {
		w.Write(LEVEL_0, "DataSource:\n")
		if pvc.Spec.DataSource.APIGroup != nil {
			w.Write(LEVEL_1, "APIGroup:\t%v\n", *pvc.Spec.DataSource.APIGroup)
		}
		w.Write(LEVEL_1, "Kind:\t%v\n", pvc.Spec.DataSource.Kind)
		w.Write(LEVEL_1, "Name:\t%v\n", pvc.Spec.DataSource.Name)
	}
}

func describeContainers(label string, containers []corev1.Container, containerStatuses []corev1.ContainerStatus,
	resolverFn EnvVarResolverFunc, w PrefixWriter, space string) {
	statuses := map[string]corev1.ContainerStatus{}
	for _, status := range containerStatuses {
		statuses[status.Name] = status
	}

	describeContainersLabel(containers, label, space, w)

	for _, container := range containers {
		status, ok := statuses[container.Name]
		describeContainerBasicInfo(container, status, ok, space, w)
		describeContainerCommand(container, w)
		if ok {
			describeContainerState(status, w)
		}
		describeResources(&container.Resources, w, LEVEL_2)
		describeContainerProbe(container, w)
		if len(container.EnvFrom) > 0 {
			describeContainerEnvFrom(container, resolverFn, w)
		}
		describeContainerEnvVars(container, resolverFn, w)
		describeContainerVolumes(container, w)
	}
}

func describeContainersLabel(containers []corev1.Container, label, space string, w PrefixWriter) {
	none := ""
	if len(containers) == 0 {
		none = " <none>"
	}
	w.Write(LEVEL_0, "%s%s:%s\n", space, label, none)
}

func describeContainerBasicInfo(container corev1.Container, status corev1.ContainerStatus, ok bool, space string, w PrefixWriter) {
	nameIndent := ""
	if len(space) > 0 {
		nameIndent = " "
	}
	w.Write(LEVEL_1, "%s%v:\n", nameIndent, container.Name)
	if ok {
		w.Write(LEVEL_2, "Container ID:\t%s\n", status.ContainerID)
	}
	w.Write(LEVEL_2, "Image:\t%s\n", container.Image)
	if ok {
		w.Write(LEVEL_2, "Image ID:\t%s\n", status.ImageID)
	}
	portString := describeContainerPorts(container.Ports)
	if strings.Contains(portString, ",") {
		w.Write(LEVEL_2, "Ports:\t%s\n", portString)
	} else {
		w.Write(LEVEL_2, "Port:\t%s\n", stringOrNone(portString))
	}
	hostPortString := describeContainerHostPorts(container.Ports)
	if strings.Contains(hostPortString, ",") {
		w.Write(LEVEL_2, "Host Ports:\t%s\n", hostPortString)
	} else {
		w.Write(LEVEL_2, "Host Port:\t%s\n", stringOrNone(hostPortString))
	}
	if container.SecurityContext != nil && container.SecurityContext.SeccompProfile != nil {
		w.Write(LEVEL_2, "SeccompProfile:\t%s\n", container.SecurityContext.SeccompProfile.Type)
		if container.SecurityContext.SeccompProfile.Type == corev1.SeccompProfileTypeLocalhost {
			w.Write(LEVEL_3, "LocalhostProfile:\t%s\n", *container.SecurityContext.SeccompProfile.LocalhostProfile)
		}
	}
}

func describeContainerPorts(cPorts []corev1.ContainerPort) string {
	ports := make([]string, 0, len(cPorts))
	for _, cPort := range cPorts {
		ports = append(ports, fmt.Sprintf("%d/%s", cPort.ContainerPort, cPort.Protocol))
	}
	return strings.Join(ports, ", ")
}

func describeContainerHostPorts(cPorts []corev1.ContainerPort) string {
	ports := make([]string, 0, len(cPorts))
	for _, cPort := range cPorts {
		ports = append(ports, fmt.Sprintf("%d/%s", cPort.HostPort, cPort.Protocol))
	}
	return strings.Join(ports, ", ")
}

func describeContainerCommand(container corev1.Container, w PrefixWriter) {
	if len(container.Command) > 0 {
		w.Write(LEVEL_2, "Command:\n")
		for _, c := range container.Command {
			for _, s := range strings.Split(c, "\n") {
				w.Write(LEVEL_3, "%s\n", s)
			}
		}
	}
	if len(container.Args) > 0 {
		w.Write(LEVEL_2, "Args:\n")
		for _, arg := range container.Args {
			for _, s := range strings.Split(arg, "\n") {
				w.Write(LEVEL_3, "%s\n", s)
			}
		}
	}
}

func describeResources(resources *corev1.ResourceRequirements, w PrefixWriter, level int) {
	if resources == nil {
		return
	}

	if len(resources.Limits) > 0 {
		w.Write(level, "Limits:\n")
	}
	for _, name := range SortedResourceNames(resources.Limits) {
		quantity := resources.Limits[name]
		w.Write(level+1, "%s:\t%s\n", name, quantity.String())
	}

	if len(resources.Requests) > 0 {
		w.Write(level, "Requests:\n")
	}
	for _, name := range SortedResourceNames(resources.Requests) {
		quantity := resources.Requests[name]
		w.Write(level+1, "%s:\t%s\n", name, quantity.String())
	}
}

func describeContainerState(status corev1.ContainerStatus, w PrefixWriter) {
	describeStatus("State", status.State, w)
	if status.LastTerminationState.Terminated != nil {
		describeStatus("Last State", status.LastTerminationState, w)
	}
	w.Write(LEVEL_2, "Ready:\t%v\n", printBool(status.Ready))
	w.Write(LEVEL_2, "Restart Count:\t%d\n", status.RestartCount)
}

func describeContainerProbe(container corev1.Container, w PrefixWriter) {
	if container.LivenessProbe != nil {
		probe := DescribeProbe(container.LivenessProbe)
		w.Write(LEVEL_2, "Liveness:\t%s\n", probe)
	}
	if container.ReadinessProbe != nil {
		probe := DescribeProbe(container.ReadinessProbe)
		w.Write(LEVEL_2, "Readiness:\t%s\n", probe)
	}
	if container.StartupProbe != nil {
		probe := DescribeProbe(container.StartupProbe)
		w.Write(LEVEL_2, "Startup:\t%s\n", probe)
	}
}

func describeContainerVolumes(container corev1.Container, w PrefixWriter) {
	// Show volumeMounts
	none := ""
	if len(container.VolumeMounts) == 0 {
		none = "\t<none>"
	}
	w.Write(LEVEL_2, "Mounts:%s\n", none)
	sort.Sort(SortableVolumeMounts(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		flags := []string{}
		if mount.ReadOnly {
			flags = append(flags, "ro")
		} else {
			flags = append(flags, "rw")
		}
		if len(mount.SubPath) > 0 {
			flags = append(flags, fmt.Sprintf("path=%q", mount.SubPath))
		}
		w.Write(LEVEL_3, "%s from %s (%s)\n", mount.MountPath, mount.Name, strings.Join(flags, ","))
	}
	// Show volumeDevices if exists
	if len(container.VolumeDevices) > 0 {
		w.Write(LEVEL_2, "Devices:%s\n", none)
		sort.Sort(SortableVolumeDevices(container.VolumeDevices))
		for _, device := range container.VolumeDevices {
			w.Write(LEVEL_3, "%s from %s\n", device.DevicePath, device.Name)
		}
	}
}

func describeContainerEnvVars(container corev1.Container, resolverFn EnvVarResolverFunc, w PrefixWriter) {
	none := ""
	if len(container.Env) == 0 {
		none = "\t<none>"
	}
	w.Write(LEVEL_2, "Environment:%s\n", none)

	for _, e := range container.Env {
		if e.ValueFrom == nil {
			for i, s := range strings.Split(e.Value, "\n") {
				if i == 0 {
					w.Write(LEVEL_3, "%s:\t%s\n", e.Name, s)
				} else {
					w.Write(LEVEL_3, "\t%s\n", s)
				}
			}
			continue
		}

		switch {
		case e.ValueFrom.FieldRef != nil:
			var valueFrom string
			if resolverFn != nil {
				valueFrom = resolverFn(e)
			}
			w.Write(LEVEL_3, "%s:\t%s (%s:%s)\n", e.Name, valueFrom, e.ValueFrom.FieldRef.APIVersion, e.ValueFrom.FieldRef.FieldPath)
		case e.ValueFrom.ResourceFieldRef != nil:
			valueFrom, err := resourcehelper.ExtractContainerResourceValue(e.ValueFrom.ResourceFieldRef, &container)
			if err != nil {
				valueFrom = ""
			}
			resource := e.ValueFrom.ResourceFieldRef.Resource
			if valueFrom == "0" && (resource == "limits.cpu" || resource == "limits.memory") {
				valueFrom = "node allocatable"
			}
			w.Write(LEVEL_3, "%s:\t%s (%s)\n", e.Name, valueFrom, resource)
		case e.ValueFrom.SecretKeyRef != nil:
			optional := e.ValueFrom.SecretKeyRef.Optional != nil && *e.ValueFrom.SecretKeyRef.Optional
			w.Write(LEVEL_3, "%s:\t<set to the key '%s' in secret '%s'>\tOptional: %t\n", e.Name, e.ValueFrom.SecretKeyRef.Key, e.ValueFrom.SecretKeyRef.Name, optional)
		case e.ValueFrom.ConfigMapKeyRef != nil:
			optional := e.ValueFrom.ConfigMapKeyRef.Optional != nil && *e.ValueFrom.ConfigMapKeyRef.Optional
			w.Write(LEVEL_3, "%s:\t<set to the key '%s' of config map '%s'>\tOptional: %t\n", e.Name, e.ValueFrom.ConfigMapKeyRef.Key, e.ValueFrom.ConfigMapKeyRef.Name, optional)
		}
	}
}

func describeContainerEnvFrom(container corev1.Container, resolverFn EnvVarResolverFunc, w PrefixWriter) {
	none := ""
	if len(container.EnvFrom) == 0 {
		none = "\t<none>"
	}
	w.Write(LEVEL_2, "Environment Variables from:%s\n", none)

	for _, e := range container.EnvFrom {
		from := ""
		name := ""
		optional := false
		if e.ConfigMapRef != nil {
			from = "ConfigMap"
			name = e.ConfigMapRef.Name
			optional = e.ConfigMapRef.Optional != nil && *e.ConfigMapRef.Optional
		} else if e.SecretRef != nil {
			from = "Secret"
			name = e.SecretRef.Name
			optional = e.SecretRef.Optional != nil && *e.SecretRef.Optional
		}
		if len(e.Prefix) == 0 {
			w.Write(LEVEL_3, "%s\t%s\tOptional: %t\n", name, from, optional)
		} else {
			w.Write(LEVEL_3, "%s\t%s with prefix '%s'\tOptional: %t\n", name, from, e.Prefix, optional)
		}
	}
}

// DescribeProbe is exported for consumers in other API groups that have probes
func DescribeProbe(probe *corev1.Probe) string {
	attrs := fmt.Sprintf("delay=%ds timeout=%ds period=%ds #success=%d #failure=%d", probe.InitialDelaySeconds, probe.TimeoutSeconds, probe.PeriodSeconds, probe.SuccessThreshold, probe.FailureThreshold)
	switch {
	case probe.Exec != nil:
		return fmt.Sprintf("exec %v %s", probe.Exec.Command, attrs)
	case probe.HTTPGet != nil:
		url := &url.URL{}
		url.Scheme = strings.ToLower(string(probe.HTTPGet.Scheme))
		if len(probe.HTTPGet.Port.String()) > 0 {
			url.Host = net.JoinHostPort(probe.HTTPGet.Host, probe.HTTPGet.Port.String())
		} else {
			url.Host = probe.HTTPGet.Host
		}
		url.Path = probe.HTTPGet.Path
		return fmt.Sprintf("http-get %s %s", url.String(), attrs)
	case probe.TCPSocket != nil:
		return fmt.Sprintf("tcp-socket %s:%s %s", probe.TCPSocket.Host, probe.TCPSocket.Port.String(), attrs)

	case probe.GRPC != nil:
		return fmt.Sprintf("grpc <pod>:%d %s %s", probe.GRPC.Port, *(probe.GRPC.Service), attrs)
	}
	return fmt.Sprintf("unknown %s", attrs)
}

type EnvVarResolverFunc func(e corev1.EnvVar) string

// EnvValueFrom is exported for use by describers in other packages
func EnvValueRetriever(pod *corev1.Pod) EnvVarResolverFunc {
	return func(e corev1.EnvVar) string {
		gv, err := schema.ParseGroupVersion(e.ValueFrom.FieldRef.APIVersion)
		if err != nil {
			return ""
		}
		gvk := gv.WithKind("Pod")
		internalFieldPath, _, err := scheme.Scheme.ConvertFieldLabel(gvk, e.ValueFrom.FieldRef.FieldPath, "")
		if err != nil {
			return "" // pod validation should catch this on create
		}

		valueFrom, err := fieldpath.ExtractFieldPathAsString(pod, internalFieldPath)
		if err != nil {
			return "" // pod validation should catch this on create
		}

		return valueFrom
	}
}

func describeStatus(stateName string, state corev1.ContainerState, w PrefixWriter) {
	switch {
	case state.Running != nil:
		w.Write(LEVEL_2, "%s:\tRunning\n", stateName)
		w.Write(LEVEL_3, "Started:\t%v\n", state.Running.StartedAt.Time.Format(time.RFC1123Z))
	case state.Waiting != nil:
		w.Write(LEVEL_2, "%s:\tWaiting\n", stateName)
		if state.Waiting.Reason != "" {
			w.Write(LEVEL_3, "Reason:\t%s\n", state.Waiting.Reason)
		}
	case state.Terminated != nil:
		w.Write(LEVEL_2, "%s:\tTerminated\n", stateName)
		if state.Terminated.Reason != "" {
			w.Write(LEVEL_3, "Reason:\t%s\n", state.Terminated.Reason)
		}
		if state.Terminated.Message != "" {
			w.Write(LEVEL_3, "Message:\t%s\n", state.Terminated.Message)
		}
		w.Write(LEVEL_3, "Exit Code:\t%d\n", state.Terminated.ExitCode)
		if state.Terminated.Signal > 0 {
			w.Write(LEVEL_3, "Signal:\t%d\n", state.Terminated.Signal)
		}
		w.Write(LEVEL_3, "Started:\t%s\n", state.Terminated.StartedAt.Time.Format(time.RFC1123Z))
		w.Write(LEVEL_3, "Finished:\t%s\n", state.Terminated.FinishedAt.Time.Format(time.RFC1123Z))
	default:
		w.Write(LEVEL_2, "%s:\tWaiting\n", stateName)
	}
}

func describeVolumeClaimTemplates(templates []corev1.PersistentVolumeClaim, w PrefixWriter) {
	if len(templates) == 0 {
		w.Write(LEVEL_0, "Volume Claims:\t<none>\n")
		return
	}
	w.Write(LEVEL_0, "Volume Claims:\n")
	for _, pvc := range templates {
		w.Write(LEVEL_1, "Name:\t%s\n", pvc.Name)
		w.Write(LEVEL_1, "StorageClass:\t%s\n", storageutil.GetPersistentVolumeClaimClass(&pvc))
		printLabelsMultilineWithIndent(w, "  ", "Labels", "\t", pvc.Labels, sets.New[string]())
		printLabelsMultilineWithIndent(w, "  ", "Annotations", "\t", pvc.Annotations, sets.New[string]())
		if capacity, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			w.Write(LEVEL_1, "Capacity:\t%s\n", capacity.String())
		} else {
			w.Write(LEVEL_1, "Capacity:\t%s\n", "<default>")
		}
		w.Write(LEVEL_1, "Access Modes:\t%s\n", pvc.Spec.AccessModes)
	}
}

func printBoolPtr(value *bool) string {
	if value != nil {
		return printBool(*value)
	}

	return "<unset>"
}

func printBool(value bool) string {
	if value {
		return "True"
	}

	return "False"
}

// ReplicationControllerDescriber generates information about a replication controller
// and the pods it has created.
type ReplicationControllerDescriber struct {
	clientset.Interface
}

func (d *ReplicationControllerDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	rc := d.CoreV1().ReplicationControllers(namespace)
	pc := d.CoreV1().Pods(namespace)

	controller, err := rc.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	selector := labels.SelectorFromSet(controller.Spec.Selector)
	running, waiting, succeeded, failed, err := getPodStatusForController(pc, selector, controller.UID, describerSettings)
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), controller, describerSettings.ChunkSize)
	}

	return describeReplicationController(controller, events, running, waiting, succeeded, failed)
}

func describeReplicationController(controller *corev1.ReplicationController, events *corev1.EventList, running, waiting, succeeded, failed int) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", controller.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", controller.Namespace)
		w.Write(LEVEL_0, "Selector:\t%s\n", labels.FormatLabels(controller.Spec.Selector))
		printLabelsMultiline(w, "Labels", controller.Labels)
		printAnnotationsMultiline(w, "Annotations", controller.Annotations)
		w.Write(LEVEL_0, "Replicas:\t%d current / %d desired\n", controller.Status.Replicas, *controller.Spec.Replicas)
		w.Write(LEVEL_0, "Pods Status:\t%d Running / %d Waiting / %d Succeeded / %d Failed\n", running, waiting, succeeded, failed)
		DescribePodTemplate(controller.Spec.Template, w)
		if len(controller.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n  Type\tStatus\tReason\n")
			w.Write(LEVEL_1, "----\t------\t------\n")
			for _, c := range controller.Status.Conditions {
				w.Write(LEVEL_1, "%v \t%v\t%v\n", c.Type, c.Status, c.Reason)
			}
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func DescribePodTemplate(template *corev1.PodTemplateSpec, w PrefixWriter) {
	w.Write(LEVEL_0, "Pod Template:\n")
	if template == nil {
		w.Write(LEVEL_1, "<unset>")
		return
	}
	printLabelsMultiline(w, "  Labels", template.Labels)
	if len(template.Annotations) > 0 {
		printAnnotationsMultiline(w, "  Annotations", template.Annotations)
	}
	if len(template.Spec.ServiceAccountName) > 0 {
		w.Write(LEVEL_1, "Service Account:\t%s\n", template.Spec.ServiceAccountName)
	}
	if len(template.Spec.InitContainers) > 0 {
		describeContainers("Init Containers", template.Spec.InitContainers, nil, nil, w, "  ")
	}
	describeContainers("Containers", template.Spec.Containers, nil, nil, w, "  ")
	describeVolumes(template.Spec.Volumes, w, "  ")
	describeTopologySpreadConstraints(template.Spec.TopologySpreadConstraints, w, "  ")
	if len(template.Spec.PriorityClassName) > 0 {
		w.Write(LEVEL_1, "Priority Class Name:\t%s\n", template.Spec.PriorityClassName)
	}
	printLabelsMultiline(w, "  Node-Selectors", template.Spec.NodeSelector)
	printPodTolerationsMultiline(w, "  Tolerations", template.Spec.Tolerations)
}

// ReplicaSetDescriber generates information about a ReplicaSet and the pods it has created.
type ReplicaSetDescriber struct {
	clientset.Interface
}

func (d *ReplicaSetDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	rsc := d.AppsV1().ReplicaSets(namespace)
	pc := d.CoreV1().Pods(namespace)

	rs, err := rsc.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
	if err != nil {
		return "", err
	}

	running, waiting, succeeded, failed, getPodErr := getPodStatusForController(pc, selector, rs.UID, describerSettings)

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), rs, describerSettings.ChunkSize)
	}

	return describeReplicaSet(rs, events, running, waiting, succeeded, failed, getPodErr)
}

func describeReplicaSet(rs *appsv1.ReplicaSet, events *corev1.EventList, running, waiting, succeeded, failed int, getPodErr error) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", rs.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", rs.Namespace)
		w.Write(LEVEL_0, "Selector:\t%s\n", metav1.FormatLabelSelector(rs.Spec.Selector))
		printLabelsMultiline(w, "Labels", rs.Labels)
		printAnnotationsMultiline(w, "Annotations", rs.Annotations)
		if controlledBy := printController(rs); len(controlledBy) > 0 {
			w.Write(LEVEL_0, "Controlled By:\t%s\n", controlledBy)
		}
		w.Write(LEVEL_0, "Replicas:\t%d current / %d desired\n", rs.Status.Replicas, *rs.Spec.Replicas)
		w.Write(LEVEL_0, "Pods Status:\t")
		if getPodErr != nil {
			w.Write(LEVEL_0, "error in fetching pods: %s\n", getPodErr)
		} else {
			w.Write(LEVEL_0, "%d Running / %d Waiting / %d Succeeded / %d Failed\n", running, waiting, succeeded, failed)
		}
		DescribePodTemplate(&rs.Spec.Template, w)
		if len(rs.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n  Type\tStatus\tReason\n")
			w.Write(LEVEL_1, "----\t------\t------\n")
			for _, c := range rs.Status.Conditions {
				w.Write(LEVEL_1, "%v \t%v\t%v\n", c.Type, c.Status, c.Reason)
			}
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// JobDescriber generates information about a job and the pods it has created.
type JobDescriber struct {
	clientset.Interface
}

func (d *JobDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	job, err := d.BatchV1().Jobs(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), job, describerSettings.ChunkSize)
	}

	return describeJob(job, events)
}

func describeJob(job *batchv1.Job, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", job.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", job.Namespace)
		if selector, err := metav1.LabelSelectorAsSelector(job.Spec.Selector); err == nil {
			w.Write(LEVEL_0, "Selector:\t%s\n", selector)
		} else {
			w.Write(LEVEL_0, "Selector:\tFailed to get selector: %s\n", err)
		}
		printLabelsMultiline(w, "Labels", job.Labels)
		printAnnotationsMultiline(w, "Annotations", job.Annotations)
		if controlledBy := printController(job); len(controlledBy) > 0 {
			w.Write(LEVEL_0, "Controlled By:\t%s\n", controlledBy)
		}
		if job.Spec.Parallelism != nil {
			w.Write(LEVEL_0, "Parallelism:\t%d\n", *job.Spec.Parallelism)
		}
		if job.Spec.Completions != nil {
			w.Write(LEVEL_0, "Completions:\t%d\n", *job.Spec.Completions)
		} else {
			w.Write(LEVEL_0, "Completions:\t<unset>\n")
		}
		if job.Spec.CompletionMode != nil {
			w.Write(LEVEL_0, "Completion Mode:\t%s\n", *job.Spec.CompletionMode)
		}
		if job.Spec.Suspend != nil {
			w.Write(LEVEL_0, "Suspend:\t%v\n", *job.Spec.Suspend)
		}
		if job.Spec.BackoffLimit != nil {
			w.Write(LEVEL_0, "Backoff Limit:\t%v\n", *job.Spec.BackoffLimit)
		}
		if job.Spec.TTLSecondsAfterFinished != nil {
			w.Write(LEVEL_0, "TTL Seconds After Finished:\t%v\n", *job.Spec.TTLSecondsAfterFinished)
		}
		if job.Status.StartTime != nil {
			w.Write(LEVEL_0, "Start Time:\t%s\n", job.Status.StartTime.Time.Format(time.RFC1123Z))
		}
		if job.Status.CompletionTime != nil {
			w.Write(LEVEL_0, "Completed At:\t%s\n", job.Status.CompletionTime.Time.Format(time.RFC1123Z))
		}
		if job.Status.StartTime != nil && job.Status.CompletionTime != nil {
			w.Write(LEVEL_0, "Duration:\t%s\n", duration.HumanDuration(job.Status.CompletionTime.Sub(job.Status.StartTime.Time)))
		}
		if job.Spec.ActiveDeadlineSeconds != nil {
			w.Write(LEVEL_0, "Active Deadline Seconds:\t%ds\n", *job.Spec.ActiveDeadlineSeconds)
		}
		if job.Status.Ready == nil {
			w.Write(LEVEL_0, "Pods Statuses:\t%d Active / %d Succeeded / %d Failed\n", job.Status.Active, job.Status.Succeeded, job.Status.Failed)
		} else {
			w.Write(LEVEL_0, "Pods Statuses:\t%d Active (%d Ready) / %d Succeeded / %d Failed\n", job.Status.Active, *job.Status.Ready, job.Status.Succeeded, job.Status.Failed)
		}
		if job.Spec.CompletionMode != nil && *job.Spec.CompletionMode == batchv1.IndexedCompletion {
			w.Write(LEVEL_0, "Completed Indexes:\t%s\n", capIndexesListOrNone(job.Status.CompletedIndexes, 50))
		}
		DescribePodTemplate(&job.Spec.Template, w)
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func capIndexesListOrNone(indexes string, softLimit int) string {
	if len(indexes) == 0 {
		return "<none>"
	}
	ix := softLimit
	for ; ix < len(indexes); ix++ {
		if indexes[ix] == ',' {
			break
		}
	}
	if ix >= len(indexes) {
		return indexes
	}
	return indexes[:ix+1] + "..."
}

// CronJobDescriber generates information about a cron job and the jobs it has created.
type CronJobDescriber struct {
	client clientset.Interface
}

func (d *CronJobDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList

	cronJob, err := d.client.BatchV1().CronJobs(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.client.CoreV1(), cronJob, describerSettings.ChunkSize)
	}
	return describeCronJob(cronJob, events)
}

func describeCronJob(cronJob *batchv1.CronJob, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", cronJob.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", cronJob.Namespace)
		printLabelsMultiline(w, "Labels", cronJob.Labels)
		printAnnotationsMultiline(w, "Annotations", cronJob.Annotations)
		w.Write(LEVEL_0, "Schedule:\t%s\n", cronJob.Spec.Schedule)
		w.Write(LEVEL_0, "Concurrency Policy:\t%s\n", cronJob.Spec.ConcurrencyPolicy)
		w.Write(LEVEL_0, "Suspend:\t%s\n", printBoolPtr(cronJob.Spec.Suspend))
		if cronJob.Spec.SuccessfulJobsHistoryLimit != nil {
			w.Write(LEVEL_0, "Successful Job History Limit:\t%d\n", *cronJob.Spec.SuccessfulJobsHistoryLimit)
		} else {
			w.Write(LEVEL_0, "Successful Job History Limit:\t<unset>\n")
		}
		if cronJob.Spec.FailedJobsHistoryLimit != nil {
			w.Write(LEVEL_0, "Failed Job History Limit:\t%d\n", *cronJob.Spec.FailedJobsHistoryLimit)
		} else {
			w.Write(LEVEL_0, "Failed Job History Limit:\t<unset>\n")
		}
		if cronJob.Spec.StartingDeadlineSeconds != nil {
			w.Write(LEVEL_0, "Starting Deadline Seconds:\t%ds\n", *cronJob.Spec.StartingDeadlineSeconds)
		} else {
			w.Write(LEVEL_0, "Starting Deadline Seconds:\t<unset>\n")
		}
		describeJobTemplate(cronJob.Spec.JobTemplate, w)
		if cronJob.Status.LastScheduleTime != nil {
			w.Write(LEVEL_0, "Last Schedule Time:\t%s\n", cronJob.Status.LastScheduleTime.Time.Format(time.RFC1123Z))
		} else {
			w.Write(LEVEL_0, "Last Schedule Time:\t<unset>\n")
		}
		printActiveJobs(w, "Active Jobs", cronJob.Status.Active)
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func describeJobTemplate(jobTemplate batchv1.JobTemplateSpec, w PrefixWriter) {
	if jobTemplate.Spec.Selector != nil {
		if selector, err := metav1.LabelSelectorAsSelector(jobTemplate.Spec.Selector); err == nil {
			w.Write(LEVEL_0, "Selector:\t%s\n", selector)
		} else {
			w.Write(LEVEL_0, "Selector:\tFailed to get selector: %s\n", err)
		}
	} else {
		w.Write(LEVEL_0, "Selector:\t<unset>\n")
	}
	if jobTemplate.Spec.Parallelism != nil {
		w.Write(LEVEL_0, "Parallelism:\t%d\n", *jobTemplate.Spec.Parallelism)
	} else {
		w.Write(LEVEL_0, "Parallelism:\t<unset>\n")
	}
	if jobTemplate.Spec.Completions != nil {
		w.Write(LEVEL_0, "Completions:\t%d\n", *jobTemplate.Spec.Completions)
	} else {
		w.Write(LEVEL_0, "Completions:\t<unset>\n")
	}
	if jobTemplate.Spec.ActiveDeadlineSeconds != nil {
		w.Write(LEVEL_0, "Active Deadline Seconds:\t%ds\n", *jobTemplate.Spec.ActiveDeadlineSeconds)
	}
	DescribePodTemplate(&jobTemplate.Spec.Template, w)
}

func printActiveJobs(w PrefixWriter, title string, jobs []corev1.ObjectReference) {
	w.Write(LEVEL_0, "%s:\t", title)
	if len(jobs) == 0 {
		w.WriteLine("<none>")
		return
	}

	for i, job := range jobs {
		if i != 0 {
			w.Write(LEVEL_0, ", ")
		}
		w.Write(LEVEL_0, "%s", job.Name)
	}
	w.WriteLine("")
}

// DaemonSetDescriber generates information about a daemon set and the pods it has created.
type DaemonSetDescriber struct {
	clientset.Interface
}

func (d *DaemonSetDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	dc := d.AppsV1().DaemonSets(namespace)
	pc := d.CoreV1().Pods(namespace)

	daemon, err := dc.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	selector, err := metav1.LabelSelectorAsSelector(daemon.Spec.Selector)
	if err != nil {
		return "", err
	}
	running, waiting, succeeded, failed, err := getPodStatusForController(pc, selector, daemon.UID, describerSettings)
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), daemon, describerSettings.ChunkSize)
	}

	return describeDaemonSet(daemon, events, running, waiting, succeeded, failed)
}

func describeDaemonSet(daemon *appsv1.DaemonSet, events *corev1.EventList, running, waiting, succeeded, failed int) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", daemon.Name)
		selector, err := metav1.LabelSelectorAsSelector(daemon.Spec.Selector)
		if err != nil {
			// this shouldn't happen if LabelSelector passed validation
			return err
		}
		w.Write(LEVEL_0, "Selector:\t%s\n", selector)
		w.Write(LEVEL_0, "Node-Selector:\t%s\n", labels.FormatLabels(daemon.Spec.Template.Spec.NodeSelector))
		printLabelsMultiline(w, "Labels", daemon.Labels)
		printAnnotationsMultiline(w, "Annotations", daemon.Annotations)
		w.Write(LEVEL_0, "Desired Number of Nodes Scheduled: %d\n", daemon.Status.DesiredNumberScheduled)
		w.Write(LEVEL_0, "Current Number of Nodes Scheduled: %d\n", daemon.Status.CurrentNumberScheduled)
		w.Write(LEVEL_0, "Number of Nodes Scheduled with Up-to-date Pods: %d\n", daemon.Status.UpdatedNumberScheduled)
		w.Write(LEVEL_0, "Number of Nodes Scheduled with Available Pods: %d\n", daemon.Status.NumberAvailable)
		w.Write(LEVEL_0, "Number of Nodes Misscheduled: %d\n", daemon.Status.NumberMisscheduled)
		w.Write(LEVEL_0, "Pods Status:\t%d Running / %d Waiting / %d Succeeded / %d Failed\n", running, waiting, succeeded, failed)
		DescribePodTemplate(&daemon.Spec.Template, w)
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// SecretDescriber generates information about a secret
type SecretDescriber struct {
	clientset.Interface
}

func (d *SecretDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().Secrets(namespace)

	secret, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return describeSecret(secret)
}

func describeSecret(secret *corev1.Secret) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", secret.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", secret.Namespace)
		printLabelsMultiline(w, "Labels", secret.Labels)
		printAnnotationsMultiline(w, "Annotations", secret.Annotations)

		w.Write(LEVEL_0, "\nType:\t%s\n", secret.Type)

		w.Write(LEVEL_0, "\nData\n====\n")
		for k, v := range secret.Data {
			switch {
			case k == corev1.ServiceAccountTokenKey && secret.Type == corev1.SecretTypeServiceAccountToken:
				w.Write(LEVEL_0, "%s:\t%s\n", k, string(v))
			default:
				w.Write(LEVEL_0, "%s:\t%d bytes\n", k, len(v))
			}
		}

		return nil
	})
}

type IngressDescriber struct {
	client clientset.Interface
}

func (i *IngressDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList

	// try ingress/v1 first (v1.19) and fallback to ingress/v1beta if an err occurs
	netV1, err := i.client.NetworkingV1().Ingresses(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(i.client.CoreV1(), netV1, describerSettings.ChunkSize)
		}
		return i.describeIngressV1(netV1, events)
	}
	netV1beta1, err := i.client.NetworkingV1beta1().Ingresses(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(i.client.CoreV1(), netV1beta1, describerSettings.ChunkSize)
		}
		return i.describeIngressV1beta1(netV1beta1, events)
	}
	return "", err
}

func (i *IngressDescriber) describeBackendV1beta1(ns string, backend *networkingv1beta1.IngressBackend) string {
	endpointSliceList, err := i.client.DiscoveryV1().EndpointSlices(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", discoveryv1.LabelServiceName, backend.ServiceName),
	})
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	service, err := i.client.CoreV1().Services(ns).Get(context.TODO(), backend.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	spName := ""
	for i := range service.Spec.Ports {
		sp := &service.Spec.Ports[i]
		switch backend.ServicePort.Type {
		case intstr.String:
			if backend.ServicePort.StrVal == sp.Name {
				spName = sp.Name
			}
		case intstr.Int:
			if int32(backend.ServicePort.IntVal) == sp.Port {
				spName = sp.Name
			}
		}
	}
	return formatEndpointSlices(endpointSliceList.Items, sets.New(spName))
}

func (i *IngressDescriber) describeBackendV1(ns string, backend *networkingv1.IngressBackend) string {

	if backend.Service != nil {
		sb := serviceBackendStringer(backend.Service)
		endpointSliceList, err := i.client.DiscoveryV1().EndpointSlices(ns).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", discoveryv1.LabelServiceName, backend.Service.Name),
		})
		if err != nil {
			return fmt.Sprintf("%v (<error: %v>)", sb, err)
		}
		service, err := i.client.CoreV1().Services(ns).Get(context.TODO(), backend.Service.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Sprintf("%v (<error: %v>)", sb, err)
		}
		spName := ""
		for i := range service.Spec.Ports {
			sp := &service.Spec.Ports[i]
			if backend.Service.Port.Number != 0 && backend.Service.Port.Number == sp.Port {
				spName = sp.Name
			} else if len(backend.Service.Port.Name) > 0 && backend.Service.Port.Name == sp.Name {
				spName = sp.Name
			}
		}
		ep := formatEndpointSlices(endpointSliceList.Items, sets.New(spName))
		return fmt.Sprintf("%s (%s)", sb, ep)
	}
	if backend.Resource != nil {
		ic := backend.Resource
		apiGroup := "<none>"
		if ic.APIGroup != nil {
			apiGroup = fmt.Sprintf("%v", *ic.APIGroup)
		}
		return fmt.Sprintf("APIGroup: %v, Kind: %v, Name: %v", apiGroup, ic.Kind, ic.Name)
	}
	return ""
}

func (i *IngressDescriber) describeIngressV1(ing *networkingv1.Ingress, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%v\n", ing.Name)
		printLabelsMultiline(w, "Labels", ing.Labels)
		w.Write(LEVEL_0, "Namespace:\t%v\n", ing.Namespace)
		w.Write(LEVEL_0, "Address:\t%v\n", ingressLoadBalancerStatusStringerV1(ing.Status.LoadBalancer, true))
		ingressClassName := "<none>"
		if ing.Spec.IngressClassName != nil {
			ingressClassName = *ing.Spec.IngressClassName
		}
		w.Write(LEVEL_0, "Ingress Class:\t%v\n", ingressClassName)
		def := ing.Spec.DefaultBackend
		ns := ing.Namespace
		defaultBackendDescribe := "<default>"
		if def != nil {
			defaultBackendDescribe = i.describeBackendV1(ns, def)
		}
		w.Write(LEVEL_0, "Default backend:\t%s\n", defaultBackendDescribe)
		if len(ing.Spec.TLS) != 0 {
			describeIngressTLSV1(w, ing.Spec.TLS)
		}
		w.Write(LEVEL_0, "Rules:\n  Host\tPath\tBackends\n")
		w.Write(LEVEL_1, "----\t----\t--------\n")
		count := 0
		for _, rules := range ing.Spec.Rules {

			if rules.HTTP == nil {
				continue
			}
			count++
			host := rules.Host
			if len(host) == 0 {
				host = "*"
			}
			w.Write(LEVEL_1, "%s\t\n", host)
			for _, path := range rules.HTTP.Paths {
				w.Write(LEVEL_2, "\t%s \t%s\n", path.Path, i.describeBackendV1(ing.Namespace, &path.Backend))
			}
		}
		if count == 0 {
			w.Write(LEVEL_1, "%s\t%s\t%s\n", "*", "*", defaultBackendDescribe)
		}
		printAnnotationsMultiline(w, "Annotations", ing.Annotations)

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func (i *IngressDescriber) describeIngressV1beta1(ing *networkingv1beta1.Ingress, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%v\n", ing.Name)
		printLabelsMultiline(w, "Labels", ing.Labels)
		w.Write(LEVEL_0, "Namespace:\t%v\n", ing.Namespace)
		w.Write(LEVEL_0, "Address:\t%v\n", ingressLoadBalancerStatusStringerV1beta1(ing.Status.LoadBalancer, true))
		ingressClassName := "<none>"
		if ing.Spec.IngressClassName != nil {
			ingressClassName = *ing.Spec.IngressClassName
		}
		w.Write(LEVEL_0, "Ingress Class:\t%v\n", ingressClassName)
		def := ing.Spec.Backend
		ns := ing.Namespace
		if def == nil {
			w.Write(LEVEL_0, "Default backend:\t<default>\n")
		} else {
			w.Write(LEVEL_0, "Default backend:\t%s\n", i.describeBackendV1beta1(ns, def))
		}
		if len(ing.Spec.TLS) != 0 {
			describeIngressTLSV1beta1(w, ing.Spec.TLS)
		}
		w.Write(LEVEL_0, "Rules:\n  Host\tPath\tBackends\n")
		w.Write(LEVEL_1, "----\t----\t--------\n")
		count := 0
		for _, rules := range ing.Spec.Rules {

			if rules.HTTP == nil {
				continue
			}
			count++
			host := rules.Host
			if len(host) == 0 {
				host = "*"
			}
			w.Write(LEVEL_1, "%s\t\n", host)
			for _, path := range rules.HTTP.Paths {
				w.Write(LEVEL_2, "\t%s \t%s (%s)\n", path.Path, backendStringer(&path.Backend), i.describeBackendV1beta1(ing.Namespace, &path.Backend))
			}
		}
		if count == 0 {
			w.Write(LEVEL_1, "%s\t%s \t<default>\n", "*", "*")
		}
		printAnnotationsMultiline(w, "Annotations", ing.Annotations)

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func describeIngressTLSV1beta1(w PrefixWriter, ingTLS []networkingv1beta1.IngressTLS) {
	w.Write(LEVEL_0, "TLS:\n")
	for _, t := range ingTLS {
		if t.SecretName == "" {
			w.Write(LEVEL_1, "SNI routes %v\n", strings.Join(t.Hosts, ","))
		} else {
			w.Write(LEVEL_1, "%v terminates %v\n", t.SecretName, strings.Join(t.Hosts, ","))
		}
	}
}

func describeIngressTLSV1(w PrefixWriter, ingTLS []networkingv1.IngressTLS) {
	w.Write(LEVEL_0, "TLS:\n")
	for _, t := range ingTLS {
		if t.SecretName == "" {
			w.Write(LEVEL_1, "SNI routes %v\n", strings.Join(t.Hosts, ","))
		} else {
			w.Write(LEVEL_1, "%v terminates %v\n", t.SecretName, strings.Join(t.Hosts, ","))
		}
	}
}

type IngressClassDescriber struct {
	client clientset.Interface
}

func (i *IngressClassDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList
	// try IngressClass/v1 first (v1.19) and fallback to IngressClass/v1beta if an err occurs
	netV1, err := i.client.NetworkingV1().IngressClasses().Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(i.client.CoreV1(), netV1, describerSettings.ChunkSize)
		}
		return i.describeIngressClassV1(netV1, events)
	}
	netV1beta1, err := i.client.NetworkingV1beta1().IngressClasses().Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(i.client.CoreV1(), netV1beta1, describerSettings.ChunkSize)
		}
		return i.describeIngressClassV1beta1(netV1beta1, events)
	}
	return "", err
}

func (i *IngressClassDescriber) describeIngressClassV1beta1(ic *networkingv1beta1.IngressClass, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", ic.Name)
		printLabelsMultiline(w, "Labels", ic.Labels)
		printAnnotationsMultiline(w, "Annotations", ic.Annotations)
		w.Write(LEVEL_0, "Controller:\t%v\n", ic.Spec.Controller)

		if ic.Spec.Parameters != nil {
			w.Write(LEVEL_0, "Parameters:\n")
			if ic.Spec.Parameters.APIGroup != nil {
				w.Write(LEVEL_1, "APIGroup:\t%v\n", *ic.Spec.Parameters.APIGroup)
			}
			w.Write(LEVEL_1, "Kind:\t%v\n", ic.Spec.Parameters.Kind)
			w.Write(LEVEL_1, "Name:\t%v\n", ic.Spec.Parameters.Name)
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func (i *IngressClassDescriber) describeIngressClassV1(ic *networkingv1.IngressClass, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", ic.Name)
		printLabelsMultiline(w, "Labels", ic.Labels)
		printAnnotationsMultiline(w, "Annotations", ic.Annotations)
		w.Write(LEVEL_0, "Controller:\t%v\n", ic.Spec.Controller)

		if ic.Spec.Parameters != nil {
			w.Write(LEVEL_0, "Parameters:\n")
			if ic.Spec.Parameters.APIGroup != nil {
				w.Write(LEVEL_1, "APIGroup:\t%v\n", *ic.Spec.Parameters.APIGroup)
			}
			w.Write(LEVEL_1, "Kind:\t%v\n", ic.Spec.Parameters.Kind)
			w.Write(LEVEL_1, "Name:\t%v\n", ic.Spec.Parameters.Name)
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// ServiceCIDRDescriber generates information about a ServiceCIDR.
type ServiceCIDRDescriber struct {
	client clientset.Interface
}

func (c *ServiceCIDRDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList

	svcV1, err := c.client.NetworkingV1().ServiceCIDRs().Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(c.client.CoreV1(), svcV1, describerSettings.ChunkSize)
		}
		return c.describeServiceCIDRV1(svcV1, events)
	}

	svcV1beta1, err := c.client.NetworkingV1beta1().ServiceCIDRs().Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(c.client.CoreV1(), svcV1beta1, describerSettings.ChunkSize)
		}
		return c.describeServiceCIDRV1beta1(svcV1beta1, events)
	}
	return "", err
}

func (c *ServiceCIDRDescriber) describeServiceCIDRV1(svc *networkingv1.ServiceCIDR, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%v\n", svc.Name)
		printLabelsMultiline(w, "Labels", svc.Labels)
		printAnnotationsMultiline(w, "Annotations", svc.Annotations)

		w.Write(LEVEL_0, "CIDRs:\t%v\n", strings.Join(svc.Spec.CIDRs, ", "))

		if len(svc.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Status:\n")
			w.Write(LEVEL_0, "Conditions:\n")
			w.Write(LEVEL_1, "Type\tStatus\tLastTransitionTime\tReason\tMessage\n")
			w.Write(LEVEL_1, "----\t------\t------------------\t------\t-------\n")
			for _, c := range svc.Status.Conditions {
				w.Write(LEVEL_1, "%v\t%v\t%s\t%v\t%v\n",
					c.Type,
					c.Status,
					c.LastTransitionTime.Time.Format(time.RFC1123Z),
					c.Reason,
					c.Message)
			}
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func (c *ServiceCIDRDescriber) describeServiceCIDRV1beta1(svc *networkingv1beta1.ServiceCIDR, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%v\n", svc.Name)
		printLabelsMultiline(w, "Labels", svc.Labels)
		printAnnotationsMultiline(w, "Annotations", svc.Annotations)

		w.Write(LEVEL_0, "CIDRs:\t%v\n", strings.Join(svc.Spec.CIDRs, ", "))

		if len(svc.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Status:\n")
			w.Write(LEVEL_0, "Conditions:\n")
			w.Write(LEVEL_1, "Type\tStatus\tLastTransitionTime\tReason\tMessage\n")
			w.Write(LEVEL_1, "----\t------\t------------------\t------\t-------\n")
			for _, c := range svc.Status.Conditions {
				w.Write(LEVEL_1, "%v\t%v\t%s\t%v\t%v\n",
					c.Type,
					c.Status,
					c.LastTransitionTime.Time.Format(time.RFC1123Z),
					c.Reason,
					c.Message)
			}
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// IPAddressDescriber generates information about an IPAddress.
type IPAddressDescriber struct {
	client clientset.Interface
}

func (c *IPAddressDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList

	ipV1, err := c.client.NetworkingV1().IPAddresses().Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(c.client.CoreV1(), ipV1, describerSettings.ChunkSize)
		}
		return c.describeIPAddressV1(ipV1, events)
	}

	ipV1beta1, err := c.client.NetworkingV1beta1().IPAddresses().Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(c.client.CoreV1(), ipV1beta1, describerSettings.ChunkSize)
		}
		return c.describeIPAddressV1beta1(ipV1beta1, events)
	}
	return "", err
}

func (c *IPAddressDescriber) describeIPAddressV1(ip *networkingv1.IPAddress, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%v\n", ip.Name)
		printLabelsMultiline(w, "Labels", ip.Labels)
		printAnnotationsMultiline(w, "Annotations", ip.Annotations)

		if ip.Spec.ParentRef != nil {
			w.Write(LEVEL_0, "Parent Reference:\n")
			w.Write(LEVEL_1, "Group:\t%v\n", ip.Spec.ParentRef.Group)
			w.Write(LEVEL_1, "Resource:\t%v\n", ip.Spec.ParentRef.Resource)
			w.Write(LEVEL_1, "Namespace:\t%v\n", ip.Spec.ParentRef.Namespace)
			w.Write(LEVEL_1, "Name:\t%v\n", ip.Spec.ParentRef.Name)
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func (c *IPAddressDescriber) describeIPAddressV1beta1(ip *networkingv1beta1.IPAddress, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%v\n", ip.Name)
		printLabelsMultiline(w, "Labels", ip.Labels)
		printAnnotationsMultiline(w, "Annotations", ip.Annotations)

		if ip.Spec.ParentRef != nil {
			w.Write(LEVEL_0, "Parent Reference:\n")
			w.Write(LEVEL_1, "Group:\t%v\n", ip.Spec.ParentRef.Group)
			w.Write(LEVEL_1, "Resource:\t%v\n", ip.Spec.ParentRef.Resource)
			w.Write(LEVEL_1, "Namespace:\t%v\n", ip.Spec.ParentRef.Namespace)
			w.Write(LEVEL_1, "Name:\t%v\n", ip.Spec.ParentRef.Name)
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// ServiceDescriber generates information about a service.
type ServiceDescriber struct {
	clientset.Interface
}

func (d *ServiceDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().Services(namespace)

	service, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	endpointSliceList, _ := d.DiscoveryV1().EndpointSlices(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", discoveryv1.LabelServiceName, name),
	})
	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), service, describerSettings.ChunkSize)
	}
	return describeService(service, endpointSliceList.Items, events)
}

func buildIngressString(ingress []corev1.LoadBalancerIngress) string {
	var buffer bytes.Buffer

	for i := range ingress {
		if i != 0 {
			buffer.WriteString(", ")
		}
		if ingress[i].IP != "" {
			buffer.WriteString(ingress[i].IP)
			if ingress[i].IPMode != nil {
				buffer.WriteString(fmt.Sprintf(" (%s)", *ingress[i].IPMode))
			}
		} else {
			buffer.WriteString(ingress[i].Hostname)
		}
	}
	return buffer.String()
}

func describeService(service *corev1.Service, endpointSlices []discoveryv1.EndpointSlice, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", service.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", service.Namespace)
		printLabelsMultiline(w, "Labels", service.Labels)
		printAnnotationsMultiline(w, "Annotations", service.Annotations)
		w.Write(LEVEL_0, "Selector:\t%s\n", labels.FormatLabels(service.Spec.Selector))
		w.Write(LEVEL_0, "Type:\t%s\n", service.Spec.Type)

		if service.Spec.IPFamilyPolicy != nil {
			w.Write(LEVEL_0, "IP Family Policy:\t%s\n", *(service.Spec.IPFamilyPolicy))
		}

		if len(service.Spec.IPFamilies) > 0 {
			ipfamiliesStrings := make([]string, 0, len(service.Spec.IPFamilies))
			for _, family := range service.Spec.IPFamilies {
				ipfamiliesStrings = append(ipfamiliesStrings, string(family))
			}

			w.Write(LEVEL_0, "IP Families:\t%s\n", strings.Join(ipfamiliesStrings, ","))
		} else {
			w.Write(LEVEL_0, "IP Families:\t%s\n", "<none>")
		}

		w.Write(LEVEL_0, "IP:\t%s\n", service.Spec.ClusterIP)
		if len(service.Spec.ClusterIPs) > 0 {
			w.Write(LEVEL_0, "IPs:\t%s\n", strings.Join(service.Spec.ClusterIPs, ","))
		} else {
			w.Write(LEVEL_0, "IPs:\t%s\n", "<none>")
		}

		if len(service.Spec.ExternalIPs) > 0 {
			w.Write(LEVEL_0, "External IPs:\t%v\n", strings.Join(service.Spec.ExternalIPs, ","))
		}
		if service.Spec.LoadBalancerIP != "" {
			w.Write(LEVEL_0, "Desired LoadBalancer IP:\t%s\n", service.Spec.LoadBalancerIP)
		}
		if service.Spec.ExternalName != "" {
			w.Write(LEVEL_0, "External Name:\t%s\n", service.Spec.ExternalName)
		}
		if len(service.Status.LoadBalancer.Ingress) > 0 {
			list := buildIngressString(service.Status.LoadBalancer.Ingress)
			w.Write(LEVEL_0, "LoadBalancer Ingress:\t%s\n", list)
		}
		for i := range service.Spec.Ports {
			sp := &service.Spec.Ports[i]

			name := sp.Name
			if name == "" {
				name = "<unset>"
			}
			w.Write(LEVEL_0, "Port:\t%s\t%d/%s\n", name, sp.Port, sp.Protocol)
			if sp.TargetPort.Type == intstr.Type(intstr.Int) {
				w.Write(LEVEL_0, "TargetPort:\t%d/%s\n", sp.TargetPort.IntVal, sp.Protocol)
			} else {
				w.Write(LEVEL_0, "TargetPort:\t%s/%s\n", sp.TargetPort.StrVal, sp.Protocol)
			}
			if sp.NodePort != 0 {
				w.Write(LEVEL_0, "NodePort:\t%s\t%d/%s\n", name, sp.NodePort, sp.Protocol)
			}
			w.Write(LEVEL_0, "Endpoints:\t%s\n", formatEndpointSlices(endpointSlices, sets.New(sp.Name)))
		}
		w.Write(LEVEL_0, "Session Affinity:\t%s\n", service.Spec.SessionAffinity)
		if service.Spec.ExternalTrafficPolicy != "" {
			w.Write(LEVEL_0, "External Traffic Policy:\t%s\n", service.Spec.ExternalTrafficPolicy)
		}
		if service.Spec.InternalTrafficPolicy != nil {
			w.Write(LEVEL_0, "Internal Traffic Policy:\t%s\n", *service.Spec.InternalTrafficPolicy)
		}
		if service.Spec.HealthCheckNodePort != 0 {
			w.Write(LEVEL_0, "HealthCheck NodePort:\t%d\n", service.Spec.HealthCheckNodePort)
		}
		if len(service.Spec.LoadBalancerSourceRanges) > 0 {
			w.Write(LEVEL_0, "LoadBalancer Source Ranges:\t%v\n", strings.Join(service.Spec.LoadBalancerSourceRanges, ","))
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// EndpointsDescriber generates information about an Endpoint.
type EndpointsDescriber struct {
	clientset.Interface
}

func (d *EndpointsDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().Endpoints(namespace)

	ep, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), ep, describerSettings.ChunkSize)
	}

	return describeEndpoints(ep, events)
}

func describeEndpoints(ep *corev1.Endpoints, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", ep.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", ep.Namespace)
		printLabelsMultiline(w, "Labels", ep.Labels)
		printAnnotationsMultiline(w, "Annotations", ep.Annotations)

		w.Write(LEVEL_0, "Subsets:\n")
		for i := range ep.Subsets {
			subset := &ep.Subsets[i]

			addresses := make([]string, 0, len(subset.Addresses))
			for _, addr := range subset.Addresses {
				addresses = append(addresses, addr.IP)
			}
			addressesString := strings.Join(addresses, ",")
			if len(addressesString) == 0 {
				addressesString = "<none>"
			}
			w.Write(LEVEL_1, "Addresses:\t%s\n", addressesString)

			notReadyAddresses := make([]string, 0, len(subset.NotReadyAddresses))
			for _, addr := range subset.NotReadyAddresses {
				notReadyAddresses = append(notReadyAddresses, addr.IP)
			}
			notReadyAddressesString := strings.Join(notReadyAddresses, ",")
			if len(notReadyAddressesString) == 0 {
				notReadyAddressesString = "<none>"
			}
			w.Write(LEVEL_1, "NotReadyAddresses:\t%s\n", notReadyAddressesString)

			if len(subset.Ports) > 0 {
				w.Write(LEVEL_1, "Ports:\n")
				w.Write(LEVEL_2, "Name\tPort\tProtocol\n")
				w.Write(LEVEL_2, "----\t----\t--------\n")
				for _, port := range subset.Ports {
					name := port.Name
					if len(name) == 0 {
						name = "<unset>"
					}
					w.Write(LEVEL_2, "%s\t%d\t%s\n", name, port.Port, port.Protocol)
				}
			}
			w.Write(LEVEL_0, "\n")
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// EndpointSliceDescriber generates information about an EndpointSlice.
type EndpointSliceDescriber struct {
	clientset.Interface
}

func (d *EndpointSliceDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList
	// try endpointslice/v1 first (v1.21) and fallback to v1beta1 if error occurs

	epsV1, err := d.DiscoveryV1().EndpointSlices(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(d.CoreV1(), epsV1, describerSettings.ChunkSize)
		}
		return describeEndpointSliceV1(epsV1, events)
	}

	epsV1beta1, err := d.DiscoveryV1beta1().EndpointSlices(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), epsV1beta1, describerSettings.ChunkSize)
	}

	return describeEndpointSliceV1beta1(epsV1beta1, events)
}

func describeEndpointSliceV1(eps *discoveryv1.EndpointSlice, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", eps.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", eps.Namespace)
		printLabelsMultiline(w, "Labels", eps.Labels)
		printAnnotationsMultiline(w, "Annotations", eps.Annotations)

		w.Write(LEVEL_0, "AddressType:\t%s\n", string(eps.AddressType))

		if len(eps.Ports) == 0 {
			w.Write(LEVEL_0, "Ports: <unset>\n")
		} else {
			w.Write(LEVEL_0, "Ports:\n")
			w.Write(LEVEL_1, "Name\tPort\tProtocol\n")
			w.Write(LEVEL_1, "----\t----\t--------\n")
			for _, port := range eps.Ports {
				portName := "<unset>"
				if port.Name != nil && len(*port.Name) > 0 {
					portName = *port.Name
				}

				portNum := "<unset>"
				if port.Port != nil {
					portNum = strconv.Itoa(int(*port.Port))
				}

				w.Write(LEVEL_1, "%s\t%s\t%s\n", portName, portNum, *port.Protocol)
			}
		}

		if len(eps.Endpoints) == 0 {
			w.Write(LEVEL_0, "Endpoints: <none>\n")
		} else {
			w.Write(LEVEL_0, "Endpoints:\n")
			for i := range eps.Endpoints {
				endpoint := &eps.Endpoints[i]

				addressesString := strings.Join(endpoint.Addresses, ", ")
				if len(addressesString) == 0 {
					addressesString = "<none>"
				}
				w.Write(LEVEL_1, "- Addresses:\t%s\n", addressesString)

				w.Write(LEVEL_2, "Conditions:\n")
				readyText := "<unset>"
				if endpoint.Conditions.Ready != nil {
					readyText = strconv.FormatBool(*endpoint.Conditions.Ready)
				}
				w.Write(LEVEL_3, "Ready:\t%s\n", readyText)

				hostnameText := "<unset>"
				if endpoint.Hostname != nil {
					hostnameText = *endpoint.Hostname
				}
				w.Write(LEVEL_2, "Hostname:\t%s\n", hostnameText)

				if endpoint.TargetRef != nil {
					w.Write(LEVEL_2, "TargetRef:\t%s/%s\n", endpoint.TargetRef.Kind, endpoint.TargetRef.Name)
				}

				nodeNameText := "<unset>"
				if endpoint.NodeName != nil {
					nodeNameText = *endpoint.NodeName
				}
				w.Write(LEVEL_2, "NodeName:\t%s\n", nodeNameText)

				zoneText := "<unset>"
				if endpoint.Zone != nil {
					zoneText = *endpoint.Zone
				}
				w.Write(LEVEL_2, "Zone:\t%s\n", zoneText)
			}
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func describeEndpointSliceV1beta1(eps *discoveryv1beta1.EndpointSlice, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", eps.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", eps.Namespace)
		printLabelsMultiline(w, "Labels", eps.Labels)
		printAnnotationsMultiline(w, "Annotations", eps.Annotations)

		w.Write(LEVEL_0, "AddressType:\t%s\n", string(eps.AddressType))

		if len(eps.Ports) == 0 {
			w.Write(LEVEL_0, "Ports: <unset>\n")
		} else {
			w.Write(LEVEL_0, "Ports:\n")
			w.Write(LEVEL_1, "Name\tPort\tProtocol\n")
			w.Write(LEVEL_1, "----\t----\t--------\n")
			for _, port := range eps.Ports {
				portName := "<unset>"
				if port.Name != nil && len(*port.Name) > 0 {
					portName = *port.Name
				}

				portNum := "<unset>"
				if port.Port != nil {
					portNum = strconv.Itoa(int(*port.Port))
				}

				w.Write(LEVEL_1, "%s\t%s\t%s\n", portName, portNum, *port.Protocol)
			}
		}

		if len(eps.Endpoints) == 0 {
			w.Write(LEVEL_0, "Endpoints: <none>\n")
		} else {
			w.Write(LEVEL_0, "Endpoints:\n")
			for i := range eps.Endpoints {
				endpoint := &eps.Endpoints[i]

				addressesString := strings.Join(endpoint.Addresses, ",")
				if len(addressesString) == 0 {
					addressesString = "<none>"
				}
				w.Write(LEVEL_1, "- Addresses:\t%s\n", addressesString)

				w.Write(LEVEL_2, "Conditions:\n")
				readyText := "<unset>"
				if endpoint.Conditions.Ready != nil {
					readyText = strconv.FormatBool(*endpoint.Conditions.Ready)
				}
				w.Write(LEVEL_3, "Ready:\t%s\n", readyText)

				hostnameText := "<unset>"
				if endpoint.Hostname != nil {
					hostnameText = *endpoint.Hostname
				}
				w.Write(LEVEL_2, "Hostname:\t%s\n", hostnameText)

				if endpoint.TargetRef != nil {
					w.Write(LEVEL_2, "TargetRef:\t%s/%s\n", endpoint.TargetRef.Kind, endpoint.TargetRef.Name)
				}

				printLabelsMultilineWithIndent(w, "    ", "Topology", "\t", endpoint.Topology, sets.New[string]())
			}
		}

		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

// ServiceAccountDescriber generates information about a service.
type ServiceAccountDescriber struct {
	clientset.Interface
}

func (d *ServiceAccountDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().ServiceAccounts(namespace)

	serviceAccount, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	tokens := []corev1.Secret{}

	// missingSecrets is the set of all secrets present in the
	// serviceAccount but not present in the set of existing secrets.
	missingSecrets := sets.New[string]()
	secrets := corev1.SecretList{}
	err = runtimeresource.FollowContinue(&metav1.ListOptions{Limit: describerSettings.ChunkSize},
		func(options metav1.ListOptions) (runtime.Object, error) {
			newList, err := d.CoreV1().Secrets(namespace).List(context.TODO(), options)
			if err != nil {
				return nil, runtimeresource.EnhanceListError(err, options, corev1.ResourceSecrets.String())
			}
			secrets.Items = append(secrets.Items, newList.Items...)
			return newList, nil
		})

	// errors are tolerated here in order to describe the serviceAccount with all
	// of the secrets that it references, even if those secrets cannot be fetched.
	if err == nil {
		// existingSecrets is the set of all secrets remaining on a
		// service account that are not present in the "tokens" slice.
		existingSecrets := sets.New[string]()

		for _, s := range secrets.Items {
			if s.Type == corev1.SecretTypeServiceAccountToken {
				name := s.Annotations[corev1.ServiceAccountNameKey]
				uid := s.Annotations[corev1.ServiceAccountUIDKey]
				if name == serviceAccount.Name && uid == string(serviceAccount.UID) {
					tokens = append(tokens, s)
				}
			}
			existingSecrets.Insert(s.Name)
		}

		for _, s := range serviceAccount.Secrets {
			if !existingSecrets.Has(s.Name) {
				missingSecrets.Insert(s.Name)
			}
		}
		for _, s := range serviceAccount.ImagePullSecrets {
			if !existingSecrets.Has(s.Name) {
				missingSecrets.Insert(s.Name)
			}
		}
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), serviceAccount, describerSettings.ChunkSize)
	}

	return describeServiceAccount(serviceAccount, tokens, missingSecrets, events)
}

func describeServiceAccount(serviceAccount *corev1.ServiceAccount, tokens []corev1.Secret, missingSecrets sets.Set[string], events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", serviceAccount.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", serviceAccount.Namespace)
		printLabelsMultiline(w, "Labels", serviceAccount.Labels)
		printAnnotationsMultiline(w, "Annotations", serviceAccount.Annotations)

		var (
			emptyHeader = "                   "
			pullHeader  = "Image pull secrets:"
			mountHeader = "Mountable secrets: "
			tokenHeader = "Tokens:            "

			pullSecretNames  = []string{}
			mountSecretNames = []string{}
			tokenSecretNames = []string{}
		)

		for _, s := range serviceAccount.ImagePullSecrets {
			pullSecretNames = append(pullSecretNames, s.Name)
		}
		for _, s := range serviceAccount.Secrets {
			mountSecretNames = append(mountSecretNames, s.Name)
		}
		for _, s := range tokens {
			tokenSecretNames = append(tokenSecretNames, s.Name)
		}

		types := map[string][]string{
			pullHeader:  pullSecretNames,
			mountHeader: mountSecretNames,
			tokenHeader: tokenSecretNames,
		}
		for _, header := range sets.List(sets.KeySet(types)) {
			names := types[header]
			if len(names) == 0 {
				w.Write(LEVEL_0, "%s\t<none>\n", header)
			} else {
				prefix := header
				for _, name := range names {
					if missingSecrets.Has(name) {
						w.Write(LEVEL_0, "%s\t%s (not found)\n", prefix, name)
					} else {
						w.Write(LEVEL_0, "%s\t%s\n", prefix, name)
					}
					prefix = emptyHeader
				}
			}
		}

		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

// RoleDescriber generates information about a node.
type RoleDescriber struct {
	clientset.Interface
}

func (d *RoleDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	role, err := d.RbacV1().Roles(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	breakdownRules := []rbacv1.PolicyRule{}
	for _, rule := range role.Rules {
		breakdownRules = append(breakdownRules, rbac.BreakdownRule(rule)...)
	}

	compactRules, err := rbac.CompactRules(breakdownRules)
	if err != nil {
		return "", err
	}
	sort.Stable(rbac.SortableRuleSlice(compactRules))

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", role.Name)
		printLabelsMultiline(w, "Labels", role.Labels)
		printAnnotationsMultiline(w, "Annotations", role.Annotations)

		w.Write(LEVEL_0, "PolicyRule:\n")
		w.Write(LEVEL_1, "Resources\tNon-Resource URLs\tResource Names\tVerbs\n")
		w.Write(LEVEL_1, "---------\t-----------------\t--------------\t-----\n")
		for _, r := range compactRules {
			w.Write(LEVEL_1, "%s\t%v\t%v\t%v\n", CombineResourceGroup(r.Resources, r.APIGroups), r.NonResourceURLs, r.ResourceNames, r.Verbs)
		}

		return nil
	})
}

// ClusterRoleDescriber generates information about a node.
type ClusterRoleDescriber struct {
	clientset.Interface
}

func (d *ClusterRoleDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	role, err := d.RbacV1().ClusterRoles().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	breakdownRules := []rbacv1.PolicyRule{}
	for _, rule := range role.Rules {
		breakdownRules = append(breakdownRules, rbac.BreakdownRule(rule)...)
	}

	compactRules, err := rbac.CompactRules(breakdownRules)
	if err != nil {
		return "", err
	}
	sort.Stable(rbac.SortableRuleSlice(compactRules))

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", role.Name)
		printLabelsMultiline(w, "Labels", role.Labels)
		printAnnotationsMultiline(w, "Annotations", role.Annotations)

		w.Write(LEVEL_0, "PolicyRule:\n")
		w.Write(LEVEL_1, "Resources\tNon-Resource URLs\tResource Names\tVerbs\n")
		w.Write(LEVEL_1, "---------\t-----------------\t--------------\t-----\n")
		for _, r := range compactRules {
			w.Write(LEVEL_1, "%s\t%v\t%v\t%v\n", CombineResourceGroup(r.Resources, r.APIGroups), r.NonResourceURLs, r.ResourceNames, r.Verbs)
		}

		return nil
	})
}

func CombineResourceGroup(resource, group []string) string {
	if len(resource) == 0 {
		return ""
	}
	parts := strings.SplitN(resource[0], "/", 2)
	combine := parts[0]

	if len(group) > 0 && group[0] != "" {
		combine = combine + "." + group[0]
	}

	if len(parts) == 2 {
		combine = combine + "/" + parts[1]
	}
	return combine
}

// RoleBindingDescriber generates information about a node.
type RoleBindingDescriber struct {
	clientset.Interface
}

func (d *RoleBindingDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	binding, err := d.RbacV1().RoleBindings(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", binding.Name)
		printLabelsMultiline(w, "Labels", binding.Labels)
		printAnnotationsMultiline(w, "Annotations", binding.Annotations)

		w.Write(LEVEL_0, "Role:\n")
		w.Write(LEVEL_1, "Kind:\t%s\n", binding.RoleRef.Kind)
		w.Write(LEVEL_1, "Name:\t%s\n", binding.RoleRef.Name)

		w.Write(LEVEL_0, "Subjects:\n")
		w.Write(LEVEL_1, "Kind\tName\tNamespace\n")
		w.Write(LEVEL_1, "----\t----\t---------\n")
		for _, s := range binding.Subjects {
			w.Write(LEVEL_1, "%s\t%s\t%s\n", s.Kind, s.Name, s.Namespace)
		}

		return nil
	})
}

// ClusterRoleBindingDescriber generates information about a node.
type ClusterRoleBindingDescriber struct {
	clientset.Interface
}

func (d *ClusterRoleBindingDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	binding, err := d.RbacV1().ClusterRoleBindings().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", binding.Name)
		printLabelsMultiline(w, "Labels", binding.Labels)
		printAnnotationsMultiline(w, "Annotations", binding.Annotations)

		w.Write(LEVEL_0, "Role:\n")
		w.Write(LEVEL_1, "Kind:\t%s\n", binding.RoleRef.Kind)
		w.Write(LEVEL_1, "Name:\t%s\n", binding.RoleRef.Name)

		w.Write(LEVEL_0, "Subjects:\n")
		w.Write(LEVEL_1, "Kind\tName\tNamespace\n")
		w.Write(LEVEL_1, "----\t----\t---------\n")
		for _, s := range binding.Subjects {
			w.Write(LEVEL_1, "%s\t%s\t%s\n", s.Kind, s.Name, s.Namespace)
		}

		return nil
	})
}

// NodeDescriber generates information about a node.
type NodeDescriber struct {
	clientset.Interface
}

func (d *NodeDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	mc := d.CoreV1().Nodes()
	node, err := mc.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	fieldSelector := fields.AndSelectors(
		fields.OneTermEqualSelector("spec.nodeName", name),
		fields.OneTermNotEqualSelector("status.phase", string(corev1.PodSucceeded)),
		fields.OneTermNotEqualSelector("status.phase", string(corev1.PodFailed)),
	)
	// in a policy aware setting, users may have access to a node, but not all pods
	// in that case, we note that the user does not have access to the pods
	canViewPods := true
	initialOpts := metav1.ListOptions{
		FieldSelector: fieldSelector.String(),
		Limit:         describerSettings.ChunkSize,
	}
	nodeNonTerminatedPodsList, err := getPodsInChunks(d.CoreV1().Pods(namespace), initialOpts)
	if err != nil {
		if !apierrors.IsForbidden(err) {
			return "", err
		}
		canViewPods = false
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		if ref, err := reference.GetReference(scheme.Scheme, node); err != nil {
			klog.Errorf("Unable to construct reference to '%#v': %v", node, err)
		} else {
			// TODO: We haven't decided the namespace for Node object yet.
			// there are two UIDs for host events:
			// controller use node.uid
			// kubelet use node.name
			// TODO: Uniform use of UID
			events, _ = searchEvents(d.CoreV1(), ref, describerSettings.ChunkSize)

			ref.UID = types.UID(ref.Name)
			eventsInvName, _ := searchEvents(d.CoreV1(), ref, describerSettings.ChunkSize)

			// Merge the results of two queries
			events.Items = append(events.Items, eventsInvName.Items...)
		}
	}

	return describeNode(node, nodeNonTerminatedPodsList, events, canViewPods, &LeaseDescriber{d})
}

type LeaseDescriber struct {
	client clientset.Interface
}

func describeNode(node *corev1.Node, nodeNonTerminatedPodsList *corev1.PodList, events *corev1.EventList,
	canViewPods bool, ld *LeaseDescriber) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", node.Name)
		if roles := findNodeRoles(node); len(roles) > 0 {
			w.Write(LEVEL_0, "Roles:\t%s\n", strings.Join(roles, ","))
		} else {
			w.Write(LEVEL_0, "Roles:\t%s\n", "<none>")
		}
		printLabelsMultiline(w, "Labels", node.Labels)
		printAnnotationsMultiline(w, "Annotations", node.Annotations)
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", node.CreationTimestamp.Time.Format(time.RFC1123Z))
		printNodeTaintsMultiline(w, "Taints", node.Spec.Taints)
		w.Write(LEVEL_0, "Unschedulable:\t%v\n", node.Spec.Unschedulable)

		if ld != nil {
			if lease, err := ld.client.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(context.TODO(), node.Name, metav1.GetOptions{}); err == nil {
				describeNodeLease(lease, w)
			} else {
				w.Write(LEVEL_0, "Lease:\tFailed to get lease: %s\n", err)
			}
		}

		if len(node.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n  Type\tStatus\tLastHeartbeatTime\tLastTransitionTime\tReason\tMessage\n")
			w.Write(LEVEL_1, "----\t------\t-----------------\t------------------\t------\t-------\n")
			for _, c := range node.Status.Conditions {
				w.Write(LEVEL_1, "%v \t%v \t%s \t%s \t%v \t%v\n",
					c.Type,
					c.Status,
					c.LastHeartbeatTime.Time.Format(time.RFC1123Z),
					c.LastTransitionTime.Time.Format(time.RFC1123Z),
					c.Reason,
					c.Message)
			}
		}

		w.Write(LEVEL_0, "Addresses:\n")
		for _, address := range node.Status.Addresses {
			w.Write(LEVEL_1, "%s:\t%s\n", address.Type, address.Address)
		}

		printResourceList := func(resourceList corev1.ResourceList) {
			resources := make([]corev1.ResourceName, 0, len(resourceList))
			for resource := range resourceList {
				resources = append(resources, resource)
			}
			sort.Sort(SortableResourceNames(resources))
			for _, resource := range resources {
				value := resourceList[resource]
				w.Write(LEVEL_0, "  %s:\t%s\n", resource, value.String())
			}
		}

		if len(node.Status.Capacity) > 0 {
			w.Write(LEVEL_0, "Capacity:\n")
			printResourceList(node.Status.Capacity)
		}
		if len(node.Status.Allocatable) > 0 {
			w.Write(LEVEL_0, "Allocatable:\n")
			printResourceList(node.Status.Allocatable)
		}

		w.Write(LEVEL_0, "System Info:\n")
		w.Write(LEVEL_0, "  Machine ID:\t%s\n", node.Status.NodeInfo.MachineID)
		w.Write(LEVEL_0, "  System UUID:\t%s\n", node.Status.NodeInfo.SystemUUID)
		w.Write(LEVEL_0, "  Boot ID:\t%s\n", node.Status.NodeInfo.BootID)
		w.Write(LEVEL_0, "  Kernel Version:\t%s\n", node.Status.NodeInfo.KernelVersion)
		w.Write(LEVEL_0, "  OS Image:\t%s\n", node.Status.NodeInfo.OSImage)
		w.Write(LEVEL_0, "  Operating System:\t%s\n", node.Status.NodeInfo.OperatingSystem)
		w.Write(LEVEL_0, "  Architecture:\t%s\n", node.Status.NodeInfo.Architecture)
		w.Write(LEVEL_0, "  Container Runtime Version:\t%s\n", node.Status.NodeInfo.ContainerRuntimeVersion)
		w.Write(LEVEL_0, "  Kubelet Version:\t%s\n", node.Status.NodeInfo.KubeletVersion)
		w.Write(LEVEL_0, "  Kube-Proxy Version:\t%s\n", node.Status.NodeInfo.KubeProxyVersion)

		// remove when .PodCIDR is deprecated
		if len(node.Spec.PodCIDR) > 0 {
			w.Write(LEVEL_0, "PodCIDR:\t%s\n", node.Spec.PodCIDR)
		}

		if len(node.Spec.PodCIDRs) > 0 {
			w.Write(LEVEL_0, "PodCIDRs:\t%s\n", strings.Join(node.Spec.PodCIDRs, ","))
		}
		if len(node.Spec.ProviderID) > 0 {
			w.Write(LEVEL_0, "ProviderID:\t%s\n", node.Spec.ProviderID)
		}
		if canViewPods && nodeNonTerminatedPodsList != nil {
			describeNodeResource(nodeNonTerminatedPodsList, node, w)
		} else {
			w.Write(LEVEL_0, "Pods:\tnot authorized\n")
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func describeNodeLease(lease *coordinationv1.Lease, w PrefixWriter) {
	w.Write(LEVEL_0, "Lease:\n")
	holderIdentity := "<unset>"
	if lease != nil && lease.Spec.HolderIdentity != nil {
		holderIdentity = *lease.Spec.HolderIdentity
	}
	w.Write(LEVEL_1, "HolderIdentity:\t%s\n", holderIdentity)
	acquireTime := "<unset>"
	if lease != nil && lease.Spec.AcquireTime != nil {
		acquireTime = lease.Spec.AcquireTime.Time.Format(time.RFC1123Z)
	}
	w.Write(LEVEL_1, "AcquireTime:\t%s\n", acquireTime)
	renewTime := "<unset>"
	if lease != nil && lease.Spec.RenewTime != nil {
		renewTime = lease.Spec.RenewTime.Time.Format(time.RFC1123Z)
	}
	w.Write(LEVEL_1, "RenewTime:\t%s\n", renewTime)
}

type StatefulSetDescriber struct {
	client clientset.Interface
}

func (p *StatefulSetDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	ps, err := p.client.AppsV1().StatefulSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	pc := p.client.CoreV1().Pods(namespace)

	selector, err := metav1.LabelSelectorAsSelector(ps.Spec.Selector)
	if err != nil {
		return "", err
	}

	running, waiting, succeeded, failed, err := getPodStatusForController(pc, selector, ps.UID, describerSettings)
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(p.client.CoreV1(), ps, describerSettings.ChunkSize)
	}

	return describeStatefulSet(ps, selector, events, running, waiting, succeeded, failed)
}

func describeStatefulSet(ps *appsv1.StatefulSet, selector labels.Selector, events *corev1.EventList, running, waiting, succeeded, failed int) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", ps.ObjectMeta.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", ps.ObjectMeta.Namespace)
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", ps.CreationTimestamp.Time.Format(time.RFC1123Z))
		w.Write(LEVEL_0, "Selector:\t%s\n", selector)
		printLabelsMultiline(w, "Labels", ps.Labels)
		printAnnotationsMultiline(w, "Annotations", ps.Annotations)
		w.Write(LEVEL_0, "Replicas:\t%d desired | %d total\n", *ps.Spec.Replicas, ps.Status.Replicas)
		w.Write(LEVEL_0, "Update Strategy:\t%s\n", ps.Spec.UpdateStrategy.Type)
		if ps.Spec.UpdateStrategy.RollingUpdate != nil {
			ru := ps.Spec.UpdateStrategy.RollingUpdate
			if ru.Partition != nil {
				w.Write(LEVEL_1, "Partition:\t%d\n", *ru.Partition)
				if ru.MaxUnavailable != nil {
					w.Write(LEVEL_1, "MaxUnavailable:\t%s\n", ru.MaxUnavailable.String())
				}
			}
		}

		w.Write(LEVEL_0, "Pods Status:\t%d Running / %d Waiting / %d Succeeded / %d Failed\n", running, waiting, succeeded, failed)
		DescribePodTemplate(&ps.Spec.Template, w)
		describeVolumeClaimTemplates(ps.Spec.VolumeClaimTemplates, w)
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

type CertificateSigningRequestDescriber struct {
	client clientset.Interface
}

func (p *CertificateSigningRequestDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {

	var (
		crBytes           []byte
		metadata          metav1.ObjectMeta
		status            string
		signerName        string
		expirationSeconds *int32
		username          string
		events            *corev1.EventList
	)

	if csr, err := p.client.CertificatesV1().CertificateSigningRequests().Get(context.TODO(), name, metav1.GetOptions{}); err == nil {
		crBytes = csr.Spec.Request
		metadata = csr.ObjectMeta
		conditionTypes := []string{}
		for _, c := range csr.Status.Conditions {
			conditionTypes = append(conditionTypes, string(c.Type))
		}
		status = extractCSRStatus(conditionTypes, csr.Status.Certificate)
		signerName = csr.Spec.SignerName
		expirationSeconds = csr.Spec.ExpirationSeconds
		username = csr.Spec.Username
		if describerSettings.ShowEvents {
			events, _ = searchEvents(p.client.CoreV1(), csr, describerSettings.ChunkSize)
		}
	} else if csr, err := p.client.CertificatesV1beta1().CertificateSigningRequests().Get(context.TODO(), name, metav1.GetOptions{}); err == nil {
		crBytes = csr.Spec.Request
		metadata = csr.ObjectMeta
		conditionTypes := []string{}
		for _, c := range csr.Status.Conditions {
			conditionTypes = append(conditionTypes, string(c.Type))
		}
		status = extractCSRStatus(conditionTypes, csr.Status.Certificate)
		if csr.Spec.SignerName != nil {
			signerName = *csr.Spec.SignerName
		}
		expirationSeconds = csr.Spec.ExpirationSeconds
		username = csr.Spec.Username
		if describerSettings.ShowEvents {
			events, _ = searchEvents(p.client.CoreV1(), csr, describerSettings.ChunkSize)
		}
	} else {
		return "", err
	}

	cr, err := certificate.ParseCSR(crBytes)
	if err != nil {
		return "", fmt.Errorf("Error parsing CSR: %v", err)
	}

	return describeCertificateSigningRequest(metadata, signerName, expirationSeconds, username, cr, status, events)
}

func describeCertificateSigningRequest(csr metav1.ObjectMeta, signerName string, expirationSeconds *int32, username string, cr *x509.CertificateRequest, status string, events *corev1.EventList) (string, error) {
	printListHelper := func(w PrefixWriter, prefix, name string, values []string) {
		if len(values) == 0 {
			return
		}
		w.Write(LEVEL_0, prefix+name+":\t")
		w.Write(LEVEL_0, strings.Join(values, "\n"+prefix+"\t"))
		w.Write(LEVEL_0, "\n")
	}

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", csr.Name)
		w.Write(LEVEL_0, "Labels:\t%s\n", labels.FormatLabels(csr.Labels))
		w.Write(LEVEL_0, "Annotations:\t%s\n", labels.FormatLabels(csr.Annotations))
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", csr.CreationTimestamp.Time.Format(time.RFC1123Z))
		w.Write(LEVEL_0, "Requesting User:\t%s\n", username)
		if len(signerName) > 0 {
			w.Write(LEVEL_0, "Signer:\t%s\n", signerName)
		}
		if expirationSeconds != nil {
			w.Write(LEVEL_0, "Requested Duration:\t%s\n", duration.HumanDuration(utilcsr.ExpirationSecondsToDuration(*expirationSeconds)))
		}
		w.Write(LEVEL_0, "Status:\t%s\n", status)

		w.Write(LEVEL_0, "Subject:\n")
		w.Write(LEVEL_0, "\tCommon Name:\t%s\n", cr.Subject.CommonName)
		w.Write(LEVEL_0, "\tSerial Number:\t%s\n", cr.Subject.SerialNumber)
		printListHelper(w, "\t", "Organization", cr.Subject.Organization)
		printListHelper(w, "\t", "Organizational Unit", cr.Subject.OrganizationalUnit)
		printListHelper(w, "\t", "Country", cr.Subject.Country)
		printListHelper(w, "\t", "Locality", cr.Subject.Locality)
		printListHelper(w, "\t", "Province", cr.Subject.Province)
		printListHelper(w, "\t", "StreetAddress", cr.Subject.StreetAddress)
		printListHelper(w, "\t", "PostalCode", cr.Subject.PostalCode)

		if len(cr.DNSNames)+len(cr.EmailAddresses)+len(cr.IPAddresses)+len(cr.URIs) > 0 {
			w.Write(LEVEL_0, "Subject Alternative Names:\n")
			printListHelper(w, "\t", "DNS Names", cr.DNSNames)
			printListHelper(w, "\t", "Email Addresses", cr.EmailAddresses)
			var uris []string
			for _, uri := range cr.URIs {
				uris = append(uris, uri.String())
			}
			printListHelper(w, "\t", "URIs", uris)
			var ipaddrs []string
			for _, ipaddr := range cr.IPAddresses {
				ipaddrs = append(ipaddrs, ipaddr.String())
			}
			printListHelper(w, "\t", "IP Addresses", ipaddrs)
		}

		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

// HorizontalPodAutoscalerDescriber generates information about a horizontal pod autoscaler.
type HorizontalPodAutoscalerDescriber struct {
	client clientset.Interface
}

func (d *HorizontalPodAutoscalerDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var events *corev1.EventList

	// autoscaling/v2 is introduced since v1.23 and autoscaling/v1 does not have full backward compatibility
	// with autoscaling/v2, so describer will try to get and describe hpa v2 object firstly, if it fails,
	// describer will fall back to do with hpa v1 object
	hpaV2, err := d.client.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(d.client.CoreV1(), hpaV2, describerSettings.ChunkSize)
		}
		return describeHorizontalPodAutoscalerV2(hpaV2, events, d)
	}

	hpaV1, err := d.client.AutoscalingV1().HorizontalPodAutoscalers(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		if describerSettings.ShowEvents {
			events, _ = searchEvents(d.client.CoreV1(), hpaV1, describerSettings.ChunkSize)
		}
		return describeHorizontalPodAutoscalerV1(hpaV1, events, d)
	}

	return "", err
}

func describeHorizontalPodAutoscalerV2(hpa *autoscalingv2.HorizontalPodAutoscaler, events *corev1.EventList, d *HorizontalPodAutoscalerDescriber) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", hpa.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", hpa.Namespace)
		printLabelsMultiline(w, "Labels", hpa.Labels)
		printAnnotationsMultiline(w, "Annotations", hpa.Annotations)
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", hpa.CreationTimestamp.Time.Format(time.RFC1123Z))
		w.Write(LEVEL_0, "Reference:\t%s/%s\n",
			hpa.Spec.ScaleTargetRef.Kind,
			hpa.Spec.ScaleTargetRef.Name)
		w.Write(LEVEL_0, "Metrics:\t( current / target )\n")
		for i, metric := range hpa.Spec.Metrics {
			switch metric.Type {
			case autoscalingv2.ExternalMetricSourceType:
				if metric.External.Target.AverageValue != nil {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].External != nil &&
						hpa.Status.CurrentMetrics[i].External.Current.AverageValue != nil {
						current = hpa.Status.CurrentMetrics[i].External.Current.AverageValue.String()
					}
					w.Write(LEVEL_1, "%q (target average value):\t%s / %s\n", metric.External.Metric.Name, current, metric.External.Target.AverageValue.String())
				} else {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].External != nil {
						current = hpa.Status.CurrentMetrics[i].External.Current.Value.String()
					}
					w.Write(LEVEL_1, "%q (target value):\t%s / %s\n", metric.External.Metric.Name, current, metric.External.Target.Value.String())

				}
			case autoscalingv2.PodsMetricSourceType:
				current := "<unknown>"
				if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].Pods != nil {
					current = hpa.Status.CurrentMetrics[i].Pods.Current.AverageValue.String()
				}
				w.Write(LEVEL_1, "%q on pods:\t%s / %s\n", metric.Pods.Metric.Name, current, metric.Pods.Target.AverageValue.String())
			case autoscalingv2.ObjectMetricSourceType:
				w.Write(LEVEL_1, "\"%s\" on %s/%s ", metric.Object.Metric.Name, metric.Object.DescribedObject.Kind, metric.Object.DescribedObject.Name)
				if metric.Object.Target.Type == autoscalingv2.AverageValueMetricType {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].Object != nil {
						current = hpa.Status.CurrentMetrics[i].Object.Current.AverageValue.String()
					}
					w.Write(LEVEL_0, "(target average value):\t%s / %s\n", current, metric.Object.Target.AverageValue.String())
				} else {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].Object != nil {
						current = hpa.Status.CurrentMetrics[i].Object.Current.Value.String()
					}
					w.Write(LEVEL_0, "(target value):\t%s / %s\n", current, metric.Object.Target.Value.String())
				}
			case autoscalingv2.ResourceMetricSourceType:
				w.Write(LEVEL_1, "resource %s on pods", string(metric.Resource.Name))
				if metric.Resource.Target.AverageValue != nil {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].Resource != nil {
						current = hpa.Status.CurrentMetrics[i].Resource.Current.AverageValue.String()
					}
					w.Write(LEVEL_0, ":\t%s / %s\n", current, metric.Resource.Target.AverageValue.String())
				} else {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].Resource != nil && hpa.Status.CurrentMetrics[i].Resource.Current.AverageUtilization != nil {
						current = fmt.Sprintf("%d%% (%s)", *hpa.Status.CurrentMetrics[i].Resource.Current.AverageUtilization, hpa.Status.CurrentMetrics[i].Resource.Current.AverageValue.String())
					}

					target := "<auto>"
					if metric.Resource.Target.AverageUtilization != nil {
						target = fmt.Sprintf("%d%%", *metric.Resource.Target.AverageUtilization)
					}
					w.Write(LEVEL_1, "(as a percentage of request):\t%s / %s\n", current, target)
				}
			case autoscalingv2.ContainerResourceMetricSourceType:
				w.Write(LEVEL_1, "resource %s of container \"%s\" on pods", string(metric.ContainerResource.Name), metric.ContainerResource.Container)
				if metric.ContainerResource.Target.AverageValue != nil {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].ContainerResource != nil {
						current = hpa.Status.CurrentMetrics[i].ContainerResource.Current.AverageValue.String()
					}
					w.Write(LEVEL_0, ":\t%s / %s\n", current, metric.ContainerResource.Target.AverageValue.String())
				} else {
					current := "<unknown>"
					if len(hpa.Status.CurrentMetrics) > i && hpa.Status.CurrentMetrics[i].ContainerResource != nil && hpa.Status.CurrentMetrics[i].ContainerResource.Current.AverageUtilization != nil {
						current = fmt.Sprintf("%d%% (%s)", *hpa.Status.CurrentMetrics[i].ContainerResource.Current.AverageUtilization, hpa.Status.CurrentMetrics[i].ContainerResource.Current.AverageValue.String())
					}

					target := "<auto>"
					if metric.ContainerResource.Target.AverageUtilization != nil {
						target = fmt.Sprintf("%d%%", *metric.ContainerResource.Target.AverageUtilization)
					}
					w.Write(LEVEL_1, "(as a percentage of request):\t%s / %s\n", current, target)
				}
			default:
				w.Write(LEVEL_1, "<unknown metric type %q>\n", string(metric.Type))
			}
		}
		minReplicas := "<unset>"
		if hpa.Spec.MinReplicas != nil {
			minReplicas = fmt.Sprintf("%d", *hpa.Spec.MinReplicas)
		}
		w.Write(LEVEL_0, "Min replicas:\t%s\n", minReplicas)
		w.Write(LEVEL_0, "Max replicas:\t%d\n", hpa.Spec.MaxReplicas)
		// only print the hpa behavior if present
		if hpa.Spec.Behavior != nil {
			w.Write(LEVEL_0, "Behavior:\n")
			printDirectionBehavior(w, "Scale Up", hpa.Spec.Behavior.ScaleUp)
			printDirectionBehavior(w, "Scale Down", hpa.Spec.Behavior.ScaleDown)
		}
		w.Write(LEVEL_0, "%s pods:\t", hpa.Spec.ScaleTargetRef.Kind)
		w.Write(LEVEL_0, "%d current / %d desired\n", hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas)

		if len(hpa.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n")
			w.Write(LEVEL_1, "Type\tStatus\tReason\tMessage\n")
			w.Write(LEVEL_1, "----\t------\t------\t-------\n")
			for _, c := range hpa.Status.Conditions {
				w.Write(LEVEL_1, "%v\t%v\t%v\t%v\n", c.Type, c.Status, c.Reason, c.Message)
			}
		}

		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

func printDirectionBehavior(w PrefixWriter, direction string, rules *autoscalingv2.HPAScalingRules) {
	if rules != nil {
		w.Write(LEVEL_1, "%s:\n", direction)
		if rules.StabilizationWindowSeconds != nil {
			w.Write(LEVEL_2, "Stabilization Window: %d seconds\n", *rules.StabilizationWindowSeconds)
		}
		if len(rules.Policies) > 0 {
			if rules.SelectPolicy != nil {
				w.Write(LEVEL_2, "Select Policy: %s\n", *rules.SelectPolicy)
			} else {
				w.Write(LEVEL_2, "Select Policy: %s\n", autoscalingv2.MaxChangePolicySelect)
			}
			w.Write(LEVEL_2, "Policies:\n")
			for _, p := range rules.Policies {
				w.Write(LEVEL_3, "- Type: %s\tValue: %d\tPeriod: %d seconds\n", p.Type, p.Value, p.PeriodSeconds)
			}
		}
	}
}

func describeHorizontalPodAutoscalerV1(hpa *autoscalingv1.HorizontalPodAutoscaler, events *corev1.EventList, d *HorizontalPodAutoscalerDescriber) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", hpa.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", hpa.Namespace)
		printLabelsMultiline(w, "Labels", hpa.Labels)
		printAnnotationsMultiline(w, "Annotations", hpa.Annotations)
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", hpa.CreationTimestamp.Time.Format(time.RFC1123Z))
		w.Write(LEVEL_0, "Reference:\t%s/%s\n",
			hpa.Spec.ScaleTargetRef.Kind,
			hpa.Spec.ScaleTargetRef.Name)

		if hpa.Spec.TargetCPUUtilizationPercentage != nil {
			w.Write(LEVEL_0, "Target CPU utilization:\t%d%%\n", *hpa.Spec.TargetCPUUtilizationPercentage)
			current := "<unknown>"
			if hpa.Status.CurrentCPUUtilizationPercentage != nil {
				current = fmt.Sprintf("%d", *hpa.Status.CurrentCPUUtilizationPercentage)
			}
			w.Write(LEVEL_0, "Current CPU utilization:\t%s%%\n", current)
		}

		minReplicas := "<unset>"
		if hpa.Spec.MinReplicas != nil {
			minReplicas = fmt.Sprintf("%d", *hpa.Spec.MinReplicas)
		}
		w.Write(LEVEL_0, "Min replicas:\t%s\n", minReplicas)
		w.Write(LEVEL_0, "Max replicas:\t%d\n", hpa.Spec.MaxReplicas)
		w.Write(LEVEL_0, "%s pods:\t", hpa.Spec.ScaleTargetRef.Kind)
		w.Write(LEVEL_0, "%d current / %d desired\n", hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas)

		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

func describeNodeResource(nodeNonTerminatedPodsList *corev1.PodList, node *corev1.Node, w PrefixWriter) {
	w.Write(LEVEL_0, "Non-terminated Pods:\t(%d in total)\n", len(nodeNonTerminatedPodsList.Items))
	w.Write(LEVEL_1, "Namespace\tName\t\tCPU Requests\tCPU Limits\tMemory Requests\tMemory Limits\tAge\n")
	w.Write(LEVEL_1, "---------\t----\t\t------------\t----------\t---------------\t-------------\t---\n")
	allocatable := node.Status.Capacity
	if len(node.Status.Allocatable) > 0 {
		allocatable = node.Status.Allocatable
	}

	for _, pod := range nodeNonTerminatedPodsList.Items {
		req, limit := resourcehelper.PodRequestsAndLimits(&pod)
		cpuReq, cpuLimit, memoryReq, memoryLimit := req[corev1.ResourceCPU], limit[corev1.ResourceCPU], req[corev1.ResourceMemory], limit[corev1.ResourceMemory]
		fractionCpuReq := float64(cpuReq.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		fractionCpuLimit := float64(cpuLimit.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		fractionMemoryReq := float64(memoryReq.Value()) / float64(allocatable.Memory().Value()) * 100
		fractionMemoryLimit := float64(memoryLimit.Value()) / float64(allocatable.Memory().Value()) * 100
		w.Write(LEVEL_1, "%s\t%s\t\t%s (%d%%)\t%s (%d%%)\t%s (%d%%)\t%s (%d%%)\t%s\n", pod.Namespace, pod.Name,
			cpuReq.String(), int64(fractionCpuReq), cpuLimit.String(), int64(fractionCpuLimit),
			memoryReq.String(), int64(fractionMemoryReq), memoryLimit.String(), int64(fractionMemoryLimit), translateTimestampSince(pod.CreationTimestamp))
	}

	w.Write(LEVEL_0, "Allocated resources:\n  (Total limits may be over 100 percent, i.e., overcommitted.)\n")
	w.Write(LEVEL_1, "Resource\tRequests\tLimits\n")
	w.Write(LEVEL_1, "--------\t--------\t------\n")
	reqs, limits := getPodsTotalRequestsAndLimits(nodeNonTerminatedPodsList)
	cpuReqs, cpuLimits, memoryReqs, memoryLimits, ephemeralstorageReqs, ephemeralstorageLimits :=
		reqs[corev1.ResourceCPU], limits[corev1.ResourceCPU], reqs[corev1.ResourceMemory], limits[corev1.ResourceMemory], reqs[corev1.ResourceEphemeralStorage], limits[corev1.ResourceEphemeralStorage]
	fractionCpuReqs := float64(0)
	fractionCpuLimits := float64(0)
	if allocatable.Cpu().MilliValue() != 0 {
		fractionCpuReqs = float64(cpuReqs.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		fractionCpuLimits = float64(cpuLimits.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
	}
	fractionMemoryReqs := float64(0)
	fractionMemoryLimits := float64(0)
	if allocatable.Memory().Value() != 0 {
		fractionMemoryReqs = float64(memoryReqs.Value()) / float64(allocatable.Memory().Value()) * 100
		fractionMemoryLimits = float64(memoryLimits.Value()) / float64(allocatable.Memory().Value()) * 100
	}
	fractionEphemeralStorageReqs := float64(0)
	fractionEphemeralStorageLimits := float64(0)
	if allocatable.StorageEphemeral().Value() != 0 {
		fractionEphemeralStorageReqs = float64(ephemeralstorageReqs.Value()) / float64(allocatable.StorageEphemeral().Value()) * 100
		fractionEphemeralStorageLimits = float64(ephemeralstorageLimits.Value()) / float64(allocatable.StorageEphemeral().Value()) * 100
	}
	w.Write(LEVEL_1, "%s\t%s (%d%%)\t%s (%d%%)\n",
		corev1.ResourceCPU, cpuReqs.String(), int64(fractionCpuReqs), cpuLimits.String(), int64(fractionCpuLimits))
	w.Write(LEVEL_1, "%s\t%s (%d%%)\t%s (%d%%)\n",
		corev1.ResourceMemory, memoryReqs.String(), int64(fractionMemoryReqs), memoryLimits.String(), int64(fractionMemoryLimits))
	w.Write(LEVEL_1, "%s\t%s (%d%%)\t%s (%d%%)\n",
		corev1.ResourceEphemeralStorage, ephemeralstorageReqs.String(), int64(fractionEphemeralStorageReqs), ephemeralstorageLimits.String(), int64(fractionEphemeralStorageLimits))

	extResources := make([]string, 0, len(allocatable))
	hugePageResources := make([]string, 0, len(allocatable))
	for resource := range allocatable {
		if resourcehelper.IsHugePageResourceName(resource) {
			hugePageResources = append(hugePageResources, string(resource))
		} else if !resourcehelper.IsStandardContainerResourceName(string(resource)) && resource != corev1.ResourcePods {
			extResources = append(extResources, string(resource))
		}
	}

	sort.Strings(extResources)
	sort.Strings(hugePageResources)

	for _, resource := range hugePageResources {
		hugePageSizeRequests, hugePageSizeLimits, hugePageSizeAllocable := reqs[corev1.ResourceName(resource)], limits[corev1.ResourceName(resource)], allocatable[corev1.ResourceName(resource)]
		fractionHugePageSizeRequests := float64(0)
		fractionHugePageSizeLimits := float64(0)
		if hugePageSizeAllocable.Value() != 0 {
			fractionHugePageSizeRequests = float64(hugePageSizeRequests.Value()) / float64(hugePageSizeAllocable.Value()) * 100
			fractionHugePageSizeLimits = float64(hugePageSizeLimits.Value()) / float64(hugePageSizeAllocable.Value()) * 100
		}
		w.Write(LEVEL_1, "%s\t%s (%d%%)\t%s (%d%%)\n",
			resource, hugePageSizeRequests.String(), int64(fractionHugePageSizeRequests), hugePageSizeLimits.String(), int64(fractionHugePageSizeLimits))
	}

	for _, ext := range extResources {
		extRequests, extLimits := reqs[corev1.ResourceName(ext)], limits[corev1.ResourceName(ext)]
		w.Write(LEVEL_1, "%s\t%s\t%s\n", ext, extRequests.String(), extLimits.String())
	}
}

func getPodsTotalRequestsAndLimits(podList *corev1.PodList) (reqs map[corev1.ResourceName]resource.Quantity, limits map[corev1.ResourceName]resource.Quantity) {
	reqs, limits = map[corev1.ResourceName]resource.Quantity{}, map[corev1.ResourceName]resource.Quantity{}
	for _, pod := range podList.Items {
		podReqs, podLimits := resourcehelper.PodRequestsAndLimits(&pod)
		for podReqName, podReqValue := range podReqs {
			if value, ok := reqs[podReqName]; !ok {
				reqs[podReqName] = podReqValue.DeepCopy()
			} else {
				value.Add(podReqValue)
				reqs[podReqName] = value
			}
		}
		for podLimitName, podLimitValue := range podLimits {
			if value, ok := limits[podLimitName]; !ok {
				limits[podLimitName] = podLimitValue.DeepCopy()
			} else {
				value.Add(podLimitValue)
				limits[podLimitName] = value
			}
		}
	}
	return
}

func DescribeEvents(el *corev1.EventList, w PrefixWriter) {
	if len(el.Items) == 0 {
		w.Write(LEVEL_0, "Events:\t<none>\n")
		return
	}
	w.Flush()
	sort.Sort(event.SortableEvents(el.Items))
	w.Write(LEVEL_0, "Events:\n  Type\tReason\tAge\tFrom\tMessage\n")
	w.Write(LEVEL_1, "----\t------\t----\t----\t-------\n")
	for _, e := range el.Items {
		var interval string
		firstTimestampSince := translateMicroTimestampSince(e.EventTime)
		if e.EventTime.IsZero() {
			firstTimestampSince = translateTimestampSince(e.FirstTimestamp)
		}
		if e.Series != nil {
			interval = fmt.Sprintf("%s (x%d over %s)", translateMicroTimestampSince(e.Series.LastObservedTime), e.Series.Count, firstTimestampSince)
		} else if e.Count > 1 {
			interval = fmt.Sprintf("%s (x%d over %s)", translateTimestampSince(e.LastTimestamp), e.Count, firstTimestampSince)
		} else {
			interval = firstTimestampSince
		}
		source := e.Source.Component
		if source == "" {
			source = e.ReportingController
		}
		w.Write(LEVEL_1, "%v\t%v\t%s\t%v\t%v\n",
			e.Type,
			e.Reason,
			interval,
			source,
			strings.TrimSpace(e.Message),
		)
	}
}

// DeploymentDescriber generates information about a deployment.
type DeploymentDescriber struct {
	clientset.Interface
}

func (dd *DeploymentDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	d, err := dd.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(dd.CoreV1(), d, describerSettings.ChunkSize)
	}

	var oldRSs, newRSs []*appsv1.ReplicaSet
	if _, oldResult, newResult, err := deploymentutil.GetAllReplicaSetsInChunks(d, dd.AppsV1(), describerSettings.ChunkSize); err == nil {
		oldRSs = oldResult
		if newResult != nil {
			newRSs = append(newRSs, newResult)
		}
	}

	return describeDeployment(d, oldRSs, newRSs, events)
}
func describeDeployment(d *appsv1.Deployment, oldRSs []*appsv1.ReplicaSet, newRSs []*appsv1.ReplicaSet, events *corev1.EventList) (string, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return "", err
	}
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", d.ObjectMeta.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", d.ObjectMeta.Namespace)
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", d.CreationTimestamp.Time.Format(time.RFC1123Z))
		printLabelsMultiline(w, "Labels", d.Labels)
		printAnnotationsMultiline(w, "Annotations", d.Annotations)
		w.Write(LEVEL_0, "Selector:\t%s\n", selector)
		w.Write(LEVEL_0, "Replicas:\t%d desired | %d updated | %d total | %d available | %d unavailable\n", *(d.Spec.Replicas), d.Status.UpdatedReplicas, d.Status.Replicas, d.Status.AvailableReplicas, d.Status.UnavailableReplicas)
		w.Write(LEVEL_0, "StrategyType:\t%s\n", d.Spec.Strategy.Type)
		w.Write(LEVEL_0, "MinReadySeconds:\t%d\n", d.Spec.MinReadySeconds)
		if d.Spec.Strategy.RollingUpdate != nil {
			ru := d.Spec.Strategy.RollingUpdate
			w.Write(LEVEL_0, "RollingUpdateStrategy:\t%s max unavailable, %s max surge\n", ru.MaxUnavailable.String(), ru.MaxSurge.String())
		}
		DescribePodTemplate(&d.Spec.Template, w)
		if len(d.Status.Conditions) > 0 {
			w.Write(LEVEL_0, "Conditions:\n  Type\tStatus\tReason\n")
			w.Write(LEVEL_1, "----\t------\t------\n")
			for _, c := range d.Status.Conditions {
				w.Write(LEVEL_1, "%v \t%v\t%v\n", c.Type, c.Status, c.Reason)
			}
		}

		if len(oldRSs) > 0 || len(newRSs) > 0 {
			w.Write(LEVEL_0, "OldReplicaSets:\t%s\n", printReplicaSetsByLabels(oldRSs))
			w.Write(LEVEL_0, "NewReplicaSet:\t%s\n", printReplicaSetsByLabels(newRSs))
		}
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

func printReplicaSetsByLabels(matchingRSs []*appsv1.ReplicaSet) string {
	// Format the matching ReplicaSets into strings.
	rsStrings := make([]string, 0, len(matchingRSs))
	for _, rs := range matchingRSs {
		rsStrings = append(rsStrings, fmt.Sprintf("%s (%d/%d replicas created)", rs.Name, rs.Status.Replicas, *rs.Spec.Replicas))
	}

	list := strings.Join(rsStrings, ", ")
	if list == "" {
		return "<none>"
	}
	return list
}

func getPodStatusForController(c corev1client.PodInterface, selector labels.Selector, uid types.UID, settings DescriberSettings) (
	running, waiting, succeeded, failed int, err error) {
	initialOpts := metav1.ListOptions{LabelSelector: selector.String(), Limit: settings.ChunkSize}
	rcPods, err := getPodsInChunks(c, initialOpts)
	if err != nil {
		return
	}
	for _, pod := range rcPods.Items {
		controllerRef := metav1.GetControllerOf(&pod)
		// Skip pods that are orphans or owned by other controllers.
		if controllerRef == nil || controllerRef.UID != uid {
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			running++
		case corev1.PodPending:
			waiting++
		case corev1.PodSucceeded:
			succeeded++
		case corev1.PodFailed:
			failed++
		}
	}
	return
}

func getPodsInChunks(c corev1client.PodInterface, initialOpts metav1.ListOptions) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	err := runtimeresource.FollowContinue(&initialOpts,
		func(options metav1.ListOptions) (runtime.Object, error) {
			newList, err := c.List(context.TODO(), options)
			if err != nil {
				return nil, runtimeresource.EnhanceListError(err, options, corev1.ResourcePods.String())
			}
			podList.Items = append(podList.Items, newList.Items...)
			return newList, nil
		})
	return podList, err
}

// ConfigMapDescriber generates information about a ConfigMap
type ConfigMapDescriber struct {
	clientset.Interface
}

func (d *ConfigMapDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.CoreV1().ConfigMaps(namespace)

	configMap, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", configMap.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", configMap.Namespace)
		printLabelsMultiline(w, "Labels", configMap.Labels)
		printAnnotationsMultiline(w, "Annotations", configMap.Annotations)

		w.Write(LEVEL_0, "\nData\n====\n")
		for k, v := range configMap.Data {
			w.Write(LEVEL_0, "%s:\n----\n", k)
			w.Write(LEVEL_0, "%s\n", string(v))
			w.Write(LEVEL_0, "\n")
		}
		w.Write(LEVEL_0, "\nBinaryData\n====\n")
		for k, v := range configMap.BinaryData {
			w.Write(LEVEL_0, "%s: %s bytes\n", k, strconv.Itoa(len(v)))
		}
		w.Write(LEVEL_0, "\n")

		if describerSettings.ShowEvents {
			events, err := searchEvents(d.CoreV1(), configMap, describerSettings.ChunkSize)
			if err != nil {
				return err
			}
			if events != nil {
				DescribeEvents(events, w)
			}
		}
		return nil
	})
}

// NetworkPolicyDescriber generates information about a networkingv1.NetworkPolicy
type NetworkPolicyDescriber struct {
	clientset.Interface
}

func (d *NetworkPolicyDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	c := d.NetworkingV1().NetworkPolicies(namespace)

	networkPolicy, err := c.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return describeNetworkPolicy(networkPolicy)
}

func describeNetworkPolicy(networkPolicy *networkingv1.NetworkPolicy) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", networkPolicy.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", networkPolicy.Namespace)
		w.Write(LEVEL_0, "Created on:\t%s\n", networkPolicy.CreationTimestamp)
		printLabelsMultiline(w, "Labels", networkPolicy.Labels)
		printAnnotationsMultiline(w, "Annotations", networkPolicy.Annotations)
		describeNetworkPolicySpec(networkPolicy.Spec, w)
		return nil
	})
}

func describeNetworkPolicySpec(nps networkingv1.NetworkPolicySpec, w PrefixWriter) {
	w.Write(LEVEL_0, "Spec:\n")
	w.Write(LEVEL_1, "PodSelector: ")
	if len(nps.PodSelector.MatchLabels) == 0 && len(nps.PodSelector.MatchExpressions) == 0 {
		w.Write(LEVEL_2, "<none> (Allowing the specific traffic to all pods in this namespace)\n")
	} else {
		w.Write(LEVEL_2, "%s\n", metav1.FormatLabelSelector(&nps.PodSelector))
	}

	ingressEnabled, egressEnabled := getPolicyType(nps)
	if ingressEnabled {
		w.Write(LEVEL_1, "Allowing ingress traffic:\n")
		printNetworkPolicySpecIngressFrom(nps.Ingress, "    ", w)
	} else {
		w.Write(LEVEL_1, "Not affecting ingress traffic\n")
	}
	if egressEnabled {
		w.Write(LEVEL_1, "Allowing egress traffic:\n")
		printNetworkPolicySpecEgressTo(nps.Egress, "    ", w)
	} else {
		w.Write(LEVEL_1, "Not affecting egress traffic\n")

	}
	w.Write(LEVEL_1, "Policy Types: %v\n", policyTypesToString(nps.PolicyTypes))
}

func getPolicyType(nps networkingv1.NetworkPolicySpec) (bool, bool) {
	var ingress, egress bool
	for _, pt := range nps.PolicyTypes {
		switch pt {
		case networkingv1.PolicyTypeIngress:
			ingress = true
		case networkingv1.PolicyTypeEgress:
			egress = true
		}
	}

	return ingress, egress
}

func printNetworkPolicySpecIngressFrom(npirs []networkingv1.NetworkPolicyIngressRule, initialIndent string, w PrefixWriter) {
	if len(npirs) == 0 {
		w.Write(LEVEL_0, "%s%s\n", initialIndent, "<none> (Selected pods are isolated for ingress connectivity)")
		return
	}
	for i, npir := range npirs {
		if len(npir.Ports) == 0 {
			w.Write(LEVEL_0, "%s%s\n", initialIndent, "To Port: <any> (traffic allowed to all ports)")
		} else {
			for _, port := range npir.Ports {
				var proto corev1.Protocol
				if port.Protocol != nil {
					proto = *port.Protocol
				} else {
					proto = corev1.ProtocolTCP
				}
				if port.EndPort == nil {
					w.Write(LEVEL_0, "%s%s: %s/%s\n", initialIndent, "To Port", port.Port, proto)
				} else {
					w.Write(LEVEL_0, "%s%s: %s-%d/%s\n", initialIndent, "To Port Range", port.Port, *port.EndPort, proto)
				}
			}
		}
		if len(npir.From) == 0 {
			w.Write(LEVEL_0, "%s%s\n", initialIndent, "From: <any> (traffic not restricted by source)")
		} else {
			for _, from := range npir.From {
				w.Write(LEVEL_0, "%s%s\n", initialIndent, "From:")
				if from.PodSelector != nil && from.NamespaceSelector != nil {
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "NamespaceSelector", metav1.FormatLabelSelector(from.NamespaceSelector))
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "PodSelector", metav1.FormatLabelSelector(from.PodSelector))
				} else if from.PodSelector != nil {
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "PodSelector", metav1.FormatLabelSelector(from.PodSelector))
				} else if from.NamespaceSelector != nil {
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "NamespaceSelector", metav1.FormatLabelSelector(from.NamespaceSelector))
				} else if from.IPBlock != nil {
					w.Write(LEVEL_1, "%sIPBlock:\n", initialIndent)
					w.Write(LEVEL_2, "%sCIDR: %s\n", initialIndent, from.IPBlock.CIDR)
					w.Write(LEVEL_2, "%sExcept: %v\n", initialIndent, strings.Join(from.IPBlock.Except, ", "))
				}
			}
		}
		if i != len(npirs)-1 {
			w.Write(LEVEL_0, "%s%s\n", initialIndent, "----------")
		}
	}
}

func printNetworkPolicySpecEgressTo(npers []networkingv1.NetworkPolicyEgressRule, initialIndent string, w PrefixWriter) {
	if len(npers) == 0 {
		w.Write(LEVEL_0, "%s%s\n", initialIndent, "<none> (Selected pods are isolated for egress connectivity)")
		return
	}
	for i, nper := range npers {
		if len(nper.Ports) == 0 {
			w.Write(LEVEL_0, "%s%s\n", initialIndent, "To Port: <any> (traffic allowed to all ports)")
		} else {
			for _, port := range nper.Ports {
				var proto corev1.Protocol
				if port.Protocol != nil {
					proto = *port.Protocol
				} else {
					proto = corev1.ProtocolTCP
				}
				if port.EndPort == nil {
					w.Write(LEVEL_0, "%s%s: %s/%s\n", initialIndent, "To Port", port.Port, proto)
				} else {
					w.Write(LEVEL_0, "%s%s: %s-%d/%s\n", initialIndent, "To Port Range", port.Port, *port.EndPort, proto)
				}
			}
		}
		if len(nper.To) == 0 {
			w.Write(LEVEL_0, "%s%s\n", initialIndent, "To: <any> (traffic not restricted by destination)")
		} else {
			for _, to := range nper.To {
				w.Write(LEVEL_0, "%s%s\n", initialIndent, "To:")
				if to.PodSelector != nil && to.NamespaceSelector != nil {
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "NamespaceSelector", metav1.FormatLabelSelector(to.NamespaceSelector))
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "PodSelector", metav1.FormatLabelSelector(to.PodSelector))
				} else if to.PodSelector != nil {
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "PodSelector", metav1.FormatLabelSelector(to.PodSelector))
				} else if to.NamespaceSelector != nil {
					w.Write(LEVEL_1, "%s%s: %s\n", initialIndent, "NamespaceSelector", metav1.FormatLabelSelector(to.NamespaceSelector))
				} else if to.IPBlock != nil {
					w.Write(LEVEL_1, "%sIPBlock:\n", initialIndent)
					w.Write(LEVEL_2, "%sCIDR: %s\n", initialIndent, to.IPBlock.CIDR)
					w.Write(LEVEL_2, "%sExcept: %v\n", initialIndent, strings.Join(to.IPBlock.Except, ", "))
				}
			}
		}
		if i != len(npers)-1 {
			w.Write(LEVEL_0, "%s%s\n", initialIndent, "----------")
		}
	}
}

type StorageClassDescriber struct {
	clientset.Interface
}

func (s *StorageClassDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	sc, err := s.StorageV1().StorageClasses().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(s.CoreV1(), sc, describerSettings.ChunkSize)
	}

	return describeStorageClass(sc, events)
}

func describeStorageClass(sc *storagev1.StorageClass, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", sc.Name)
		w.Write(LEVEL_0, "IsDefaultClass:\t%s\n", storageutil.IsDefaultAnnotationText(sc.ObjectMeta))
		w.Write(LEVEL_0, "Annotations:\t%s\n", labels.FormatLabels(sc.Annotations))
		w.Write(LEVEL_0, "Provisioner:\t%s\n", sc.Provisioner)
		w.Write(LEVEL_0, "Parameters:\t%s\n", labels.FormatLabels(sc.Parameters))
		w.Write(LEVEL_0, "AllowVolumeExpansion:\t%s\n", printBoolPtr(sc.AllowVolumeExpansion))
		if len(sc.MountOptions) == 0 {
			w.Write(LEVEL_0, "MountOptions:\t<none>\n")
		} else {
			w.Write(LEVEL_0, "MountOptions:\n")
			for _, option := range sc.MountOptions {
				w.Write(LEVEL_1, "%s\n", option)
			}
		}
		if sc.ReclaimPolicy != nil {
			w.Write(LEVEL_0, "ReclaimPolicy:\t%s\n", *sc.ReclaimPolicy)
		}
		if sc.VolumeBindingMode != nil {
			w.Write(LEVEL_0, "VolumeBindingMode:\t%s\n", *sc.VolumeBindingMode)
		}
		if sc.AllowedTopologies != nil {
			printAllowedTopologies(w, sc.AllowedTopologies)
		}
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

type VolumeAttributesClassDescriber struct {
	clientset.Interface
}

func (d *VolumeAttributesClassDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	vac, err := d.StorageV1beta1().VolumeAttributesClasses().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(d.CoreV1(), vac, describerSettings.ChunkSize)
	}

	return describeVolumeAttributesClass(vac, events)
}

func describeVolumeAttributesClass(vac *storagev1beta1.VolumeAttributesClass, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", vac.Name)
		w.Write(LEVEL_0, "Annotations:\t%s\n", labels.FormatLabels(vac.Annotations))
		w.Write(LEVEL_0, "DriverName:\t%s\n", vac.DriverName)
		w.Write(LEVEL_0, "Parameters:\t%s\n", labels.FormatLabels(vac.Parameters))

		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

type CSINodeDescriber struct {
	clientset.Interface
}

func (c *CSINodeDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	csi, err := c.StorageV1().CSINodes().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(c.CoreV1(), csi, describerSettings.ChunkSize)
	}

	return describeCSINode(csi, events)
}

func describeCSINode(csi *storagev1.CSINode, events *corev1.EventList) (output string, err error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", csi.GetName())
		printLabelsMultiline(w, "Labels", csi.GetLabels())
		printAnnotationsMultiline(w, "Annotations", csi.GetAnnotations())
		w.Write(LEVEL_0, "CreationTimestamp:\t%s\n", csi.CreationTimestamp.Time.Format(time.RFC1123Z))
		w.Write(LEVEL_0, "Spec:\n")
		if csi.Spec.Drivers != nil {
			w.Write(LEVEL_1, "Drivers:\n")
			for _, driver := range csi.Spec.Drivers {
				w.Write(LEVEL_2, "%s:\n", driver.Name)
				w.Write(LEVEL_3, "Node ID:\t%s\n", driver.NodeID)
				if driver.Allocatable != nil && driver.Allocatable.Count != nil {
					w.Write(LEVEL_3, "Allocatables:\n")
					w.Write(LEVEL_4, "Count:\t%d\n", *driver.Allocatable.Count)
				}
				if driver.TopologyKeys != nil {
					w.Write(LEVEL_3, "Topology Keys:\t%s\n", driver.TopologyKeys)
				}
			}
		}
		if events != nil {
			DescribeEvents(events, w)
		}
		return nil
	})
}

func printAllowedTopologies(w PrefixWriter, topologies []corev1.TopologySelectorTerm) {
	w.Write(LEVEL_0, "AllowedTopologies:\t")
	if len(topologies) == 0 {
		w.WriteLine("<none>")
		return
	}
	w.WriteLine("")
	for i, term := range topologies {
		printTopologySelectorTermsMultilineWithIndent(w, LEVEL_1, fmt.Sprintf("Term %d", i), "\t", term.MatchLabelExpressions)
	}
}

func printTopologySelectorTermsMultilineWithIndent(w PrefixWriter, indentLevel int, title, innerIndent string, reqs []corev1.TopologySelectorLabelRequirement) {
	w.Write(indentLevel, "%s:%s", title, innerIndent)

	if len(reqs) == 0 {
		w.WriteLine("<none>")
		return
	}

	for i, req := range reqs {
		if i != 0 {
			w.Write(indentLevel, "%s", innerIndent)
		}
		exprStr := fmt.Sprintf("%s %s", req.Key, "in")
		if len(req.Values) > 0 {
			exprStr = fmt.Sprintf("%s [%s]", exprStr, strings.Join(req.Values, ", "))
		}
		w.Write(LEVEL_0, "%s\n", exprStr)
	}
}

type PodDisruptionBudgetDescriber struct {
	clientset.Interface
}

func (p *PodDisruptionBudgetDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	var (
		pdbv1      *policyv1.PodDisruptionBudget
		pdbv1beta1 *policyv1beta1.PodDisruptionBudget
		err        error
	)

	pdbv1, err = p.PolicyV1().PodDisruptionBudgets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		var events *corev1.EventList
		if describerSettings.ShowEvents {
			events, _ = searchEvents(p.CoreV1(), pdbv1, describerSettings.ChunkSize)
		}
		return describePodDisruptionBudgetV1(pdbv1, events)
	}

	// try falling back to v1beta1 in NotFound error cases
	if apierrors.IsNotFound(err) {
		pdbv1beta1, err = p.PolicyV1beta1().PodDisruptionBudgets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}
	if err == nil {
		var events *corev1.EventList
		if describerSettings.ShowEvents {
			events, _ = searchEvents(p.CoreV1(), pdbv1beta1, describerSettings.ChunkSize)
		}
		return describePodDisruptionBudgetV1beta1(pdbv1beta1, events)
	}

	return "", err
}

func describePodDisruptionBudgetV1(pdb *policyv1.PodDisruptionBudget, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", pdb.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", pdb.Namespace)

		if pdb.Spec.MinAvailable != nil {
			w.Write(LEVEL_0, "Min available:\t%s\n", pdb.Spec.MinAvailable.String())
		} else if pdb.Spec.MaxUnavailable != nil {
			w.Write(LEVEL_0, "Max unavailable:\t%s\n", pdb.Spec.MaxUnavailable.String())
		}

		if pdb.Spec.Selector != nil {
			w.Write(LEVEL_0, "Selector:\t%s\n", metav1.FormatLabelSelector(pdb.Spec.Selector))
		} else {
			w.Write(LEVEL_0, "Selector:\t<unset>\n")
		}
		w.Write(LEVEL_0, "Status:\n")
		w.Write(LEVEL_2, "Allowed disruptions:\t%d\n", pdb.Status.DisruptionsAllowed)
		w.Write(LEVEL_2, "Current:\t%d\n", pdb.Status.CurrentHealthy)
		w.Write(LEVEL_2, "Desired:\t%d\n", pdb.Status.DesiredHealthy)
		w.Write(LEVEL_2, "Total:\t%d\n", pdb.Status.ExpectedPods)
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

func describePodDisruptionBudgetV1beta1(pdb *policyv1beta1.PodDisruptionBudget, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", pdb.Name)
		w.Write(LEVEL_0, "Namespace:\t%s\n", pdb.Namespace)

		if pdb.Spec.MinAvailable != nil {
			w.Write(LEVEL_0, "Min available:\t%s\n", pdb.Spec.MinAvailable.String())
		} else if pdb.Spec.MaxUnavailable != nil {
			w.Write(LEVEL_0, "Max unavailable:\t%s\n", pdb.Spec.MaxUnavailable.String())
		}

		if pdb.Spec.Selector != nil {
			w.Write(LEVEL_0, "Selector:\t%s\n", metav1.FormatLabelSelector(pdb.Spec.Selector))
		} else {
			w.Write(LEVEL_0, "Selector:\t<unset>\n")
		}
		w.Write(LEVEL_0, "Status:\n")
		w.Write(LEVEL_2, "Allowed disruptions:\t%d\n", pdb.Status.DisruptionsAllowed)
		w.Write(LEVEL_2, "Current:\t%d\n", pdb.Status.CurrentHealthy)
		w.Write(LEVEL_2, "Desired:\t%d\n", pdb.Status.DesiredHealthy)
		w.Write(LEVEL_2, "Total:\t%d\n", pdb.Status.ExpectedPods)
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

// PriorityClassDescriber generates information about a PriorityClass.
type PriorityClassDescriber struct {
	clientset.Interface
}

func (s *PriorityClassDescriber) Describe(namespace, name string, describerSettings DescriberSettings) (string, error) {
	pc, err := s.SchedulingV1().PriorityClasses().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var events *corev1.EventList
	if describerSettings.ShowEvents {
		events, _ = searchEvents(s.CoreV1(), pc, describerSettings.ChunkSize)
	}

	return describePriorityClass(pc, events)
}

func describePriorityClass(pc *schedulingv1.PriorityClass, events *corev1.EventList) (string, error) {
	return tabbedString(func(out io.Writer) error {
		w := NewPrefixWriter(out)
		w.Write(LEVEL_0, "Name:\t%s\n", pc.Name)
		w.Write(LEVEL_0, "Value:\t%v\n", pc.Value)
		w.Write(LEVEL_0, "GlobalDefault:\t%v\n", pc.GlobalDefault)
		w.Write(LEVEL_0, "PreemptionPolicy:\t%s\n", *pc.PreemptionPolicy)
		w.Write(LEVEL_0, "Description:\t%s\n", pc.Description)

		w.Write(LEVEL_0, "Annotations:\t%s\n", labels.FormatLabels(pc.Annotations))
		if events != nil {
			DescribeEvents(events, w)
		}

		return nil
	})
}

func stringOrNone(s string) string {
	return stringOrDefaultValue(s, "<none>")
}

func stringOrDefaultValue(s, defaultValue string) string {
	if len(s) > 0 {
		return s
	}
	return defaultValue
}

func policyTypesToString(pts []networkingv1.PolicyType) string {
	formattedString := ""
	if pts != nil {
		strPts := []string{}
		for _, p := range pts {
			strPts = append(strPts, string(p))
		}
		formattedString = strings.Join(strPts, ", ")
	}
	return stringOrNone(formattedString)
}

// newErrNoDescriber creates a new ErrNoDescriber with the names of the provided types.
func newErrNoDescriber(types ...reflect.Type) error {
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, t.String())
	}
	return ErrNoDescriber{Types: names}
}

// Describers implements ObjectDescriber against functions registered via Add. Those functions can
// be strongly typed. Types are exactly matched (no conversion or assignable checks).
type Describers struct {
	searchFns map[reflect.Type][]typeFunc
}

// DescribeObject implements ObjectDescriber and will attempt to print the provided object to a string,
// if at least one describer function has been registered with the exact types passed, or if any
// describer can print the exact object in its first argument (the remainder will be provided empty
// values). If no function registered with Add can satisfy the passed objects, an ErrNoDescriber will
// be returned
// TODO: reorder and partial match extra.
func (d *Describers) DescribeObject(exact interface{}, extra ...interface{}) (string, error) {
	exactType := reflect.TypeOf(exact)
	fns, ok := d.searchFns[exactType]
	if !ok {
		return "", newErrNoDescriber(exactType)
	}
	if len(extra) == 0 {
		for _, typeFn := range fns {
			if len(typeFn.Extra) == 0 {
				return typeFn.Describe(exact, extra...)
			}
		}
		typeFn := fns[0]
		for _, t := range typeFn.Extra {
			v := reflect.New(t).Elem()
			extra = append(extra, v.Interface())
		}
		return fns[0].Describe(exact, extra...)
	}

	types := make([]reflect.Type, 0, len(extra))
	for _, obj := range extra {
		types = append(types, reflect.TypeOf(obj))
	}
	for _, typeFn := range fns {
		if typeFn.Matches(types) {
			return typeFn.Describe(exact, extra...)
		}
	}
	return "", newErrNoDescriber(append([]reflect.Type{exactType}, types...)...)
}

// Add adds one or more describer functions to the Describer. The passed function must
// match the signature:
//
//	func(...) (string, error)
//
// Any number of arguments may be provided.
func (d *Describers) Add(fns ...interface{}) error {
	for _, fn := range fns {
		fv := reflect.ValueOf(fn)
		ft := fv.Type()
		if ft.Kind() != reflect.Func {
			return fmt.Errorf("expected func, got: %v", ft)
		}
		numIn := ft.NumIn()
		if numIn == 0 {
			return fmt.Errorf("expected at least one 'in' params, got: %v", ft)
		}
		if ft.NumOut() != 2 {
			return fmt.Errorf("expected two 'out' params - (string, error), got: %v", ft)
		}
		types := make([]reflect.Type, 0, numIn)
		for i := 0; i < numIn; i++ {
			types = append(types, ft.In(i))
		}
		if ft.Out(0) != reflect.TypeOf(string("")) {
			return fmt.Errorf("expected string return, got: %v", ft)
		}
		var forErrorType error
		// This convolution is necessary, otherwise TypeOf picks up on the fact
		// that forErrorType is nil.
		errorType := reflect.TypeOf(&forErrorType).Elem()
		if ft.Out(1) != errorType {
			return fmt.Errorf("expected error return, got: %v", ft)
		}

		exact := types[0]
		extra := types[1:]
		if d.searchFns == nil {
			d.searchFns = make(map[reflect.Type][]typeFunc)
		}
		fns := d.searchFns[exact]
		fn := typeFunc{Extra: extra, Fn: fv}
		fns = append(fns, fn)
		d.searchFns[exact] = fns
	}
	return nil
}

// typeFunc holds information about a describer function and the types it accepts
type typeFunc struct {
	Extra []reflect.Type
	Fn    reflect.Value
}

// Matches returns true when the passed types exactly match the Extra list.
func (fn typeFunc) Matches(types []reflect.Type) bool {
	if len(fn.Extra) != len(types) {
		return false
	}
	// reorder the items in array types and fn.Extra
	// convert the type into string and sort them, check if they are matched
	varMap := make(map[reflect.Type]bool)
	for i := range fn.Extra {
		varMap[fn.Extra[i]] = true
	}
	for i := range types {
		if _, found := varMap[types[i]]; !found {
			return false
		}
	}
	return true
}

// Describe invokes the nested function with the exact number of arguments.
func (fn typeFunc) Describe(exact interface{}, extra ...interface{}) (string, error) {
	values := []reflect.Value{reflect.ValueOf(exact)}
	for i, obj := range extra {
		if obj != nil {
			values = append(values, reflect.ValueOf(obj))
		} else {
			values = append(values, reflect.New(fn.Extra[i]).Elem())
		}
	}
	out := fn.Fn.Call(values)
	s := out[0].Interface().(string)
	var err error
	if !out[1].IsNil() {
		err = out[1].Interface().(error)
	}
	return s, err
}

// printLabelsMultiline prints multiple labels with a proper alignment.
func printLabelsMultiline(w PrefixWriter, title string, labels map[string]string) {
	printLabelsMultilineWithIndent(w, "", title, "\t", labels, sets.New[string]())
}

// printLabelsMultiline prints multiple labels with a user-defined alignment.
func printLabelsMultilineWithIndent(w PrefixWriter, initialIndent, title, innerIndent string, labels map[string]string, skip sets.Set[string]) {
	w.Write(LEVEL_0, "%s%s:%s", initialIndent, title, innerIndent)

	if len(labels) == 0 {
		w.WriteLine("<none>")
		return
	}

	// to print labels in the sorted order
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if skip.Has(key) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		w.WriteLine("<none>")
		return
	}
	sort.Strings(keys)

	for i, key := range keys {
		if i != 0 {
			w.Write(LEVEL_0, "%s", initialIndent)
			w.Write(LEVEL_0, "%s", innerIndent)
		}
		w.Write(LEVEL_0, "%s=%s\n", key, labels[key])
	}
}

// printTaintsMultiline prints multiple taints with a proper alignment.
func printNodeTaintsMultiline(w PrefixWriter, title string, taints []corev1.Taint) {
	printTaintsMultilineWithIndent(w, "", title, "\t", taints)
}

// printTaintsMultilineWithIndent prints multiple taints with a user-defined alignment.
func printTaintsMultilineWithIndent(w PrefixWriter, initialIndent, title, innerIndent string, taints []corev1.Taint) {
	w.Write(LEVEL_0, "%s%s:%s", initialIndent, title, innerIndent)

	if len(taints) == 0 {
		w.WriteLine("<none>")
		return
	}

	// to print taints in the sorted order
	sort.Slice(taints, func(i, j int) bool {
		cmpKey := func(taint corev1.Taint) string {
			return string(taint.Effect) + "," + taint.Key
		}
		return cmpKey(taints[i]) < cmpKey(taints[j])
	})

	for i, taint := range taints {
		if i != 0 {
			w.Write(LEVEL_0, "%s", initialIndent)
			w.Write(LEVEL_0, "%s", innerIndent)
		}
		w.Write(LEVEL_0, "%s\n", taint.ToString())
	}
}

// printPodsMultiline prints multiple pods with a proper alignment.
func printPodsMultiline(w PrefixWriter, title string, pods []corev1.Pod) {
	printPodsMultilineWithIndent(w, "", title, "\t", pods)
}

// printPodsMultilineWithIndent prints multiple pods with a user-defined alignment.
func printPodsMultilineWithIndent(w PrefixWriter, initialIndent, title, innerIndent string, pods []corev1.Pod) {
	w.Write(LEVEL_0, "%s%s:%s", initialIndent, title, innerIndent)

	if len(pods) == 0 {
		w.WriteLine("<none>")
		return
	}

	// to print pods in the sorted order
	sort.Slice(pods, func(i, j int) bool {
		cmpKey := func(pod corev1.Pod) string {
			return pod.Name
		}
		return cmpKey(pods[i]) < cmpKey(pods[j])
	})

	for i, pod := range pods {
		if i != 0 {
			w.Write(LEVEL_0, "%s", initialIndent)
			w.Write(LEVEL_0, "%s", innerIndent)
		}
		w.Write(LEVEL_0, "%s\n", pod.Name)
	}
}

// printPodTolerationsMultiline prints multiple tolerations with a proper alignment.
func printPodTolerationsMultiline(w PrefixWriter, title string, tolerations []corev1.Toleration) {
	printTolerationsMultilineWithIndent(w, "", title, "\t", tolerations)
}

// printTolerationsMultilineWithIndent prints multiple tolerations with a user-defined alignment.
func printTolerationsMultilineWithIndent(w PrefixWriter, initialIndent, title, innerIndent string, tolerations []corev1.Toleration) {
	w.Write(LEVEL_0, "%s%s:%s", initialIndent, title, innerIndent)

	if len(tolerations) == 0 {
		w.WriteLine("<none>")
		return
	}

	// to print tolerations in the sorted order
	sort.Slice(tolerations, func(i, j int) bool {
		return tolerations[i].Key < tolerations[j].Key
	})

	for i, toleration := range tolerations {
		if i != 0 {
			w.Write(LEVEL_0, "%s", initialIndent)
			w.Write(LEVEL_0, "%s", innerIndent)
		}
		w.Write(LEVEL_0, "%s", toleration.Key)
		if len(toleration.Value) != 0 {
			w.Write(LEVEL_0, "=%s", toleration.Value)
		}
		if len(toleration.Effect) != 0 {
			w.Write(LEVEL_0, ":%s", toleration.Effect)
		}
		// tolerations:
		// - operator: "Exists"
		// is a special case which tolerates everything
		if toleration.Operator == corev1.TolerationOpExists && len(toleration.Value) == 0 {
			if len(toleration.Key) != 0 || len(toleration.Effect) != 0 {
				w.Write(LEVEL_0, " op=Exists")
			} else {
				w.Write(LEVEL_0, "op=Exists")
			}
		}

		if toleration.TolerationSeconds != nil {
			w.Write(LEVEL_0, " for %ds", *toleration.TolerationSeconds)
		}
		w.Write(LEVEL_0, "\n")
	}
}

type flusher interface {
	Flush()
}

func tabbedString(f func(io.Writer) error) (string, error) {
	out := new(tabwriter.Writer)
	buf := &bytes.Buffer{}
	out.Init(buf, 0, 8, 2, ' ', 0)

	err := f(out)
	if err != nil {
		return "", err
	}

	out.Flush()
	return buf.String(), nil
}

type SortableResourceNames []corev1.ResourceName

func (list SortableResourceNames) Len() int {
	return len(list)
}

func (list SortableResourceNames) Swap(i, j int) {
	list[i], list[j] = list[j], list[i]
}

func (list SortableResourceNames) Less(i, j int) bool {
	return list[i] < list[j]
}

// SortedResourceNames returns the sorted resource names of a resource list.
func SortedResourceNames(list corev1.ResourceList) []corev1.ResourceName {
	resources := make([]corev1.ResourceName, 0, len(list))
	for res := range list {
		resources = append(resources, res)
	}
	sort.Sort(SortableResourceNames(resources))
	return resources
}

type SortableResourceQuotas []corev1.ResourceQuota

func (list SortableResourceQuotas) Len() int {
	return len(list)
}

func (list SortableResourceQuotas) Swap(i, j int) {
	list[i], list[j] = list[j], list[i]
}

func (list SortableResourceQuotas) Less(i, j int) bool {
	return list[i].Name < list[j].Name
}

type SortableVolumeMounts []corev1.VolumeMount

func (list SortableVolumeMounts) Len() int {
	return len(list)
}

func (list SortableVolumeMounts) Swap(i, j int) {
	list[i], list[j] = list[j], list[i]
}

func (list SortableVolumeMounts) Less(i, j int) bool {
	return list[i].MountPath < list[j].MountPath
}

type SortableVolumeDevices []corev1.VolumeDevice

func (list SortableVolumeDevices) Len() int {
	return len(list)
}

func (list SortableVolumeDevices) Swap(i, j int) {
	list[i], list[j] = list[j], list[i]
}

func (list SortableVolumeDevices) Less(i, j int) bool {
	return list[i].DevicePath < list[j].DevicePath
}

var maxAnnotationLen = 140

// printAnnotationsMultiline prints multiple annotations with a proper alignment.
// If annotation string is too long, we omit chars more than 200 length.
func printAnnotationsMultiline(w PrefixWriter, title string, annotations map[string]string) {
	w.Write(LEVEL_0, "%s:\t", title)

	// to print labels in the sorted order
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		if skipAnnotations.Has(key) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		w.WriteLine("<none>")
		return
	}
	sort.Strings(keys)
	indent := "\t"
	for i, key := range keys {
		if i != 0 {
			w.Write(LEVEL_0, indent)
		}
		value := strings.TrimSuffix(annotations[key], "\n")
		if (len(value)+len(key)+2) > maxAnnotationLen || strings.Contains(value, "\n") {
			w.Write(LEVEL_0, "%s:\n", key)
			for _, s := range strings.Split(value, "\n") {
				w.Write(LEVEL_0, "%s  %s\n", indent, shorten(s, maxAnnotationLen-2))
			}
		} else {
			w.Write(LEVEL_0, "%s: %s\n", key, value)
		}
	}
}

func shorten(s string, maxLength int) string {
	if len(s) > maxLength {
		return s[:maxLength] + "..."
	}
	return s
}

// translateMicroTimestampSince returns the elapsed time since timestamp in
// human-readable approximation.
func translateMicroTimestampSince(timestamp metav1.MicroTime) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}

	return duration.HumanDuration(time.Since(timestamp.Time))
}

// translateTimestampSince returns the elapsed time since timestamp in
// human-readable approximation.
func translateTimestampSince(timestamp metav1.Time) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}

	return duration.HumanDuration(time.Since(timestamp.Time))
}

// Pass ports=nil for all ports.
func formatEndpointSlices(endpointSlices []discoveryv1.EndpointSlice, ports sets.Set[string]) string {
	if len(endpointSlices) == 0 {
		return "<none>"
	}
	var list []string
	max := 3
	more := false
	count := 0
	for i := range endpointSlices {
		if len(endpointSlices[i].Ports) == 0 {
			// It's possible to have headless services with no ports.
			for j := range endpointSlices[i].Endpoints {
				if len(list) == max {
					more = true
				}
				isReady := endpointSlices[i].Endpoints[j].Conditions.Ready == nil || *endpointSlices[i].Endpoints[j].Conditions.Ready
				if !isReady {
					// ready indicates that this endpoint is prepared to receive traffic,
					// according to whatever system is managing the endpoint. A nil value
					// indicates an unknown state. In most cases consumers should interpret this
					// unknown state as ready.
					// More info: vendor/k8s.io/api/discovery/v1/types.go
					continue
				}
				if !more {
					list = append(list, endpointSlices[i].Endpoints[j].Addresses[0])
				}
				count++
			}
		} else {
			// "Normal" services with ports defined.
			for j := range endpointSlices[i].Ports {
				port := endpointSlices[i].Ports[j]
				if ports == nil || ports.Has(*port.Name) {
					for k := range endpointSlices[i].Endpoints {
						if len(list) == max {
							more = true
						}
						addr := endpointSlices[i].Endpoints[k].Addresses[0]
						isReady := endpointSlices[i].Endpoints[k].Conditions.Ready == nil || *endpointSlices[i].Endpoints[k].Conditions.Ready
						if !isReady {
							// ready indicates that this endpoint is prepared to receive traffic,
							// according to whatever system is managing the endpoint. A nil value
							// indicates an unknown state. In most cases consumers should interpret this
							// unknown state as ready.
							// More info: vendor/k8s.io/api/discovery/v1/types.go
							continue
						}
						if !more {
							hostPort := net.JoinHostPort(addr, strconv.Itoa(int(*port.Port)))
							list = append(list, hostPort)
						}
						count++
					}
				}
			}
		}
	}
	ret := strings.Join(list, ",")
	if more {
		return fmt.Sprintf("%s + %d more...", ret, count-max)
	}
	return ret
}

func extractCSRStatus(conditions []string, certificateBytes []byte) string {
	var approved, denied, failed bool
	for _, c := range conditions {
		switch c {
		case string(certificatesv1beta1.CertificateApproved):
			approved = true
		case string(certificatesv1beta1.CertificateDenied):
			denied = true
		case string(certificatesv1beta1.CertificateFailed):
			failed = true
		}
	}
	var status string
	// must be in order of precedence
	if denied {
		status += "Denied"
	} else if approved {
		status += "Approved"
	} else {
		status += "Pending"
	}
	if failed {
		status += ",Failed"
	}
	if len(certificateBytes) > 0 {
		status += ",Issued"
	}
	return status
}

// backendStringer behaves just like a string interface and converts the given backend to a string.
func serviceBackendStringer(backend *networkingv1.IngressServiceBackend) string {
	if backend == nil {
		return ""
	}
	var bPort string
	if backend.Port.Number != 0 {
		sNum := int64(backend.Port.Number)
		bPort = strconv.FormatInt(sNum, 10)
	} else {
		bPort = backend.Port.Name
	}
	return fmt.Sprintf("%v:%v", backend.Name, bPort)
}

// backendStringer behaves just like a string interface and converts the given backend to a string.
func backendStringer(backend *networkingv1beta1.IngressBackend) string {
	if backend == nil {
		return ""
	}
	return fmt.Sprintf("%v:%v", backend.ServiceName, backend.ServicePort.String())
}

// findNodeRoles returns the roles of a given node.
// The roles are determined by looking for:
// * a node-role.kubernetes.io/<role>="" label
// * a kubernetes.io/role="<role>" label
func findNodeRoles(node *corev1.Node) []string {
	roles := sets.New[string]()
	for k, v := range node.Labels {
		switch {
		case strings.HasPrefix(k, LabelNodeRolePrefix):
			if role := strings.TrimPrefix(k, LabelNodeRolePrefix); len(role) > 0 {
				roles.Insert(role)
			}

		case k == NodeLabelRole && v != "":
			roles.Insert(v)
		}
	}
	return sets.List(roles)
}

// ingressLoadBalancerStatusStringerV1 behaves mostly like a string interface and converts the given status to a string.
// `wide` indicates whether the returned value is meant for --o=wide output. If not, it's clipped to 16 bytes.
func ingressLoadBalancerStatusStringerV1(s networkingv1.IngressLoadBalancerStatus, wide bool) string {
	ingress := s.Ingress
	result := sets.New[string]()
	for i := range ingress {
		if ingress[i].IP != "" {
			result.Insert(ingress[i].IP)
		} else if ingress[i].Hostname != "" {
			result.Insert(ingress[i].Hostname)
		}
	}

	r := strings.Join(sets.List(result), ",")
	if !wide && len(r) > LoadBalancerWidth {
		r = r[0:(LoadBalancerWidth-3)] + "..."
	}
	return r
}

// ingressLoadBalancerStatusStringerV1beta1 behaves mostly like a string interface and converts the given status to a string.
// `wide` indicates whether the returned value is meant for --o=wide output. If not, it's clipped to 16 bytes.
func ingressLoadBalancerStatusStringerV1beta1(s networkingv1beta1.IngressLoadBalancerStatus, wide bool) string {
	ingress := s.Ingress
	result := sets.New[string]()
	for i := range ingress {
		if ingress[i].IP != "" {
			result.Insert(ingress[i].IP)
		} else if ingress[i].Hostname != "" {
			result.Insert(ingress[i].Hostname)
		}
	}

	r := strings.Join(sets.List(result), ",")
	if !wide && len(r) > LoadBalancerWidth {
		r = r[0:(LoadBalancerWidth-3)] + "..."
	}
	return r
}

// searchEvents finds events about the specified object.
// It is very similar to CoreV1.Events.Search, but supports the Limit parameter.
func searchEvents(client corev1client.EventsGetter, objOrRef runtime.Object, limit int64) (*corev1.EventList, error) {
	ref, err := reference.GetReference(scheme.Scheme, objOrRef)
	if err != nil {
		return nil, err
	}
	stringRefKind := string(ref.Kind)
	var refKind *string
	if len(stringRefKind) > 0 {
		refKind = &stringRefKind
	}
	stringRefUID := string(ref.UID)
	var refUID *string
	if len(stringRefUID) > 0 {
		refUID = &stringRefUID
	}

	e := client.Events(ref.Namespace)
	fieldSelector := e.GetFieldSelector(&ref.Name, &ref.Namespace, refKind, refUID)
	initialOpts := metav1.ListOptions{FieldSelector: fieldSelector.String(), Limit: limit}
	eventList := &corev1.EventList{}
	err = runtimeresource.FollowContinue(&initialOpts,
		func(options metav1.ListOptions) (runtime.Object, error) {
			newEvents, err := e.List(context.TODO(), options)
			if err != nil {
				return nil, runtimeresource.EnhanceListError(err, options, "events")
			}
			eventList.Items = append(eventList.Items, newEvents.Items...)
			return newEvents, nil
		})
	return eventList, err
}
