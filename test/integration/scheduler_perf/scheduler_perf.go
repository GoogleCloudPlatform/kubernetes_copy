/*
Copyright 2023 The Kubernetes Authors.

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

package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/validation"
	schedframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
	"k8s.io/kubernetes/test/integration/framework"
	testutils "k8s.io/kubernetes/test/utils"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/kubernetes/test/utils/ktesting/initoption"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

type operationCode string

const (
	allocResourceClaimsOpcode    operationCode = "allocResourceClaims"
	createAnyOpcode              operationCode = "createAny"
	createNodesOpcode            operationCode = "createNodes"
	createNamespacesOpcode       operationCode = "createNamespaces"
	createPodsOpcode             operationCode = "createPods"
	createPodSetsOpcode          operationCode = "createPodSets"
	deletePodsOpcode             operationCode = "deletePods"
	createResourceClaimsOpcode   operationCode = "createResourceClaims"
	createResourceDriverOpcode   operationCode = "createResourceDriver"
	churnOpcode                  operationCode = "churn"
	updateAnyOpcode              operationCode = "updateAny"
	barrierOpcode                operationCode = "barrier"
	sleepOpcode                  operationCode = "sleep"
	startCollectingMetricsOpcode operationCode = "startCollectingMetrics"
	stopCollectingMetricsOpcode  operationCode = "stopCollectingMetrics"
)

const (
	// Two modes supported in "churn" operator.

	// Create continuously create API objects without deleting them.
	Create = "create"
	// Recreate creates a number of API objects and then delete them, and repeat the iteration.
	Recreate = "recreate"
)

const (
	extensionPointsLabelName = "extension_point"
	resultLabelName          = "result"
	pluginLabelName          = "plugin"
	eventLabelName           = "event"
)

// Run with -v=2, this is the default log level in production.
//
// In a PR this can be bumped up temporarily to run pull-kubernetes-scheduler-perf
// with more log output.
const DefaultLoggingVerbosity = 2

var LoggingFeatureGate FeatureGateFlag
var LoggingConfig *logsapi.LoggingConfiguration

type FeatureGateFlag interface {
	featuregate.FeatureGate
	flag.Value
}

func init() {
	f := featuregate.NewFeatureGate()
	runtime.Must(logsapi.AddFeatureGates(f))
	LoggingFeatureGate = f

	LoggingConfig = logsapi.NewLoggingConfiguration()
	LoggingConfig.Verbosity = DefaultLoggingVerbosity
}

var (
	defaultMetricsCollectorConfig = metricsCollectorConfig{
		Metrics: map[string][]*labelValues{
			"scheduler_framework_extension_point_duration_seconds": {
				{
					label:  extensionPointsLabelName,
					values: metrics.ExtentionPoints,
				},
			},
			"scheduler_scheduling_attempt_duration_seconds": {
				{
					label:  resultLabelName,
					values: []string{metrics.ScheduledResult, metrics.UnschedulableResult, metrics.ErrorResult},
				},
			},
			"scheduler_pod_scheduling_duration_seconds": nil,
			"scheduler_plugin_execution_duration_seconds": {
				{
					label:  pluginLabelName,
					values: PluginNames,
				},
				{
					label:  extensionPointsLabelName,
					values: metrics.ExtentionPoints,
				},
			},
		},
	}

	qHintMetrics = map[string][]*labelValues{
		"scheduler_queueing_hint_execution_duration_seconds": {
			{
				label:  pluginLabelName,
				values: PluginNames,
			},
			{
				label:  eventLabelName,
				values: schedframework.AllClusterEventLabels(),
			},
		},
		"scheduler_event_handling_duration_seconds": {
			{
				label:  eventLabelName,
				values: schedframework.AllClusterEventLabels(),
			},
		},
	}

	// PluginNames is the names of the plugins that scheduler_perf collects metrics for.
	// We export this variable because people outside k/k may want to put their custom plugins.
	PluginNames = []string{
		names.PrioritySort,
		names.DefaultBinder,
		names.DefaultPreemption,
		names.DynamicResources,
		names.ImageLocality,
		names.InterPodAffinity,
		names.NodeAffinity,
		names.NodeName,
		names.NodePorts,
		names.NodeResourcesBalancedAllocation,
		names.NodeResourcesFit,
		names.NodeUnschedulable,
		names.NodeVolumeLimits,
		names.PodTopologySpread,
		names.SchedulingGates,
		names.TaintToleration,
		names.VolumeBinding,
		names.VolumeRestrictions,
		names.VolumeZone,
	}
)

var UseTestingLog bool
var PerfSchedulingLabelFilter string
var TestSchedulingLabelFilter string

// InitTests should be called in a TestMain in each config subdirectory.
func InitTests() error {
	// Run with -v=2, this is the default log level in production.
	ktesting.SetDefaultVerbosity(DefaultLoggingVerbosity)

	// test/integration/framework/flags.go unconditionally initializes the
	// logging flags. That's correct for most tests, but in the
	// scheduler_perf test we want more control over the flags, therefore
	// here strip them out.
	var fs flag.FlagSet
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		switch f.Name {
		case "log-flush-frequency", "v", "vmodule":
			// These will be added below ourselves, don't copy.
		default:
			fs.Var(f.Value, f.Name, f.Usage)
		}
	})
	flag.CommandLine = &fs

	flag.Var(LoggingFeatureGate, "feature-gate",
		"A set of key=value pairs that describe feature gates for alpha/experimental features. "+
			"Options are:\n"+strings.Join(LoggingFeatureGate.KnownFeatures(), "\n"))

	flag.BoolVar(&UseTestingLog, "use-testing-log", false, "Write log entries with testing.TB.Log. This is more suitable for unit testing and debugging, but less realistic in real benchmarks.")
	flag.StringVar(&PerfSchedulingLabelFilter, "perf-scheduling-label-filter", "performance", "comma-separated list of labels which a testcase must have (no prefix or +) or must not have (-), used by BenchmarkPerfScheduling")
	flag.StringVar(&TestSchedulingLabelFilter, "test-scheduling-label-filter", "integration-test,-performance", "comma-separated list of labels which a testcase must have (no prefix or +) or must not have (-), used by TestScheduling")

	// This would fail if we hadn't removed the logging flags above.
	logsapi.AddGoFlags(LoggingConfig, flag.CommandLine)

	flag.Parse()

	logs.InitLogs()
	return logsapi.ValidateAndApply(LoggingConfig, LoggingFeatureGate)
}

func registerQHintMetrics() {
	for k, v := range qHintMetrics {
		defaultMetricsCollectorConfig.Metrics[k] = v
	}
}

func unregisterQHintMetrics() {
	for k := range qHintMetrics {
		delete(defaultMetricsCollectorConfig.Metrics, k)
	}
}

// testCase defines a set of test cases that intends to test the performance of
// similar workloads of varying sizes with shared overall settings such as
// feature gates and metrics collected.
type testCase struct {
	// Name of the testCase.
	Name string
	// Feature gates to set before running the test.
	// Optional
	FeatureGates map[featuregate.Feature]bool
	// List of metrics to collect. Defaults to
	// defaultMetricsCollectorConfig if unspecified.
	// Optional
	MetricsCollectorConfig *metricsCollectorConfig
	// Template for sequence of ops that each workload must follow. Each op will
	// be executed serially one after another. Each element of the list must be
	// createNodesOp, createPodsOp, or barrierOp.
	WorkloadTemplate []op
	// List of workloads to run under this testCase.
	Workloads []*workload
	// SchedulerConfigPath is the path of scheduler configuration
	// Optional
	SchedulerConfigPath string
	// Default path to spec file describing the pods to create.
	// This path can be overridden in createPodsOp by setting PodTemplatePath .
	// Optional
	DefaultPodTemplatePath *string
	// Labels can be used to enable or disable workloads inside this test case.
	Labels []string
	// DefaultThresholdMetricSelector defines default metric used for threshold comparison.
	// It is only populated to workloads without their ThresholdMetricSelector set.
	// If nil, the default metric is set to "SchedulingThroughput".
	// Optional
	DefaultThresholdMetricSelector *thresholdMetricSelector
}

func (tc *testCase) collectsMetrics() bool {
	for _, op := range tc.WorkloadTemplate {
		if op.realOp.collectsMetrics() {
			return true
		}
	}
	return false
}

func (tc *testCase) workloadNamesUnique() error {
	workloadUniqueNames := map[string]bool{}
	for _, w := range tc.Workloads {
		if workloadUniqueNames[w.Name] {
			return fmt.Errorf("%s: workload name %s is not unique", tc.Name, w.Name)
		}
		workloadUniqueNames[w.Name] = true
	}
	return nil
}

// workload is a subtest under a testCase that tests the scheduler performance
// for a certain ordering of ops. The set of nodes created and pods scheduled
// in a workload may be heterogeneous.
type workload struct {
	// Name of the workload.
	Name string
	// Values of parameters used in the workloadTemplate.
	Params params
	// Labels can be used to enable or disable a workload.
	Labels []string
	// Threshold is compared to average value of metric specified using thresholdMetricSelector.
	// The comparison is performed for op with CollectMetrics set to true.
	// If the measured value is below the threshold, the workload's test case will fail.
	// If set to zero, the threshold check is disabled.
	// Optional
	Threshold float64
	// ThresholdMetricSelector defines to what metric the Threshold should be compared.
	// If nil, the metric is set to DefaultThresholdMetricSelector of the testCase.
	// If DefaultThresholdMetricSelector is nil, the metric is set to "SchedulingThroughput".
	// Optional
	ThresholdMetricSelector *thresholdMetricSelector
	// Feature gates to set before running the workload.
	// Explicitly setting a feature in this map overrides the test case settings.
	// Optional
	FeatureGates map[featuregate.Feature]bool
}

func (w *workload) isValid(mcc *metricsCollectorConfig) error {
	if w.Threshold < 0 {
		return fmt.Errorf("invalid Threshold=%f; should be non-negative", w.Threshold)
	}

	return w.ThresholdMetricSelector.isValid(mcc)
}

func (w *workload) setDefaults(testCaseThresholdMetricSelector *thresholdMetricSelector) {
	if w.ThresholdMetricSelector != nil {
		return
	}
	if testCaseThresholdMetricSelector != nil {
		w.ThresholdMetricSelector = testCaseThresholdMetricSelector
		return
	}
	// By defult, SchedulingThroughput should be compared with the threshold.
	w.ThresholdMetricSelector = &thresholdMetricSelector{
		Name: "SchedulingThroughput",
	}
}

// thresholdMetricSelector defines the name and labels of metric to compare with threshold.
type thresholdMetricSelector struct {
	// Name of the metric is compared to "Metric" field in DataItem labels.
	Name string
	// Labels of the metric. All of them needs to match the metric's labels to assume equality.
	Labels map[string]string
	// ExpectLower defines whether the threshold should denote the maximum allowable value of the metric.
	// If false, the threshold defines minimum allowable value.
	// Optional
	ExpectLower bool
}

func (ms thresholdMetricSelector) isValid(mcc *metricsCollectorConfig) error {
	if ms.Name == "SchedulingThroughput" {
		return nil
	}

	if mcc == nil {
		mcc = &defaultMetricsCollectorConfig
	}

	labels, ok := mcc.Metrics[ms.Name]
	if !ok {
		return fmt.Errorf("the metric %v is targeted, but it's not collected during the test. Make sure the MetricsCollectorConfig is valid", ms.Name)
	}

	for _, labelsComb := range uniqueLVCombos(labels) {
		if labelsMatch(labelsComb, ms.Labels) {
			return nil
		}
	}
	return fmt.Errorf("no matching labels found for metric %v", ms.Name)
}

type params struct {
	params map[string]any
	// isUsed field records whether params is used or not.
	isUsed map[string]bool
}

// UnmarshalJSON is a custom unmarshaler for params.
//
// from(json):
//
//	{
//		"initNodes": 500,
//		"initPods": 50
//	}
//
// to:
//
//	params{
//		params: map[string]any{
//			"intNodes": 500,
//			"initPods": 50,
//		},
//		isUsed: map[string]bool{}, // empty map
//	}
func (p *params) UnmarshalJSON(b []byte) error {
	aux := map[string]any{}

	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}

	p.params = aux
	p.isUsed = map[string]bool{}
	return nil
}

// get retrieves the parameter as an integer
func (p params) get(key string) (int, error) {
	// JSON unmarshals integer constants in an "any" field as float.
	f, err := getParam[float64](p, key)
	if err != nil {
		return 0, err
	}
	return int(f), nil
}

// getParam retrieves the parameter as specific type. There is no conversion,
// so in practice this means that only types that JSON unmarshaling uses
// (float64, string, bool) work.
func getParam[T float64 | string | bool](p params, key string) (T, error) {
	p.isUsed[key] = true
	param, ok := p.params[key]
	var t T
	if !ok {
		return t, fmt.Errorf("parameter %s is undefined", key)
	}
	t, ok = param.(T)
	if !ok {
		return t, fmt.Errorf("parameter %s has the wrong type %T", key, param)
	}
	return t, nil
}

// unusedParams returns the names of unusedParams
func (w workload) unusedParams() []string {
	var ret []string
	for name := range w.Params.params {
		if !w.Params.isUsed[name] {
			ret = append(ret, name)
		}
	}
	return ret
}

// op is a dummy struct which stores the real op in itself.
type op struct {
	realOp realOp
}

// UnmarshalJSON is a custom unmarshaler for the op struct since we don't know
// which op we're decoding at runtime.
func (op *op) UnmarshalJSON(b []byte) error {
	possibleOps := map[operationCode]realOp{
		allocResourceClaimsOpcode:    &allocResourceClaimsOp{},
		createAnyOpcode:              &createAny{},
		createNodesOpcode:            &createNodesOp{},
		createNamespacesOpcode:       &createNamespacesOp{},
		createPodsOpcode:             &createPodsOp{},
		createPodSetsOpcode:          &createPodSetsOp{},
		deletePodsOpcode:             &deletePodsOp{},
		createResourceClaimsOpcode:   &createResourceClaimsOp{},
		createResourceDriverOpcode:   &createResourceDriverOp{},
		churnOpcode:                  &churnOp{},
		updateAnyOpcode:              &updateAny{},
		barrierOpcode:                &barrierOp{},
		sleepOpcode:                  &sleepOp{},
		startCollectingMetricsOpcode: &startCollectingMetricsOp{},
		stopCollectingMetricsOpcode:  &stopCollectingMetricsOp{},
		// TODO(#94601): add a delete nodes op to simulate scaling behaviour?
	}
	// First determine the opcode using lenient decoding (= ignore extra fields).
	var possibleOp struct {
		Opcode operationCode
	}
	if err := json.Unmarshal(b, &possibleOp); err != nil {
		return fmt.Errorf("decoding opcode from %s: %w", string(b), err)
	}
	realOp, ok := possibleOps[possibleOp.Opcode]
	if !ok {
		return fmt.Errorf("unknown opcode %q in %s", possibleOp.Opcode, string(b))
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(realOp); err != nil {
		return fmt.Errorf("decoding %s into %T: %w", string(b), realOp, err)
	}
	if err := realOp.isValid(true); err != nil {
		return fmt.Errorf("%s not valid for %T: %w", string(b), realOp, err)
	}
	op.realOp = realOp
	return nil
}

// realOp is an interface that is implemented by different structs. To evaluate
// the validity of ops at parse-time, a isValid function must be implemented.
type realOp interface {
	// isValid verifies the validity of the op args such as node/pod count. Note
	// that we don't catch undefined parameters at this stage.
	//
	// This returns errInvalidOp if the configured operation does not match.
	isValid(allowParameterization bool) error
	// collectsMetrics checks if the op collects metrics.
	collectsMetrics() bool
	// patchParams returns a patched realOp of the same type after substituting
	// parameterizable values with workload-specific values. One should implement
	// this method on the value receiver base type, not a pointer receiver base
	// type, even though calls will be made from with a *realOp. This is because
	// callers don't want the receiver to inadvertently modify the realOp
	// (instead, it's returned as a return value).
	patchParams(w *workload) (realOp, error)
}

// runnableOp is an interface implemented by some operations. It makes it posssible
// to execute the operation without having to add separate code into runWorkload.
type runnableOp interface {
	realOp

	// requiredNamespaces returns all namespaces that runWorkload must create
	// before running the operation.
	requiredNamespaces() []string
	// run executes the steps provided by the operation.
	run(ktesting.TContext)
}

func isValidParameterizable(val string) bool {
	return strings.HasPrefix(val, "$")
}

func isValidCount(allowParameterization bool, count int, countParam string) bool {
	if !allowParameterization || countParam == "" {
		// Ignore parameter. The value itself must be okay.
		return count >= 0
	}
	return isValidParameterizable(countParam)
}

// createNodesOp defines an op where nodes are created as a part of a workload.
type createNodesOp struct {
	// Must be "createNodes".
	Opcode operationCode
	// Number of nodes to create. Parameterizable through CountParam.
	Count int
	// Template parameter for Count.
	CountParam string
	// Path to spec file describing the nodes to create.
	// Optional
	NodeTemplatePath *string
	// At most one of the following strategies can be defined. Defaults
	// to TrivialNodePrepareStrategy if unspecified.
	// Optional
	NodeAllocatableStrategy  *testutils.NodeAllocatableStrategy
	LabelNodePrepareStrategy *testutils.LabelNodePrepareStrategy
	UniqueNodeLabelStrategy  *testutils.UniqueNodeLabelStrategy
}

func (cno *createNodesOp) isValid(allowParameterization bool) error {
	if !isValidCount(allowParameterization, cno.Count, cno.CountParam) {
		return fmt.Errorf("invalid Count=%d / CountParam=%q", cno.Count, cno.CountParam)
	}
	return nil
}

func (*createNodesOp) collectsMetrics() bool {
	return false
}

func (cno createNodesOp) patchParams(w *workload) (realOp, error) {
	if cno.CountParam != "" {
		var err error
		cno.Count, err = w.Params.get(cno.CountParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return &cno, (&cno).isValid(false)
}

// createNamespacesOp defines an op for creating namespaces
type createNamespacesOp struct {
	// Must be "createNamespaces".
	Opcode operationCode
	// Name prefix of the Namespace. The format is "<prefix>-<number>", where number is
	// between 0 and count-1.
	Prefix string
	// Number of namespaces to create. Parameterizable through CountParam.
	Count int
	// Template parameter for Count. Takes precedence over Count if both set.
	CountParam string
	// Path to spec file describing the Namespaces to create.
	// Optional
	NamespaceTemplatePath *string
}

func (cmo *createNamespacesOp) isValid(allowParameterization bool) error {
	if !isValidCount(allowParameterization, cmo.Count, cmo.CountParam) {
		return fmt.Errorf("invalid Count=%d / CountParam=%q", cmo.Count, cmo.CountParam)
	}
	return nil
}

func (*createNamespacesOp) collectsMetrics() bool {
	return false
}

func (cmo createNamespacesOp) patchParams(w *workload) (realOp, error) {
	if cmo.CountParam != "" {
		var err error
		cmo.Count, err = w.Params.get(cmo.CountParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return &cmo, (&cmo).isValid(false)
}

// createPodsOp defines an op where pods are scheduled as a part of a workload.
// The test can block on the completion of this op before moving forward or
// continue asynchronously.
type createPodsOp struct {
	// Must be "createPods".
	Opcode operationCode
	// Number of pods to schedule. Parameterizable through CountParam.
	Count int
	// Template parameter for Count.
	CountParam string
	// If false, Count pods get created rapidly. This can be used to
	// measure how quickly the scheduler can fill up a cluster.
	//
	// If true, Count pods get created, the operation waits for
	// a pod to get scheduled, deletes it and then creates another.
	// This continues until the configured Duration is over.
	// Metrics collection, if enabled, runs in parallel.
	//
	// This mode can be used to measure how the scheduler behaves
	// in a steady state where the cluster is always at roughly the
	// same level of utilization. Pods can be created in a separate,
	// earlier operation to simulate non-empty clusters.
	//
	// Note that the operation will delete any scheduled pod in
	// the namespace, so use different namespaces for pods that
	// are supposed to be kept running.
	SteadyState bool
	// How long to keep the cluster in a steady state.
	Duration metav1.Duration
	// Template parameter for Duration.
	DurationParam string
	// Whether or not to enable metrics collection for this createPodsOp.
	// Optional. Both CollectMetrics and SkipWaitToCompletion cannot be true at
	// the same time for a particular createPodsOp.
	CollectMetrics bool
	// Namespace the pods should be created in. Defaults to a unique
	// namespace of the format "namespace-<number>".
	// Optional
	Namespace *string
	// Path to spec file describing the pods to schedule.
	// If nil, DefaultPodTemplatePath will be used.
	// Optional
	PodTemplatePath *string
	// Whether or not to wait for all pods in this op to get scheduled.
	// Defaults to false if not specified.
	// Optional
	SkipWaitToCompletion bool
	// Persistent volume settings for the pods to be scheduled.
	// Optional
	PersistentVolumeTemplatePath      *string
	PersistentVolumeClaimTemplatePath *string
}

func (cpo *createPodsOp) isValid(allowParameterization bool) error {
	if !isValidCount(allowParameterization, cpo.Count, cpo.CountParam) {
		return fmt.Errorf("invalid Count=%d / CountParam=%q", cpo.Count, cpo.CountParam)
	}
	if cpo.CollectMetrics && cpo.SkipWaitToCompletion {
		// While it's technically possible to achieve this, the additional
		// complexity is not worth it, especially given that we don't have any
		// use-cases right now.
		return fmt.Errorf("collectMetrics and skipWaitToCompletion cannot be true at the same time")
	}
	if cpo.SkipWaitToCompletion && cpo.SteadyState {
		return errors.New("skipWaitToCompletion and steadyState cannot be true at the same time")
	}
	return nil
}

func (cpo *createPodsOp) collectsMetrics() bool {
	return cpo.CollectMetrics
}

func (cpo createPodsOp) patchParams(w *workload) (realOp, error) {
	if cpo.CountParam != "" {
		var err error
		cpo.Count, err = w.Params.get(cpo.CountParam[1:])
		if err != nil {
			return nil, err
		}
	}
	if cpo.DurationParam != "" {
		durationStr, err := getParam[string](w.Params, cpo.DurationParam[1:])
		if err != nil {
			return nil, err
		}
		if cpo.Duration.Duration, err = time.ParseDuration(durationStr); err != nil {
			return nil, fmt.Errorf("parsing duration parameter %s: %w", cpo.DurationParam, err)
		}
	}
	return &cpo, (&cpo).isValid(false)
}

// createPodSetsOp defines an op where a set of createPodsOps is created in each unique namespace.
type createPodSetsOp struct {
	// Must be "createPodSets".
	Opcode operationCode
	// Number of sets to create.
	Count int
	// Template parameter for Count.
	CountParam string
	// Each set of pods will be created in a namespace of the form namespacePrefix-<number>,
	// where number is from 0 to count-1
	NamespacePrefix string
	// The template of a createPodsOp.
	CreatePodsOp createPodsOp
}

func (cpso *createPodSetsOp) isValid(allowParameterization bool) error {
	if !isValidCount(allowParameterization, cpso.Count, cpso.CountParam) {
		return fmt.Errorf("invalid Count=%d / CountParam=%q", cpso.Count, cpso.CountParam)
	}
	return cpso.CreatePodsOp.isValid(allowParameterization)
}

func (cpso *createPodSetsOp) collectsMetrics() bool {
	return cpso.CreatePodsOp.CollectMetrics
}

func (cpso createPodSetsOp) patchParams(w *workload) (realOp, error) {
	if cpso.CountParam != "" {
		var err error
		cpso.Count, err = w.Params.get(cpso.CountParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return &cpso, (&cpso).isValid(true)
}

// deletePodsOp defines an op where previously created pods are deleted.
// The test can block on the completion of this op before moving forward or
// continue asynchronously.
type deletePodsOp struct {
	// Must be "deletePods".
	Opcode operationCode
	// Namespace the pods should be deleted from.
	Namespace string
	// Labels used to filter the pods to delete.
	// If empty, it will delete all Pods in the namespace.
	// Optional.
	LabelSelector map[string]string
	// Whether or not to wait for all pods in this op to be deleted.
	// Defaults to false if not specified.
	// Optional
	SkipWaitToCompletion bool
	// Number of pods to be deleted per second.
	// If zero, all pods are deleted at once.
	// Optional
	DeletePodsPerSecond int
}

func (dpo *deletePodsOp) isValid(allowParameterization bool) error {
	if dpo.Opcode != deletePodsOpcode {
		return fmt.Errorf("invalid opcode %q; expected %q", dpo.Opcode, deletePodsOpcode)
	}
	if dpo.DeletePodsPerSecond < 0 {
		return fmt.Errorf("invalid DeletePodsPerSecond=%d; should be non-negative", dpo.DeletePodsPerSecond)
	}
	return nil
}

func (dpo *deletePodsOp) collectsMetrics() bool {
	return false
}

func (dpo deletePodsOp) patchParams(w *workload) (realOp, error) {
	return &dpo, nil
}

// churnOp defines an op where services are created as a part of a workload.
type churnOp struct {
	// Must be "churnOp".
	Opcode operationCode
	// Value must be one of the followings:
	// - recreate. In this mode, API objects will be created for N cycles, and then
	//   deleted in the next N cycles. N is specified by the "Number" field.
	// - create. In this mode, API objects will be created (without deletion) until
	//   reaching a threshold - which is specified by the "Number" field.
	Mode string
	// Maximum number of API objects to be created.
	// Defaults to 0, which means unlimited.
	Number int
	// Intervals of churning. Defaults to 500 millisecond.
	IntervalMilliseconds int64
	// Namespace the churning objects should be created in. Defaults to a unique
	// namespace of the format "namespace-<number>".
	// Optional
	Namespace *string
	// Path of API spec files.
	TemplatePaths []string
}

func (co *churnOp) isValid(_ bool) error {
	if co.Mode != Recreate && co.Mode != Create {
		return fmt.Errorf("invalid mode: %v. must be one of %v", co.Mode, []string{Recreate, Create})
	}
	if co.Number < 0 {
		return fmt.Errorf("number (%v) cannot be negative", co.Number)
	}
	if co.Mode == Recreate && co.Number == 0 {
		return fmt.Errorf("number cannot be 0 when mode is %v", Recreate)
	}
	if len(co.TemplatePaths) == 0 {
		return fmt.Errorf("at least one template spec file needs to be specified")
	}
	return nil
}

func (*churnOp) collectsMetrics() bool {
	return false
}

func (co churnOp) patchParams(w *workload) (realOp, error) {
	return &co, nil
}

type SchedulingStage string

const (
	Scheduled SchedulingStage = "Scheduled"
	Attempted SchedulingStage = "Attempted"
)

// barrierOp defines an op that can be used to wait until all scheduled pods of
// one or many namespaces have been bound to nodes. This is useful when pods
// were scheduled with SkipWaitToCompletion set to true.
type barrierOp struct {
	// Must be "barrier".
	Opcode operationCode
	// Namespaces to block on. Empty array or not specifying this field signifies
	// that the barrier should block on all namespaces.
	Namespaces []string
	// Labels used to filter the pods to block on.
	// If empty, it won't filter the labels.
	// Optional.
	LabelSelector map[string]string
	// Determines what stage of pods scheduling the barrier should wait for.
	// If empty, it is interpreted as "Scheduled".
	// Optional
	StageRequirement SchedulingStage
}

func (bo *barrierOp) isValid(allowParameterization bool) error {
	if bo.StageRequirement != "" && bo.StageRequirement != Scheduled && bo.StageRequirement != Attempted {
		return fmt.Errorf("invalid StageRequirement %s", bo.StageRequirement)
	}
	return nil
}

func (*barrierOp) collectsMetrics() bool {
	return false
}

func (bo barrierOp) patchParams(w *workload) (realOp, error) {
	if bo.StageRequirement == "" {
		bo.StageRequirement = Scheduled
	}
	return &bo, nil
}

// sleepOp defines an op that can be used to sleep for a specified amount of time.
// This is useful in simulating workloads that require some sort of time-based synchronisation.
type sleepOp struct {
	// Must be "sleep".
	Opcode operationCode
	// Duration of sleep.
	Duration metav1.Duration
	// Template parameter for Duration.
	DurationParam string
}

func (so *sleepOp) isValid(_ bool) error {
	return nil
}

func (so *sleepOp) collectsMetrics() bool {
	return false
}

func (so sleepOp) patchParams(w *workload) (realOp, error) {
	if so.DurationParam != "" {
		durationStr, err := getParam[string](w.Params, so.DurationParam[1:])
		if err != nil {
			return nil, err
		}
		if so.Duration.Duration, err = time.ParseDuration(durationStr); err != nil {
			return nil, fmt.Errorf("invalid duration parameter %s: %w", so.DurationParam, err)
		}
	}
	return &so, nil
}

// startCollectingMetricsOp defines an op that starts metrics collectors.
// stopCollectingMetricsOp has to be used after this op to finish collecting.
type startCollectingMetricsOp struct {
	// Must be "startCollectingMetrics".
	Opcode operationCode
	// Name appended to workload's name in results.
	Name string
	// Namespaces for which the scheduling throughput metric is calculated.
	Namespaces []string
	// Labels used to filter the pods for which the scheduling throughput metric is collected.
	// If empty, it will collect the metric for all pods in the selected namespaces.
	// Optional.
	LabelSelector map[string]string
}

func (scm *startCollectingMetricsOp) isValid(_ bool) error {
	if len(scm.Namespaces) == 0 {
		return fmt.Errorf("namespaces cannot be empty")
	}
	return nil
}

func (*startCollectingMetricsOp) collectsMetrics() bool {
	return false
}

func (scm startCollectingMetricsOp) patchParams(_ *workload) (realOp, error) {
	return &scm, nil
}

// stopCollectingMetricsOp defines an op that stops collecting the metrics
// and writes them into the result slice.
// startCollectingMetricsOp has be used before this op to begin collecting.
type stopCollectingMetricsOp struct {
	// Must be "stopCollectingMetrics".
	Opcode operationCode
}

func (scm *stopCollectingMetricsOp) isValid(_ bool) error {
	return nil
}

func (*stopCollectingMetricsOp) collectsMetrics() bool {
	return true
}

func (scm stopCollectingMetricsOp) patchParams(_ *workload) (realOp, error) {
	return &scm, nil
}

func initTestOutput(tb testing.TB) io.Writer {
	var output io.Writer
	if UseTestingLog {
		output = framework.NewTBWriter(tb)
	} else {
		tmpDir := tb.TempDir()
		logfileName := path.Join(tmpDir, "output.log")
		fileOutput, err := os.Create(logfileName)
		if err != nil {
			tb.Fatalf("create log file: %v", err)
		}
		output = fileOutput

		tb.Cleanup(func() {
			// Dump the log output when the test is done.  The user
			// can decide how much of it will be visible in case of
			// success: then "go test" truncates, "go test -v"
			// doesn't. All of it will be shown for a failure.
			if err := fileOutput.Close(); err != nil {
				tb.Fatalf("close log file: %v", err)
			}
			log, err := os.ReadFile(logfileName)
			if err != nil {
				tb.Fatalf("read log file: %v", err)
			}
			tb.Logf("full log output:\n%s", string(log))
		})
	}
	return output
}

var specialFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9-_]`)

func setupTestCase(t testing.TB, tc *testCase, featureGates map[featuregate.Feature]bool, output io.Writer, outOfTreePluginRegistry frameworkruntime.Registry) (informers.SharedInformerFactory, ktesting.TContext) {
	tCtx := ktesting.Init(t, initoption.PerTestOutput(UseTestingLog))
	artifacts, doArtifacts := os.LookupEnv("ARTIFACTS")
	if !UseTestingLog && doArtifacts {
		// Reconfigure logging so that it goes to a separate file per
		// test instead of stderr. If the test passes, the file gets
		// deleted. The overall output can be very large (> 200 MB for
		// ci-benchmark-scheduler-perf-master). With this approach, we
		// have log output for failures without having to store large
		// amounts of data that no-one is looking at. The performance
		// is the same as writing to stderr.
		if err := logsapi.ResetForTest(LoggingFeatureGate); err != nil {
			t.Fatalf("Failed to reset the logging configuration: %v", err)
		}
		logfileName := path.Join(artifacts, specialFilenameChars.ReplaceAllString(t.Name(), "_")+".log")
		out, err := os.Create(logfileName)
		if err != nil {
			t.Fatalf("Failed to create per-test log output file: %v", err)
		}
		t.Cleanup(func() {
			// Everything should have stopped by now, checked below
			// by GoleakCheck (which runs first during test
			// shutdown!). Therefore we can clean up. Errors get logged
			// and fail the test, but cleanup tries to continue.
			//
			// Note that the race detector will flag any goroutine
			// as causing a race if there is no explicit wait for
			// that goroutine to stop.  We know that they must have
			// stopped (GoLeakCheck!) but the race detector
			// doesn't.
			//
			// This is a major issue because many Kubernetes goroutines get
			// started without waiting for them to stop :-(
			if err := logsapi.ResetForTest(LoggingFeatureGate); err != nil {
				t.Errorf("Failed to reset the logging configuration: %v", err)
			}
			if err := out.Close(); err != nil {
				t.Errorf("Failed to close the per-test log output file: %s: %v", logfileName, err)
			}
			if !t.Failed() {
				if err := os.Remove(logfileName); err != nil {
					t.Errorf("Failed to remove the per-test log output file: %v", err)
				}
			}
		})
		opts := &logsapi.LoggingOptions{
			ErrorStream: out,
			InfoStream:  out,
		}
		if err := logsapi.ValidateAndApplyWithOptions(LoggingConfig, opts, LoggingFeatureGate); err != nil {
			t.Fatalf("Failed to apply the per-test logging configuration: %v", err)
		}
	}

	// Ensure that there are no leaked
	// goroutines.  They could influence
	// performance of the next benchmark.
	// This must *after* RedirectKlog
	// because then during cleanup, the
	// test will wait for goroutines to
	// quit *before* restoring klog settings.
	framework.GoleakCheck(t)

	// Now that we are ready to run, start
	// a brand new etcd.
	framework.StartEtcd(t, output, true)

	for feature, flag := range featureGates {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, feature, flag)
	}

	// 30 minutes should be plenty enough even for the 5000-node tests.
	timeout := 30 * time.Minute
	tCtx = ktesting.WithTimeout(tCtx, timeout, fmt.Sprintf("timed out after the %s per-test timeout", timeout))

	if utilfeature.DefaultFeatureGate.Enabled(features.SchedulerQueueingHints) {
		registerQHintMetrics()
		t.Cleanup(func() {
			unregisterQHintMetrics()
		})
	}

	return setupClusterForWorkload(tCtx, tc.SchedulerConfigPath, featureGates, outOfTreePluginRegistry)
}

func featureGatesMerge(src map[featuregate.Feature]bool, overrides map[featuregate.Feature]bool) map[featuregate.Feature]bool {
	if len(src) == 0 {
		return maps.Clone(overrides)
	}
	result := maps.Clone(src)
	for feature, enabled := range overrides {
		result[feature] = enabled
	}
	return result
}

// fixJSONOutput works around Go not emitting a "pass" action for
// sub-benchmarks
// (https://github.com/golang/go/issues/66825#issuecomment-2343229005), which
// causes gotestsum to report a successful benchmark run as failed
// (https://github.com/gotestyourself/gotestsum/issues/413#issuecomment-2343206787).
//
// It does this by printing the missing "PASS" output line that test2json
// then converts into the "pass" action.
func fixJSONOutput(b *testing.B) {
	if !slices.Contains(os.Args, "-test.v=test2json") {
		// Not printing JSON.
		return
	}

	start := time.Now()
	b.Cleanup(func() {
		if b.Failed() {
			// Really has failed, do nothing.
			return
		}
		// SYN gets injected when using -test.v=test2json, see
		// https://cs.opensource.google/go/go/+/refs/tags/go1.23.3:src/testing/testing.go;drc=87ec2c959c73e62bfae230ef7efca11ec2a90804;l=527
		fmt.Fprintf(os.Stderr, "%c--- PASS: %s (%.2fs)\n", 22 /* SYN */, b.Name(), time.Since(start).Seconds())
	})
}

