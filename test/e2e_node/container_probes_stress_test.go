package e2enode

import (
	"context"
	"fmt"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
	"time"
)

const (
	testPodCount      = 1
	containerCount    = 600
	baseContainerPort = 1111
)

// This test must run [Serial] due to the impact of running other parallel
// tests can have on its performance.
var _ = SIGDescribe("Container-probes-stress", func() {
	f := framework.NewDefaultFramework("container-probes-stress")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.It("GRPC probe", func() {
		pods := createTestPods("GRPC", f, context.Background())
		runLivenessTest(pods, f)
	})
	ginkgo.It("TCP probe", func() {
		pods := createTestPods("TCP", f, context.Background())
		runLivenessTest(pods, f)
	})
	ginkgo.It("HTTP probe", func() {
		pods := createTestPods("HTTP", f, context.Background())
		runLivenessTest(pods, f)
	})
})

func runLivenessTest(pods *[]v1.Pod, f *framework.Framework) {
	for _, pod := range *pods {
		podClient := e2epod.NewPodClient(f)
		containerCount := len(pod.Spec.Containers)
		gomega.Expect(containerCount).To(gomega.Equal(containerCount), "pod should have a %v countainers but have %v", containerCount, containerCount)
		// At the end of the test, clean up by removing the pod.
		ginkgo.DeferCleanup(func(ctx context.Context) error {
			ginkgo.By("deleting the pod")
			return podClient.Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		})
		ginkgo.By(fmt.Sprintf("Creating pod %s in namespace %s", pod.Name, f.Namespace.Name))
		podClient.Create(context.Background(), &pod)
		// Wait until the pod is running.
		framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(context.Background(), f.ClientSet, &pod),
			fmt.Sprintf("starting pod %s in namespace %s", pod.Name, f.Namespace.Name))
		framework.Logf("Started pod %s in namespace %s", pod.Name, f.Namespace.Name)

		retries := int(framework.DefaultObservationTimeout.Seconds() / 10)
		for attempt := 0; attempt < retries; attempt++ {
			// Check the pod's current state and verify that restartCount is present.
			ginkgo.By("checking the pod's current state and verifying that restartCount is present")
			pod, err := podClient.Get(context.Background(), pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, fmt.Sprintf("getting pod %s in namespace %s", pod.Name, f.Namespace.Name))
			restartCount := e2epod.GetRestartCount(pod)

			if restartCount != 0 {
				framework.Failf("Restart count of pod %s/%s is now %d (attempt %d)",
					f.Namespace.Name, pod.Name, restartCount, attempt+1)
			}

			time.Sleep(10 * time.Second)
		}
	}
}

func createTestPods(probeType string, f *framework.Framework, ctx context.Context) *[]v1.Pod {
	client := e2epod.NewPodClient(f)
	var pods []v1.Pod
	for podNum := 0; podNum < testPodCount; podNum++ {
		pod := newPod(*client, podNum, ctx)
		pods = append(pods, *pod)
		for containerNum := 0; containerNum < containerCount; containerNum++ {
			container := newContainer(containerNum)
			probe := createLivenessProbe(probeType, container)
			container.LivenessProbe = probe
			pod.Spec.Containers = append(pod.Spec.Containers, *container)
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, newContainerStatus(podNum, containerNum))
		}
	}
	return &pods
}

func newPod(client e2epod.PodClient, podNum int, ctx context.Context) *v1.Pod {
	pod := client.Create(ctx, &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(fmt.Sprintf("pod%d", podNum)),
			Name:      fmt.Sprintf("pod%d", podNum),
			Namespace: "test",
		},
		Spec: v1.PodSpec{},
		Status: v1.PodStatus{
			Phase:  v1.PodPhase(v1.PodReady),
			PodIPs: []v1.PodIP{{IP: "127.0.0.1"}},
		},
	})
	return pod
}

func newContainerStatus(podNum int, containerNum int) v1.ContainerStatus {
	containerStatus := &v1.ContainerStatus{
		Name:        fmt.Sprintf("container%d", containerNum),
		ContainerID: fmt.Sprintf("pod%d://container%d", podNum, containerNum),
		State: v1.ContainerState{
			Running: &v1.ContainerStateRunning{
				StartedAt: metav1.Now(),
			},
		},
		Started: ptr.To(true),
	}
	return *containerStatus
}

func newContainer(n int) *v1.Container {
	container := &v1.Container{
		Name:  fmt.Sprintf("container-%d", n),
		Ports: []v1.ContainerPort{{ContainerPort: baseContainerPort + int32(n)}}}
	return container
}

func createLivenessProbe(handlerType string, container *v1.Container) *v1.Probe {
	probe := v1.Probe{
		TimeoutSeconds:   1,
		PeriodSeconds:    1,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	switch handlerType {
	case "GRPC":
		probe.ProbeHandler = v1.ProbeHandler{
			GRPC: &v1.GRPCAction{
				Port:    container.Ports[0].ContainerPort,
				Service: nil,
			},
		}
	case "TCP":
		probe.ProbeHandler = v1.ProbeHandler{
			TCPSocket: &v1.TCPSocketAction{
				Port: intstr.FromInt32(container.Ports[0].ContainerPort),
				Host: container.Name,
			},
		}
	case "HTTP":
		probe.ProbeHandler = v1.ProbeHandler{
			HTTPGet: &v1.HTTPGetAction{
				Port: intstr.FromInt32(container.Ports[0].ContainerPort),
				Host: container.Name,
			},
		}
	default:
		ginkgo.Fail(fmt.Sprintf("unknown handler type: %s", handlerType))
	}

	return &probe
}
