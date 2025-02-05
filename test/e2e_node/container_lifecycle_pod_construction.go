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

package e2enode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

type execCommand struct {
	// ExitCode is the exit status of the container
	ExitCode int
	// StartDelay is how long the container should delay before starting
	StartDelay int
	// Delay is how long the container should delay before exiting
	Delay int
	// LoopForever if set will cause the command to log once per second in a loop until
	// terminated
	LoopForever bool
	// TerminationSeconds is the time it takes for the container before
	// terminating if it catches SIGTERM.
	TerminationSeconds int
	// ContainerName is the name of the container to append the log. If empty,
	// the name specified in ExecCommand will be used.
	ContainerName string
}

// ExecCommand returns the command to execute in the container that implements
// execCommand and logs activities to a container specific log that persists
// across container restarts. The final log is written to container log so it
// can be retrieved by the test harness during the container execution.
// Log to /proc/1/fd/1 so that the lifecycle hook handler logs are captured as
// well.
func ExecCommand(name string, c execCommand) []string {
	var cmd bytes.Buffer
	// all outputs are in the format of:
	// time-since-boot timestamp container-name message

	// The busybox time command doesn't support sub-second display.
	// We have to use nmeter to get the milliseconds part.
	timeCmd := "`date -u +%FT$(nmeter -d0 '%3t' | head -n1)Z`"
	containerName := name
	if c.ContainerName != "" {
		containerName = c.ContainerName
	}
	containerLog := fmt.Sprintf("/persistent/%s.log", containerName)

	fmt.Fprintf(&cmd, "touch %s; ", containerLog)
	if c.ContainerName == "" {
		fmt.Fprintf(&cmd, "cat %s >> /proc/1/fd/1; ", containerLog)
	}

	fmt.Fprintf(&cmd, "echo %s '%s Starting %d' | tee -a %s >> /proc/1/fd/1; ", timeCmd, name, c.StartDelay, containerLog)
	fmt.Fprintf(&cmd, `
_term() {
  echo %s '%s SIGTERM' | tee -a %s >> /proc/1/fd/1
  sleep %d
  echo %s '%s Exiting' | tee -a %s >> /proc/1/fd/1
  exit %d
}; `,
		timeCmd, name, containerLog,
		c.TerminationSeconds,
		timeCmd, name, containerLog,
		c.ExitCode)
	fmt.Fprintf(&cmd, "trap _term TERM; ")
	if c.StartDelay != 0 {
		fmt.Fprint(&cmd, sleepCommand(c.StartDelay))
	}
	// You can check started file to see if the container has started
	fmt.Fprintf(&cmd, "touch started; ")
	fmt.Fprintf(&cmd, "echo %s '%s Started' | tee -a %s >> /proc/1/fd/1; ", timeCmd, name, containerLog)
	fmt.Fprintf(&cmd, "echo %s '%s Delaying %d' | tee -a %s >> /proc/1/fd/1; ", timeCmd, name, c.Delay, containerLog)
	if c.Delay != 0 {
		fmt.Fprint(&cmd, sleepCommand(c.Delay))
	}
	if c.LoopForever {
		fmt.Fprintf(&cmd, "while true; do echo %s '%s Looping' | tee -a %s >> /proc/1/fd/1 ; sleep 1 ; done; ", timeCmd, name, containerLog)
	}
	fmt.Fprintf(&cmd, "echo %s '%s Exiting'  | tee -a %s >> /proc/1/fd/1; ", timeCmd, name, containerLog)
	fmt.Fprintf(&cmd, "exit %d", c.ExitCode)
	return []string{"sh", "-c", cmd.String()}
}

// sleepCommand returns a command that sleeps for the given number of seconds
// in background and waits for it to finish so that the parent process can
// handle signals.
func sleepCommand(seconds int) string {
	return fmt.Sprintf("exec sleep %d & wait $!; ", seconds)
}

type containerOutput struct {
	// time the message was seen to the nearest second
	timestamp     time.Time
	containerName string
	command       string
}
type containerOutputList []containerOutput

func (o containerOutputList) String() string {
	var b bytes.Buffer
	for i, v := range o {
		fmt.Fprintf(&b, "%d) %s %s %s\n", i, v.timestamp, v.containerName, v.command)
	}
	return b.String()
}