// RunBenchmarkPerfScheduling runs the scheduler performance benchmark tests.
//
// You can pass your own scheduler plugins via outOfTreePluginRegistry.
// Also, you may want to put your plugins in PluginNames variable in this package
// to collect metrics for them.
func RunBenchmarkPerfScheduling(b *testing.B, configFile string, topicName string, outOfTreePluginRegistry frameworkruntime.Registry) {
	testCases, err := getTestCases(configFile)
	if err != nil {
		b.Fatal(err)
	}
	if err = validateTestCases(testCases); err != nil {
		b.Fatal(err)
	}
	fixJSONOutput(b)

	if testing.Short() {
		PerfSchedulingLabelFilter += ",+short"
	}
	testcaseLabelSelectors := strings.Split(PerfSchedulingLabelFilter, ",")

	output := initTestOutput(b)

	// Because we run sequentially, it is possible to change the global
	// klog logger and redirect log output. Quite a lot of code still uses
	// it instead of supporting contextual logging.
	//
	// Because we leak one goroutine which calls klog, we cannot restore
	// the previous state.
	_ = framework.RedirectKlog(b, output)

	dataItems := DataItems{Version: "v1"}
	for _, tc := range testCases {
		b.Run(tc.Name, func(b *testing.B) {
			fixJSONOutput(b)
			for _, w := range tc.Workloads {
				b.Run(w.Name, func(b *testing.B) {
					if !enabled(testcaseLabelSelectors, append(tc.Labels, w.Labels...)...) {
						b.Skipf("disabled by label filter %q", PerfSchedulingLabelFilter)
					}
					fixJSONOutput(b)

					featureGates := featureGatesMerge(tc.FeatureGates, w.FeatureGates)
					informerFactory, tCtx := setupTestCase(b, tc, featureGates, output, outOfTreePluginRegistry)

					// TODO(#93795): make sure each workload within a test case has a unique
					// name? The name is used to identify the stats in benchmark reports.
					// TODO(#94404): check for unused template parameters? Probably a typo.
					err := w.isValid(tc.MetricsCollectorConfig)
					if err != nil {
						b.Fatalf("workload %s is not valid: %v", w.Name, err)
					}

					results := runWorkload(tCtx, tc, w, informerFactory)
					dataItems.DataItems = append(dataItems.DataItems, results...)

					if len(results) > 0 {
						// The default ns/op is not
						// useful because it includes
						// the time spent on
						// initialization and shutdown. Here we suppress it.
						b.ReportMetric(0, "ns/op")

						// Instead, report the same
						// results that also get stored
						// in the JSON file.
						for _, result := range results {
							// For some metrics like
							// scheduler_framework_extension_point_duration_seconds
							// the actual value has some
							// other unit. We patch the key
							// to make it look right.
							metric := strings.ReplaceAll(result.Labels["Metric"], "_seconds", "_"+result.Unit)
							for key, value := range result.Data {
								b.ReportMetric(value, metric+"/"+key)
							}
						}
					}

					if featureGates[features.SchedulerQueueingHints] {
						// In any case, we should make sure InFlightEvents is empty after running the scenario.
						if err = checkEmptyInFlightEvents(); err != nil {
							tCtx.Errorf("%s: %s", w.Name, err)
						}
					}

					// Reset metrics to prevent metrics generated in current workload gets
					// carried over to the next workload.
					legacyregistry.Reset()

					// Exactly one result is expected to contain the progress information.
					for _, item := range results {
						if len(item.progress) == 0 {
							continue
						}

						destFile, err := dataFilename(strings.ReplaceAll(fmt.Sprintf("%s_%s_%s_%s.dat", tc.Name, w.Name, topicName, runID), "/", "_"))
						if err != nil {
							b.Fatalf("prepare data file: %v", err)
						}
						f, err := os.Create(destFile)
						if err != nil {
							b.Fatalf("create data file: %v", err)
						}

						// Print progress over time.
						for _, sample := range item.progress {
							fmt.Fprintf(f, "%.1fs %d %d %d %f\n", sample.ts.Sub(item.start).Seconds(), sample.completed, sample.attempts, sample.observedTotal, sample.observedRate)
						}
						if err := f.Close(); err != nil {
							b.Fatalf("closing data file: %v", err)
						}
					}
				})
			}
		})
	}
	if err := dataItems2JSONFile(dataItems, b.Name()+"_benchmark_"+topicName); err != nil {
		b.Fatalf("unable to write measured data %+v: %v", dataItems, err)
	}
}

