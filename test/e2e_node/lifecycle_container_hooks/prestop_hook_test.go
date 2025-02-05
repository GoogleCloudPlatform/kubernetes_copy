/*
Copyright 2024 The Kubernetes Authors.

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

package lifecycle_container_hooks

import (
	"context"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

const (
	PreStopPrefix = "PreStop"
)

// PreStop Hook test cases
var _ = framework.SIGDescribe("Lifecycle Event: PreStop Hook", func() {
	f := framework.NewDefaultFramework("prestop-hook")
	
	// Test Case: Pod Deletion with Grace Period and PreStop Hook Execution
	ginkgo.It("should execute the PreStop hook during the pod deletion with a defined grace period", func() {
		client := e2epod.NewPodClient(f)
		gracePeriod := int64(30)  // Define a 30-second grace period
		bufferSeconds := int64(10)  // Add a buffer for potential delays

		ginkgo.By("creating a pod with a PreStop hook and a defined grace period")
		pod := createPodWithPreStopHook(f, time.Duration(gracePeriod)*time.Second)
		pod.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
			PreStop: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: []string{"sleep", "10000"},  // Simulate a long-running process
				},
			},
		}
		pod = client.Create(context.TODO(), pod)

		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)

		ginkgo.By("deleting the pod and triggering the PreStop hook")
		err = client.Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("verifying the PreStop hook execution")
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "busybox")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook triggered"), true, "Expected PreStop hook to run")

		ginkgo.By("ensuring the pod terminates within the grace period plus buffer")
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), f.ClientSet, pod.Name, pod.Namespace, time.Duration(gracePeriod+bufferSeconds)*time.Second)
		framework.ExpectNoError(err, "Pod did not terminate within the expected grace period")
	})


	// Test Case: Natural Container Exit and PreStop Hook Execution
	ginkgo.It("should execute the PreStop hook when the container process exits naturally", func() {
		client := e2epod.NewPodClient(f)
		gracePeriod := int64(30)   // Define a 30-second grace period

		ginkgo.By("creating a pod with a PreStop hook that will handle a natural container exit")
		pod := createPodWithPreStopHook(f, time.Duration(gracePeriod)*time.Second)   // Create pod with a PreStop hook
		pod = client.Create(context.TODO(), pod)

		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)

		ginkgo.By("simulating the natural exit of the container")
		completePodProcess(f.ClientSet, pod)   // Simulate the natural exit of the container

		ginkgo.By("verifying that the PreStop hook was executed upon container exit")
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "busybox")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook triggered"), true, "Expected PreStop hook to run on natural container exit")
	})
	

	// Test Case: Forceful Pod Deletion and PreStop Hook Execution
	ginkgo.It("should not execute the PreStop hook on forceful pod deletion", func() {
		client := e2epod.NewPodClient(f)
		
		// Define 0-second grace period for forceful deletion
		gracePeriod := int64(0) 

		ginkgo.By("creating a pod with a PreStop hook and 0s grace period for forceful deletion")
		pod := createPodWithPreStopHook(f, time.Duration(gracePeriod)*time.Second)
		pod.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
			PreStop: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: []string{"sleep", "10000"},  // Simulate a process that would be skipped on forceful delete
				},
			},
		}
		pod = client.Create(context.TODO(), pod)

		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)

		ginkgo.By("forcefully deleting the pod without triggering the PreStop hook")
		err = e2epod.DeletePodWithForce(f.ClientSet, f.Namespace.Name, pod.Name)
		framework.ExpectNoError(err)

		ginkgo.By("verifying that the PreStop hook was NOT triggered during forceful deletion")
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "busybox")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook triggered"), false, "PreStop hook should NOT have executed on forceful pod deletion")
	})
	

	// Test Case: OOM Kill Scenario and PreStop Hook Execution
	ginkgo.It("should execute the PreStop hook before OOM kill", func() {
		client := e2epod.NewPodClient(f)

		ginkgo.By("creating a pod with a memory limit that will trigger OOM")
		pod := createPodWithMemoryLimit(f) // Pod that is expected to exceed its memory limit and trigger an OOM kill
		pod = client.Create(context.TODO(), pod)

		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)

		ginkgo.By("waiting for the pod to hit the memory limit and trigger OOM")
		time.Sleep(60 * time.Second) // Simulate waiting for OOM to occur

		ginkgo.By("verifying that the PreStop hook was executed before the OOM kill")
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "memory-hog")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook executed before OOM"), true, "Expected PreStop hook to run before OOM kill")
	})


	// Test Case: PreStop Hook Failure
	ginkgo.It("should handle PreStop hook failure correctly", func() {
		pod := createPodWithFailingPreStopHook(f) // Pod with a failing PreStop hook
		e2epod.WaitForPodRunningInNamespace(f.ClientSet, pod)

		// Trigger failing PreStop hook by deleting the pod
		e2epod.DeletePodWithGrace(f.ClientSet, f.Namespace.Name, pod.Name, 30*time.Second)

		// Verify PreStop hook failure and pod termination
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "busybox")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook failed"), true, "PreStop hook failure should be logged")
	})


	// Test Case: PreStop Hook for Init Containers
	ginkgo.It("should execute the PreStop hook for init containers correctly", func() {
		client := e2epod.NewPodClient(f)

		ginkgo.By("creating a pod with an init container and a PreStop hook")
		pod := createPodWithInitAndPreStopHook(f)
		pod = client.Create(context.TODO(), pod)

		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)

		ginkgo.By("deleting the pod and triggering the PreStop hook")
		err = e2epod.DeletePodWithGrace(f.ClientSet, f.Namespace.Name, pod.Name, 30*time.Second)
		framework.ExpectNoError(err)

		ginkgo.By("verifying PreStop hook execution for init containers")
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "busybox")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook triggered"), true)
	})


	// Test Case: PreStop Hook for Ephemeral Containers
	ginkgo.It("should execute the PreStop hook for ephemeral containers correctly", func() {
		client := e2epod.NewPodClient(f)
	
		ginkgo.By("creating a pod with an ephemeral container and a PreStop hook")
		pod := createPodWithEphemeralContainerAndPreStop(f)
		pod = client.Create(context.TODO(), pod)
	
		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)
	
		ginkgo.By("deleting the pod and triggering the PreStop hook")
		err = e2epod.DeletePodWithGrace(f.ClientSet, f.Namespace.Name, pod.Name, 30*time.Second)
		framework.ExpectNoError(err)
	
		ginkgo.By("verifying PreStop hook execution for ephemeral containers")
		logs, err := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, "busybox")
		framework.ExpectNoError(err)
		framework.ExpectEqual(strings.Contains(logs, "PreStop hook triggered for ephemeral container"), true)
	})
	
	// Test Case: Multi-Container Pod PreStop Hook Execution
	
	// Test Case: Pod Deletion During Node Drain
	
	// Test Case: Delayed PreStop Hook Execution
	
})
