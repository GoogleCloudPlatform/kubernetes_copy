/*
Copyright 2021 The Kubernetes Authors.

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

package admission

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/klog/v2/ktesting"
	admissionapi "k8s.io/pod-security-admission/admission/api"
	"k8s.io/pod-security-admission/admission/api/load"
	"k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/metrics"
	"k8s.io/pod-security-admission/policy"
	"k8s.io/pod-security-admission/test"
	"k8s.io/utils/pointer"
)

func TestDefaultExtractPodSpec(t *testing.T) {
	metadata := metav1.ObjectMeta{
		Name: "foo-pod",
	}
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "foo-container",
		}},
	}
	objects := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metadata,
			Spec:       spec,
		},
		&corev1.PodTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-template"},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metadata,
				Spec:       spec,
			},
		},
		&corev1.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-rc"},
			Spec: corev1.ReplicationControllerSpec{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metadata,
					Spec:       spec,
				},
			},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-rs"},
			Spec: appsv1.ReplicaSetSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metadata,
					Spec:       spec,
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-deployment"},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metadata,
					Spec:       spec,
				},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-ss"},
			Spec: appsv1.StatefulSetSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metadata,
					Spec:       spec,
				},
			},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-ds"},
			Spec: appsv1.DaemonSetSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metadata,
					Spec:       spec,
				},
			},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-job"},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metadata,
					Spec:       spec,
				},
			},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-cronjob"},
			Spec: batchv1.CronJobSpec{
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metadata,
							Spec:       spec,
						},
					},
				},
			},
		},
	}
	extractor := &DefaultPodSpecExtractor{}
	for _, obj := range objects {
		name := obj.(metav1.Object).GetName()
		actualMetadata, actualSpec, err := extractor.ExtractPodSpec(obj)
		assert.NoError(t, err, name)
		assert.Equal(t, &metadata, actualMetadata, "%s: Metadata mismatch", name)
		assert.Equal(t, &spec, actualSpec, "%s: PodSpec mismatch", name)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo-svc",
		},
	}
	_, _, err := extractor.ExtractPodSpec(service)
	assert.Error(t, err, "service should not have an extractable pod spec")
}

func TestDefaultHasPodSpec(t *testing.T) {
	podLikeResources := []schema.GroupResource{
		corev1.Resource("pods"),
		corev1.Resource("replicationcontrollers"),
		corev1.Resource("podtemplates"),
		appsv1.Resource("replicasets"),
		appsv1.Resource("deployments"),
		appsv1.Resource("statefulsets"),
		appsv1.Resource("daemonsets"),
		batchv1.Resource("jobs"),
		batchv1.Resource("cronjobs"),
	}
	extractor := &DefaultPodSpecExtractor{}
	for _, gr := range podLikeResources {
		assert.True(t, extractor.HasPodSpec(gr), gr.String())
	}

	nonPodResources := []schema.GroupResource{
		corev1.Resource("services"),
		admissionv1.Resource("admissionreviews"),
		appsv1.Resource("foobars"),
	}
	for _, gr := range nonPodResources {
		assert.False(t, extractor.HasPodSpec(gr), gr.String())
	}
}

type testEvaluator struct {
	lv api.LevelVersion

	delay time.Duration
}

func (t *testEvaluator) EvaluatePod(lv api.LevelVersion, meta *metav1.ObjectMeta, spec *corev1.PodSpec) []policy.CheckResult {
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	t.lv = lv
	if meta.Annotations["error"] != "" {
		return []policy.CheckResult{{Allowed: false, ForbiddenReason: meta.Annotations["error"]}}
	} else {
		return []policy.CheckResult{{Allowed: true}}
	}
}

type testNamespaceGetter map[string]*corev1.Namespace

func (t testNamespaceGetter) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	if ns, ok := t[name]; ok {
		return ns.DeepCopy(), nil
	} else {
		return nil, apierrors.NewNotFound(corev1.Resource("namespaces"), name)
	}
}

type testPodLister struct {
	called bool
	pods   []*corev1.Pod
	delay  time.Duration
}

func (t *testPodLister) ListPods(ctx context.Context, namespace string) ([]*corev1.Pod, error) {
	t.called = true
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.pods, nil
}

func TestValidateNamespace(t *testing.T) {
	testcases := []struct {
		name                 string
		exemptNamespaces     []string
		exemptRuntimeClasses []string
		// override default policy
		defaultPolicy *api.Policy
		// request subresource
		subresource string
		// labels for the new namespace
		newLabels map[string]string
		// labels for the old namespace (only used if update=true)
		oldLabels map[string]string
		// list of pods to return
		pods []*corev1.Pod
		// time to sleep while listing
		delayList time.Duration
		// time to sleep while evaluating
		delayEvaluation time.Duration

		expectAllowed  bool
		expectError    string
		expectListPods bool
		expectEvaluate api.LevelVersion
		expectWarnings []string
	}{
		// creation tests, just validate labels
		{
			name:           "create privileged",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelPrivileged), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "create baseline",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "create restricted",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:             "create restricted exempt",
			newLabels:        map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			exemptNamespaces: []string{"test"},
			expectAllowed:    true,
			expectListPods:   false,
			expectWarnings: []string{
				`namespace "test" is exempt from Pod Security, and the policy (enforce=restricted:latest) will be ignored`,
			},
		},
		{
			name:           "create malformed level",
			newLabels:      map[string]string{api.EnforceLevelLabel: "unknown"},
			expectAllowed:  false,
			expectError:    `must be one of privileged, baseline, restricted`,
			expectListPods: false,
		},
		{
			name:           "create malformed version",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelPrivileged), api.EnforceVersionLabel: "unknown"},
			expectAllowed:  false,
			expectError:    `must be "latest" or "v1.x"`,
			expectListPods: false,
		},

		// update tests that don't tighten effective policy, no pod list/evaluate
		{
			name:           "update no-op",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update no-op malformed level",
			newLabels:      map[string]string{api.EnforceLevelLabel: "unknown"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: "unknown"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update no-op malformed version",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline), api.EnforceVersionLabel: "unknown"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline), api.EnforceVersionLabel: "unknown"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update relax level identical version",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline), api.EnforceVersionLabel: "v1.0"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update relax level explicit latest",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline), api.EnforceVersionLabel: "latest"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "latest"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update relax level implicit latest",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update to explicit privileged",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelPrivileged)},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:           "update to implicit privileged",
			newLabels:      map[string]string{},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  true,
			expectListPods: false,
		},
		{
			name:             "update exempt to restricted",
			exemptNamespaces: []string{"test"},
			newLabels: map[string]string{
				api.EnforceLevelLabel:   string(api.LevelRestricted),
				api.EnforceVersionLabel: "v1.0",
				api.AuditLevelLabel:     string(api.LevelRestricted),
				api.WarnLevelLabel:      string(api.LevelBaseline),
			},
			oldLabels:      map[string]string{},
			expectAllowed:  true,
			expectListPods: false,
			expectWarnings: []string{
				`namespace "test" is exempt from Pod Security, and the policy (enforce=restricted:v1.0, audit=restricted:latest, warn=baseline:latest) will be ignored`,
			},
		},

		// update tests that introduce labels errors
		{
			name:           "update malformed level",
			newLabels:      map[string]string{api.EnforceLevelLabel: "unknown"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  false,
			expectError:    `must be one of privileged, baseline, restricted`,
			expectListPods: false,
		},
		{
			name:           "update malformed version",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelPrivileged), api.EnforceVersionLabel: "unknown"},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted), api.EnforceVersionLabel: "v1.0"},
			expectAllowed:  false,
			expectError:    `must be "latest" or "v1.x"`,
			expectListPods: false,
		},

		// update tests that tighten effective policy
		{
			name:           "update to implicit restricted",
			newLabels:      map[string]string{},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline), api.EnforceVersionLabel: "v1.0"},
			defaultPolicy:  &api.Policy{Enforce: api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()}},
			expectAllowed:  true,
			expectListPods: true,
			expectEvaluate: api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()},
			expectWarnings: []string{
				`existing pods in namespace "test" violate the new PodSecurity enforce level "restricted:latest"`,
				"noruntimeclasspod (and 2 other pods): message",
				"runtimeclass3pod: message, message2",
			},
		},
		{
			name:                 "update with runtimeclass exempt pods",
			exemptRuntimeClasses: []string{"runtimeclass1"},
			newLabels:            map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			oldLabels:            map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			expectAllowed:        true,
			expectListPods:       true,
			expectEvaluate:       api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()},
			expectWarnings: []string{
				`existing pods in namespace "test" violate the new PodSecurity enforce level "restricted:latest"`,
				"noruntimeclasspod (and 1 other pod): message",
				"runtimeclass3pod: message, message2",
			},
		},
		{
			name:           "timeout on list",
			newLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			oldLabels:      map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			delayList:      time.Second + 100*time.Millisecond,
			expectAllowed:  true,
			expectListPods: true,
			expectWarnings: []string{
				`failed to list pods while checking new PodSecurity enforce level`,
			},
		},
		{
			name:            "timeout on evaluate",
			newLabels:       map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			oldLabels:       map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			delayEvaluation: (time.Second + 100*time.Millisecond) / 2, // leave time for two evaluations
			expectAllowed:   true,
			expectListPods:  true,
			expectEvaluate:  api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()},
			expectWarnings: []string{
				`new PodSecurity enforce level only checked against the first 2 of 4 existing pods`,
				`existing pods in namespace "test" violate the new PodSecurity enforce level "restricted:latest"`,
				`noruntimeclasspod (and 1 other pod): message`,
			},
		},
		{
			name:      "bound number of pods",
			newLabels: map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			oldLabels: map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Annotations: map[string]string{"error": "message"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Annotations: map[string]string{"error": "message"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Annotations: map[string]string{"error": "message"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod4", Annotations: map[string]string{"error": "message"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod5", Annotations: map[string]string{"error": "message"}}},
			},
			expectAllowed:  true,
			expectListPods: true,
			expectEvaluate: api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()},
			expectWarnings: []string{
				`new PodSecurity enforce level only checked against the first 4 of 5 existing pods`,
				`existing pods in namespace "test" violate the new PodSecurity enforce level "restricted:latest"`,
				`pod1 (and 3 other pods): message`,
			},
		},
		{
			name:                 "prioritized pods",
			exemptRuntimeClasses: []string{"runtimeclass1"},
			newLabels:            map[string]string{api.EnforceLevelLabel: string(api.LevelRestricted)},
			oldLabels:            map[string]string{api.EnforceLevelLabel: string(api.LevelBaseline)},
			pods: []*corev1.Pod{
				// ensure exempt pods don't use up the limit of evaluated pods
				{ObjectMeta: metav1.ObjectMeta{Name: "exemptpod1", Annotations: map[string]string{"error": "message1"}}, Spec: corev1.PodSpec{RuntimeClassName: pointer.String("runtimeclass1")}},
				{ObjectMeta: metav1.ObjectMeta{Name: "exemptpod2", Annotations: map[string]string{"error": "message1"}}, Spec: corev1.PodSpec{RuntimeClassName: pointer.String("runtimeclass1")}},
				{ObjectMeta: metav1.ObjectMeta{Name: "exemptpod3", Annotations: map[string]string{"error": "message1"}}, Spec: corev1.PodSpec{RuntimeClassName: pointer.String("runtimeclass1")}},
				{ObjectMeta: metav1.ObjectMeta{Name: "exemptpod4", Annotations: map[string]string{"error": "message1"}}, Spec: corev1.PodSpec{RuntimeClassName: pointer.String("runtimeclass1")}},
				// ensure replicas from the same controller don't use up limit of evaluated pods
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset1pod1", Annotations: map[string]string{"error": "replicaset1error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("1"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset1pod2", Annotations: map[string]string{"error": "replicaset1error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("1"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset1pod3", Annotations: map[string]string{"error": "replicaset1error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("1"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset1pod4", Annotations: map[string]string{"error": "replicaset1error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("1"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset2pod1", Annotations: map[string]string{"error": "replicaset2error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("2"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset2pod2", Annotations: map[string]string{"error": "replicaset2error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("2"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset2pod3", Annotations: map[string]string{"error": "replicaset2error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("2"), Controller: pointer.Bool(true)}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "replicaset2pod4", Annotations: map[string]string{"error": "replicaset2error"}, OwnerReferences: []metav1.OwnerReference{{UID: types.UID("2"), Controller: pointer.Bool(true)}}}},
				// ensure unique pods are prioritized before additional replicas
				{ObjectMeta: metav1.ObjectMeta{Name: "uniquepod1", Annotations: map[string]string{"error": "uniquemessage1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "uniquepod2", Annotations: map[string]string{"error": "uniquemessage2"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "uniquepod3", Annotations: map[string]string{"error": "uniquemessage3"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "uniquepod4", Annotations: map[string]string{"error": "uniquemessage4"}}},
			},
			expectAllowed:  true,
			expectListPods: true,
			expectEvaluate: api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()},
			expectWarnings: []string{
				`new PodSecurity enforce level only checked against the first 4 of 12 existing pods`,
				`existing pods in namespace "test" violate the new PodSecurity enforce level "restricted:latest"`,
				`replicaset1pod1: replicaset1error`,
				`replicaset2pod1: replicaset2error`,
				`uniquepod1: uniquemessage1`,
				`uniquepod2: uniquemessage2`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			newObject := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: tc.newLabels,
				},
			}
			var operation = admissionv1.Create
			var oldObject runtime.Object
			if tc.oldLabels != nil {
				operation = admissionv1.Update
				oldObject = &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "test",
						Labels: tc.oldLabels,
					},
				}
			}

			attrs := &api.AttributesRecord{
				Object:      newObject,
				OldObject:   oldObject,
				Name:        newObject.Name,
				Namespace:   newObject.Name,
				Kind:        schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"},
				Resource:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"},
				Subresource: tc.subresource,
				Operation:   operation,
			}

			defaultPolicy := api.Policy{
				Enforce: api.LevelVersion{
					Level:   api.LevelPrivileged,
					Version: api.LatestVersion(),
				},
				Audit: api.LevelVersion{
					Level:   api.LevelPrivileged,
					Version: api.LatestVersion(),
				},
				Warn: api.LevelVersion{
					Level:   api.LevelPrivileged,
					Version: api.LatestVersion(),
				},
			}
			if tc.defaultPolicy != nil {
				defaultPolicy = *tc.defaultPolicy
			}

			pods := tc.pods
			if pods == nil {
				pods = []*corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "noruntimeclasspod", Annotations: map[string]string{"error": "message"}},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "runtimeclass1pod", Annotations: map[string]string{"error": "message"}},
						Spec:       corev1.PodSpec{RuntimeClassName: pointer.String("runtimeclass1")},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "runtimeclass2pod", Annotations: map[string]string{"error": "message"}},
						Spec:       corev1.PodSpec{RuntimeClassName: pointer.String("runtimeclass2")},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "runtimeclass3pod", Annotations: map[string]string{"error": "message, message2"}},
					},
				}
			}
			podLister := &testPodLister{pods: pods, delay: tc.delayList}
			evaluator := &testEvaluator{delay: tc.delayEvaluation}
			a := &Admission{
				PodLister: podLister,
				Evaluator: evaluator,
				Configuration: &admissionapi.PodSecurityConfiguration{
					Exemptions: admissionapi.PodSecurityExemptions{
						Namespaces:     tc.exemptNamespaces,
						RuntimeClasses: tc.exemptRuntimeClasses,
					},
				},
				Metrics:       &FakeRecorder{},
				defaultPolicy: defaultPolicy,

				namespacePodCheckTimeout: time.Second,
				namespaceMaxPodsToCheck:  4,
			}
			result := a.ValidateNamespace(ctx, attrs)
			if result.Allowed != tc.expectAllowed {
				t.Errorf("expected allowed=%v, got %v", tc.expectAllowed, result.Allowed)
			}

			resultError := ""
			if result.Result != nil {
				resultError = result.Result.Message
			}
			if (len(resultError) > 0) != (len(tc.expectError) > 0) {
				t.Errorf("expected error=%v, got %v", tc.expectError, resultError)
			}
			if len(tc.expectError) > 0 && !strings.Contains(resultError, tc.expectError) {
				t.Errorf("expected error containing '%s', got %s", tc.expectError, resultError)
			}
			if podLister.called != tc.expectListPods {
				t.Errorf("expected getPods=%v, got %v", tc.expectListPods, podLister.called)
			}
			if evaluator.lv != tc.expectEvaluate {
				t.Errorf("expected to evaluate %v, got %v", tc.expectEvaluate, evaluator.lv)
			}
			if !reflect.DeepEqual(result.Warnings, tc.expectWarnings) {
				t.Errorf("expected warnings:\n%v\ngot\n%v", strings.Join(tc.expectWarnings, "\n"), strings.Join(result.Warnings, "\n"))
			}
		})
	}
}

func TestValidatePodAndController(t *testing.T) {
	const (
		exemptNs        = "exempt-ns"
		implicitNs      = "implicit-ns"
		privilegedNs    = "privileged-ns"
		baselineNs      = "baseline-ns"
		baselineWarnNs  = "baseline-warn-ns"
		baselineAuditNs = "baseline-audit-ns"
		restrictedNs    = "restricted-ns"
		invalidNs       = "invalid-ns"

		exemptUser         = "exempt-user"
		exemptRuntimeClass = "exempt-runtimeclass"

		podName = "test-pod"
	)

	objMetadata := metav1.ObjectMeta{Name: podName, Labels: map[string]string{"foo": "bar"}}

	restrictedPod, err := test.GetMinimalValidPod(api.LevelRestricted, api.MajorMinorVersion(1, 23))
	require.NoError(t, err)
	restrictedPod.ObjectMeta = *objMetadata.DeepCopy()

	baselinePod, err := test.GetMinimalValidPod(api.LevelBaseline, api.MajorMinorVersion(1, 23))
	require.NoError(t, err)
	baselinePod.ObjectMeta = *objMetadata.DeepCopy()

	privilegedPod := *baselinePod.DeepCopy()
	privilegedPod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		Privileged: pointer.Bool(true),
	}

	exemptRCPod := *privilegedPod.DeepCopy()
	exemptRCPod.Spec.RuntimeClassName = pointer.String(exemptRuntimeClass)

	tolerantPod := *privilegedPod.DeepCopy()
	tolerantPod.Spec.Tolerations = []corev1.Toleration{{
		Operator: corev1.TolerationOpExists,
	}}

	differentPrivilegedPod := *privilegedPod.DeepCopy()
	differentPrivilegedPod.Spec.Containers[0].Image = "https://example.com/a-different-image"

	differentRestrictedPod := *restrictedPod.DeepCopy()
	differentRestrictedPod.Spec.Containers[0].Image = "https://example.com/a-different-image"

	emptyDeployment := appsv1.Deployment{
		ObjectMeta: *objMetadata.DeepCopy(),
		Spec:       appsv1.DeploymentSpec{},
	}

	makeNs := func(enforceLevel, warnLevel, auditLevel api.Level) *corev1.Namespace {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{},
			},
		}
		if enforceLevel != "" {
			ns.Labels[api.EnforceLevelLabel] = string(enforceLevel)
		}
		if warnLevel != "" {
			ns.Labels[api.WarnLevelLabel] = string(warnLevel)
		}
		if auditLevel != "" {
			ns.Labels[api.AuditLevelLabel] = string(auditLevel)
		}
		return ns
	}
	nsGetter := testNamespaceGetter{
		exemptNs:        makeNs(api.LevelRestricted, api.LevelRestricted, api.LevelRestricted),
		implicitNs:      makeNs("", "", ""),
		privilegedNs:    makeNs(api.LevelPrivileged, api.LevelPrivileged, api.LevelPrivileged),
		baselineNs:      makeNs(api.LevelBaseline, api.LevelBaseline, api.LevelBaseline),
		baselineWarnNs:  makeNs("", api.LevelBaseline, ""),
		baselineAuditNs: makeNs("", "", api.LevelBaseline),
		restrictedNs:    makeNs(api.LevelRestricted, api.LevelRestricted, api.LevelRestricted),
		invalidNs:       makeNs("not-a-valid-level", "", ""),
	}

	config, err := load.LoadFromData(nil) // Start with the default config.
	require.NoError(t, err, "loading default config")
	config.Exemptions.Namespaces = []string{exemptNs}
	config.Exemptions.RuntimeClasses = []string{exemptRuntimeClass}
	config.Exemptions.Usernames = []string{exemptUser}

	evaluator, err := policy.NewEvaluator(policy.DefaultChecks())
	assert.NoError(t, err)

	type testCase struct {
		desc string

		namespace string
		username  string

		// pod and oldPod are used to populate obj and oldObj respectively, according to the test type (pod or deployment).
		pod    *corev1.Pod
		oldPod *corev1.Pod

		operation   admissionv1.Operation
		resource    schema.GroupVersionResource
		kind        schema.GroupVersionKind
		obj         runtime.Object
		oldObj      runtime.Object
		objErr      error // Error to return instead of obj by attrs.GetObject()
		oldObjErr   error // Error to return instead of oldObj by attrs.GetOldObject()
		subresource string

		skipPod        bool // Whether to skip the ValidatePod test case.
		skipDeployment bool // Whether to skip the ValidatePodController test case.

		expectAllowed bool
		expectReason  metav1.StatusReason
		expectExempt  bool
		expectError   bool

		expectEnforce api.Level
		expectWarning api.Level
		expectAudit   api.Level
	}
	podCases := []testCase{
		{
			desc:          "ignored subresource",
			namespace:     restrictedNs,
			pod:           privilegedPod.DeepCopy(),
			subresource:   "status",
			expectAllowed: true,
		},
		{
			desc:          "exempt namespace",
			namespace:     exemptNs,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: true,
			expectExempt:  true,
		},
		{
			desc:          "exempt user",
			namespace:     restrictedNs,
			username:      exemptUser,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: true,
			expectExempt:  true,
		},
		{
			desc:          "exempt runtimeClass",
			namespace:     restrictedNs,
			pod:           exemptRCPod.DeepCopy(),
			expectAllowed: true,
			expectExempt:  true,
		},
		{
			desc:          "namespace not found",
			namespace:     "missing-ns",
			pod:           restrictedPod.DeepCopy(),
			expectAllowed: false,
			expectReason:  metav1.StatusReasonInternalError,
			expectError:   true,
		},
		{
			desc:          "short-circuit privileged:latest (implicit)",
			namespace:     implicitNs,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: true,
			expectEnforce: api.LevelPrivileged,
		},
		{
			desc:          "short-circuit privileged:latest (explicit)",
			namespace:     privilegedNs,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: true,
			expectEnforce: api.LevelPrivileged,
		},
		{
			desc:          "failed decode",
			namespace:     baselineNs,
			objErr:        fmt.Errorf("expected (failed decode)"),
			expectAllowed: false,
			expectReason:  metav1.StatusReasonBadRequest,
			expectError:   true,
		},
		{
			desc:          "invalid object",
			namespace:     baselineNs,
			operation:     admissionv1.Update,
			obj:           &corev1.Namespace{},
			expectAllowed: false,
			expectReason:  metav1.StatusReasonBadRequest,
			expectError:   true,
		},
		{
			desc:           "failed decode old object",
			namespace:      baselineNs,
			operation:      admissionv1.Update,
			pod:            restrictedPod.DeepCopy(),
			oldObjErr:      fmt.Errorf("expected (failed decode)"),
			expectAllowed:  false,
			expectReason:   metav1.StatusReasonBadRequest,
			expectError:    true,
			skipDeployment: true, // Updates aren't special cased for controller resources.
		},
		{
			desc:           "invalid old object",
			namespace:      baselineNs,
			operation:      admissionv1.Update,
			pod:            restrictedPod.DeepCopy(),
			oldObj:         &corev1.Namespace{},
			expectAllowed:  false,
			expectReason:   metav1.StatusReasonBadRequest,
			expectError:    true,
			skipDeployment: true, // Updates aren't special cased for controller resources.
		},
		{
			desc:           "insignificant update",
			namespace:      restrictedNs,
			operation:      admissionv1.Update,
			pod:            tolerantPod.DeepCopy(),
			oldPod:         privilegedPod.DeepCopy(),
			expectAllowed:  true,
			skipDeployment: true, // Updates aren't special cased for controller resources.
		},
		{
			desc:          "significant update denied",
			namespace:     restrictedNs,
			operation:     admissionv1.Update,
			pod:           differentPrivilegedPod.DeepCopy(),
			oldPod:        privilegedPod.DeepCopy(),
			expectAllowed: false,
			expectReason:  metav1.StatusReasonForbidden,
			expectEnforce: api.LevelRestricted,
			expectWarning: api.LevelRestricted,
			expectAudit:   api.LevelRestricted,
		},
		{
			desc:          "significant update allowed",
			namespace:     restrictedNs,
			operation:     admissionv1.Update,
			pod:           differentRestrictedPod.DeepCopy(),
			oldPod:        restrictedPod,
			expectAllowed: true,
			expectEnforce: api.LevelRestricted,
		},
		{
			desc:          "invalid namespace labels",
			namespace:     invalidNs,
			pod:           baselinePod.DeepCopy(),
			expectAllowed: false,
			expectReason:  metav1.StatusReasonForbidden,
			expectEnforce: api.LevelRestricted,
			expectError:   true,
		},
		{
			desc:          "enforce deny",
			namespace:     restrictedNs,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: false,
			expectReason:  metav1.StatusReasonForbidden,
			expectEnforce: api.LevelRestricted,
			expectWarning: api.LevelRestricted,
			expectAudit:   api.LevelRestricted,
		},
		{
			desc:          "enforce allow",
			namespace:     baselineNs,
			pod:           baselinePod.DeepCopy(),
			expectAllowed: true,
			expectEnforce: api.LevelBaseline,
		},
		{
			desc:          "warn deny",
			namespace:     baselineWarnNs,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: true,
			expectEnforce: api.LevelPrivileged,
			expectWarning: api.LevelBaseline,
		},
		{
			desc:          "audit deny",
			namespace:     baselineAuditNs,
			pod:           privilegedPod.DeepCopy(),
			expectAllowed: true,
			expectEnforce: api.LevelPrivileged,
			expectAudit:   api.LevelBaseline,
		},
		{
			desc:          "no pod template",
			namespace:     restrictedNs,
			obj:           emptyDeployment.DeepCopy(),
			expectAllowed: true,
			expectWarning: "", // No pod template skips validation.
			skipPod:       true,
		},
	}

	podToDeployment := func(pod *corev1.Pod) *appsv1.Deployment {
		if pod == nil {
			return nil
		}
		return &appsv1.Deployment{
			ObjectMeta: pod.ObjectMeta,
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: pod.ObjectMeta,
					Spec:       pod.Spec,
				},
			},
		}
	}

	// Convert "pod cases" into pod test cases & deployment test cases.
	testCases := []testCase{}
	for _, tc := range podCases {
		podTest := tc
		podTest.desc = "pod:" + tc.desc
		podTest.resource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		podTest.kind = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
		if !tc.expectAllowed {
			podTest.expectWarning = "" // Warnings should only be returned when the request is allowed.
		}

		deploymentTest := tc
		deploymentTest.desc = "deployment:" + tc.desc
		deploymentTest.resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		deploymentTest.kind = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
		// PodController validation is always non-enforcing.
		deploymentTest.expectAllowed = true
		deploymentTest.expectEnforce = ""
		deploymentTest.expectReason = ""

		if tc.pod != nil {
			podTest.obj = tc.pod
			deploymentTest.obj = podToDeployment(tc.pod)
		}
		if tc.oldPod != nil {
			podTest.oldObj = tc.oldPod
			deploymentTest.oldObj = podToDeployment(tc.oldPod)
		}
		if !tc.skipPod {
			testCases = append(testCases, podTest)
		}
		if !tc.skipDeployment {
			testCases = append(testCases, deploymentTest)
		}
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			if tc.obj != nil {
				tc.obj.(metav1.ObjectMetaAccessor).GetObjectMeta().SetNamespace(tc.namespace)
			}
			if tc.oldObj != nil {
				tc.oldObj.(metav1.ObjectMetaAccessor).GetObjectMeta().SetNamespace(tc.namespace)
			}
			attrs := &testAttributes{
				AttributesRecord: api.AttributesRecord{
					Name:        "test-pod",
					Namespace:   tc.namespace,
					Kind:        tc.kind,
					Resource:    tc.resource,
					Subresource: tc.subresource,
					Operation:   admissionv1.Create,
					Object:      tc.obj,
					OldObject:   tc.oldObj,
					Username:    "test-user",
				},
				objectErr:    tc.objErr,
				oldObjectErr: tc.oldObjErr,
			}
			if tc.operation != "" {
				attrs.Operation = tc.operation
			}
			if tc.username != "" {
				attrs.Username = tc.username
			}

			recorder := &FakeRecorder{}
			a := &Admission{
				PodLister:       &testPodLister{},
				Evaluator:       evaluator,
				Configuration:   config,
				Metrics:         recorder,
				NamespaceGetter: nsGetter,
			}
			require.NoError(t, a.CompleteConfiguration(), "CompleteConfiguration()")
			require.NoError(t, a.ValidateConfiguration(), "ValidateConfiguration()")

			response := a.Validate(ctx, attrs)

			var expectedEvaluations []MetricsRecord
			var expectedAuditAnnotationKeys []string
			if tc.expectAllowed {
				assert.True(t, response.Allowed, "Allowed")
				assert.Nil(t, response.Result)
			} else {
				assert.False(t, response.Allowed)
				if assert.NotNil(t, response.Result, "Result") {
					assert.Equal(t, tc.expectReason, response.Result.Reason, "Reason")
				}
			}

			if tc.expectWarning != "" {
				assert.NotEmpty(t, response.Warnings, "Warnings")
			} else {
				assert.Empty(t, response.Warnings, "Warnings")
			}

			if tc.expectEnforce != "" {
				expectedAuditAnnotationKeys = append(expectedAuditAnnotationKeys, "enforce-policy")
				record := MetricsRecord{podName, metrics.DecisionAllow, tc.expectEnforce, metrics.ModeEnforce}
				if !tc.expectAllowed {
					record.EvalDecision = metrics.DecisionDeny
				}
				expectedEvaluations = append(expectedEvaluations, record)
			}
			if tc.expectWarning != "" {
				expectedEvaluations = append(expectedEvaluations, MetricsRecord{podName, metrics.DecisionDeny, tc.expectWarning, metrics.ModeWarn})
			}
			if tc.expectAudit != "" {
				expectedEvaluations = append(expectedEvaluations, MetricsRecord{podName, metrics.DecisionDeny, tc.expectAudit, metrics.ModeAudit})
				expectedAuditAnnotationKeys = append(expectedAuditAnnotationKeys, "audit-violations")
			}
			if tc.expectError {
				expectedAuditAnnotationKeys = append(expectedAuditAnnotationKeys, "error")
				assert.ElementsMatch(t, []MetricsRecord{{ObjectName: podName}}, recorder.errors, "expected RecordError() calls")
			} else {
				assert.Empty(t, recorder.errors, "expected RecordError() calls")
			}
			if tc.expectExempt {
				expectedAuditAnnotationKeys = append(expectedAuditAnnotationKeys, "exempt")
				assert.ElementsMatch(t, []MetricsRecord{{ObjectName: podName}}, recorder.exemptions, "expected RecordExemption() calls")
			} else {
				assert.Empty(t, recorder.exemptions, "expected RecordExemption() calls")
			}

			assert.Len(t, response.AuditAnnotations, len(expectedAuditAnnotationKeys), "AuditAnnotations")
			for _, key := range expectedAuditAnnotationKeys {
				assert.Contains(t, response.AuditAnnotations, key, "AuditAnnotations")
			}

			assert.ElementsMatch(t, expectedEvaluations, recorder.evaluations, "expected RecordEvaluation() calls")
		})
	}
}

type FakeRecorder struct {
	evaluations []MetricsRecord
	exemptions  []MetricsRecord
	errors      []MetricsRecord
}

type MetricsRecord struct {
	ObjectName   string
	EvalDecision metrics.Decision
	EvalPolicy   api.Level
	EvalMode     metrics.Mode
}

func (r *FakeRecorder) RecordEvaluation(decision metrics.Decision, policy api.LevelVersion, evalMode metrics.Mode, attrs api.Attributes) {
	r.evaluations = append(r.evaluations, MetricsRecord{attrs.GetName(), decision, policy.Level, evalMode})
}

func (r *FakeRecorder) RecordExemption(attrs api.Attributes) {
	r.exemptions = append(r.exemptions, MetricsRecord{ObjectName: attrs.GetName()})
}
func (r *FakeRecorder) RecordError(_ bool, attrs api.Attributes) {
	r.errors = append(r.errors, MetricsRecord{ObjectName: attrs.GetName()})
}

func TestPrioritizePods(t *testing.T) {
	isController := true
	sampleOwnerReferences := []struct {
		ownerRefs []metav1.OwnerReference
	}{
		{
			ownerRefs: []metav1.OwnerReference{
				{
					UID:        uuid.NewUUID(),
					Controller: &isController,
				},
			},
		}, {
			ownerRefs: []metav1.OwnerReference{
				{
					UID:        uuid.NewUUID(),
					Controller: &isController,
				},
			},
		}, {
			ownerRefs: []metav1.OwnerReference{
				{
					UID:        uuid.NewUUID(),
					Controller: &isController,
				},
			},
		},
	}

	var pods []*corev1.Pod
	randomSource := rand.NewSource(time.Now().Unix())
	for _, sampleOwnerRef := range sampleOwnerReferences {
		// Generate multiple pods for a controller
		for i := 0; i < rand.New(randomSource).Intn(5)+len(sampleOwnerReferences); i++ {
			pods = append(pods, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: sampleOwnerRef.ownerRefs,
				},
				Spec: corev1.PodSpec{},
			})
		}
	}
	a := &Admission{}
	prioritizedPods := a.prioritizePods(pods)
	controllerRef := make(map[types.UID]bool)

	for i := 0; i < len(sampleOwnerReferences); i++ {
		if controllerRef[metav1.GetControllerOfNoCopy(prioritizedPods[i]).UID] {
			assert.Fail(t, "Pods are not prioritized based on uniqueness of the controller")
		}
		controllerRef[metav1.GetControllerOfNoCopy(prioritizedPods[i]).UID] = true
	}
	if len(prioritizedPods) != len(pods) {
		assert.Fail(t, "Pod count is not the same after prioritization")
	}
}

func TestExemptNamespaceWarning(t *testing.T) {
	privileged := api.LevelVersion{
		Level:   api.LevelPrivileged,
		Version: api.LatestVersion(),
	}
	privilegedPolicy := api.Policy{
		Enforce: privileged,
		Audit:   privileged,
		Warn:    privileged,
	}
	baseline := api.LevelVersion{
		Level:   api.LevelBaseline,
		Version: api.MajorMinorVersion(1, 23),
	}
	baselinePolicy := api.Policy{
		Enforce: baseline,
		Audit:   baseline,
		Warn:    baseline,
	}
	tests := []struct {
		name                  string
		labels                map[string]string
		defaultPolicy         api.Policy // Defaults to privilegedPolicy if empty.
		expectWarning         bool
		expectWarningContains string
	}{{
		name:          "empty-case",
		expectWarning: false,
	}, {
		name: "ignore-privileged",
		labels: map[string]string{
			api.EnforceLevelLabel:   string(api.LevelPrivileged),
			api.EnforceVersionLabel: "v1.24",
			api.WarnVersionLabel:    "v1.25",
		},
		expectWarning: false,
	}, {
		name: "warn-on-enforce",
		labels: map[string]string{
			api.EnforceLevelLabel: string(api.LevelBaseline),
		},
		expectWarning:         true,
		expectWarningContains: "(enforce=baseline:latest)",
	}, {
		name: "warn-on-warn",
		labels: map[string]string{
			api.WarnLevelLabel: string(api.LevelBaseline),
		},
		expectWarning:         true,
		expectWarningContains: "(warn=baseline:latest)",
	}, {
		name: "warn-on-audit",
		labels: map[string]string{
			api.AuditLevelLabel: string(api.LevelRestricted),
		},
		expectWarning:         true,
		expectWarningContains: "(audit=restricted:latest)",
	}, {
		name:          "ignore-default-policy",
		defaultPolicy: baselinePolicy,
		expectWarning: false,
	}, {
		name: "warn-versions-default-policy",
		labels: map[string]string{
			api.WarnVersionLabel: "latest",
		},
		defaultPolicy:         baselinePolicy,
		expectWarning:         true,
		expectWarningContains: "(warn=baseline:latest)",
	}}

	const (
		sentinelLabelKey   = "qqincegidneocgu"
		sentinelLabelValue = "vpmxkpcjphxrcpx"
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defaultPolicy := test.defaultPolicy
			if defaultPolicy == (api.Policy{}) {
				defaultPolicy = privilegedPolicy
			}
			a := &Admission{defaultPolicy: defaultPolicy}
			labels := test.labels
			if labels == nil {
				labels = map[string]string{}
			}
			labels[sentinelLabelKey] = sentinelLabelValue
			policy, err := api.PolicyToEvaluate(labels, defaultPolicy)
			require.NoError(t, err.ToAggregate())

			warning := a.exemptNamespaceWarning(test.name, policy, labels)
			if !test.expectWarning {
				assert.Empty(t, warning)
				return
			}
			require.NotEmpty(t, warning)

			assert.NotContains(t, warning, sentinelLabelKey, "non-podsecurity label key included")
			assert.NotContains(t, warning, sentinelLabelValue, "non-podsecurity label value included")

			assert.Contains(t, warning, test.expectWarningContains)
		})
	}
}

type testAttributes struct {
	api.AttributesRecord

	objectErr    error
	oldObjectErr error
}

func (a *testAttributes) GetObject() (runtime.Object, error) {
	if a.objectErr != nil {
		return nil, a.objectErr
	} else {
		return a.AttributesRecord.GetObject()
	}
}

func (a *testAttributes) GetOldObject() (runtime.Object, error) {
	if a.oldObjectErr != nil {
		return nil, a.oldObjectErr
	} else {
		return a.AttributesRecord.GetOldObject()
	}
}