// RunIntegrationPerfScheduling runs the scheduler performance integration tests.
func RunIntegrationPerfScheduling(t *testing.T, configFile string) {
	testCases, err := getTestCases(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateTestCases(testCases); err != nil {
		t.Fatal(err)
	}

	if testing.Short() {
		TestSchedulingLabelFilter += ",+short"
	}
	testcaseLabelSelectors := strings.Split(TestSchedulingLabelFilter, ",")

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			for _, w := range tc.Workloads {
				t.Run(w.Name, func(t *testing.T) {
					if !enabled(testcaseLabelSelectors, append(tc.Labels, w.Labels...)...) {
						t.Skipf("disabled by label filter %q", TestSchedulingLabelFilter)
					}
					featureGates := featureGatesMerge(tc.FeatureGates, w.FeatureGates)
					informerFactory, tCtx := setupTestCase(t, tc, featureGates, nil, nil)
					err := w.isValid(tc.MetricsCollectorConfig)
					if err != nil {
						t.Fatalf("workload %s is not valid: %v", w.Name, err)
					}

					runWorkload(tCtx, tc, w, informerFactory)

					if featureGates[features.SchedulerQueueingHints] {
						// In any case, we should make sure InFlightEvents is empty after running the scenario.
						if err = checkEmptyInFlightEvents(); err != nil {
							tCtx.Errorf("%s: %s", w.Name, err)
						}
					}

					// Reset metrics to prevent metrics generated in current workload gets
					// carried over to the next workload.
					legacyregistry.Reset()
				})
			}
		})
	}
}