// RunTogether returns an error if containers don't run together
func (o containerOutputList) RunTogether(lhs, rhs string) error {
	if err := o.RunTogetherLhsFirst(lhs, rhs); err != nil {
		if err := o.RunTogetherLhsFirst(rhs, lhs); err != nil {
			return err
		}
	}
	return nil
}

// RunTogetherLhsFirst returns an error if containers don't run together or if rhs starts before lhs
func (o containerOutputList) RunTogetherLhsFirst(lhs, rhs string) error {
	lhsStart := o.findIndex(lhs, "Started", 0)
	if lhsStart == -1 {
		return fmt.Errorf("couldn't find that %s ever started, got\n%v", lhs, o)
	}

	rhsStart := o.findIndex(rhs, "Started", lhsStart+1)
	if rhsStart == -1 {
		return fmt.Errorf("couldn't find that %s ever started, got\n%v", rhs, o)
	}

	lhsExit := o.findIndex(lhs, "Exiting", lhsStart+1)
	if lhsExit == -1 {
		return fmt.Errorf("couldn't find that %s ever exited, got\n%v", lhs, o)
	}

	if rhsStart > lhsExit {
		return fmt.Errorf("expected %s to start before exiting %s, got\n%v", rhs, lhs, o)
	}

	rhsExit := o.findIndex(rhs, "Exiting", rhsStart+1)
	if rhsExit == -1 {
		return fmt.Errorf("couldn't find that %s ever exited, got\n%v", rhs, o)
	}

	if lhsStart > rhsExit {
		return fmt.Errorf("expected %s to start before exiting %s, got\n%v", lhs, rhs, o)
	}

	return nil
}

// StartsBefore returns an error if lhs did not start before rhs
func (o containerOutputList) StartsBefore(lhs, rhs string) error {
	lhsStart := o.findIndex(lhs, "Started", 0)

	if lhsStart == -1 {
		return fmt.Errorf("couldn't find that %s ever started, got\n%v", lhs, o)
	}

	// this works even for the same names (restart case)
	rhsStart := o.findIndex(rhs, "Starting", lhsStart+1)

	if rhsStart == -1 {
		return fmt.Errorf("couldn't find that %s started after %s, got\n%v", rhs, lhs, o)
	}
	return nil
}

// ExitsBefore returns an error if lhs did not end before rhs
func (o containerOutputList) ExitsBefore(lhs, rhs string) error {
	lhsExit := o.findIndex(lhs, "Exiting", 0)

	if lhsExit == -1 {
		return fmt.Errorf("couldn't find that %s ever exited, got\n%v", lhs, o)
	}

	// this works even for the same names (restart case)
	rhsExit := o.findIndex(rhs, "Exiting", lhsExit+1)

	if rhsExit == -1 {
		return fmt.Errorf("couldn't find that %s starting before %s exited (starting at idx %d), got\n%v", rhs, lhs, lhsExit+1, o)
	}
	return nil
}

// Starts returns an error if the container was not found to have started
func (o containerOutputList) Starts(name string) error {
	if idx := o.findIndex(name, "Started", 0); idx == -1 {
		return fmt.Errorf("couldn't find that %s ever started, got\n%v", name, o)
	}
	return nil
}

// DoesntStart returns an error if the container was found to have started
func (o containerOutputList) DoesntStart(name string) error {
	if idx := o.findIndex(name, "Started", 0); idx != -1 {
		return fmt.Errorf("find %s started, but didn't expect to, got\n%v", name, o)
	}
	return nil
}

// Exits returns an error if the container was not found to have exited
func (o containerOutputList) Exits(name string) error {
	if idx := o.findIndex(name, "Exiting", 0); idx == -1 {
		return fmt.Errorf("couldn't find that %s ever exited, got\n%v", name, o)
	}
	return nil
}

// HasRestarted returns an error if the container was not found to have restarted
func (o containerOutputList) HasRestarted(name string) error {
	idx := o.findIndex(name, "Starting", 0)
	if idx == -1 {
		return fmt.Errorf("couldn't find that %s ever started, got\n%v", name, o)
	}

	idx = o.findIndex(name, "Starting", idx+1)

	if idx == -1 {
		return fmt.Errorf("couldn't find that %s ever restarted, got\n%v", name, o)
	}

	return nil
}

