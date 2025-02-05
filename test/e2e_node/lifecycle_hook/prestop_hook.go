package lifecycle_hooks
 
import (
    "context"
    "fmt"
    "time"
 
    "k8s.io/kubernetes/test/e2e/framework/ssh"
    v1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/kubernetes/test/e2e/framework"
    e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
    imageutils "k8s.io/kubernetes/test/utils/image"
 
    "github.com/onsi/ginkgo/v2"
    "github.com/onsi/gomega"
)
 
 
var _ = ginkgo.Describe("PreStop Hook TestSuite", func() {
 
    f := framework.NewDefaultFramework("prestop-hook-testsuite")
    
    var podClient *e2epod.PodClient
    ginkgo.BeforeEach(func() {
        podClient = e2epod.NewPodClient(f)
    })
 
    ginkgo.It("should run PreStop hook basic test case and verify execution ", func(ctx context.Context) {
 
        TestName("PreStop Hook")
        TestStep("When you have a PreStop hook defined on your container, it will execute before the container is terminated within grace period.")
 
        volumeName := "shared-data"
        mountPath := "/tmp/prestop-test-logs"
 
        // Define the lifecycle with the PreStop hook
        lifecycle := &v1.Lifecycle{
            PreStop: &v1.LifecycleHandler{
                Exec: &v1.ExecAction{
                    Command: []string{
                        "sh",
                        "-c",
                        fmt.Sprintf("echo 'PreStop Hook Executed' > %s/prestop_hook_executed; sleep 10", mountPath),
                        //"echo 'PreStop Hook Executed' > /tmp/prestop-test-logs/prestop_hook_executed",
                    },
                },
            },
        }
 
        TestStep("Imagine You Have a Pod with the Following Specification")
 
        // Define the pod with the emptyDir volume and appropriate security context
        podWithHook := &v1.Pod{
            ObjectMeta: metav1.ObjectMeta{
                Name: "prestop-pod",
            },
            Spec: v1.PodSpec{
                Containers: []v1.Container{
                    {
                        Name:    "prestop-container",
                        Image:   imageutils.GetE2EImage(imageutils.Agnhost),
                        Args:    []string{"pause"},
                        Lifecycle: lifecycle,
                        VolumeMounts: []v1.VolumeMount{
                            {
                                Name:      volumeName,
                                MountPath: mountPath,
                            },
                        },
                        SecurityContext: &v1.SecurityContext{
                            RunAsNonRoot:             boolPtr(true),
                            RunAsUser:                int64Ptr(1000),
                            RunAsGroup:               int64Ptr(1000),
                            AllowPrivilegeEscalation: boolPtr(false),
                            Capabilities: &v1.Capabilities{
                                Drop: []v1.Capability{"ALL"},
                            },
                            SeccompProfile: &v1.SeccompProfile{
                                Type: v1.SeccompProfileTypeRuntimeDefault,
                            },
                        },
                    },
                },
                Volumes: []v1.Volume{
                    {
                        Name: volumeName,
                        VolumeSource: v1.VolumeSource{
                            EmptyDir: &v1.EmptyDirVolumeSource{
                                Medium: v1.StorageMediumDefault,
                            },
                        },
                    },
                },
                TerminationGracePeriodSeconds: int64Ptr(30),
            },
        }
 
        PodSpec(podWithHook)
 
        // Create and run the pod
        TestStep("This Pod should start successfully and run for some time")
 
        createdPod := podClient.CreateSync(ctx, podWithHook)
    
        ginkgo.By("Pod is created successfully")
        TestLog("Pod is created successfully")
 
        // Wait for the pod to be running
        err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, createdPod)
        framework.ExpectNoError(err)
 
        TestLog("The Pod is running successfully")
 
        TestStep("When the container is terminated, the PreStop hook will be triggered within the grace period")
 
        // Delete the pod to trigger the PreStop hook
        err = podClient.Delete(ctx, createdPod.Name, metav1.DeleteOptions{})
        framework.ExpectNoError(err)
 
        TestLog("Pod was deleted successfully")
 
        ginkgo.By("ensuring the pod is terminated within the grace period seconds")
        err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, podWithHook.Name, podWithHook.Namespace, 30*time.Second)
        framework.ExpectNoError(err)
 
        // Verify the file on the host using SSH helper
        ginkgo.By("verifying the PreStop hook output on the host")
        node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, podWithHook.Spec.NodeName, metav1.GetOptions{})
        cmd := fmt.Sprintf("cat %s/prestop_hook_executed", mountPath)
        result, err := ssh.IssueSSHCommandWithResult(ctx, cmd, "local", node)
 
        // Print Stdout and validate it contains the expected message
        TestLog(result.Stdout)
        gomega.Expect(result.Stdout).To(gomega.ContainSubstring("PreStop Hook Executed"))
 
        TestStep("Once the PreStop hook is successfully executed, the container terminates successfully.")
 
    })
 
})
 