func loadSchedulerConfig(file string) (*config.KubeSchedulerConfiguration, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	// The UniversalDecoder runs defaulting and returns the internal type by default.
	obj, gvk, err := scheme.Codecs.UniversalDecoder().Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}
	if cfgObj, ok := obj.(*config.KubeSchedulerConfiguration); ok {
		return cfgObj, nil
	}
	return nil, fmt.Errorf("couldn't decode as KubeSchedulerConfiguration, got %s: ", gvk)
}

func unrollWorkloadTemplate(tb ktesting.TB, wt []op, w *workload) []op {
	var unrolled []op
	for opIndex, o := range wt {
		realOp, err := o.realOp.patchParams(w)
		if err != nil {
			tb.Fatalf("op %d: %v", opIndex, err)
		}
		switch concreteOp := realOp.(type) {
		case *createPodSetsOp:
			tb.Logf("Creating %d pod sets %s", concreteOp.Count, concreteOp.CountParam)
			for i := 0; i < concreteOp.Count; i++ {
				copy := concreteOp.CreatePodsOp
				ns := fmt.Sprintf("%s-%d", concreteOp.NamespacePrefix, i)
				copy.Namespace = &ns
				unrolled = append(unrolled, op{realOp: &copy})
			}
		default:
			unrolled = append(unrolled, o)
		}
	}
	return unrolled
}

