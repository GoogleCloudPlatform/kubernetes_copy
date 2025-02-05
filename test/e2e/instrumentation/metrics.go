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

package instrumentation

import (
	"context"
	"errors"
	"time"

	"github.com/onsi/gomega"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2emetrics "k8s.io/kubernetes/test/e2e/framework/metrics"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/instrumentation/common"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

var _ = common.SIGDescribe("Metrics", func() {
	f := framework.NewDefaultFramework("metrics")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var c, ec clientset.Interface
	var grabber *e2emetrics.Grabber
	ginkgo.BeforeEach(func(ctx context.Context) {
		var err error
		c = f.ClientSet
		ec = f.KubemarkExternalClusterClientSet
		gomega.Eventually(ctx, func() error {
			grabber, err = e2emetrics.NewMetricsGrabber(ctx, c, ec, f.ClientConfig(), true, true, true, true, true, true)
			return err
		}, 5*time.Minute, 10*time.Second).Should(gomega.BeNil())
	})

	/*
	   Release: v1.29
	   Testname: Kubelet resource metrics
	   Description: Should attempt to grab all resource metrics from kubelet metrics/resource endpoint.
	*/
	ginkgo.It("should grab all metrics from kubelet /metrics/resource endpoint", func(ctx context.Context) {
		ginkgo.By("Connecting to kubelet's /metrics/resource endpoint")
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		if errors.Is(err, e2emetrics.MetricsGrabbingDisabledError) {
			e2eskipper.Skipf("%v", err)
		}
		framework.ExpectNoError(err)
		response, err := grabber.GrabResourceMetricsFromKubelet(ctx, node.Name)
		framework.ExpectNoError(err)
		gomega.Expect(response).NotTo(gomega.BeEmpty())
	})
})
