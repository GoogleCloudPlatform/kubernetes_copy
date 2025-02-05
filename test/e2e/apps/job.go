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

package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	batchinternal "k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ejob "k8s.io/kubernetes/test/e2e/framework/job"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eresource "k8s.io/kubernetes/test/e2e/framework/resource"
	"k8s.io/kubernetes/test/e2e/scheduling"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

type watchEventConfig struct {
	framework           *framework.Framework
	resourceVersion     string
	w                   *cache.ListWatch
	jobName             string
	watchEvent          watch.EventType
	extJob              *batchv1.Job
	updatedMetadataType string
	updatedKey          string
	updatedValue        string
}

var _ = SIGDescribe("Job", func() {
	f := framework.NewDefaultFramework("job")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	// Simplest case: N pods succeed
	ginkgo.It("should run a job to completion when tasks succeed", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("succeed", "all-succeed", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonCompletionsReached), completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring pods for job exist")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get pod list for job in namespace: %s", f.Namespace.Name)
		successes := int32(0)
		for _, pod := range pods.Items {
			if pod.Status.Phase == v1.PodSucceeded {
				successes++
			}
		}
		gomega.Expect(successes).To(gomega.Equal(completions), "expected %d successful job pods, but got  %d", completions, successes)
	})

	/*
		Release: v1.31
		Testname: Verify Pod Failure policy allows to fail job early on exit code.
		Description: Create a job with pod failure policy, and exactly one
		pod failing. The exit code of the failed pod matches the pod failure
		policy triggering the Job failure.
	*/
	framework.ConformanceIt("should allow to use the pod failure policy on exit code to fail the job early", func(ctx context.Context) {

		// We fail the Job's pod only once to ensure the backoffLimit is not
		// reached and thus the job is failed due to the pod failure policy
		// with FailJob action.
		// In order to ensure a Job's pod fails once before succeeding we force
		// the Job's Pods to be scheduled to a single Node and use a hostPath
		// volume to persist data across new Pods.
		ginkgo.By("Looking for a node to schedule job pod")
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)

		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value
		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJobOnNode("failOnce", "pod-failure-failjob", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit, node.Name)
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{
				{
					Action: batchv1.PodFailurePolicyActionFailJob,
					OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
						Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
						Values:   []int32{1},
					},
				},
			},
		}
		job, err = e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job fails")
		err = e2ejob.WaitForJobFailed(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to ensure job failure in namespace: %s", f.Namespace.Name)
	})

	/*
		Testname: Ensure pod failure policy allows to ignore failure matching on the exit code
		Description: This test is using an indexed job. The pod corresponding to each index
		creates a marker file on the host and fails. Once the marker file is
		created the pod succeeds seeing it on restart. Thus, we trigger one
		failure per index, so the Job would be marked as failed, if not for the
		ignore rule matching on exit codes.
	*/
	ginkgo.It("should allow to use a pod failure policy to ignore failure matching on exit code", func(ctx context.Context) {
		// We set the backoffLimit = numPods-1  so that we can tolerate random
		// failures (like OutOfPods from kubelet). Yet, the Job would fail if the
		// pod failures were not be ignored.
		numPods := 3
		parallelism := int32(numPods)
		completions := int32(numPods)
		backoffLimit := int32(numPods) - 1

		ginkgo.By("Looking for a node to schedule job pods")
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJobOnNode("failOncePerIndex", "fail-pod-ignore-on-exit-code", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit, node.Name)
		job.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{
				{
					Action: batchv1.PodFailurePolicyActionIgnore,
					OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
						Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
						Values:   []int32{42},
					},
				},
			},
		}
		job, err = e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, nil, completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)
	})

	/*
		Release: v1.32
		Testname: Ensure pod failure policy allows to ignore failure matching on the DisruptionTarget condition
		Description: This test is using an indexed job. The pod corresponding to each index
		creates a marker file on the host and runs 'forever' until evicted. Once
		the marker file is created the pod succeeds seeing it on restart. Thus,
		we trigger one failure per index due to eviction (DisruptionTarget
		condition is added in the process). The Job would be marked as failed,
		if not for the ignore rule matching on exit codes.
	*/
	framework.ConformanceIt("should allow to use a pod failure policy to ignore failure matching on DisruptionTarget condition", func(ctx context.Context) {
		// We set the backoffLimit = numPods-1 so that we can tolerate random
		// failures (like OutOfPods from kubelet). Yet, the Job would fail if the
		// pod failures were not be ignored.
		numPods := 3
		parallelism := int32(numPods)
		completions := int32(numPods)
		backoffLimit := int32(numPods) - 1

		ginkgo.By("Looking for a node to schedule job pods")
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJobOnNode("notTerminateOncePerIndex", "evicted-pod-ignore-on-disruption-condition", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit, node.Name)
		job.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{
				{
					// Ignore the pod failure caused by the eviction based on the
					// DisruptionTarget condition
					Action: batchv1.PodFailurePolicyActionIgnore,
					OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
		}
		job, err = e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Waiting for all the pods to be ready")
		err = e2ejob.WaitForJobReady(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(int32(numPods)))
		framework.ExpectNoError(err, "failed to await for all pods to be ready for job: %s/%s", job.Name, job.Namespace)

		ginkgo.By("Fetch all running pods")
		pods, err := e2ejob.GetAllRunningJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get running pods for the job: %s/%s", job.Name, job.Namespace)
		gomega.Expect(pods).To(gomega.HaveLen(numPods), "Number of running pods doesn't match parallelism")

		ginkgo.By("Evict all the Pods")
		workqueue.ParallelizeUntil(ctx, numPods, numPods, func(index int) {
			defer ginkgo.GinkgoRecover()

			pod := pods[index]
			ginkgo.By(fmt.Sprintf("Evicting the running pod: %s/%s", pod.Name, pod.Namespace))
			evictTarget := &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pod.Name,
					Namespace: pod.Namespace,
				},
			}
			err = f.ClientSet.CoreV1().Pods(pod.Namespace).EvictV1(ctx, evictTarget)
			framework.ExpectNoError(err, "failed to evict the pod: %s/%s", pod.Name, pod.Namespace)

			ginkgo.By(fmt.Sprintf("Awaiting for the pod: %s/%s to be deleted", pod.Name, pod.Namespace))
			err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, f.Timeouts.PodDelete)
			framework.ExpectNoError(err, "failed to await for all pods to be deleted: %s/%s", pod.Name, pod.Namespace)
		})

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, nil, completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)
	})

	ginkgo.It("should not create pods when created in suspend state", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job with suspend=true")
		job := e2ejob.NewTestJob("succeed", "suspend-true-to-false", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.Suspend = pointer.BoolPtr(true)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Checking Job status to observe Suspended state")
		err = e2ejob.WaitForJobSuspend(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to observe suspend state: %s", f.Namespace.Name)

		ginkgo.By("Ensuring pods aren't created for job")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to list pod for a given job %s in namespace %s", job.Name, f.Namespace.Name)
		gomega.Expect(pods.Items).To(gomega.BeEmpty())

		ginkgo.By("Updating the job with suspend=false")
		job, err = f.ClientSet.BatchV1().Jobs(f.Namespace.Name).Get(ctx, job.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get job in namespace: %s", f.Namespace.Name)
		job.Spec.Suspend = pointer.BoolPtr(false)
		job, err = e2ejob.UpdateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to update job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Waiting for job to complete")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonCompletionsReached), completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)
	})

	ginkgo.It("should delete pods when suspended", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job with suspend=false")
		job := e2ejob.NewTestJob("notTerminate", "suspend-false-to-true", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.Suspend = pointer.Bool(false)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure pods equal to parallelism count is attached to the job")
		err = e2ejob.WaitForJobPodsRunning(ctx, f.ClientSet, f.Namespace.Name, job.Name, parallelism)
		framework.ExpectNoError(err, "failed to ensure number of pods associated with job %s is equal to parallelism count in namespace: %s", job.Name, f.Namespace.Name)

		ginkgo.By("Updating the job with suspend=true")
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
			framework.ExpectNoError(err, "unable to get job %s in namespace %s", job.Name, f.Namespace.Name)
			job.Spec.Suspend = pointer.Bool(true)
			updatedJob, err := e2ejob.UpdateJob(ctx, f.ClientSet, f.Namespace.Name, job)
			if err == nil {
				job = updatedJob
			}
			return err
		})
		framework.ExpectNoError(err, "failed to update job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring pods are deleted")
		err = e2ejob.WaitForAllJobPodsGone(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to ensure pods are deleted after suspend=true")

		ginkgo.By("Checking Job status to observe Suspended state")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		exists := false
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobSuspended {
				exists = true
				break
			}
		}
		if !exists {
			framework.Failf("Job was expected to be completed or failed")
		}
	})

	ginkgo.It("should recreate pods only after they have failed if pod replacement policy is set to Failed", func(ctx context.Context) {
		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("", "pod-recreate-failed", v1.RestartPolicyNever, 1, 1, nil, 1)
		job.Spec.PodReplacementPolicy = ptr.To(batchv1.Failed)
		job.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh", "-c", `_term(){
	sleep 5
	exit 143
}
trap _term SIGTERM
while true; do
	sleep 1
done`}
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		err = e2ejob.WaitForJobPodsRunning(ctx, f.ClientSet, f.Namespace.Name, job.Name, 1)
		framework.ExpectNoError(err, "failed to wait for job pod to become running in namespace: %s", f.Namespace.Name)

		ginkgo.By("Deleting job pod")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get pod list for job %s in namespace: %s", job.Name, f.Namespace.Name)

		framework.ExpectNoError(e2epod.DeletePodsWithGracePeriod(ctx, f.ClientSet, pods.Items, 30), "failed to delete pods in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring pod does not get recreated while it is in terminating state")
		err = e2ejob.WaitForJobState(ctx, f.ClientSet, f.Namespace.Name, job.Name, f.Timeouts.PodDelete, func(job *batchv1.Job) string {
			if job.Status.Active == 0 && job.Status.Failed == 0 && *job.Status.Terminating == 1 {
				return ""
			} else {
				return fmt.Sprintf(
					"expected job to have 0 active pod, 0 failed pod and 1 terminating pods, but got %d active pods, %d failed pods and %d terminating pods",
					job.Status.Active,
					job.Status.Failed,
					*job.Status.Terminating,
				)
			}
		})
		framework.ExpectNoError(err, "failed to ensure pod is not recreated while it is in terminating state")

		ginkgo.By("Ensuring pod gets recreated after it has failed")
		err = e2ejob.WaitForJobState(ctx, f.ClientSet, f.Namespace.Name, job.Name, f.Timeouts.PodDelete, func(job *batchv1.Job) string {
			if job.Status.Active == 1 && job.Status.Failed == 1 && *job.Status.Terminating == 0 {
				return ""
			} else {
				return fmt.Sprintf(
					"expected job to have 1 active pods, 1 failed pods and 0 terminating pod, but got %d active pods, %d failed pods and %d terminating pods",
					job.Status.Active,
					job.Status.Failed,
					*job.Status.Terminating,
				)
			}
		})
		framework.ExpectNoError(err, "failed to wait for pod to get recreated")
	})

	/*
		  Release: v1.24
			Testname: Ensure Pods of an Indexed Job get a unique index.
			Description: Create an Indexed job. Job MUST complete successfully.
			Ensure that created pods have completion index annotation and environment variable.
	*/
	framework.ConformanceIt("should create pods for an Indexed job with completion indexes and specified hostname", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating Indexed job")
		job := e2ejob.NewTestJob("succeed", "indexed-job", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		mode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &mode
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create indexed job in namespace %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, nil, completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring pods with index for job exist")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get pod list for job in namespace: %s", f.Namespace.Name)
		succeededIndexes := sets.NewInt()
		for _, pod := range pods.Items {
			if pod.Status.Phase == v1.PodSucceeded && pod.Annotations != nil {
				ix, err := strconv.Atoi(pod.Annotations[batchv1.JobCompletionIndexAnnotation])
				framework.ExpectNoError(err, "failed obtaining completion index from pod in namespace: %s", f.Namespace.Name)
				succeededIndexes.Insert(ix)
				expectedName := fmt.Sprintf("%s-%d", job.Name, ix)
				gomega.Expect(pod.Spec.Hostname).To(gomega.Equal(expectedName), "expected completed pod with hostname %s, but got %s", expectedName, pod.Spec.Hostname)
			}
		}
		gotIndexes := succeededIndexes.List()
		wantIndexes := []int{0, 1, 2, 3}
		gomega.Expect(gotIndexes).To(gomega.Equal(wantIndexes), "expected completed indexes %s, but got %s", wantIndexes, gotIndexes)
	})

	/*
		  Release: v1.32
			Testname: Ensure Pods of an Indexed Job get a unique index for PodIndexLabel key.
			Description: Create an Indexed job. Job MUST complete successfully.
			Ensure that created pods have completion index label.
	*/
	// TODO: once this test is stable, squash the functionality into pre-existing conformance test called "should create
	//   pods for an Indexed job with completion indexes and specified hostname" earlier in this file.
	framework.It("should create pods with completion indexes for an Indexed Job", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating Indexed job")
		job := e2ejob.NewTestJob("succeed", "indexed-job", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create indexed job in namespace %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, nil, completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring all pods have the required index labels")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get pod list for job in namespace: %s", f.Namespace.Name)
		succeededIndexes := sets.NewInt()
		for _, pod := range pods.Items {
			ix, err := strconv.Atoi(pod.Labels[batchv1.JobCompletionIndexAnnotation])
			framework.ExpectNoError(err, "failed obtaining completion index in namespace: %s for pod: %s", pod.Namespace, pod.Name)
			succeededIndexes.Insert(ix)
		}
		gotIndexes := succeededIndexes.List()
		wantIndexes := []int{0, 1, 2, 3}
		gomega.Expect(gotIndexes).To(gomega.Equal(wantIndexes), "expected completed indexes in namespace: %s for job: %s", job.Namespace, job.Name)
	})

	/*
		Testcase: Ensure that job with successPolicy succeeded when all indexes succeeded
		Description: Create an indexed job with successPolicy.
		Verify that job got SuccessCriteriaMet with SuccessPolicy reason and Complete condition
		once all indexes succeeded.
	*/
	ginkgo.It("with successPolicy should succeeded when all indexes succeeded", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(2)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating an indexed job with successPolicy")
		job := e2ejob.NewTestJob("succeeded", "with-success-policy-all-index-succeeded", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
		job.Spec.SuccessPolicy = &batchv1.SuccessPolicy{
			Rules: []batchv1.SuccessPolicyRule{{
				SucceededCount: ptr.To[int32](2),
			}},
		}
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to have the interim SuccessCriteriaMet with SuccessPolicy reason condition")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobSuccessCriteriaMet, ptr.To(batchv1.JobReasonSuccessPolicy))
		framework.ExpectNoError(err, "failed to ensure that job has SuccessCriteriaMet with SuccessPolicy reason condition")

		ginkgo.By("Ensure that the job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonSuccessPolicy), completions)
		framework.ExpectNoError(err, "failed to ensure that job completed")

		ginkgo.By("Verifying that the job status to ensure correct final state")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get latest job object")
		gomega.Expect(job.Status.Active).Should(gomega.Equal(int32(0)))
		gomega.Expect(job.Status.Ready).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Terminating).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Failed).Should(gomega.Equal(int32(0)))
	})

	/*
		Testcase: Ensure that job with successPolicy succeededIndexes rule succeeded even when some indexes remain pending
		Description: Create an indexed job with successPolicy succeededIndexes rule.
		Verify that the job got SuccessCriteriaMet with SuccessPolicy reason condition and Complete condition
		when the job met successPolicy even if some indexed remain pending.
	*/
	ginkgo.It("with successPolicy succeededIndexes rule should succeeded even when some indexes remain pending", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(5)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating an indexed job with successPolicy succeededIndexes rule")
		job := e2ejob.NewTestJob("failOddSucceedEven", "with-success-policy-succeeded-indexed", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
		job.Spec.SuccessPolicy = &batchv1.SuccessPolicy{
			Rules: []batchv1.SuccessPolicyRule{{
				SucceededIndexes: ptr.To("0"),
			}},
		}
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to have the interim SuccessCriteriaMet with SuccessPolicy reason condition")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobSuccessCriteriaMet, ptr.To(batchv1.JobReasonSuccessPolicy))
		framework.ExpectNoError(err, "failed to ensure that job has SuccessCriteriaMet with SuccessPolicy reason condition")

		ginkgo.By("Ensure that the job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonSuccessPolicy), 1)
		framework.ExpectNoError(err, "failed to ensure that job completed")

		ginkgo.By("Verifying that the only appropriately index succeeded")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get the latest job object")
		gomega.Expect(job.Status.CompletedIndexes).Should(gomega.Equal("0"))
		gomega.Expect(job.Status.Active).Should(gomega.Equal(int32(0)))
		gomega.Expect(job.Status.Ready).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Terminating).Should(gomega.Equal(ptr.To[int32](0)))
	})

	/*
		Testcase: Ensure that job with successPolicy succeededCount rule succeeded even when some indexes remain pending
		Description: Create an indexed job with successPolicy succeededCount rule.
		Verify that the job got the SuccessCriteriaMet with SuccessPolicy reason condition and Complete condition
		when the job met successPolicy even if some indexed remain pending.
	*/
	ginkgo.It("with successPolicy succeededCount rule should succeeded even when some indexes remain pending", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(5)
		backoffLimit := int32(math.MaxInt32)

		ginkgo.By("Creating an indexed job with successPolicy succeededCount rule")
		job := e2ejob.NewTestJob("failOddSucceedEven", "with-success-policy-succeeded-count", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
		job.Spec.SuccessPolicy = &batchv1.SuccessPolicy{
			Rules: []batchv1.SuccessPolicyRule{{
				SucceededCount: ptr.To[int32](1),
			}},
		}
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to have the interim SuccessCriteriaMet condition with SuccessPolicy reason")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobSuccessCriteriaMet, ptr.To(batchv1.JobReasonSuccessPolicy))
		framework.ExpectNoError(err, "failed to ensure that the job has SuccessCriteriaMet condition with SuccessPolicy rule")

		ginkgo.By("Ensure that the job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonSuccessPolicy), 1)
		framework.ExpectNoError(err, "failed to ensure that job completed")

		ginkgo.By("Verifying that the job status to ensure correct final state")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get the latest job object")
		gomega.Expect(job.Status.CompletedIndexes).Should(gomega.Equal("0"))
		gomega.Expect(job.Status.Active).Should(gomega.Equal(int32(0)))
		gomega.Expect(job.Status.Ready).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Terminating).Should(gomega.Equal(ptr.To[int32](0)))
	})

	/*
		Testcase: Ensure that all indexes are executed for an indexed job with backoffLimitPerIndex despite some failing
		Description: Create an indexed job and ensure that all indexes are either failed or succeeded, depending
		on the end state of the corresponding pods. Pods with odd indexes fail, while the pods with even indexes
		succeeded. Also, verify that the number of failed pods doubles the number of failing indexes, as the
		backoffLimitPerIndex=1, allowing for one pod recreation before marking that indexed failed.
	*/
	ginkgo.It("should execute all indexes despite some failing when using backoffLimitPerIndex", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating an indexed job with backoffLimit per index and failing pods")
		job := e2ejob.NewTestJob("failOddSucceedEven", "with-backoff-limit-per-index", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.BackoffLimit = nil
		job.Spec.BackoffLimitPerIndex = ptr.To[int32](1)
		mode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &mode
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to fail as there are failed indexes")
		err = e2ejob.WaitForJobFailed(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Verifying the Job status fields to ensure all indexes were executed")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		gomega.Expect(job.Status.FailedIndexes).Should(gomega.HaveValue(gomega.Equal("1,3")))
		gomega.Expect(job.Status.CompletedIndexes).Should(gomega.Equal("0,2"))
		gomega.Expect(job.Status.Failed).Should(gomega.Equal(int32(4)))
		gomega.Expect(job.Status.Succeeded).Should(gomega.Equal(int32(2)))
	})

	/*
		Testcase: Terminate job execution when the maxFailedIndexes is exceeded
		Description: Create an indexed job with backoffLimitPerIndex and maxFailedIndexes.
		Verify the job execution is terminated as soon as the number of failed
		indexes exceeds maxFailedIndexes.
	*/
	ginkgo.It("should terminate job execution when the number of failed indexes exceeds maxFailedIndexes", func(ctx context.Context) {
		// we use parallelism=1 to make sure in the asserts only one pod was created
		parallelism := int32(1)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating an indexed job with backoffLimit per index and maxFailedIndexes")
		job := e2ejob.NewTestJob("fail", "with-max-failed-indexes", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.BackoffLimit = nil
		job.Spec.BackoffLimitPerIndex = ptr.To[int32](0)
		job.Spec.MaxFailedIndexes = ptr.To[int32](0)

		mode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &mode
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to fail as the number of max failed indexes is exceeded")
		err = e2ejob.WaitForJobFailed(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Verifying the Job status fields to ensure early termination of the job")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		gomega.Expect(job.Status.FailedIndexes).Should(gomega.HaveValue(gomega.Equal("0")))
		gomega.Expect(job.Status.Failed).Should(gomega.Equal(int32(1)))
	})

	/*
		Testcase: Mark indexes as failed when the FailIndex action is matched in podFailurePolicy
		Description: Create an indexed job with backoffLimitPerIndex, and podFailurePolicy
		with the FailIndex action. Verify the failed pods matching the pod failure policy
		result in marking the corresponding indexes as failed without restarts, despite
		backoffLimitPerIndex > 0.
	*/
	ginkgo.It("should mark indexes as failed when the FailIndex action is matched in podFailurePolicy", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(2)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating an indexed job with failing pods matching the FailIndex action")
		job := e2ejob.NewTestJob("failOddSucceedEven", "matching-fail-index-action", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.BackoffLimit = nil
		job.Spec.BackoffLimitPerIndex = ptr.To[int32](1)
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{
				{
					Action: batchv1.PodFailurePolicyActionFailIndex,
					OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
						Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
						Values:   []int32{1},
					},
				},
			},
		}
		mode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &mode
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to fail as all indexes are failed")
		err = e2ejob.WaitForJobFailed(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Verifying the Job status fields to ensure the upper indexes didn't execute")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		gomega.Expect(job.Status.FailedIndexes).Should(gomega.HaveValue(gomega.Equal("1")))
		gomega.Expect(job.Status.CompletedIndexes).Should(gomega.Equal("0"))
		gomega.Expect(job.Status.Failed).Should(gomega.Equal(int32(1)))
		gomega.Expect(job.Status.Succeeded).Should(gomega.Equal(int32(1)))
	})

	/*
		Testcase: Ensure that the pods associated with the job are removed once the job is deleted
		Description: Create a job and ensure the associated pod count is equal to parallelism count. Delete the
		job and ensure if the pods associated with the job have been removed
	*/
	ginkgo.It("should remove pods when job is deleted", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("notTerminate", "all-pods-removed", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure pods equal to parallelism count is attached to the job")
		err = e2ejob.WaitForJobPodsRunning(ctx, f.ClientSet, f.Namespace.Name, job.Name, parallelism)
		framework.ExpectNoError(err, "failed to ensure number of pods associated with job %s is equal to parallelism count in namespace: %s", job.Name, f.Namespace.Name)

		ginkgo.By("Delete the job")
		err = e2eresource.DeleteResourceAndWaitForGC(ctx, f.ClientSet, batchinternal.Kind("Job"), f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to delete the job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure the pods associated with the job are also deleted")
		err = e2ejob.WaitForAllJobPodsGone(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get PodList for job %s in namespace: %s", job.Name, f.Namespace.Name)
	})

	/*
		Release: v1.16
		Testname: Jobs, completion after task failure
		Description: Explicitly cause the tasks to fail once initially. After restarting, the Job MUST
		execute to completion.
	*/
	framework.ConformanceIt("should run a job to completion when tasks sometimes fail and are locally restarted", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		// One failure, then a success, local restarts.
		// We can't use the random failure approach, because kubelet will
		// throttle frequently failing containers in a given pod, ramping
		// up to 5 minutes between restarts, making test timeout due to
		// successive failures too likely with a reasonable test timeout.
		job := e2ejob.NewTestJob("failOnce", "fail-once-local", v1.RestartPolicyOnFailure, parallelism, completions, nil, backoffLimit)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, nil, completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)
	})

	// Pods sometimes fail, but eventually succeed, after pod restarts
	ginkgo.It("should run a job to completion when tasks sometimes fail and are not locally restarted", func(ctx context.Context) {
		// One failure, then a success, no local restarts.
		// We can't use the random failure approach, because JobController
		// will throttle frequently failing Pods of a given Job, ramping
		// up to 6 minutes between restarts, making test timeout due to
		// successive failures.
		// Instead, we force the Job's Pods to be scheduled to a single Node
		// and use a hostPath volume to persist data across new Pods.
		ginkgo.By("Looking for a node to schedule job pod")
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)

		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJobOnNode("failOnce", "fail-once-non-local", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit, node.Name)
		job, err = e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to have the interim success condition")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobSuccessCriteriaMet, ptr.To(batchv1.JobReasonCompletionsReached))
		framework.ExpectNoError(err, "failed to ensure job has the interim success condition: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonCompletionsReached), *job.Spec.Completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Verifying the Job status fields to ensure correct final state")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		gomega.Expect(job.Status.Active).Should(gomega.Equal(int32(0)))
		gomega.Expect(job.Status.Ready).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Terminating).Should(gomega.Equal(ptr.To[int32](0)))
	})

	ginkgo.It("should fail when exceeds active deadline", func(ctx context.Context) {
		activeDeadlineSeconds := int64(1)
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("notTerminate", "exceed-active-deadline", v1.RestartPolicyNever, parallelism, completions, &activeDeadlineSeconds, backoffLimit)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to have the interim failure condition")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobFailureTarget, ptr.To(batchv1.JobReasonDeadlineExceeded))
		framework.ExpectNoError(err, "failed to ensure job has the interim failure condition: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job past active deadline")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobFailed, ptr.To(batchv1.JobReasonDeadlineExceeded))
		framework.ExpectNoError(err, "failed to ensure job past active deadline in namespace: %s", f.Namespace.Name)

		ginkgo.By("Verifying the Job status fields to ensure correct final state")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		gomega.Expect(job.Status.Active).Should(gomega.Equal(int32(0)))
		gomega.Expect(job.Status.Ready).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Terminating).Should(gomega.Equal(ptr.To[int32](0)))
	})

	/*
		Release: v1.15
		Testname: Jobs, active pods, graceful termination
		Description: Create a job. Ensure the active pods reflect parallelism in the namespace and delete the job. Job MUST be deleted successfully.
	*/
	framework.ConformanceIt("should delete a job", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("notTerminate", "foo", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring active pods == parallelism")
		err = e2ejob.WaitForJobPodsRunning(ctx, f.ClientSet, f.Namespace.Name, job.Name, parallelism)
		framework.ExpectNoError(err, "failed to ensure active pods == parallelism in namespace: %s", f.Namespace.Name)

		ginkgo.By("delete a job")
		framework.ExpectNoError(e2eresource.DeleteResourceAndWaitForGC(ctx, f.ClientSet, batchinternal.Kind("Job"), f.Namespace.Name, job.Name))

		ginkgo.By("Ensuring job was deleted")
		_, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, fmt.Sprintf("failed to ensure job %s was deleted in namespace: %s", job.Name, f.Namespace.Name)))
	})

	/*
		Release: v1.16
		Testname: Jobs, orphan pods, re-adoption
		Description: Create a parallel job. The number of Pods MUST equal the level of parallelism.
		Orphan a Pod by modifying its owner reference. The Job MUST re-adopt the orphan pod.
		Modify the labels of one of the Job's Pods. The Job MUST release the Pod.
	*/
	framework.ConformanceIt("should adopt matching orphans and release non-matching pods", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("notTerminate", "adopt-release", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		// Replace job with the one returned from Create() so it has the UID.
		// Save Kind since it won't be populated in the returned job.
		kind := job.Kind
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)
		job.Kind = kind

		ginkgo.By("Ensuring active pods == parallelism")
		err = e2ejob.WaitForJobPodsRunning(ctx, f.ClientSet, f.Namespace.Name, job.Name, parallelism)
		framework.ExpectNoError(err, "failed to ensure active pods == parallelism in namespace: %s", f.Namespace.Name)

		ginkgo.By("Orphaning one of the Job's Pods")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get PodList for job %s in namespace: %s", job.Name, f.Namespace.Name)
		gomega.Expect(pods.Items).To(gomega.HaveLen(int(parallelism)))
		pod := pods.Items[0]
		e2epod.NewPodClient(f).Update(ctx, pod.Name, func(pod *v1.Pod) {
			pod.OwnerReferences = nil
		})

		ginkgo.By("Checking that the Job readopts the Pod")
		gomega.Expect(e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "adopted", e2ejob.JobTimeout,
			func(pod *v1.Pod) (bool, error) {
				controllerRef := metav1.GetControllerOf(pod)
				if controllerRef == nil {
					return false, nil
				}
				if controllerRef.Kind != job.Kind || controllerRef.Name != job.Name || controllerRef.UID != job.UID {
					return false, fmt.Errorf("pod has wrong controllerRef: got %v, want %v", controllerRef, job)
				}
				return true, nil
			},
		)).To(gomega.Succeed(), "wait for pod %q to be readopted", pod.Name)

		ginkgo.By("Removing the labels from the Job's Pod")
		e2epod.NewPodClient(f).Update(ctx, pod.Name, func(pod *v1.Pod) {
			pod.Labels = nil
		})

		ginkgo.By("Checking that the Job releases the Pod")
		gomega.Expect(e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "released", e2ejob.JobTimeout,
			func(pod *v1.Pod) (bool, error) {
				controllerRef := metav1.GetControllerOf(pod)
				if controllerRef != nil {
					return false, nil
				}
				return true, nil
			},
		)).To(gomega.Succeed(), "wait for pod %q to be released", pod.Name)
	})

	ginkgo.It("should fail to exceed backoffLimit", func(ctx context.Context) {
		ginkgo.By("Creating a job")
		backoff := 1
		job := e2ejob.NewTestJob("fail", "backofflimit", v1.RestartPolicyNever, 1, 1, nil, int32(backoff))
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Awaiting for the job to have the interim failure condition")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobFailureTarget, ptr.To(batchv1.JobReasonBackoffLimitExceeded))
		framework.ExpectNoError(err, "failed to ensure job has the interim failure condition: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job exceed backofflimit")
		err = e2ejob.WaitForJobCondition(ctx, f.ClientSet, f.Namespace.Name, job.Name, batchv1.JobFailed, ptr.To(batchv1.JobReasonBackoffLimitExceeded))
		framework.ExpectNoError(err, "failed to ensure job exceed backofflimit in namespace: %s", f.Namespace.Name)

		ginkgo.By(fmt.Sprintf("Checking that %d pod created and status is failed", backoff+1))
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get PodList for job %s in namespace: %s", job.Name, f.Namespace.Name)
		gomega.Expect(pods.Items).To(gomega.HaveLen(backoff + 1))
		for _, pod := range pods.Items {
			gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodFailed))
		}

		ginkgo.By("Verifying the Job status fields to ensure correct final state")
		job, err = e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to retrieve latest job object")
		gomega.Expect(job.Status.Active).Should(gomega.Equal(int32(0)))
		gomega.Expect(job.Status.Ready).Should(gomega.Equal(ptr.To[int32](0)))
		gomega.Expect(job.Status.Terminating).Should(gomega.Equal(ptr.To[int32](0)))
	})

	f.It("should run a job to completion with CPU requests", f.WithSerial(), func(ctx context.Context) {
		ginkgo.By("Creating a job that with CPU requests")

		testNodeName := scheduling.GetNodeThatCanRunPod(ctx, f)
		targetNode, err := f.ClientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
		framework.ExpectNoError(err, "unable to get node object for node %v", testNodeName)

		cpu, ok := targetNode.Status.Allocatable[v1.ResourceCPU]
		if !ok {
			framework.Failf("Unable to get node's %q cpu", targetNode.Name)
		}

		cpuRequest := fmt.Sprint(int64(0.2 * float64(cpu.Value())))

		parallelism := int32(90)
		completions := int32(90)
		backoffLimit := int32(0)

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("succeed", "all-succeed", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		for i := range job.Spec.Template.Spec.Containers {
			job.Spec.Template.Spec.Containers[i].Resources = v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse(cpuRequest),
				},
			}
			job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": testNodeName}
		}

		framework.Logf("Creating job %q with a node hostname selector %q with cpu request %q", job.Name, testNodeName, cpuRequest)
		job, err = e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring job reaches completions")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To(batchv1.JobReasonCompletionsReached), completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensuring pods for job exist")
		pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "failed to get pod list for job in namespace: %s", f.Namespace.Name)
		successes := int32(0)
		for _, pod := range pods.Items {
			if pod.Status.Phase == v1.PodSucceeded {
				successes++
			}
		}
		gomega.Expect(successes).To(gomega.Equal(completions), "expected %d successful job pods, but got  %d", completions, successes)
	})

	/*
		Release: v1.24
		Testname: Jobs, apply changes to status
		Description: Attempt to create a running Job which MUST succeed.
		Attempt to patch the Job status which MUST succeed.
		An annotation for the job that was patched MUST be found.
		Attempt to replace the job status with update which MUST succeed.
		Attempt to read its status sub-resource which MUST succeed
	*/
	framework.ConformanceIt("should apply changes to a job status", func(ctx context.Context) {

		ns := f.Namespace.Name
		jClient := f.ClientSet.BatchV1().Jobs(ns)

		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job")
		job := e2ejob.NewTestJob("notTerminate", "suspend-false-to-true", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure pods equal to parallelism count is attached to the job")
		err = e2ejob.WaitForJobPodsRunning(ctx, f.ClientSet, f.Namespace.Name, job.Name, parallelism)
		framework.ExpectNoError(err, "failed to ensure number of pods associated with job %s is equal to parallelism count in namespace: %s", job.Name, f.Namespace.Name)

		customConditionType := batchv1.JobConditionType("CustomConditionType")
		// /status subresource operations
		ginkgo.By("patching /status")
		// we need to use RFC3339 version since conversion over the wire cuts nanoseconds
		now1 := metav1.Now().Rfc3339Copy()
		jStatus := batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:               customConditionType,
					Status:             v1.ConditionTrue,
					LastTransitionTime: now1,
				},
			},
		}

		jStatusJSON, err := json.Marshal(jStatus)
		framework.ExpectNoError(err)
		patchedStatus, err := jClient.Patch(ctx, job.Name, types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"patchedstatus":"true"}},"status":`+string(jStatusJSON)+`}`),
			metav1.PatchOptions{}, "status")
		framework.ExpectNoError(err)
		if condition := findConditionByType(patchedStatus.Status.Conditions, customConditionType); condition != nil {
			if !condition.LastTransitionTime.Equal(&now1) {
				framework.Failf("patched object should have the applied condition with LastTransitionTime %#v, got %#v instead", now1, condition.LastTransitionTime)
			}
		} else {
			framework.Failf("patched object does not have the required condition %v", customConditionType)
		}
		gomega.Expect(patchedStatus.Annotations).To(gomega.HaveKeyWithValue("patchedstatus", "true"), "patched object should have the applied annotation")

		ginkgo.By("updating /status")
		// we need to use RFC3339 version since conversion over the wire cuts nanoseconds
		now2 := metav1.Now().Rfc3339Copy()
		var statusToUpdate, updatedStatus *batchv1.Job
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			statusToUpdate, err = jClient.Get(ctx, job.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if condition := findConditionByType(statusToUpdate.Status.Conditions, customConditionType); condition != nil {
				condition.LastTransitionTime = now2
			} else {
				framework.Failf("patched object does not have the required condition %v", customConditionType)
			}
			updatedStatus, err = jClient.UpdateStatus(ctx, statusToUpdate, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err)
		if condition := findConditionByType(updatedStatus.Status.Conditions, customConditionType); condition != nil {
			if !condition.LastTransitionTime.Equal(&now2) {
				framework.Failf("patched object should have the applied condition with LastTransitionTime %#v, got %#v instead", now2, condition.LastTransitionTime)
			}
		} else {
			framework.Failf("patched object does not have the required condition %v", customConditionType)
		}

		ginkgo.By("get /status")
		jResource := schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
		gottenStatus, err := f.DynamicClient.Resource(jResource).Namespace(ns).Get(ctx, job.Name, metav1.GetOptions{}, "status")
		framework.ExpectNoError(err)
		statusUID, _, err := unstructured.NestedFieldCopy(gottenStatus.Object, "metadata", "uid")
		framework.ExpectNoError(err)
		gomega.Expect(string(job.UID)).To(gomega.Equal(statusUID), fmt.Sprintf("job.UID: %v expected to match statusUID: %v ", job.UID, statusUID))
	})

	/*
		Release: v1.25
		Testname: Jobs, manage lifecycle
		Description: Attempt to create a suspended Job which MUST succeed.
		Attempt to patch the Job to include a new label which MUST succeed.
		The label MUST be found. Attempt to replace the Job to include a
		new annotation which MUST succeed. The annotation MUST be found.
		Attempt to list all namespaces with a label selector which MUST
		succeed. One list MUST be found. It MUST succeed at deleting a
		collection of jobs via a label selector.
	*/
	framework.ConformanceIt("should manage the lifecycle of a job", func(ctx context.Context) {
		jobName := "e2e-" + utilrand.String(5)
		label := map[string]string{"e2e-job-label": jobName}
		labelSelector := labels.SelectorFromSet(label).String()

		ns := f.Namespace.Name
		jobClient := f.ClientSet.BatchV1().Jobs(ns)

		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = labelSelector
				return jobClient.Watch(ctx, options)
			},
		}
		jobsList, err := jobClient.List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "failed to list Job")

		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a suspended job")
		job := e2ejob.NewTestJob("succeed", jobName, v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Labels = label
		job.Spec.Suspend = pointer.BoolPtr(true)
		job, err = e2ejob.CreateJob(ctx, f.ClientSet, ns, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", ns)

		ginkgo.By("Patching the Job")
		payload := "{\"metadata\":{\"labels\":{\"" + jobName + "\":\"patched\"}}}"
		patchedJob, err := f.ClientSet.BatchV1().Jobs(ns).Patch(ctx, jobName, types.StrategicMergePatchType, []byte(payload), metav1.PatchOptions{})
		framework.ExpectNoError(err, "failed to patch Job %s in namespace %s", jobName, ns)

		ginkgo.By("Watching for Job to be patched")
		c := watchEventConfig{
			framework:           f,
			resourceVersion:     jobsList.ResourceVersion,
			w:                   w,
			jobName:             jobName,
			watchEvent:          watch.Modified,
			extJob:              patchedJob,
			updatedMetadataType: "label",
			updatedKey:          jobName,
			updatedValue:        "patched",
		}
		waitForJobEvent(ctx, c)
		gomega.Expect(patchedJob.Labels).To(gomega.HaveKeyWithValue(jobName, "patched"), "Did not find job label for this job. Current labels: %v", patchedJob.Labels)

		ginkgo.By("Updating the job")
		var updatedJob *batchv1.Job

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			patchedJob, err = jobClient.Get(ctx, jobName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Unable to get job %s", jobName)
			patchedJob.Spec.Suspend = pointer.BoolPtr(false)
			if patchedJob.Annotations == nil {
				patchedJob.Annotations = map[string]string{}
			}
			patchedJob.Annotations["updated"] = "true"
			updatedJob, err = e2ejob.UpdateJob(ctx, f.ClientSet, ns, patchedJob)
			return err
		})
		framework.ExpectNoError(err, "failed to update job in namespace: %s", ns)

		ginkgo.By("Watching for Job to be updated")
		c = watchEventConfig{
			framework:           f,
			resourceVersion:     patchedJob.ResourceVersion,
			w:                   w,
			jobName:             jobName,
			watchEvent:          watch.Modified,
			extJob:              updatedJob,
			updatedMetadataType: "annotation",
			updatedKey:          "updated",
			updatedValue:        "true",
		}
		waitForJobEvent(ctx, c)
		gomega.Expect(updatedJob.Annotations).To(gomega.HaveKeyWithValue("updated", "true"), "updated Job should have the applied annotation")
		framework.Logf("Found Job annotations: %#v", patchedJob.Annotations)

		ginkgo.By("Listing all Jobs with LabelSelector")
		jobs, err := f.ClientSet.BatchV1().Jobs("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "Failed to list job. %v", err)
		gomega.Expect(jobs.Items).To(gomega.HaveLen(1), "Failed to find job %v", jobName)
		testJob := jobs.Items[0]
		framework.Logf("Job: %v as labels: %v", testJob.Name, testJob.Labels)

		ginkgo.By("Waiting for job to complete")
		err = e2ejob.WaitForJobComplete(ctx, f.ClientSet, ns, jobName, nil, completions)
		framework.ExpectNoError(err, "failed to ensure job completion in namespace: %s", ns)

		ginkgo.By("Delete a job collection with a labelselector")
		propagationPolicy := metav1.DeletePropagationBackground
		err = f.ClientSet.BatchV1().Jobs(ns).DeleteCollection(ctx, metav1.DeleteOptions{PropagationPolicy: &propagationPolicy}, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "failed to delete job %s in namespace: %s", job.Name, ns)

		ginkgo.By("Watching for Job to be deleted")
		c = watchEventConfig{
			framework:           f,
			resourceVersion:     updatedJob.ResourceVersion,
			w:                   w,
			jobName:             jobName,
			watchEvent:          watch.Deleted,
			extJob:              &testJob,
			updatedMetadataType: "label",
			updatedKey:          "e2e-job-label",
			updatedValue:        jobName,
		}
		waitForJobEvent(ctx, c)

		ginkgo.By("Relist jobs to confirm deletion")
		jobs, err = f.ClientSet.BatchV1().Jobs("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "Failed to list job. %v", err)
		gomega.Expect(jobs.Items).To(gomega.BeEmpty(), "Found job %v", jobName)
	})

	ginkgo.It("should update the status ready field", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6) // default value

		ginkgo.By("Creating a job with suspend=true")
		job := e2ejob.NewTestJob("notTerminate", "all-ready", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.Suspend = ptr.To[bool](true)
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure the job controller updates the status.ready field")
		err = e2ejob.WaitForJobReady(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To[int32](0))
		framework.ExpectNoError(err, "failed to ensure job status ready field in namespace: %s", f.Namespace.Name)

		ginkgo.By("Updating the job with suspend=false")
		err = updateJobSuspendWithRetries(ctx, f, job, ptr.To[bool](false))
		framework.ExpectNoError(err, "failed to update job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure the job controller updates the status.ready field")
		err = e2ejob.WaitForJobReady(ctx, f.ClientSet, f.Namespace.Name, job.Name, &parallelism)
		framework.ExpectNoError(err, "failed to ensure job status ready field in namespace: %s", f.Namespace.Name)

		ginkgo.By("Updating the job with suspend=true")
		err = updateJobSuspendWithRetries(ctx, f, job, ptr.To[bool](true))
		framework.ExpectNoError(err, "failed to update job in namespace: %s", f.Namespace.Name)

		ginkgo.By("Ensure the job controller updates the status.ready field")
		err = e2ejob.WaitForJobReady(ctx, f.ClientSet, f.Namespace.Name, job.Name, ptr.To[int32](0))
		framework.ExpectNoError(err, "failed to ensure job status ready field in namespace: %s", f.Namespace.Name)
	})

	/*
		Testname: Jobs, managed-by mechanism
		Description: This test verifies the built-in Job controller does not
		reconcile the Job, allowing to delegate the reconciliation to an
		external controller.
	*/
	ginkgo.It("should allow to delegate reconciliation to external controller", func(ctx context.Context) {
		parallelism := int32(2)
		completions := int32(4)
		backoffLimit := int32(6)

		ginkgo.By("Creating a job with a custom managed-by field")
		job := e2ejob.NewTestJob("succeed", "managed-by", v1.RestartPolicyNever, parallelism, completions, nil, backoffLimit)
		job.Spec.ManagedBy = ptr.To("example.com/custom-job-controller")
		job, err := e2ejob.CreateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		framework.ExpectNoError(err, "failed to create job in namespace: %s/%s", job.Namespace, job.Name)

		ginkgo.By(fmt.Sprintf("Verify the Job %s/%s status isn't updated by the built-in controller", job.Namespace, job.Name))
		// This get function uses HandleRetry to retry on transient API errors
		get := framework.GetObject(f.ClientSet.BatchV1().Jobs(f.Namespace.Name).Get, job.Name, metav1.GetOptions{})
		gomega.Consistently(ctx, get).
			WithPolling(time.Second).WithTimeout(3 * time.Second).
			Should(gomega.HaveField("Status", gomega.BeEquivalentTo(batchv1.JobStatus{})))
	})
})