func setupClusterForWorkload(tCtx ktesting.TContext, configPath string, featureGates map[featuregate.Feature]bool, outOfTreePluginRegistry frameworkruntime.Registry) (informers.SharedInformerFactory, ktesting.TContext) {
	var cfg *config.KubeSchedulerConfiguration
	var err error
	if configPath != "" {
		cfg, err = loadSchedulerConfig(configPath)
		if err != nil {
			tCtx.Fatalf("error loading scheduler config file: %v", err)
		}
		if err = validation.ValidateKubeSchedulerConfiguration(cfg); err != nil {
			tCtx.Fatalf("validate scheduler config file failed: %v", err)
		}
	}
	return mustSetupCluster(tCtx, cfg, featureGates, outOfTreePluginRegistry)
}

func labelsMatch(actualLabels, requiredLabels map[string]string) bool {
	for requiredLabel, requiredValue := range requiredLabels {
		actualValue, ok := actualLabels[requiredLabel]
		if !ok || requiredValue != actualValue {
			return false
		}
	}
	return true
}

func valueWithinThreshold(value, threshold float64, expectLower bool) bool {
	if expectLower {
		return value < threshold
	}
	return value > threshold
}

func compareMetricWithThreshold(items []DataItem, threshold float64, metricSelector thresholdMetricSelector) error {
	if threshold == 0 {
		return nil
	}
	for _, item := range items {
		if item.Labels["Metric"] == metricSelector.Name && labelsMatch(item.Labels, metricSelector.Labels) && !valueWithinThreshold(item.Data["Average"], threshold, metricSelector.ExpectLower) {
			if metricSelector.ExpectLower {
				return fmt.Errorf("%s: expected %s Average to be lower: got %f, want %f", item.Labels["Name"], metricSelector.Name, item.Data["Average"], threshold)
			}
			return fmt.Errorf("%s: expected %s Average to be higher: got %f, want %f", item.Labels["Name"], metricSelector.Name, item.Data["Average"], threshold)
		}
	}
	return nil
}

func checkEmptyInFlightEvents() error {
	labels := append(schedframework.AllClusterEventLabels(), metrics.PodPoppedInFlightEvent)
	for _, label := range labels {
		value, err := testutil.GetGaugeMetricValue(metrics.InFlightEvents.WithLabelValues(label))
		if err != nil {
			return fmt.Errorf("failed to get InFlightEvents metric for label %s", label)
		}
		if value > 0 {
			return fmt.Errorf("InFlightEvents for label %s should be empty, but has %v items", label, value)
		}
	}
	return nil
}

func startCollectingMetrics(tCtx ktesting.TContext, collectorWG *sync.WaitGroup, podInformer coreinformers.PodInformer, mcc *metricsCollectorConfig, throughputErrorMargin float64, opIndex int, name string, namespaces []string, labelSelector map[string]string) (ktesting.TContext, []testDataCollector) {
	collectorCtx := ktesting.WithCancel(tCtx)
	workloadName := tCtx.Name()
	// The first part is the same for each workload, therefore we can strip it.
	workloadName = workloadName[strings.Index(name, "/")+1:]
	collectors := getTestDataCollectors(podInformer, fmt.Sprintf("%s/%s", workloadName, name), namespaces, labelSelector, mcc, throughputErrorMargin)
	for _, collector := range collectors {
		// Need loop-local variable for function below.
		collector := collector
		err := collector.init()
		if err != nil {
			tCtx.Fatalf("op %d: Failed to initialize data collector: %v", opIndex, err)
		}
		collectorWG.Add(1)
		go func() {
			defer collectorWG.Done()
			collector.run(collectorCtx)
		}()
	}
	return collectorCtx, collectors
}

func stopCollectingMetrics(tCtx ktesting.TContext, collectorCtx ktesting.TContext, collectorWG *sync.WaitGroup, threshold float64, tms thresholdMetricSelector, opIndex int, collectors []testDataCollector) []DataItem {
	if collectorCtx == nil {
		tCtx.Fatalf("op %d: Missing startCollectingMetrics operation before stopping", opIndex)
	}
	collectorCtx.Cancel("collecting metrics, collector must stop first")
	collectorWG.Wait()
	var dataItems []DataItem
	for _, collector := range collectors {
		items := collector.collect()
		dataItems = append(dataItems, items...)
		err := compareMetricWithThreshold(items, threshold, tms)
		if err != nil {
			tCtx.Errorf("op %d: %s", opIndex, err)
		}
	}
	return dataItems
}

// metricsCollectionData manages the state and synchronization of metrics collection
// during workload execution.
type metricsCollectionData struct {
	// collectors holds a list of test data collectors used to gather performance metrics.
	collectors []testDataCollector
	// collectorCtx is a separate context specifically for managing the lifecycle
	// of metrics collection goroutines.
	// This needs a separate context and wait group because
	// the metrics collecting needs to be sure that the goroutines
	// are stopped.
	collectorCtx ktesting.TContext
	collectorWG  *sync.WaitGroup
	// disable error checking of the sampling interval length in the
	// throughput collector by default. When running benchmarks, report
	// it as test failure when samples are not taken regularly.
	throughputErrorMargin float64
}

// WorkloadState holds the state information about the workload being executed.
type workloadState struct {
	// dataItems stores the collected data from the workload execution.
	dataItems []DataItem
	// nextNodeIndex keeps track of the next node index to be used when creating nodes.
	nextNodeIndex int
	// numPodsScheduledPerNamespace has all namespaces created in workload and the number of pods they (will) have.
	// All namespaces listed in numPodsScheduledPerNamespace will be cleaned up.
	numPodsScheduledPerNamespace map[string]int
}

// sharedOperationData encapsulates all shared state and dependencies used during workload execution.
type sharedOperationData struct {
	// podInformer provides access to the informer for monitoring Pod events in the cluster.
	// Additional informers needed for testing. The pod informer was
	// already created before (scheduler.NewInformerFactory) and the
	// factory was started for it (mustSetupCluster), therefore we don't
	// need to start again.
	podInformer coreinformers.PodInformer
	// metricsData contains information and synchronization primitives for managing
	// metrics collection during workload execution.
	metricsData *metricsCollectionData
	// workloadState holds information about the current state of the workload,
	// including scheduled pods and created namespaces.
	workloadState *workloadState
	// tCtx is the root test context, used for cancellation and logging throughout
	// the execution of workload operations.
	tCtx ktesting.TContext
	// wg is a wait group that tracks all ongoing goroutines in the workload execution.
	// Ensures proper synchronization and prevents premature termination of operations.
	wg *sync.WaitGroup
}

func runWorkload(tCtx ktesting.TContext, tc *testCase, w *workload, informerFactory informers.SharedInformerFactory) []DataItem {
	b, benchmarking := tCtx.TB().(*testing.B)
	if benchmarking {
		start := time.Now()
		b.Cleanup(func() {
			duration := time.Since(start)
			// This includes startup and shutdown time and thus does not
			// reflect scheduling performance. It's useful to get a feeling
			// for how long each workload runs overall.
			b.ReportMetric(duration.Seconds(), "runtime_seconds")
		})
	}

	var throughputErrorMargin float64
	if benchmarking {
		// TODO: To prevent the perf-test failure, we increased the error margin, if still not enough
		// one day, we should think of another approach to avoid this trick.
		throughputErrorMargin = 30
	}

	// Everything else started by this function gets stopped before it returns.
	tCtx = ktesting.WithCancel(tCtx)
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		tCtx.Cancel("workload is done")
	}()

	var dataItems []DataItem

	var collectorWG sync.WaitGroup
	defer collectorWG.Wait()

	sharedOperationData := sharedOperationData{
		tCtx: tCtx,
		wg:   &wg,
		metricsData: &metricsCollectionData{
			collectorWG:           &sync.WaitGroup{},
			throughputErrorMargin: throughputErrorMargin,
		},
		workloadState: &workloadState{
			numPodsScheduledPerNamespace: make(map[string]int),
		},
		podInformer: informerFactory.Core().V1().Pods(),
	}

	for opIndex, op := range unrollWorkloadTemplate(tCtx, tc.WorkloadTemplate, w) {
		runOperation(tc, opIndex, op, w, &sharedOperationData)
	}

	// check unused params and inform users
	unusedParams := w.unusedParams()
	if len(unusedParams) != 0 {
		tCtx.Fatalf("the parameters %v are defined on workload %s, but unused.\nPlease make sure there are no typos.", unusedParams, w.Name)
	}

	// Some tests have unschedulable pods. Do not add an implicit barrier at the
	// end as we do not want to wait for them.
	return dataItems
}

func runCreateNodesOp(opIndex int, concreteOp *createNodesOp, sharedOperationData *sharedOperationData) {
	nodePreparer, err := getNodePreparer(fmt.Sprintf("node-%d-", opIndex), concreteOp, sharedOperationData.tCtx.Client())
	if err != nil {
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
	}
	if err := nodePreparer.PrepareNodes(sharedOperationData.tCtx, sharedOperationData.workloadState.nextNodeIndex); err != nil {
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
	}
	sharedOperationData.workloadState.nextNodeIndex += concreteOp.Count
}

func runCreateNamespacesOp(opIndex int, concreteOp *createNamespacesOp, sharedOperationData *sharedOperationData) {
	nsPreparer, err := newNamespacePreparer(sharedOperationData.tCtx, concreteOp)
	if err != nil {
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
	}
	if err := nsPreparer.prepare(sharedOperationData.tCtx); err != nil {
		err2 := nsPreparer.cleanup(sharedOperationData.tCtx)
		if err2 != nil {
			err = fmt.Errorf("prepare: %v; cleanup: %v", err, err2)
		}
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
	}
	for _, n := range nsPreparer.namespaces() {
		if _, ok := sharedOperationData.workloadState.numPodsScheduledPerNamespace[n]; ok {
			// this namespace has been already created.
			continue
		}
		sharedOperationData.workloadState.numPodsScheduledPerNamespace[n] = 0
	}
}

