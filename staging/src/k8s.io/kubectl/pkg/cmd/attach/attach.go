/*
Copyright 2014 The Kubernetes Authors.

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

package attach

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/cmd/exec"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	attachExample = templates.Examples(i18n.T(`
		# Get output from running pod mypod; use the 'kubectl.kubernetes.io/default-container' annotation
		# for selecting the container to be attached or the first container in the pod will be chosen
		kubectl attach mypod

		# Get output from ruby-container from pod mypod
		kubectl attach mypod -c ruby-container

		# Switch to raw terminal mode; sends stdin to 'bash' in ruby-container from pod mypod
		# and sends stdout/stderr from 'bash' back to the client
		kubectl attach mypod -c ruby-container -i -t

		# Get output from the first pod of a replica set named nginx
		kubectl attach rs/nginx
		`))
)

const (
	defaultPodAttachTimeout = 60 * time.Second
	defaultPodLogsTimeout   = 20 * time.Second
)

// AttachOptions declare the arguments accepted by the Attach command
type AttachOptions struct {
	exec.StreamOptions

	// whether to disable use of standard error when streaming output from tty
	DisableStderr bool

	CommandName string

	Pod *corev1.Pod

	AttachFunc       func(*AttachOptions, *corev1.Container, bool, remotecommand.TerminalSizeQueue) func() error
	Resources        []string
	Builder          func() *resource.Builder
	AttachablePodFn  polymorphichelpers.AttachablePodForObjectFunc
	restClientGetter genericclioptions.RESTClientGetter

	Attach        RemoteAttach
	GetPodTimeout time.Duration
	Config        *restclient.Config
}

// NewAttachOptions creates the options for attach
func NewAttachOptions(streams genericiooptions.IOStreams) *AttachOptions {
	return &AttachOptions{
		StreamOptions: exec.StreamOptions{
			IOStreams: streams,
		},
		Attach:     &DefaultRemoteAttach{},
		AttachFunc: DefaultAttachFunc,
	}
}

// NewCmdAttach returns the attach Cobra command
func NewCmdAttach(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	o := NewAttachOptions(streams)
	cmd := &cobra.Command{
		Use:                   "attach (POD | TYPE/NAME) -c CONTAINER",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Attach to a running container"),
		Long:                  i18n.T("Attach to a process that is already running inside an existing container."),
		Example:               attachExample,
		ValidArgsFunction:     completion.PodResourceNameCompletionFunc(f),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}
	cmdutil.AddPodRunningTimeoutFlag(cmd, defaultPodAttachTimeout)
	cmdutil.AddContainerVarFlags(cmd, &o.ContainerName, o.ContainerName)
	cmd.Flags().BoolVarP(&o.Stdin, "stdin", "i", o.Stdin, "Pass stdin to the container")
	cmd.Flags().BoolVarP(&o.TTY, "tty", "t", o.TTY, "Stdin is a TTY")
	cmd.Flags().BoolVarP(&o.Quiet, "quiet", "q", o.Quiet, "Only print output from the remote session")
	return cmd
}

// RemoteAttach defines the interface accepted by the Attach command - provided for test stubbing
type RemoteAttach interface {
	Attach(url *url.URL, config *restclient.Config, stdin io.Reader, stdout, stderr io.Writer, tty bool, terminalSizeQueue remotecommand.TerminalSizeQueue) error
}

// DefaultAttachFunc is the default AttachFunc used
func DefaultAttachFunc(o *AttachOptions, containerToAttach *corev1.Container, raw bool, sizeQueue remotecommand.TerminalSizeQueue) func() error {
	return func() error {
		restClient, err := restclient.RESTClientFor(o.Config)
		if err != nil {
			return err
		}
		req := restClient.Post().
			Resource("pods").
			Name(o.Pod.Name).
			Namespace(o.Pod.Namespace).
			SubResource("attach")
		req.VersionedParams(&corev1.PodAttachOptions{
			Container: containerToAttach.Name,
			Stdin:     o.Stdin,
			Stdout:    o.Out != nil,
			Stderr:    !o.DisableStderr,
			TTY:       raw,
		}, scheme.ParameterCodec)

		return o.Attach.Attach(req.URL(), o.Config, o.In, o.Out, o.ErrOut, raw, sizeQueue)
	}
}

// DefaultRemoteAttach is the standard implementation of attaching
type DefaultRemoteAttach struct{}

// Attach executes attach to a running container
func (*DefaultRemoteAttach) Attach(url *url.URL, config *restclient.Config, stdin io.Reader, stdout, stderr io.Writer, tty bool, terminalSizeQueue remotecommand.TerminalSizeQueue) error {
	exec, err := createExecutor(url, config)
	if err != nil {
		return err
	}
	return exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		Tty:               tty,
		TerminalSizeQueue: terminalSizeQueue,
	})
}

// createExecutor returns the Executor or an error if one occurred.
func createExecutor(url *url.URL, config *restclient.Config) (remotecommand.Executor, error) {
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", url)
	if err != nil {
		return nil, err
	}
	// Fallback executor is default, unless feature flag is explicitly disabled.
	if !cmdutil.RemoteCommandWebsockets.IsDisabled() {
		// WebSocketExecutor must be "GET" method as described in RFC 6455 Sec. 4.1 (page 17).
		websocketExec, err := remotecommand.NewWebSocketExecutor(config, "GET", url.String())
		if err != nil {
			return nil, err
		}
		exec, err = remotecommand.NewFallbackExecutor(websocketExec, exec, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
		if err != nil {
			return nil, err
		}
	}
	return exec, nil
}

// Complete verifies command line arguments and loads data from the command environment
func (o *AttachOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.AttachablePodFn = polymorphichelpers.AttachablePodForObjectFn

	o.GetPodTimeout, err = cmdutil.GetPodRunningTimeoutFlag(cmd)
	if err != nil {
		return cmdutil.UsageErrorf(cmd, "%s", err.Error())
	}

	o.Builder = f.NewBuilder
	o.Resources = args
	o.restClientGetter = f

	config, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.Config = config

	if o.CommandName == "" {
		o.CommandName = cmd.CommandPath()
	}

	return nil
}

// Validate checks that the provided attach options are specified.
func (o *AttachOptions) Validate() error {
	if len(o.Resources) == 0 {
		return fmt.Errorf("at least 1 argument is required for attach")
	}
	if len(o.Resources) > 2 {
		return fmt.Errorf("expected POD, TYPE/NAME, or TYPE NAME, (at most 2 arguments) saw %d: %v", len(o.Resources), o.Resources)
	}
	if o.GetPodTimeout <= 0 {
		return fmt.Errorf("--pod-running-timeout must be higher than zero")
	}

	return nil
}

// Run executes a validated remote execution against a pod.
func (o *AttachOptions) Run() error {
	if o.Pod == nil {
		b := o.Builder().
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			NamespaceParam(o.Namespace).DefaultNamespace()

		switch len(o.Resources) {
		case 1:
			b.ResourceNames("pods", o.Resources[0])
		case 2:
			b.ResourceNames(o.Resources[0], o.Resources[1])
		}

		obj, err := b.Do().Object()
		if err != nil {
			return err
		}

		o.Pod, err = o.findAttachablePod(obj)
		if err != nil {
			return err
		}

		if o.Pod.Status.Phase == corev1.PodSucceeded || o.Pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("cannot attach a container in a completed pod; current phase is %s", o.Pod.Status.Phase)
		}
		// TODO: convert this to a clean "wait" behavior
	}

	// check for TTY
	containerToAttach, err := o.containerToAttachTo(o.Pod)
	if err != nil {
		return fmt.Errorf("cannot attach to the container: %v", err)
	}
	if o.TTY && !containerToAttach.TTY {
		o.TTY = false
		if !o.Quiet && o.ErrOut != nil {
			fmt.Fprintf(o.ErrOut, "error: Unable to use a TTY - container %s did not allocate one\n", containerToAttach.Name)
		}
	} else if !o.TTY && containerToAttach.TTY {
		// the container was launched with a TTY, so we have to force a TTY here, otherwise you'll get
		// an error "Unrecognized input header"
		o.TTY = true
	}

	// ensure we can recover the terminal while attached
	t := o.SetupTTY()

	var sizeQueue remotecommand.TerminalSizeQueue
	if t.Raw {
		if size := t.GetSize(); size != nil {
			// fake resizing +1 and then back to normal so that attach-detach-reattach will result in the
			// screen being redrawn
			sizePlusOne := *size
			sizePlusOne.Width++
			sizePlusOne.Height++

			// this call spawns a goroutine to monitor/update the terminal size
			sizeQueue = t.MonitorSize(&sizePlusOne, size)
		}

		o.DisableStderr = true
	}

	if !o.Quiet {
		fmt.Fprintln(o.ErrOut, "All commands and output executed when attached to a container will be visible via pods/log.")
		fmt.Fprintln(o.ErrOut, "If you don't see a command prompt, try pressing enter.")
	}
	if err := t.Safe(o.AttachFunc(o, containerToAttach, t.Raw, sizeQueue)); err != nil {
		return err
	}

	if msg := o.reattachMessage(containerToAttach.Name, t.Raw); msg != "" {
		fmt.Fprintln(o.Out, msg)
	}
	return nil
}

func (o *AttachOptions) findAttachablePod(obj runtime.Object) (*corev1.Pod, error) {
	attachablePod, err := o.AttachablePodFn(o.restClientGetter, obj, o.GetPodTimeout)
	if err != nil {
		return nil, err
	}

	o.StreamOptions.PodName = attachablePod.Name
	return attachablePod, nil
}

// containerToAttach returns a reference to the container to attach to, given by name.
// use the kubectl.kubernetes.io/default-container annotation for selecting the container to be attached
// or the first container in the pod will be chosen If name is empty.
func (o *AttachOptions) containerToAttachTo(pod *corev1.Pod) (*corev1.Container, error) {
	return podcmd.FindOrDefaultContainerByName(pod, o.ContainerName, o.Quiet, o.ErrOut)
}

// GetContainerName returns the name of the container to attach to, with a fallback.
func (o *AttachOptions) GetContainerName(pod *corev1.Pod) (string, error) {
	c, err := o.containerToAttachTo(pod)
	if err != nil {
		return "", err
	}
	return c.Name, nil
}

// reattachMessage returns a message to print after attach has completed, or
// the empty string if no message should be printed.
func (o *AttachOptions) reattachMessage(containerName string, rawTTY bool) string {
	if o.Quiet || !o.Stdin || !rawTTY || o.Pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		return ""
	}
	if _, path := podcmd.FindContainerByName(o.Pod, containerName); strings.HasPrefix(path, "spec.ephemeralContainers") {
		return fmt.Sprintf("Session ended, the ephemeral container will not be restarted but may be reattached using '%s %s -c %s -i -t' if it is still running", o.CommandName, o.Pod.Name, containerName)
	}
	return fmt.Sprintf("Session ended, resume using '%s %s -c %s -i -t' command when the pod is running", o.CommandName, o.Pod.Name, containerName)
}