func updateJobSuspendWithRetries(ctx context.Context, f *framework.Framework, job *batchv1.Job, suspend *bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		job, err := e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, job.Name)
		framework.ExpectNoError(err, "unable to get job %s in namespace %s", job.Name, f.Namespace.Name)
		job.Spec.Suspend = suspend
		_, err = e2ejob.UpdateJob(ctx, f.ClientSet, f.Namespace.Name, job)
		return err
	})
}

// waitForJobEvent is used to track and log Job events.
// As delivery of events is not actually guaranteed we
// will not return an error if we miss the required event.
func waitForJobEvent(ctx context.Context, config watchEventConfig) {
	f := config.framework
	ctx, cancel := context.WithTimeout(ctx, f.Timeouts.PodStartShort)
	defer cancel()
	_, err := watchtools.Until(ctx, config.resourceVersion, config.w, func(event watch.Event) (bool, error) {
		if job, ok := event.Object.(*batchv1.Job); ok {

			var key string
			switch config.updatedMetadataType {
			case "annotation":
				key = job.Annotations[config.updatedKey]
			case "label":
				key = job.Labels[config.updatedKey]
			}

			found := job.ObjectMeta.Name == config.extJob.ObjectMeta.Name &&
				job.ObjectMeta.Namespace == f.Namespace.Name &&
				key == config.updatedValue &&
				event.Type == config.watchEvent
			if !found {
				framework.Logf("Event %v observed for Job %v in namespace %v with labels: %v and annotations: %v", event.Type, job.ObjectMeta.Name, job.ObjectMeta.Namespace, job.Labels, job.Annotations)
				return false, nil
			}
			framework.Logf("Event %v found for Job %v in namespace %v with labels: %v and annotations: %v", event.Type, job.ObjectMeta.Name, job.ObjectMeta.Namespace, job.Labels, job.Annotations)
			return found, nil
		}
		framework.Logf("Observed event: %+v", event.Object)
		return false, nil
	})
	if err != nil {
		j, _ := f.ClientSet.BatchV1().Jobs(f.Namespace.Name).Get(ctx, config.jobName, metav1.GetOptions{})
		framework.Logf("We missed the %v event. Job details: %+v", config.watchEvent, j)
	}
}

func findConditionByType(list []batchv1.JobCondition, cType batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range list {
		if list[i].Type == cType {
			return &list[i]
		}
	}
	return nil
}