func runCreatePodsOp(tc *testCase, w *workload, opIndex int, concreteOp *createPodsOp, sharedOperationData *sharedOperationData) {
	var namespace string
	// define Pod's namespace automatically, and create that namespace.
	namespace = fmt.Sprintf("namespace-%d", opIndex)
	if concreteOp.Namespace != nil {
		namespace = *concreteOp.Namespace
	}
	createNamespaceIfNotPresent(sharedOperationData.tCtx, namespace, &sharedOperationData.workloadState.numPodsScheduledPerNamespace)
	if concreteOp.PodTemplatePath == nil {
		concreteOp.PodTemplatePath = tc.DefaultPodTemplatePath
	}

	if concreteOp.CollectMetrics {
		if sharedOperationData.metricsData.collectorCtx != nil {
			sharedOperationData.tCtx.Fatalf("op %d: Metrics collection is overlapping. Probably second collector was started before stopping a previous one", opIndex)
		}
		sharedOperationData.metricsData.collectorCtx, sharedOperationData.metricsData.collectors = startCollectingMetrics(sharedOperationData.tCtx, sharedOperationData.metricsData.collectorWG, sharedOperationData.podInformer, tc.MetricsCollectorConfig, sharedOperationData.metricsData.throughputErrorMargin, opIndex, namespace, []string{namespace}, nil)
		defer sharedOperationData.metricsData.collectorCtx.Cancel("cleaning up")
	}
	if err := createPodsRapidly(sharedOperationData.tCtx, namespace, concreteOp); err != nil {
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
	}
	switch {
	case concreteOp.SkipWaitToCompletion:
		// Only record those namespaces that may potentially require barriers
		// in the future.
		sharedOperationData.workloadState.numPodsScheduledPerNamespace[namespace] += concreteOp.Count
	case concreteOp.SteadyState:
		if err := createPodsSteadily(sharedOperationData.tCtx, namespace, sharedOperationData.podInformer, concreteOp); err != nil {
			sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
		}
	default:
		if err := waitUntilPodsScheduledInNamespace(sharedOperationData.tCtx, sharedOperationData.podInformer, nil, namespace, concreteOp.Count); err != nil {
			sharedOperationData.tCtx.Fatalf("op %d: error in waiting for pods to get scheduled: %v", opIndex, err)
		}
	}
	if concreteOp.CollectMetrics {
		// CollectMetrics and SkipWaitToCompletion can never be true at the
		// same time, so if we're here, it means that all pods have been
		// scheduled.
		items := stopCollectingMetrics(sharedOperationData.tCtx, sharedOperationData.metricsData.collectorCtx, sharedOperationData.metricsData.collectorWG, w.Threshold, *w.ThresholdMetricSelector, opIndex, sharedOperationData.metricsData.collectors)
		sharedOperationData.workloadState.dataItems = append(sharedOperationData.workloadState.dataItems, items...)
		sharedOperationData.metricsData.collectorCtx = nil
	}
}

func runDeletePodsOp(opIndex int, concreteOp *deletePodsOp, sharedOperationData *sharedOperationData) {
	labelSelector := labels.ValidatedSetSelector(concreteOp.LabelSelector)

	podsToDelete, err := sharedOperationData.podInformer.Lister().Pods(concreteOp.Namespace).List(labelSelector)
	if err != nil {
		sharedOperationData.tCtx.Fatalf("op %d: error in listing pods in the namespace %s: %v", opIndex, concreteOp.Namespace, err)
	}

	deletePods := func(opIndex int) {
		if concreteOp.DeletePodsPerSecond > 0 {
			ticker := time.NewTicker(time.Second / time.Duration(concreteOp.DeletePodsPerSecond))
			defer ticker.Stop()

			for i := 0; i < len(podsToDelete); i++ {
				select {
				case <-ticker.C:
					if err := sharedOperationData.tCtx.Client().CoreV1().Pods(concreteOp.Namespace).Delete(sharedOperationData.tCtx, podsToDelete[i].Name, metav1.DeleteOptions{}); err != nil {
						if errors.Is(err, context.Canceled) {
							return
						}
						sharedOperationData.tCtx.Errorf("op %d: unable to delete pod %v: %v", opIndex, podsToDelete[i].Name, err)
					}
				case <-sharedOperationData.tCtx.Done():
					return
				}
			}
			return
		}
		listOpts := metav1.ListOptions{
			LabelSelector: labelSelector.String(),
		}
		if err := sharedOperationData.tCtx.Client().CoreV1().Pods(concreteOp.Namespace).DeleteCollection(sharedOperationData.tCtx, metav1.DeleteOptions{}, listOpts); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			sharedOperationData.tCtx.Errorf("op %d: unable to delete pods in namespace %v: %v", opIndex, concreteOp.Namespace, err)
		}
	}

	if concreteOp.SkipWaitToCompletion {
		sharedOperationData.wg.Add(1)
		go func(opIndex int) {
			defer sharedOperationData.wg.Done()
			deletePods(opIndex)
		}(opIndex)
	} else {
		deletePods(opIndex)
	}
}

func runChurnOp(opIndex int, concreteOp *churnOp, sharedOperationData *sharedOperationData) {
	var namespace string
	if concreteOp.Namespace != nil {
		namespace = *concreteOp.Namespace
	} else {
		namespace = fmt.Sprintf("namespace-%d", opIndex)
	}
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cacheddiscovery.NewMemCacheClient(sharedOperationData.tCtx.Client().Discovery()))
	// Ensure the namespace exists.
	nsObj := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if _, err := sharedOperationData.tCtx.Client().CoreV1().Namespaces().Create(sharedOperationData.tCtx, nsObj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		sharedOperationData.tCtx.Fatalf("op %d: unable to create namespace %v: %v", opIndex, namespace, err)
	}

	var churnFns []func(name string) string

	for i, path := range concreteOp.TemplatePaths {
		unstructuredObj, gvk, err := getUnstructuredFromFile(path)
		if err != nil {
			sharedOperationData.tCtx.Fatalf("op %d: unable to parse the %v-th template path: %v", opIndex, i, err)
		}
		// Obtain GVR.
		mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			sharedOperationData.tCtx.Fatalf("op %d: unable to find GVR for %v: %v", opIndex, gvk, err)
		}
		gvr := mapping.Resource
		// Distinguish cluster-scoped with namespaced API objects.
		var dynRes dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			dynRes = sharedOperationData.tCtx.Dynamic().Resource(gvr).Namespace(namespace)
		} else {
			dynRes = sharedOperationData.tCtx.Dynamic().Resource(gvr)
		}

		churnFns = append(churnFns, func(name string) string {
			if name != "" {
				if err := dynRes.Delete(sharedOperationData.tCtx, name, metav1.DeleteOptions{}); err != nil && !errors.Is(err, context.Canceled) {
					sharedOperationData.tCtx.Errorf("op %d: unable to delete %v: %v", opIndex, name, err)
				}
				return ""
			}

			live, err := dynRes.Create(sharedOperationData.tCtx, unstructuredObj, metav1.CreateOptions{})
			if err != nil {
				return ""
			}
			return live.GetName()
		})
	}

	var interval int64 = 500
	if concreteOp.IntervalMilliseconds != 0 {
		interval = concreteOp.IntervalMilliseconds
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	defer ticker.Stop()

	switch concreteOp.Mode {
	case Create:
		sharedOperationData.wg.Add(1)
		go func() {
			defer sharedOperationData.wg.Done()
			count, threshold := 0, concreteOp.Number
			if threshold == 0 {
				threshold = math.MaxInt32
			}
			for count < threshold {
				select {
				case <-ticker.C:
					for i := range churnFns {
						churnFns[i]("")
					}
					count++
				case <-sharedOperationData.tCtx.Done():
					return
				}
			}
		}()
	case Recreate:
		sharedOperationData.wg.Add(1)
		go func() {
			defer sharedOperationData.wg.Done()
			retVals := make([][]string, len(churnFns))
			// For each churn function, instantiate a slice of strings with length "concreteOp.Number".
			for i := range retVals {
				retVals[i] = make([]string, concreteOp.Number)
			}

			count := 0
			for {
				select {
				case <-ticker.C:
					for i := range churnFns {
						retVals[i][count%concreteOp.Number] = churnFns[i](retVals[i][count%concreteOp.Number])
					}
					count++
				case <-sharedOperationData.tCtx.Done():
					return
				}
			}
		}()
	}
}

func runBarrierOp(opIndex int, concreteOp *barrierOp, sharedOperationData *sharedOperationData) {
	for _, namespace := range concreteOp.Namespaces {
		if _, ok := sharedOperationData.workloadState.numPodsScheduledPerNamespace[namespace]; !ok {
			sharedOperationData.tCtx.Fatalf("op %d: unknown namespace %s", opIndex, namespace)
		}
	}
	switch concreteOp.StageRequirement {
	case Attempted:
		if err := waitUntilPodsAttempted(sharedOperationData.tCtx, sharedOperationData.podInformer, concreteOp.LabelSelector, concreteOp.Namespaces, sharedOperationData.workloadState.numPodsScheduledPerNamespace); err != nil {
			sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
		}
	case Scheduled:
		// Default should be treated like "Scheduled", so handling both in the same way.
		fallthrough
	default:
		if err := waitUntilPodsScheduled(sharedOperationData.tCtx, sharedOperationData.podInformer, concreteOp.LabelSelector, concreteOp.Namespaces, sharedOperationData.workloadState.numPodsScheduledPerNamespace); err != nil {
			sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
		}
		// At the end of the barrier, we can be sure that there are no pods
		// pending scheduling in the namespaces that we just blocked on.
		if len(concreteOp.Namespaces) == 0 {
			sharedOperationData.workloadState.numPodsScheduledPerNamespace = make(map[string]int)
		} else {
			for _, namespace := range concreteOp.Namespaces {
				delete(sharedOperationData.workloadState.numPodsScheduledPerNamespace, namespace)
			}
		}
	}
}

func runSleepOp(concreteOp *sleepOp, sharedOperationData *sharedOperationData) {
	select {
	case <-sharedOperationData.tCtx.Done():
	case <-time.After(concreteOp.Duration.Duration):
	}
}

func runStartCollectingMetricsOp(opIndex int, tc *testCase, concreteOp *startCollectingMetricsOp, sharedOperationData *sharedOperationData) {
	if sharedOperationData.metricsData.collectorCtx != nil {
		sharedOperationData.tCtx.Fatalf("op %d: Metrics collection is overlapping. Probably second collector was started before stopping a previous one", opIndex)
	}
	sharedOperationData.metricsData.collectorCtx, sharedOperationData.metricsData.collectors = startCollectingMetrics(sharedOperationData.tCtx, sharedOperationData.metricsData.collectorWG, sharedOperationData.podInformer, tc.MetricsCollectorConfig, sharedOperationData.metricsData.throughputErrorMargin, opIndex, concreteOp.Name, concreteOp.Namespaces, concreteOp.LabelSelector)
}

func runStopCollectingMetricsOp(opIndex int, w *workload, sharedOperationData *sharedOperationData) {
	items := stopCollectingMetrics(sharedOperationData.tCtx, sharedOperationData.metricsData.collectorCtx, sharedOperationData.metricsData.collectorWG, w.Threshold, *w.ThresholdMetricSelector, opIndex, sharedOperationData.metricsData.collectors)
	sharedOperationData.workloadState.dataItems = append(sharedOperationData.workloadState.dataItems, items...)
	sharedOperationData.metricsData.collectorCtx = nil
}

