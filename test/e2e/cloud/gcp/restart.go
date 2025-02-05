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

package gcp

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/kubernetes/test/e2e/common"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

func nodeNames(nodes []v1.Node) []string {
	result := make([]string, 0, len(nodes))
	for i := range nodes {
		result = append(result, nodes[i].Name)
	}
	return result
}

var _ = SIGDescribe("Restart", framework.WithDisruptive(), func() {
	f := framework.NewDefaultFramework("restart")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var ps *testutils.PodStore
	var originalNodes []v1.Node
	var originalPodNames []string
	var numNodes int
	var systemNamespace string

	ginkgo.BeforeEach(func(ctx context.Context) {
		// This test requires the ability to restart all nodes, so the provider
		// check must be identical to that call.
		e2eskipper.SkipUnlessProviderIs("gce")
		var err error
		ps, err = testutils.NewPodStore(f.ClientSet, metav1.NamespaceSystem, labels.Everything(), fields.Everything())
		framework.ExpectNoError(err)
		numNodes, err = e2enode.TotalRegistered(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		systemNamespace = metav1.NamespaceSystem

		ginkgo.By("ensuring all nodes are ready")
		originalNodes, err = e2enode.CheckReady(ctx, f.ClientSet, numNodes, framework.NodeReadyInitialTimeout)
		framework.ExpectNoError(err)
		framework.Logf("Got the following nodes before restart: %v", nodeNames(originalNodes))

		ginkgo.By("ensuring all pods are running and ready")
		allPods := ps.List()
		pods := e2epod.FilterNonRecreatablePods(allPods)

		originalPodNames = make([]string, len(pods))
		for i, p := range pods {
			originalPodNames[i] = p.ObjectMeta.Name
		}
		if !e2epod.CheckPodsRunningReadyOrSucceeded(ctx, f.ClientSet, systemNamespace, originalPodNames, framework.PodReadyBeforeTimeout) {
			printStatusAndLogsForNotReadyPods(ctx, f.ClientSet, systemNamespace, originalPodNames, pods)
			framework.Failf("At least one pod wasn't running and ready or succeeded at test start.")
		}
	})

	ginkgo.AfterEach(func() {
		if ps != nil {
			ps.Stop()
		}
	})

	// TODO(upodroid) This test will be removed in 1.33 when kubeup is removed
	f.It(f.WithLabel("KubeUp"), "should restart all nodes and ensure all nodes and pods recover", func(ctx context.Context) {
		ginkgo.By("restarting all of the nodes")
		err := common.RestartNodes(f.ClientSet, originalNodes)
		framework.ExpectNoError(err)

		ginkgo.By("ensuring all nodes are ready after the restart")
		nodesAfter, err := e2enode.CheckReady(ctx, f.ClientSet, numNodes, framework.RestartNodeReadyAgainTimeout)
		framework.ExpectNoError(err)
		framework.Logf("Got the following nodes after restart: %v", nodeNames(nodesAfter))

		// Make sure that we have the same number of nodes. We're not checking
		// that the names match because that's implementation specific.
		ginkgo.By("ensuring the same number of nodes exist after the restart")
		if len(originalNodes) != len(nodesAfter) {
			framework.Failf("Had %d nodes before nodes were restarted, but now only have %d",
				len(originalNodes), len(nodesAfter))
		}

		// Make sure that we have the same number of pods. We're not checking
		// that the names match because they are recreated with different names
		// across node restarts.
		ginkgo.By("ensuring the same number of pods are running and ready after restart")
		podCheckStart := time.Now()
		podNamesAfter, err := e2epod.WaitForNRestartablePods(ctx, ps, len(originalPodNames), framework.RestartPodReadyAgainTimeout)
		framework.ExpectNoError(err)
		remaining := framework.RestartPodReadyAgainTimeout - time.Since(podCheckStart)
		if !e2epod.CheckPodsRunningReadyOrSucceeded(ctx, f.ClientSet, systemNamespace, podNamesAfter, remaining) {
			pods := ps.List()
			printStatusAndLogsForNotReadyPods(ctx, f.ClientSet, systemNamespace, podNamesAfter, pods)
			framework.Failf("At least one pod wasn't running and ready after the restart.")
		}
	})
})
