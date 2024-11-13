/*
Copyright 2019 The Kubernetes Authors.

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

package pod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/test/e2e/framework"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

// LabelLogOnPodFailure can be used to mark which Pods will have their logs logged in the case of
// a test failure. By default, if there are no Pods with this label, only the first 5 Pods will
// have their logs fetched.
const LabelLogOnPodFailure = "log-on-pod-failure"

// TODO: Move to its own subpkg.
// expectNoError checks if "err" is set, and if so, fails assertion while logging the error.
func expectNoError(err error, explain ...interface{}) {
	expectNoErrorWithOffset(1, err, explain...)
}

// TODO: Move to its own subpkg.
// expectNoErrorWithOffset checks if "err" is set, and if so, fails assertion while logging the error at "offset" levels above its caller
// (for example, for call chain f -> g -> expectNoErrorWithOffset(1, ...) error would be logged for "f").
func expectNoErrorWithOffset(offset int, err error, explain ...interface{}) {
	if err != nil {
		framework.Logf("Unexpected error occurred: %v", err)
	}
	gomega.ExpectWithOffset(1+offset, err).NotTo(gomega.HaveOccurred(), explain...)
}

// PodsCreated returns a pod list matched by the given name.
func PodsCreated(ctx context.Context, c clientset.Interface, ns, name string, replicas int32) (*v1.PodList, error) {
	label := labels.SelectorFromSet(labels.Set(map[string]string{"name": name}))
	return PodsCreatedByLabel(ctx, c, ns, name, replicas, label)
}

// PodsCreatedByLabel returns a created pod list matched by the given label.
func PodsCreatedByLabel(ctx context.Context, c clientset.Interface, ns, name string, replicas int32, label labels.Selector) (*v1.PodList, error) {
	timeout := 2 * time.Minute
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(5 * time.Second) {
		options := metav1.ListOptions{LabelSelector: label.String()}

		// List the pods, making sure we observe all the replicas.
		pods, err := c.CoreV1().Pods(ns).List(ctx, options)
		if err != nil {
			return nil, err
		}

		created := []v1.Pod{}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			created = append(created, pod)
		}
		framework.Logf("Pod name %s: Found %d pods out of %d", name, len(created), replicas)

		if int32(len(created)) == replicas {
			pods.Items = created
			return pods, nil
		}
	}
	return nil, fmt.Errorf("Pod name %s: Gave up waiting %v for %d pods to come up", name, timeout, replicas)
}

// VerifyPods checks if the specified pod is responding.
func VerifyPods(ctx context.Context, c clientset.Interface, ns, name string, wantName bool, replicas int32) error {
	return podRunningMaybeResponding(ctx, c, ns, name, wantName, replicas, true)
}

// VerifyPodsRunning checks if the specified pod is running.
func VerifyPodsRunning(ctx context.Context, c clientset.Interface, ns, name string, wantName bool, replicas int32) error {
	return podRunningMaybeResponding(ctx, c, ns, name, wantName, replicas, false)
}

func podRunningMaybeResponding(ctx context.Context, c clientset.Interface, ns, name string, wantName bool, replicas int32, checkResponding bool) error {
	pods, err := PodsCreated(ctx, c, ns, name, replicas)
	if err != nil {
		return err
	}
	e := podsRunning(ctx, c, pods)
	if len(e) > 0 {
		return fmt.Errorf("failed to wait for pods running: %v", e)
	}
	if checkResponding {
		return WaitForPodsResponding(ctx, c, ns, name, wantName, podRespondingTimeout, pods)
	}
	return nil
}

func podsRunning(ctx context.Context, c clientset.Interface, pods *v1.PodList) []error {
	// Wait for the pods to enter the running state. Waiting loops until the pods
	// are running so non-running pods cause a timeout for this test.
	ginkgo.By("ensuring each pod is running")
	e := []error{}
	errorChan := make(chan error)

	for _, pod := range pods.Items {
		go func(p v1.Pod) {
			errorChan <- WaitForPodRunningInNamespace(ctx, c, &p)
		}(pod)
	}

	for range pods.Items {
		err := <-errorChan
		if err != nil {
			e = append(e, err)
		}
	}

	return e
}

// LogPodStates logs basic info of provided pods for debugging.
func LogPodStates(pods []v1.Pod) {
	// Find maximum widths for pod, node, and phase strings for column printing.
	maxPodW, maxNodeW, maxPhaseW, maxGraceW := len("POD"), len("NODE"), len("PHASE"), len("GRACE")
	for i := range pods {
		pod := &pods[i]
		if len(pod.ObjectMeta.Name) > maxPodW {
			maxPodW = len(pod.ObjectMeta.Name)
		}
		if len(pod.Spec.NodeName) > maxNodeW {
			maxNodeW = len(pod.Spec.NodeName)
		}
		if len(pod.Status.Phase) > maxPhaseW {
			maxPhaseW = len(pod.Status.Phase)
		}
	}
	// Increase widths by one to separate by a single space.
	maxPodW++
	maxNodeW++
	maxPhaseW++
	maxGraceW++

	// Log pod info. * does space padding, - makes them left-aligned.
	framework.Logf("%-[1]*[2]s %-[3]*[4]s %-[5]*[6]s %-[7]*[8]s %[9]s",
		maxPodW, "POD", maxNodeW, "NODE", maxPhaseW, "PHASE", maxGraceW, "GRACE", "CONDITIONS")
	for _, pod := range pods {
		grace := ""
		if pod.DeletionGracePeriodSeconds != nil {
			grace = fmt.Sprintf("%ds", *pod.DeletionGracePeriodSeconds)
		}
		framework.Logf("%-[1]*[2]s %-[3]*[4]s %-[5]*[6]s %-[7]*[8]s %[9]s",
			maxPodW, pod.ObjectMeta.Name, maxNodeW, pod.Spec.NodeName, maxPhaseW, pod.Status.Phase, maxGraceW, grace, pod.Status.Conditions)
	}
	framework.Logf("") // Final empty line helps for readability.
}

// logPodTerminationMessages logs termination messages for failing pods.  It's a short snippet (much smaller than full logs), but it often shows
// why pods crashed and since it is in the API, it's fast to retrieve.
func logPodTerminationMessages(pods []v1.Pod) {
	for _, pod := range pods {
		for _, status := range pod.Status.InitContainerStatuses {
			if status.LastTerminationState.Terminated != nil && len(status.LastTerminationState.Terminated.Message) > 0 {
				framework.Logf("%s[%s].initContainer[%s]=%s", pod.Name, pod.Namespace, status.Name, status.LastTerminationState.Terminated.Message)
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.LastTerminationState.Terminated != nil && len(status.LastTerminationState.Terminated.Message) > 0 {
				framework.Logf("%s[%s].container[%s]=%s", pod.Name, pod.Namespace, status.Name, status.LastTerminationState.Terminated.Message)
			}
		}
	}
}

// logPodLogs logs the container logs from pods in the given namespace. This can be helpful for debugging
// issues that do not cause the container to fail (e.g.: network connectivity issues)
// We will log the Pods that have the LabelLogOnPodFailure label. If there aren't any, we default to
// logging only the first 5 Pods. This requires the reportDir to be set, and the pods are logged into:
// {report_dir}/pods/{namespace}/{pod}/{container_name}/logs.txt
func logPodLogs(ctx context.Context, c clientset.Interface, namespace string, pods []v1.Pod, reportDir string) {
	if reportDir == "" {
		return
	}

	var logPods []v1.Pod
	for _, pod := range pods {
		if _, ok := pod.Labels[LabelLogOnPodFailure]; ok {
			logPods = append(logPods, pod)
		}
	}
	maxPods := len(logPods)

	// There are no pods with the LabelLogOnPodFailure label, we default to the first 5 Pods.
	if maxPods == 0 {
		logPods = pods
		maxPods = len(pods)
		if maxPods > 5 {
			maxPods = 5
		}
	}

	tailLen := 42
	for i := 0; i < maxPods; i++ {
		pod := logPods[i]
		for _, container := range pod.Spec.Containers {
			logs, err := getPodLogsInternal(ctx, c, namespace, pod.Name, container.Name, false, nil, &tailLen)
			if err != nil {
				framework.Logf("Unable to fetch %s/%s/%s logs: %v", pod.Namespace, pod.Name, container.Name, err)
				continue
			}

			logDir := filepath.Join(reportDir, namespace, pod.Name, container.Name)
			err = os.MkdirAll(logDir, 0755)
			if err != nil {
				framework.Logf("Unable to create path '%s'. Err: %v", logDir, err)
				continue
			}

			logPath := filepath.Join(logDir, "logs.txt")
			err = os.WriteFile(logPath, []byte(logs), 0644)
			if err != nil {
				framework.Logf("Could not write the container logs in: %s. Err: %v", logPath, err)
			}
		}
	}
}

// DumpAllPodInfoForNamespace logs all pod information for a given namespace.
func DumpAllPodInfoForNamespace(ctx context.Context, c clientset.Interface, namespace, reportDir string) {
	pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		framework.Logf("unable to fetch pod debug info: %v", err)
	}
	LogPodStates(pods.Items)
	logPodTerminationMessages(pods.Items)
	logPodLogs(ctx, c, namespace, pods.Items, reportDir)
}

// FilterNonRestartablePods filters out pods that will never get recreated if
// deleted after termination.
func FilterNonRestartablePods(pods []*v1.Pod) []*v1.Pod {
	var results []*v1.Pod
	for _, p := range pods {
		if isNotRestartAlwaysMirrorPod(p) {
			// Mirror pods with restart policy == Never will not get
			// recreated if they are deleted after the pods have
			// terminated. For now, we discount such pods.
			// https://github.com/kubernetes/kubernetes/issues/34003
			continue
		}
		results = append(results, p)
	}
	return results
}

func isNotRestartAlwaysMirrorPod(p *v1.Pod) bool {
	// Check if the pod is a mirror pod
	if _, ok := p.Annotations[v1.MirrorPodAnnotationKey]; !ok {
		return false
	}
	return p.Spec.RestartPolicy != v1.RestartPolicyAlways
}

// NewAgnhostPod returns a pod that uses the agnhost image. The image's binary supports various subcommands
// that behave the same, no matter the underlying OS. If no args are given, it defaults to the pause subcommand.
// For more information about agnhost subcommands, see: https://github.com/kubernetes/kubernetes/tree/master/test/images/agnhost#agnhost
func NewAgnhostPod(ns, podName string, volumes []v1.Volume, mounts []v1.VolumeMount, ports []v1.ContainerPort, args ...string) *v1.Pod {
	immediate := int64(0)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				NewAgnhostContainer("agnhost-container", mounts, ports, args...),
			},
			Volumes:                       volumes,
			SecurityContext:               &v1.PodSecurityContext{},
			TerminationGracePeriodSeconds: &immediate,
		},
	}
	return pod
}

func NewAgnhostPodFromContainers(ns, podName string, volumes []v1.Volume, containers ...v1.Container) *v1.Pod {
	immediate := int64(0)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: v1.PodSpec{
			Containers:                    containers[:],
			Volumes:                       volumes,
			SecurityContext:               &v1.PodSecurityContext{},
			TerminationGracePeriodSeconds: &immediate,
		},
	}
	return pod
}

// NewAgnhostContainer returns the container Spec of an agnhost container.
func NewAgnhostContainer(containerName string, mounts []v1.VolumeMount, ports []v1.ContainerPort, args ...string) v1.Container {
	if len(args) == 0 {
		args = []string{"pause"}
	}
	return v1.Container{
		Name:            containerName,
		Image:           imageutils.GetE2EImage(imageutils.Agnhost),
		Args:            args,
		VolumeMounts:    mounts,
		Ports:           ports,
		SecurityContext: &v1.SecurityContext{},
		ImagePullPolicy: v1.PullIfNotPresent,
	}
}

// NewExecPodSpec returns the pod spec of hostexec pod
func NewExecPodSpec(ns, name string, hostNetwork bool) *v1.Pod {
	pod := NewAgnhostPod(ns, name, nil, nil, nil)
	pod.Spec.HostNetwork = hostNetwork
	return pod
}

// newExecPodSpec returns the pod spec of exec pod
func newExecPodSpec(ns, generateName string) *v1.Pod {
	// GenerateName is an optional prefix, used by the server,
	// to generate a unique name ONLY IF the Name field has not been provided
	pod := NewAgnhostPod(ns, "", nil, nil, nil)
	pod.ObjectMeta.GenerateName = generateName
	return pod
}

// CreateExecPodOrFail creates a agnhost pause pod used as a vessel for kubectl exec commands.
// Pod name is uniquely generated.
func CreateExecPodOrFail(ctx context.Context, client clientset.Interface, ns, generateName string, tweak func(*v1.Pod)) *v1.Pod {
	framework.Logf("Creating new exec pod")
	pod := newExecPodSpec(ns, generateName)
	if tweak != nil {
		tweak(pod)
	}
	execPod, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	expectNoError(err, "failed to create new exec pod in namespace: %s", ns)
	err = WaitForPodNameRunningInNamespace(ctx, client, execPod.Name, execPod.Namespace)
	expectNoError(err, "failed to create new exec pod in namespace: %s", ns)
	return execPod
}

// WithWindowsHostProcess sets the Pod's Windows HostProcess option to true. When this option is set,
// HostNetwork can be enabled.
// Containers running as HostProcess will require certain usernames to be set, otherwise the Pod will
// not start: NT AUTHORITY\SYSTEM, NT AUTHORITY\Local service, NT AUTHORITY\NetworkService.
// If the given username is empty, NT AUTHORITY\SYSTEM will be used instead.
// See: https://kubernetes.io/docs/tasks/configure-pod-container/create-hostprocess-pod/
func WithWindowsHostProcess(pod *v1.Pod, username string) {
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	if pod.Spec.SecurityContext.WindowsOptions == nil {
		pod.Spec.SecurityContext.WindowsOptions = &v1.WindowsSecurityContextOptions{}
	}

	trueVar := true
	if username == "" {
		username = "NT AUTHORITY\\SYSTEM"
	}
	pod.Spec.SecurityContext.WindowsOptions.HostProcess = &trueVar
	pod.Spec.SecurityContext.WindowsOptions.RunAsUserName = &username
}

// CheckPodsRunningReady returns whether all pods whose names are listed in
// podNames in namespace ns are running and ready, using c and waiting at most
// timeout.
func CheckPodsRunningReady(ctx context.Context, c clientset.Interface, ns string, podNames []string, timeout time.Duration) bool {
	return checkPodsCondition(ctx, c, ns, podNames, timeout, testutils.PodRunningReady, "running and ready")
}

// CheckPodsRunningReadyOrSucceeded returns whether all pods whose names are
// listed in podNames in namespace ns are running and ready, or succeeded; use
// c and waiting at most timeout.
func CheckPodsRunningReadyOrSucceeded(ctx context.Context, c clientset.Interface, ns string, podNames []string, timeout time.Duration) bool {
	return checkPodsCondition(ctx, c, ns, podNames, timeout, testutils.PodRunningReadyOrSucceeded, "running and ready, or succeeded")
}

// checkPodsCondition returns whether all pods whose names are listed in podNames
// in namespace ns are in the condition, using c and waiting at most timeout.
func checkPodsCondition(ctx context.Context, c clientset.Interface, ns string, podNames []string, timeout time.Duration, condition podCondition, desc string) bool {
	np := len(podNames)
	framework.Logf("Waiting up to %v for %d pods to be %s: %s", timeout, np, desc, podNames)
	type waitPodResult struct {
		success bool
		podName string
	}
	result := make(chan waitPodResult, len(podNames))
	for _, podName := range podNames {
		// Launch off pod readiness checkers.
		go func(name string) {
			err := WaitForPodCondition(ctx, c, ns, name, desc, timeout, condition)
			result <- waitPodResult{err == nil, name}
		}(podName)
	}
	// Wait for them all to finish.
	success := true
	for range podNames {
		res := <-result
		if !res.success {
			framework.Logf("Pod %[1]s failed to be %[2]s.", res.podName, desc)
			success = false
		}
	}
	framework.Logf("Wanted all %d pods to be %s. Result: %t. Pods: %v", np, desc, success, podNames)
	return success
}

// GetPodLogs returns the logs of the specified container (namespace/pod/container).
func GetPodLogs(ctx context.Context, c clientset.Interface, namespace, podName, containerName string) (string, error) {
	return getPodLogsInternal(ctx, c, namespace, podName, containerName, false, nil, nil)
}

// GetPodLogsSince returns the logs of the specified container (namespace/pod/container) since a timestamp.
func GetPodLogsSince(ctx context.Context, c clientset.Interface, namespace, podName, containerName string, since time.Time) (string, error) {
	sinceTime := metav1.NewTime(since)
	return getPodLogsInternal(ctx, c, namespace, podName, containerName, false, &sinceTime, nil)
}

// GetPreviousPodLogs returns the logs of the previous instance of the
// specified container (namespace/pod/container).
func GetPreviousPodLogs(ctx context.Context, c clientset.Interface, namespace, podName, containerName string) (string, error) {
	return getPodLogsInternal(ctx, c, namespace, podName, containerName, true, nil, nil)
}

// utility function for gomega Eventually
func getPodLogsInternal(ctx context.Context, c clientset.Interface, namespace, podName, containerName string, previous bool, sinceTime *metav1.Time, tailLines *int) (string, error) {
	request := c.CoreV1().RESTClient().Get().
		Resource("pods").
		Namespace(namespace).
		Name(podName).SubResource("log").
		Param("container", containerName).
		Param("previous", strconv.FormatBool(previous))
	if sinceTime != nil {
		request.Param("sinceTime", sinceTime.Format(time.RFC3339))
	}
	if tailLines != nil {
		request.Param("tailLines", strconv.Itoa(*tailLines))
	}
	logs, err := request.Do(ctx).Raw()
	if err != nil {
		return "", err
	}
	if strings.Contains(string(logs), "Internal Error") {
		return "", fmt.Errorf("Fetched log contains \"Internal Error\": %q", string(logs))
	}
	return string(logs), err
}

// GetPodsInNamespace returns the pods in the given namespace.
func GetPodsInNamespace(ctx context.Context, c clientset.Interface, ns string, ignoreLabels map[string]string) ([]*v1.Pod, error) {
	pods, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return []*v1.Pod{}, err
	}
	ignoreSelector := labels.SelectorFromSet(ignoreLabels)
	var filtered []*v1.Pod
	for i := range pods.Items {
		p := pods.Items[i]
		if len(ignoreLabels) != 0 && ignoreSelector.Matches(labels.Set(p.Labels)) {
			continue
		}
		filtered = append(filtered, &p)
	}
	return filtered, nil
}

// GetPods return the label matched pods in the given ns
func GetPods(ctx context.Context, c clientset.Interface, ns string, matchLabels map[string]string) ([]v1.Pod, error) {
	label := labels.SelectorFromSet(matchLabels)
	listOpts := metav1.ListOptions{LabelSelector: label.String()}
	pods, err := c.CoreV1().Pods(ns).List(ctx, listOpts)
	if err != nil {
		return []v1.Pod{}, err
	}
	return pods.Items, nil
}

// GetPodSecretUpdateTimeout returns the timeout duration for updating pod secret.
func GetPodSecretUpdateTimeout(ctx context.Context, c clientset.Interface) time.Duration {
	// With SecretManager(ConfigMapManager), we may have to wait up to full sync period +
	// TTL of secret(configmap) to elapse before the Kubelet projects the update into the
	// volume and the container picks it up.
	// So this timeout is based on default Kubelet sync period (1 minute) + maximum TTL for
	// secret(configmap) that's based on cluster size + additional time as a fudge factor.
	secretTTL, err := getNodeTTLAnnotationValue(ctx, c)
	if err != nil {
		framework.Logf("Couldn't get node TTL annotation (using default value of 0): %v", err)
	}
	podLogTimeout := 240*time.Second + secretTTL
	return podLogTimeout
}

// VerifyPodHasConditionWithType verifies the pod has the expected condition by type
func VerifyPodHasConditionWithType(ctx context.Context, f *framework.Framework, pod *v1.Pod, cType v1.PodConditionType) {
	pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
	framework.ExpectNoError(err, "Failed to get the recent pod object for name: %q", pod.Name)
	if condition := FindPodConditionByType(&pod.Status, cType); condition == nil {
		framework.Failf("pod %q should have the condition: %q, pod status: %v", pod.Name, cType, pod.Status)
	}
}

func getNodeTTLAnnotationValue(ctx context.Context, c clientset.Interface) (time.Duration, error) {
	nodes, err := c.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return time.Duration(0), fmt.Errorf("Couldn't list any nodes to get TTL annotation: %w", err)
	}
	// Since TTL the kubelet is using is stored in node object, for the timeout
	// purpose we take it from the first node (all of them should be the same).
	node := &nodes.Items[0]
	if node.Annotations == nil {
		return time.Duration(0), fmt.Errorf("No annotations found on the node")
	}
	value, ok := node.Annotations[v1.ObjectTTLAnnotationKey]
	if !ok {
		return time.Duration(0), fmt.Errorf("No TTL annotation found on the node")
	}
	intValue, err := strconv.Atoi(value)
	if err != nil {
		return time.Duration(0), fmt.Errorf("Cannot convert TTL annotation from %#v to int", *node)
	}
	return time.Duration(intValue) * time.Second, nil
}

// FilterActivePods returns pods that have not terminated.
func FilterActivePods(pods []*v1.Pod) []*v1.Pod {
	var result []*v1.Pod
	for _, p := range pods {
		if IsPodActive(p) {
			result = append(result, p)
		} else {
			klog.V(4).Infof("Ignoring inactive pod %v/%v in state %v, deletion time %v",
				p.Namespace, p.Name, p.Status.Phase, p.DeletionTimestamp)
		}
	}
	return result
}

// IsPodActive return true if the pod meets certain conditions.
func IsPodActive(p *v1.Pod) bool {
	return v1.PodSucceeded != p.Status.Phase &&
		v1.PodFailed != p.Status.Phase &&
		p.DeletionTimestamp == nil
}