func runDefault(opIndex int, concreteOp realOp, sharedOperationData *sharedOperationData) {
	runable, ok := concreteOp.(runnableOp)
	if !ok {
		sharedOperationData.tCtx.Fatalf("op %d: invalid op %v", opIndex, concreteOp)
	}
	for _, namespace := range runable.requiredNamespaces() {
		createNamespaceIfNotPresent(sharedOperationData.tCtx, namespace, &sharedOperationData.workloadState.numPodsScheduledPerNamespace)
	}
	runable.run(sharedOperationData.tCtx)
}

func runOperation(tc *testCase, opIndex int, op op, w *workload, sharedOperationData *sharedOperationData) {
	realOp, err := op.realOp.patchParams(w)
	if err != nil {
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, err)
	}
	select {
	case <-sharedOperationData.tCtx.Done():
		sharedOperationData.tCtx.Fatalf("op %d: %v", opIndex, context.Cause(sharedOperationData.tCtx))
	default:
	}
	switch concreteOp := realOp.(type) {
	case *createNodesOp:
		runCreateNodesOp(opIndex, concreteOp, sharedOperationData)
	case *createNamespacesOp:
		runCreateNamespacesOp(opIndex, concreteOp, sharedOperationData)
	case *createPodsOp:
		runCreatePodsOp(tc, w, opIndex, concreteOp, sharedOperationData)
	case *deletePodsOp:
		runDeletePodsOp(opIndex, concreteOp, sharedOperationData)
	case *churnOp:
		runChurnOp(opIndex, concreteOp, sharedOperationData)
	case *barrierOp:
		runBarrierOp(opIndex, concreteOp, sharedOperationData)
	case *sleepOp:
		runSleepOp(concreteOp, sharedOperationData)
	case *startCollectingMetricsOp:
		runStartCollectingMetricsOp(opIndex, tc, concreteOp, sharedOperationData)
	case *stopCollectingMetricsOp:
		runStopCollectingMetricsOp(opIndex, w, sharedOperationData)
	default:
		runDefault(opIndex, concreteOp, sharedOperationData)
	}
}

func createNamespaceIfNotPresent(tCtx ktesting.TContext, namespace string, podsPerNamespace *map[string]int) {
	if _, ok := (*podsPerNamespace)[namespace]; !ok {
		// The namespace has not created yet.
		// So, create that and register it.
		_, err := tCtx.Client().CoreV1().Namespaces().Create(tCtx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
		if err != nil {
			tCtx.Fatalf("failed to create namespace for Pod: %v", namespace)
		}
		(*podsPerNamespace)[namespace] = 0
	}
}

type testDataCollector interface {
	init() error
	run(tCtx ktesting.TContext)
	collect() []DataItem
}

func getTestDataCollectors(podInformer coreinformers.PodInformer, name string, namespaces []string, labelSelector map[string]string, mcc *metricsCollectorConfig, throughputErrorMargin float64) []testDataCollector {
	if mcc == nil {
		mcc = &defaultMetricsCollectorConfig
	}
	return []testDataCollector{
		newThroughputCollector(podInformer, map[string]string{"Name": name}, labelSelector, namespaces, throughputErrorMargin),
		newMetricsCollector(mcc, map[string]string{"Name": name}),
	}
}

func getNodePreparer(prefix string, cno *createNodesOp, clientset clientset.Interface) (testutils.TestNodePreparer, error) {
	var nodeStrategy testutils.PrepareNodeStrategy = &testutils.TrivialNodePrepareStrategy{}
	if cno.NodeAllocatableStrategy != nil {
		nodeStrategy = cno.NodeAllocatableStrategy
	} else if cno.LabelNodePrepareStrategy != nil {
		nodeStrategy = cno.LabelNodePrepareStrategy
	} else if cno.UniqueNodeLabelStrategy != nil {
		nodeStrategy = cno.UniqueNodeLabelStrategy
	}

	nodeTemplate := StaticNodeTemplate(makeBaseNode(prefix))
	if cno.NodeTemplatePath != nil {
		nodeTemplate = nodeTemplateFromFile(*cno.NodeTemplatePath)
	}

	return NewIntegrationTestNodePreparer(
		clientset,
		[]testutils.CountToStrategy{{Count: cno.Count, Strategy: nodeStrategy}},
		nodeTemplate,
	), nil
}

// createPodsRapidly implements the "create pods rapidly" mode of [createPodsOp].
// It's a nop when cpo.SteadyState is true.
func createPodsRapidly(tCtx ktesting.TContext, namespace string, cpo *createPodsOp) error {
	if cpo.SteadyState {
		return nil
	}
	strategy, err := getPodStrategy(cpo)
	if err != nil {
		return err
	}
	tCtx.Logf("creating %d pods in namespace %q", cpo.Count, namespace)
	config := testutils.NewTestPodCreatorConfig()
	config.AddStrategy(namespace, cpo.Count, strategy)
	podCreator := testutils.NewTestPodCreator(tCtx.Client(), config)
	return podCreator.CreatePods(tCtx)
}

// createPodsSteadily implements the "create pods and delete pods" mode of [createPodsOp].
// It's a nop when cpo.SteadyState is false.
func createPodsSteadily(tCtx ktesting.TContext, namespace string, podInformer coreinformers.PodInformer, cpo *createPodsOp) error {
	if !cpo.SteadyState {
		return nil
	}
	strategy, err := getPodStrategy(cpo)
	if err != nil {
		return err
	}
	tCtx.Logf("creating pods in namespace %q for %s", namespace, cpo.Duration)
	tCtx = ktesting.WithTimeout(tCtx, cpo.Duration.Duration, fmt.Sprintf("the operation ran for the configured %s", cpo.Duration.Duration))

	// Start watching pods in the namespace. Any pod which is seen as being scheduled
	// gets deleted.
	scheduledPods := make(chan *v1.Pod, cpo.Count)
	scheduledPodsClosed := false
	var mutex sync.Mutex
	defer func() {
		mutex.Lock()
		defer mutex.Unlock()
		close(scheduledPods)
		scheduledPodsClosed = true
	}()

	existingPods := 0
	runningPods := 0
	onPodChange := func(oldObj, newObj any) {
		oldPod, newPod, err := schedutil.As[*v1.Pod](oldObj, newObj)
		if err != nil {
			tCtx.Errorf("unexpected pod events: %v", err)
			return
		}

		mutex.Lock()
		defer mutex.Unlock()
		if oldPod == nil {
			existingPods++
		}
		if (oldPod == nil || oldPod.Spec.NodeName == "") && newPod.Spec.NodeName != "" {
			// Got scheduled.
			runningPods++

			// Only ask for deletion in our namespace.
			if newPod.Namespace != namespace {
				return
			}
			if !scheduledPodsClosed {
				select {
				case <-tCtx.Done():
				case scheduledPods <- newPod:
				}
			}
		}
	}
	handle, err := podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			onPodChange(nil, obj)
		},
		UpdateFunc: func(oldObj, newObj any) {
			onPodChange(oldObj, newObj)
		},
		DeleteFunc: func(obj any) {
			pod, _, err := schedutil.As[*v1.Pod](obj, nil)
			if err != nil {
				tCtx.Errorf("unexpected pod events: %v", err)
				return
			}

			existingPods--
			if pod.Spec.NodeName != "" {
				runningPods--
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register event handler: %w", err)
	}
	defer func() {
		tCtx.ExpectNoError(podInformer.Informer().RemoveEventHandler(handle), "remove event handler")
	}()

	// Seed the namespace with the initial number of pods.
	if err := strategy(tCtx, tCtx.Client(), namespace, cpo.Count); err != nil {
		return fmt.Errorf("create initial %d pods: %w", cpo.Count, err)
	}

	// Now loop until we are done. Report periodically how many pods were scheduled.
	countScheduledPods := 0
	lastCountScheduledPods := 0
	logPeriod := time.Second
	ticker := time.NewTicker(logPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-tCtx.Done():
			tCtx.Logf("Completed after seeing %d scheduled pod: %v", countScheduledPods, context.Cause(tCtx))
			return nil
		case <-scheduledPods:
			countScheduledPods++
			if countScheduledPods%cpo.Count == 0 {
				// All scheduled. Start over with a new batch.
				err := tCtx.Client().CoreV1().Pods(namespace).DeleteCollection(tCtx, metav1.DeleteOptions{
					GracePeriodSeconds: ptr.To(int64(0)),
					PropagationPolicy:  ptr.To(metav1.DeletePropagationBackground), // Foreground will block.
				}, metav1.ListOptions{})
				// Ignore errors when the time is up. errors.Is(context.Canceled) would
				// be more precise, but doesn't work because client-go doesn't reliably
				// propagate it.
				if tCtx.Err() != nil {
					continue
				}
				if err != nil {
					// Worse, sometimes rate limiting gives up *before* the context deadline is reached.
					// Then we get here with this error:
					//   client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline
					//
					// This also can be ignored. We'll retry if the test is not done yet.
					if strings.Contains(err.Error(), "would exceed context deadline") {
						continue
					}
					return fmt.Errorf("delete scheduled pods: %w", err)
				}
				err = strategy(tCtx, tCtx.Client(), namespace, cpo.Count)
				if tCtx.Err() != nil {
					continue
				}
				if err != nil {
					return fmt.Errorf("create next batch of pods: %w", err)
				}
			}
		case <-ticker.C:
			delta := countScheduledPods - lastCountScheduledPods
			lastCountScheduledPods = countScheduledPods
			func() {
				mutex.Lock()
				defer mutex.Unlock()

				tCtx.Logf("%d pods got scheduled in total in namespace %q, overall %d out of %d pods scheduled: %f pods/s in last interval",
					countScheduledPods, namespace,
					runningPods, existingPods,
					float64(delta)/logPeriod.Seconds(),
				)
			}()
		}
	}
}

// waitUntilPodsScheduledInNamespace blocks until all pods in the given
// namespace are scheduled. Times out after 10 minutes because even at the
// lowest observed QPS of ~10 pods/sec, a 5000-node test should complete.
func waitUntilPodsScheduledInNamespace(tCtx ktesting.TContext, podInformer coreinformers.PodInformer, labelSelector map[string]string, namespace string, wantCount int) error {
	var pendingPod *v1.Pod

	err := wait.PollUntilContextTimeout(tCtx, 1*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		default:
		}
		scheduled, attempted, unattempted, err := getScheduledPods(podInformer, labelSelector, namespace)
		if err != nil {
			return false, err
		}
		if len(scheduled) >= wantCount {
			tCtx.Logf("scheduling succeed")
			return true, nil
		}
		tCtx.Logf("namespace: %s, pods: want %d, got %d", namespace, wantCount, len(scheduled))
		if len(attempted) > 0 {
			pendingPod = attempted[0]
		} else if len(unattempted) > 0 {
			pendingPod = unattempted[0]
		} else {
			pendingPod = nil
		}
		return false, nil
	})

	if err != nil && pendingPod != nil {
		err = fmt.Errorf("at least pod %s is not scheduled: %w", klog.KObj(pendingPod), err)
	}
	return err
}