// HasNotRestarted returns an error if the container was found to have restarted
func (o containerOutputList) HasNotRestarted(name string) error {
	idx := o.findIndex(name, "Starting", 0)
	if idx == -1 {
		return fmt.Errorf("couldn't find that %s ever started, got\n%v", name, o)
	}

	idx = o.findIndex(name, "Starting", idx+1)

	if idx != -1 {
		return fmt.Errorf("found that %s restarted but wasn't expected to, got\n%v", name, o)
	}

	return nil
}

type containerOutputIndex int

func (i containerOutputIndex) IsBefore(other containerOutputIndex) error {
	if i >= other {
		return fmt.Errorf("%d should be before %d", i, other)
	}
	return nil
}

func (o containerOutputList) FindIndex(name string, command string, startIdx containerOutputIndex) (containerOutputIndex, error) {
	idx := o.findIndex(name, command, int(startIdx))
	if idx == -1 {
		return -1, fmt.Errorf("couldn't find %s %s, got\n%v", name, command, o)
	}
	return containerOutputIndex(idx), nil
}

func (o containerOutputList) findIndex(name string, command string, startIdx int) int {
	for i, v := range o {
		if i < startIdx {
			continue
		}
		if v.containerName == name && v.command == command {
			return i
		}
	}
	return -1
}
func (o containerOutputList) findLastIndex(name string, command string) int {
	found := -1
	for i, v := range o {
		if v.containerName == name && v.command == command {
			found = i
		}
	}
	return found
}

// TimeOfStart returns the UNIX time in milliseconds when the specified container started.
func (o containerOutputList) TimeOfStart(name string) (int64, error) {
	idx := o.findIndex(name, "Starting", 0)
	if idx == -1 {
		return 0, fmt.Errorf("couldn't find that %s ever started, got\n%v", name, o)
	}
	return o[idx].timestamp.UnixMilli(), nil
}

// TimeOfLastLoop returns the UNIX time in milliseconds when the specified container last looped.
func (o containerOutputList) TimeOfLastLoop(name string) (int64, error) {
	idx := o.findLastIndex(name, "Looping")
	if idx == -1 {
		return 0, fmt.Errorf("couldn't find that %s ever looped, got\n%v", name, o)
	}
	return o[idx].timestamp.UnixMilli(), nil
}

// parseOutput combines the container log from all of the init and regular
// containers and parses/sorts the outputs to produce an execution log
func parseOutput(ctx context.Context, f *framework.Framework, pod *v1.Pod) containerOutputList {
	// accumulate all of our statuses
	var statuses []v1.ContainerStatus
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)

	var buf bytes.Buffer
	for _, cs := range statuses {
		log, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cs.Name)
		if err != nil {
			framework.Logf("error getting logs for %s: %v", cs.Name, err)
			log, err = e2epod.GetPreviousPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cs.Name)
			if err != nil {
				framework.Logf("error getting previous logs for %s: %v", cs.Name, err)
			}
		}
		buf.WriteString(log)
	}

	// parse
	sc := bufio.NewScanner(&buf)
	var res containerOutputList
	for sc.Scan() {
		log := sc.Text()
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			framework.ExpectNoError(fmt.Errorf("%v should have at least length 3", fields))
		}
		timestamp, err := time.Parse(time.RFC3339, fields[0])
		framework.ExpectNoError(err, "Failed to parse the timestamp, log: %q", log)
		res = append(res, containerOutput{
			timestamp:     timestamp,
			containerName: fields[1],
			command:       fields[2],
		})
	}

	// sort using the timeSinceBoot since it has more precision
	sort.Slice(res, func(i, j int) bool {
		return res[i].timestamp.Before(res[j].timestamp)
	})
	return res
}

// preparePod adds an empty dir volume and mounts it to each container at /persistent
func preparePod(pod *v1.Pod) {
	var defaultResourceRequirements = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: resource.MustParse("15Mi"),
		},
		Limits: v1.ResourceList{
			v1.ResourceMemory: resource.MustParse("35Mi"),
		},
	}

	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		c.Resources = defaultResourceRequirements
		c.VolumeMounts = []v1.VolumeMount{
			{
				Name:      "persistent",
				MountPath: "/persistent",
			},
		}
	}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		c.Resources = defaultResourceRequirements
		c.VolumeMounts = []v1.VolumeMount{
			{
				Name:      "persistent",
				MountPath: "/persistent",
			},
		}
	}

	pod.Spec.Volumes = []v1.Volume{
		{
			Name: "persistent",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		},
	}
}