// waitUntilPodsAttemptedInNamespace blocks until all pods in the given
// namespace at least once went through a schedyling cycle.
// Times out after 10 minutes similarly to waitUntilPodsScheduledInNamespace.
func waitUntilPodsAttemptedInNamespace(tCtx ktesting.TContext, podInformer coreinformers.PodInformer, labelSelector map[string]string, namespace string, wantCount int) error {
	var pendingPod *v1.Pod

	err := wait.PollUntilContextTimeout(tCtx, 1*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		default:
		}
		scheduled, attempted, unattempted, err := getScheduledPods(podInformer, labelSelector, namespace)
		if err != nil {
			return false, err
		}
		if len(scheduled)+len(attempted) >= wantCount {
			tCtx.Logf("all pods attempted to be scheduled")
			return true, nil
		}
		tCtx.Logf("namespace: %s, attempted pods: want %d, got %d", namespace, wantCount, len(scheduled)+len(attempted))
		if len(unattempted) > 0 {
			pendingPod = unattempted[0]
		} else {
			pendingPod = nil
		}
		return false, nil
	})

	if err != nil && pendingPod != nil {
		err = fmt.Errorf("at least pod %s is not attempted: %w", klog.KObj(pendingPod), err)
	}
	return err
}

// waitUntilPodsScheduled blocks until the all pods in the given namespaces are
// scheduled.
func waitUntilPodsScheduled(tCtx ktesting.TContext, podInformer coreinformers.PodInformer, labelSelector map[string]string, namespaces []string, numPodsScheduledPerNamespace map[string]int) error {
	// If unspecified, default to all known namespaces.
	if len(namespaces) == 0 {
		for namespace := range numPodsScheduledPerNamespace {
			namespaces = append(namespaces, namespace)
		}
	}
	for _, namespace := range namespaces {
		select {
		case <-tCtx.Done():
			return context.Cause(tCtx)
		default:
		}
		wantCount, ok := numPodsScheduledPerNamespace[namespace]
		if !ok {
			return fmt.Errorf("unknown namespace %s", namespace)
		}
		if err := waitUntilPodsScheduledInNamespace(tCtx, podInformer, labelSelector, namespace, wantCount); err != nil {
			return fmt.Errorf("error waiting for pods in namespace %q: %w", namespace, err)
		}
	}
	return nil
}

// waitUntilPodsAttempted blocks until the all pods in the given namespaces are
// attempted (at least once went through a schedyling cycle).
func waitUntilPodsAttempted(tCtx ktesting.TContext, podInformer coreinformers.PodInformer, labelSelector map[string]string, namespaces []string, numPodsScheduledPerNamespace map[string]int) error {
	// If unspecified, default to all known namespaces.
	if len(namespaces) == 0 {
		for namespace := range numPodsScheduledPerNamespace {
			namespaces = append(namespaces, namespace)
		}
	}
	for _, namespace := range namespaces {
		select {
		case <-tCtx.Done():
			return context.Cause(tCtx)
		default:
		}
		wantCount, ok := numPodsScheduledPerNamespace[namespace]
		if !ok {
			return fmt.Errorf("unknown namespace %s", namespace)
		}
		if err := waitUntilPodsAttemptedInNamespace(tCtx, podInformer, labelSelector, namespace, wantCount); err != nil {
			return fmt.Errorf("error waiting for pods in namespace %q: %w", namespace, err)
		}
	}
	return nil
}

func getSpecFromFile(path *string, spec interface{}) error {
	bytes, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	return yaml.UnmarshalStrict(bytes, spec)
}

func getUnstructuredFromFile(path string) (*unstructured.Unstructured, *schema.GroupVersionKind, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	bytes, err = yaml.YAMLToJSONStrict(bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot covert YAML to JSON: %v", err)
	}

	obj, gvk, err := unstructured.UnstructuredJSONScheme.Decode(bytes, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil, fmt.Errorf("cannot convert spec file in %v to an unstructured obj", path)
	}
	return unstructuredObj, gvk, nil
}

func getTestCases(path string) ([]*testCase, error) {
	testCases := make([]*testCase, 0)
	if err := getSpecFromFile(&path, &testCases); err != nil {
		return nil, fmt.Errorf("parsing test cases error: %w", err)
	}
	for _, tc := range testCases {
		for _, w := range tc.Workloads {
			w.setDefaults(tc.DefaultThresholdMetricSelector)
		}
	}
	return testCases, nil
}

func validateTestCases(testCases []*testCase) error {
	if len(testCases) == 0 {
		return fmt.Errorf("no test cases defined")
	}
	testCaseUniqueNames := map[string]bool{}
	for _, tc := range testCases {
		if testCaseUniqueNames[tc.Name] {
			return fmt.Errorf("%s: name is not unique", tc.Name)
		}
		testCaseUniqueNames[tc.Name] = true
		if len(tc.Workloads) == 0 {
			return fmt.Errorf("%s: no workloads defined", tc.Name)
		}
		if err := tc.workloadNamesUnique(); err != nil {
			return err
		}
		if len(tc.WorkloadTemplate) == 0 {
			return fmt.Errorf("%s: no ops defined", tc.Name)
		}
		// Make sure there's at least one CreatePods op with collectMetrics set to
		// true in each workload. What's the point of running a performance
		// benchmark if no statistics are collected for reporting?
		if !tc.collectsMetrics() {
			return fmt.Errorf("%s: no op in the workload template collects metrics", tc.Name)
		}
	}
	return nil
}

func getPodStrategy(cpo *createPodsOp) (testutils.TestPodCreateStrategy, error) {
	podTemplate := testutils.StaticPodTemplate(makeBasePod())
	if cpo.PodTemplatePath != nil {
		podTemplate = podTemplateFromFile(*cpo.PodTemplatePath)
	}
	if cpo.PersistentVolumeClaimTemplatePath == nil {
		return testutils.NewCustomCreatePodStrategy(podTemplate), nil
	}

	pvTemplate, err := getPersistentVolumeSpecFromFile(cpo.PersistentVolumeTemplatePath)
	if err != nil {
		return nil, err
	}
	pvcTemplate, err := getPersistentVolumeClaimSpecFromFile(cpo.PersistentVolumeClaimTemplatePath)
	if err != nil {
		return nil, err
	}
	return testutils.NewCreatePodWithPersistentVolumeStrategy(pvcTemplate, getCustomVolumeFactory(pvTemplate), podTemplate), nil
}

type nodeTemplateFromFile string

func (f nodeTemplateFromFile) GetNodeTemplate(index, count int) (*v1.Node, error) {
	nodeSpec := &v1.Node{}
	if err := getSpecFromTextTemplateFile(string(f), map[string]any{"Index": index, "Count": count}, nodeSpec); err != nil {
		return nil, fmt.Errorf("parsing Node: %w", err)
	}
	return nodeSpec, nil
}

type podTemplateFromFile string

func (f podTemplateFromFile) GetPodTemplate(index, count int) (*v1.Pod, error) {
	podSpec := &v1.Pod{}
	if err := getSpecFromTextTemplateFile(string(f), map[string]any{"Index": index, "Count": count}, podSpec); err != nil {
		return nil, fmt.Errorf("parsing Pod: %w", err)
	}
	return podSpec, nil
}

func getPersistentVolumeSpecFromFile(path *string) (*v1.PersistentVolume, error) {
	persistentVolumeSpec := &v1.PersistentVolume{}
	if err := getSpecFromFile(path, persistentVolumeSpec); err != nil {
		return nil, fmt.Errorf("parsing PersistentVolume: %w", err)
	}
	return persistentVolumeSpec, nil
}

func getPersistentVolumeClaimSpecFromFile(path *string) (*v1.PersistentVolumeClaim, error) {
	persistentVolumeClaimSpec := &v1.PersistentVolumeClaim{}
	if err := getSpecFromFile(path, persistentVolumeClaimSpec); err != nil {
		return nil, fmt.Errorf("parsing PersistentVolumeClaim: %w", err)
	}
	return persistentVolumeClaimSpec, nil
}

func getCustomVolumeFactory(pvTemplate *v1.PersistentVolume) func(id int) *v1.PersistentVolume {
	return func(id int) *v1.PersistentVolume {
		pv := pvTemplate.DeepCopy()
		volumeID := fmt.Sprintf("vol-%d", id)
		pv.ObjectMeta.Name = volumeID
		pvs := pv.Spec.PersistentVolumeSource
		if pvs.CSI != nil {
			pvs.CSI.VolumeHandle = volumeID
		} else if pvs.AWSElasticBlockStore != nil {
			pvs.AWSElasticBlockStore.VolumeID = volumeID
		}
		return pv
	}
}

// namespacePreparer holds configuration information for the test namespace preparer.
type namespacePreparer struct {
	count  int
	prefix string
	spec   *v1.Namespace
}

func newNamespacePreparer(tCtx ktesting.TContext, cno *createNamespacesOp) (*namespacePreparer, error) {
	ns := &v1.Namespace{}
	if cno.NamespaceTemplatePath != nil {
		if err := getSpecFromFile(cno.NamespaceTemplatePath, ns); err != nil {
			return nil, fmt.Errorf("parsing NamespaceTemplate: %w", err)
		}
	}

	return &namespacePreparer{
		count:  cno.Count,
		prefix: cno.Prefix,
		spec:   ns,
	}, nil
}

// namespaces returns namespace names have been (or will be) created by this namespacePreparer
func (p *namespacePreparer) namespaces() []string {
	namespaces := make([]string, p.count)
	for i := 0; i < p.count; i++ {
		namespaces[i] = fmt.Sprintf("%s-%d", p.prefix, i)
	}
	return namespaces
}

// prepare creates the namespaces.
func (p *namespacePreparer) prepare(tCtx ktesting.TContext) error {
	base := &v1.Namespace{}
	if p.spec != nil {
		base = p.spec
	}
	tCtx.Logf("Making %d namespaces with prefix %q and template %v", p.count, p.prefix, *base)
	for i := 0; i < p.count; i++ {
		n := base.DeepCopy()
		n.Name = fmt.Sprintf("%s-%d", p.prefix, i)
		if err := testutils.RetryWithExponentialBackOff(func() (bool, error) {
			_, err := tCtx.Client().CoreV1().Namespaces().Create(tCtx, n, metav1.CreateOptions{})
			return err == nil || apierrors.IsAlreadyExists(err), nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// cleanup deletes existing test namespaces.
func (p *namespacePreparer) cleanup(tCtx ktesting.TContext) error {
	var errRet error
	for i := 0; i < p.count; i++ {
		n := fmt.Sprintf("%s-%d", p.prefix, i)
		if err := tCtx.Client().CoreV1().Namespaces().Delete(tCtx, n, metav1.DeleteOptions{}); err != nil {
			tCtx.Errorf("Deleting Namespace: %v", err)
			errRet = err
		}
	}
	return errRet
}
