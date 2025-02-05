/*
Copyright 2020 The Kubernetes Authors.

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

package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/distribution/reference"
	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd/attach"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/logs"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/interrupt"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/util/term"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

var (
	debugLong = templates.LongDesc(i18n.T(`
		Debug cluster resources using interactive debugging containers.

		'debug' provides automation for common debugging tasks for cluster objects identified by
		resource and name. Pods will be used by default if no resource is specified.

		The action taken by 'debug' varies depending on what resource is specified. Supported
		actions include:

		* Workload: Create a copy of an existing pod with certain attributes changed,
	                for example changing the image tag to a new version.
		* Workload: Add an ephemeral container to an already running pod, for example to add
		            debugging utilities without restarting the pod.
		* Node: Create a new pod that runs in the node's host namespaces and can access
		        the node's filesystem.
`))

	debugExample = templates.Examples(i18n.T(`
		# Create an interactive debugging session in pod mypod and immediately attach to it.
		kubectl debug mypod -it --image=busybox

		# Create an interactive debugging session for the pod in the file pod.yaml and immediately attach to it.
		# (requires the EphemeralContainers feature to be enabled in the cluster)
		kubectl debug -f pod.yaml -it --image=busybox

		# Create a debug container named debugger using a custom automated debugging image.
		kubectl debug --image=myproj/debug-tools -c debugger mypod

		# Create a copy of mypod adding a debug container and attach to it
		kubectl debug mypod -it --image=busybox --copy-to=my-debugger

		# Create a copy of mypod changing the command of mycontainer
		kubectl debug mypod -it --copy-to=my-debugger --container=mycontainer -- sh

		# Create a copy of mypod changing all container images to busybox
		kubectl debug mypod --copy-to=my-debugger --set-image=*=busybox

		# Create a copy of mypod adding a debug container and changing container images
		kubectl debug mypod -it --copy-to=my-debugger --image=debian --set-image=app=app:debug,sidecar=sidecar:debug

		# Create an interactive debugging session on a node and immediately attach to it.
		# The container will run in the host namespaces and the host's filesystem will be mounted at /host
		kubectl debug node/mynode -it --image=busybox
`))
)

var nameSuffixFunc = utilrand.String

type DebugAttachFunc func(ctx context.Context, restClientGetter genericclioptions.RESTClientGetter, cmdPath string, ns, podName, containerName string) error

// DebugOptions holds the options for an invocation of kubectl debug.
type DebugOptions struct {
	Args               []string
	ArgsOnly           bool
	Attach             bool
	AttachFunc         DebugAttachFunc
	Container          string
	CopyTo             string
	Replace            bool
	Env                []corev1.EnvVar
	Image              string
	Interactive        bool
	KeepLabels         bool
	KeepAnnotations    bool
	KeepLiveness       bool
	KeepReadiness      bool
	KeepStartup        bool
	KeepInitContainers bool
	Namespace          string
	TargetNames        []string
	PullPolicy         corev1.PullPolicy
	Quiet              bool
	SameNode           bool
	SetImages          map[string]string
	ShareProcesses     bool
	TargetContainer    string
	TTY                bool
	Profile            string
	CustomProfileFile  string
	CustomProfile      *corev1.Container
	Applier            ProfileApplier

	explicitNamespace     bool
	attachChanged         bool
	shareProcessedChanged bool

	podClient corev1client.CoreV1Interface

	Builder *resource.Builder
	genericiooptions.IOStreams
	WarningPrinter *printers.WarningPrinter

	resource.FilenameOptions
}

// NewDebugOptions returns a DebugOptions initialized with default values.
func NewDebugOptions(streams genericiooptions.IOStreams) *DebugOptions {
	return &DebugOptions{
		Args:               []string{},
		IOStreams:          streams,
		KeepInitContainers: true,
		TargetNames:        []string{},
		ShareProcesses:     true,
	}
}

// NewCmdDebug returns a cobra command that runs kubectl debug.
func NewCmdDebug(restClientGetter genericclioptions.RESTClientGetter, streams genericiooptions.IOStreams) *cobra.Command {
	o := NewDebugOptions(streams)

	cmd := &cobra.Command{
		Use:                   "debug (POD | TYPE[[.VERSION].GROUP]/NAME) [ -- COMMAND [args...] ]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Create debugging sessions for troubleshooting workloads and nodes"),
		Long:                  debugLong,
		Example:               debugExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(restClientGetter, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run(restClientGetter, cmd))
		},
	}

	o.AddFlags(cmd)
	return cmd
}

func (o *DebugOptions) AddFlags(cmd *cobra.Command) {
	cmdutil.AddJsonFilenameFlag(cmd.Flags(), &o.FilenameOptions.Filenames, "identifying the resource to debug")

	cmd.Flags().BoolVar(&o.ArgsOnly, "arguments-only", o.ArgsOnly, i18n.T("If specified, everything after -- will be passed to the new container as Args instead of Command."))
	cmd.Flags().BoolVar(&o.Attach, "attach", o.Attach, i18n.T("If true, wait for the container to start running, and then attach as if 'kubectl attach ...' were called.  Default false, unless '-i/--stdin' is set, in which case the default is true."))
	cmd.Flags().StringVarP(&o.Container, "container", "c", o.Container, i18n.T("Container name to use for debug container."))
	cmd.Flags().StringVar(&o.CopyTo, "copy-to", o.CopyTo, i18n.T("Create a copy of the target Pod with this name."))
	cmd.Flags().BoolVar(&o.Replace, "replace", o.Replace, i18n.T("When used with '--copy-to', delete the original Pod."))
	cmd.Flags().StringToString("env", nil, i18n.T("Environment variables to set in the container."))
	cmd.Flags().StringVar(&o.Image, "image", o.Image, i18n.T("Container image to use for debug container."))
	cmd.Flags().BoolVar(&o.KeepLabels, "keep-labels", o.KeepLabels, i18n.T("If true, keep the original pod labels.(This flag only works when used with '--copy-to')"))
	cmd.Flags().BoolVar(&o.KeepAnnotations, "keep-annotations", o.KeepAnnotations, i18n.T("If true, keep the original pod annotations.(This flag only works when used with '--copy-to')"))
	cmd.Flags().BoolVar(&o.KeepLiveness, "keep-liveness", o.KeepLiveness, i18n.T("If true, keep the original pod liveness probes.(This flag only works when used with '--copy-to')"))
	cmd.Flags().BoolVar(&o.KeepReadiness, "keep-readiness", o.KeepReadiness, i18n.T("If true, keep the original pod readiness probes.(This flag only works when used with '--copy-to')"))
	cmd.Flags().BoolVar(&o.KeepStartup, "keep-startup", o.KeepStartup, i18n.T("If true, keep the original startup probes.(This flag only works when used with '--copy-to')"))
	cmd.Flags().BoolVar(&o.KeepInitContainers, "keep-init-containers", o.KeepInitContainers, i18n.T("Run the init containers for the pod. Defaults to true.(This flag only works when used with '--copy-to')"))
	cmd.Flags().StringToStringVar(&o.SetImages, "set-image", o.SetImages, i18n.T("When used with '--copy-to', a list of name=image pairs for changing container images, similar to how 'kubectl set image' works."))
	cmd.Flags().String("image-pull-policy", "", i18n.T("The image pull policy for the container. If left empty, this value will not be specified by the client and defaulted by the server."))
	cmd.Flags().BoolVarP(&o.Interactive, "stdin", "i", o.Interactive, i18n.T("Keep stdin open on the container(s) in the pod, even if nothing is attached."))
	cmd.Flags().BoolVarP(&o.Quiet, "quiet", "q", o.Quiet, i18n.T("If true, suppress informational messages."))
	cmd.Flags().BoolVar(&o.SameNode, "same-node", o.SameNode, i18n.T("When used with '--copy-to', schedule the copy of target Pod on the same node."))
	cmd.Flags().BoolVar(&o.ShareProcesses, "share-processes", o.ShareProcesses, i18n.T("When used with '--copy-to', enable process namespace sharing in the copy."))
	cmd.Flags().StringVar(&o.TargetContainer, "target", "", i18n.T("When using an ephemeral container, target processes in this container name."))
	cmd.Flags().BoolVarP(&o.TTY, "tty", "t", o.TTY, i18n.T("Allocate a TTY for the debugging container."))
	cmd.Flags().StringVar(&o.Profile, "profile", ProfileLegacy, i18n.T(`Options are "legacy", "general", "baseline", "netadmin", "restricted" or "sysadmin".`))
	cmd.Flags().StringVar(&o.CustomProfileFile, "custom", o.CustomProfileFile, i18n.T("Path to a JSON or YAML file containing a partial container spec to customize built-in debug profiles."))
}

// Complete finishes run-time initialization of debug.DebugOptions.
func (o *DebugOptions) Complete(restClientGetter genericclioptions.RESTClientGetter, cmd *cobra.Command, args []string) error {
	var err error

	o.PullPolicy = corev1.PullPolicy(cmdutil.GetFlagString(cmd, "image-pull-policy"))

	// Arguments
	argsLen := cmd.ArgsLenAtDash()
	o.TargetNames = args
	// If there is a dash and there are args after the dash, extract the args.
	if argsLen >= 0 && len(args) > argsLen {
		o.TargetNames, o.Args = args[:argsLen], args[argsLen:]
	}

	// Attach
	attachFlag := cmd.Flags().Lookup("attach")
	if !attachFlag.Changed && o.Interactive {
		o.Attach = true
	}

	// Downstream tools may want to use their own customized
	// attach function to do extra work or use attach command
	// with different flags instead of the static one defined in
	// handleAttachPod. But if this function is not set explicitly,
	// we fall back to default.
	if o.AttachFunc == nil {
		o.AttachFunc = o.handleAttachPod
	}

	// Environment
	envStrings, err := cmd.Flags().GetStringToString("env")
	if err != nil {
		return fmt.Errorf("internal error getting env flag: %v", err)
	}
	for k, v := range envStrings {
		o.Env = append(o.Env, corev1.EnvVar{Name: k, Value: v})
	}

	// Namespace
	o.Namespace, o.explicitNamespace, err = restClientGetter.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	// Record flags that the user explicitly changed from their defaults
	o.attachChanged = cmd.Flags().Changed("attach")
	o.shareProcessedChanged = cmd.Flags().Changed("share-processes")

	// Set default WarningPrinter
	if o.WarningPrinter == nil {
		o.WarningPrinter = printers.NewWarningPrinter(o.ErrOut, printers.WarningPrinterOptions{Color: term.AllowsColorOutput(o.ErrOut)})
	}

	if o.Applier == nil {
		kflags := KeepFlags{
			Labels:         o.KeepLabels,
			Annotations:    o.KeepAnnotations,
			Liveness:       o.KeepLiveness,
			Readiness:      o.KeepReadiness,
			Startup:        o.KeepStartup,
			InitContainers: o.KeepInitContainers,
		}
		applier, err := NewProfileApplier(o.Profile, kflags)
		if err != nil {
			return err
		}
		o.Applier = applier
	}

	if o.CustomProfileFile != "" {
		customProfileBytes, err := os.ReadFile(o.CustomProfileFile)
		if err != nil {
			return fmt.Errorf("must pass a container spec json file for custom profile: %w", err)
		}

		err = json.Unmarshal(customProfileBytes, &o.CustomProfile)
		if err != nil {
			err = yaml.Unmarshal(customProfileBytes, &o.CustomProfile)
			if err != nil {
				return fmt.Errorf("%s does not contain a valid container spec: %w", o.CustomProfileFile, err)
			}
		}
	}

	clientConfig, err := restClientGetter.ToRESTConfig()
	if err != nil {
		return err
	}

	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	o.podClient = client.CoreV1()

	o.Builder = resource.NewBuilder(restClientGetter)

	return nil
}

// Validate checks that the provided debug options are specified.
func (o *DebugOptions) Validate() error {
	// Attach
	if o.Attach && o.attachChanged && len(o.Image) == 0 && len(o.Container) == 0 {
		return fmt.Errorf("you must specify --container or create a new container using --image in order to attach.")
	}

	// CopyTo
	if len(o.CopyTo) > 0 {
		if len(o.Image) == 0 && len(o.SetImages) == 0 && len(o.Args) == 0 {
			return fmt.Errorf("you must specify --image, --set-image or command arguments.")
		}
		if len(o.Args) > 0 && len(o.Container) == 0 && len(o.Image) == 0 {
			return fmt.Errorf("you must specify an existing container or a new image when specifying args.")
		}
	} else {
		// These flags are exclusive to --copy-to
		switch {
		case o.Replace:
			return fmt.Errorf("--replace may only be used with --copy-to.")
		case o.SameNode:
			return fmt.Errorf("--same-node may only be used with --copy-to.")
		case len(o.SetImages) > 0:
			return fmt.Errorf("--set-image may only be used with --copy-to.")
		case len(o.Image) == 0:
			return fmt.Errorf("you must specify --image when not using --copy-to.")
		}
	}

	// Image
	if len(o.Image) > 0 && !reference.ReferenceRegexp.MatchString(o.Image) {
		return fmt.Errorf("invalid image name %q: %v", o.Image, reference.ErrReferenceInvalidFormat)
	}

	// Name
	if len(o.TargetNames) == 0 && len(o.FilenameOptions.Filenames) == 0 {
		return fmt.Errorf("NAME or filename is required for debug")
	}

	// Pull Policy
	switch o.PullPolicy {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever, "":
		// continue
	default:
		return fmt.Errorf("invalid image pull policy: %s", o.PullPolicy)
	}

	// SetImages
	for name, image := range o.SetImages {
		if !reference.ReferenceRegexp.MatchString(image) {
			return fmt.Errorf("invalid image name %q for container %q: %v", image, name, reference.ErrReferenceInvalidFormat)
		}
	}

	// TargetContainer
	if len(o.TargetContainer) > 0 {
		if len(o.CopyTo) > 0 {
			return fmt.Errorf("--target is incompatible with --copy-to. Use --share-processes instead.")
		}
		if !o.Quiet {
			fmt.Fprintf(o.Out, "Targeting container %q. If you don't see processes from this container it may be because the container runtime doesn't support this feature.\n", o.TargetContainer)
			// TODO(verb): Add a list of supported container runtimes to https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/ and then link here.
		}
	}

	// TTY
	if o.TTY && !o.Interactive {
		return fmt.Errorf("-i/--stdin is required for containers with -t/--tty=true")
	}

	// WarningPrinter
	if o.WarningPrinter == nil {
		return fmt.Errorf("WarningPrinter can not be used without initialization")
	}

	if o.CustomProfile != nil {
		if o.CustomProfile.Name != "" || len(o.CustomProfile.Command) > 0 || o.CustomProfile.Image != "" || o.CustomProfile.Lifecycle != nil || len(o.CustomProfile.VolumeDevices) > 0 {
			return fmt.Errorf("name, command, image, lifecycle and volume devices are not modifiable via custom profile")
		}
	}

	// Warning for legacy profile
	if o.Profile == ProfileLegacy {
		fmt.Fprintln(o.ErrOut, `--profile=legacy is deprecated and will be removed in the future. It is recommended to explicitly specify a profile, for example "--profile=general".`)
	}

	return nil
}

// Run executes a kubectl debug.
func (o *DebugOptions) Run(restClientGetter genericclioptions.RESTClientGetter, cmd *cobra.Command) error {
	ctx := context.Background()

	r := o.Builder.
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		FilenameParam(o.explicitNamespace, &o.FilenameOptions).
		NamespaceParam(o.Namespace).DefaultNamespace().ResourceNames("pods", o.TargetNames...).
		Do()
	if err := r.Err(); err != nil {
		return err
	}

	err := r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			// TODO(verb): configurable early return
			return err
		}

		var (
			debugPod      *corev1.Pod
			containerName string
			visitErr      error
		)
		switch obj := info.Object.(type) {
		case *corev1.Node:
			debugPod, containerName, visitErr = o.visitNode(ctx, obj)
		case *corev1.Pod:
			debugPod, containerName, visitErr = o.visitPod(ctx, obj)
		default:
			visitErr = fmt.Errorf("%q not supported by debug", info.Mapping.GroupVersionKind)
		}
		if visitErr != nil {
			return visitErr
		}

		if o.Attach && len(containerName) > 0 && o.AttachFunc != nil {
			if err := o.AttachFunc(ctx, restClientGetter, cmd.Parent().CommandPath(), debugPod.Namespace, debugPod.Name, containerName); err != nil {
				return err
			}
		}

		return nil
	})

	return err
}

// visitNode handles debugging for node targets by creating a privileged pod running in the host namespaces.
// Returns an already created pod and container name for subsequent attach, if applicable.
func (o *DebugOptions) visitNode(ctx context.Context, node *corev1.Node) (*corev1.Pod, string, error) {
	pods := o.podClient.Pods(o.Namespace)
	debugPod, err := o.generateNodeDebugPod(node)
	if err != nil {
		return nil, "", err
	}
	newPod, err := pods.Create(ctx, debugPod, metav1.CreateOptions{})
	if err != nil {
		return nil, "", err
	}

	return newPod, newPod.Spec.Containers[0].Name, nil
}

// visitPod handles debugging for pod targets by (depending on options):
//  1. Creating an ephemeral debug container in an existing pod, OR
//  2. Making a copy of pod with certain attributes changed
//
// visitPod returns a pod and debug container name for subsequent attach, if applicable.
func (o *DebugOptions) visitPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, string, error) {
	// Display warning message if some capabilities are set by profile and non-root user is specified in .Spec.SecurityContext.RunAsUser.(#1650)
	if o.Profile == ProfileGeneral || o.Profile == ProfileNetadmin || o.Profile == ProfileSysadmin ||
		(o.CustomProfile != nil && o.CustomProfile.SecurityContext != nil &&
			((o.CustomProfile.SecurityContext.Privileged != nil && *o.CustomProfile.SecurityContext.Privileged) ||
				(o.CustomProfile.SecurityContext.Capabilities != nil && len(o.CustomProfile.SecurityContext.Capabilities.Add) != 0))) {
		if pod.Spec.SecurityContext.RunAsUser != nil && *pod.Spec.SecurityContext.RunAsUser != 0 {
			if o.CustomProfile == nil ||
				(o.CustomProfile.SecurityContext.RunAsUser == nil || *o.CustomProfile.SecurityContext.RunAsUser != 0) {
				fmt.Fprintln(o.ErrOut, `Warning: Non-root user is specified for the entire target Pod and some capabilities granted by debug profile may not work. Please consider using "--custom" with the custom profile specifying "securityContext.runAsUser: 0".`)
			}
		}
	}
	if len(o.CopyTo) > 0 {
		return o.debugByCopy(ctx, pod)
	}
	return o.debugByEphemeralContainer(ctx, pod)
}

// debugByEphemeralContainer runs an EphemeralContainer in the target Pod for use as a debug container
func (o *DebugOptions) debugByEphemeralContainer(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, string, error) {
	klog.V(2).Infof("existing ephemeral containers: %v", pod.Spec.EphemeralContainers)
	podJS, err := json.Marshal(pod)
	if err != nil {
		return nil, "", fmt.Errorf("error creating JSON for pod: %v", err)
	}

	debugPod, debugContainer, err := o.generateDebugContainer(pod)
	if err != nil {
		return nil, "", err
	}
	klog.V(2).Infof("new ephemeral container: %#v", debugContainer)

	debugJS, err := json.Marshal(debugPod)
	if err != nil {
		return nil, "", fmt.Errorf("error creating JSON for debug container: %v", err)
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(podJS, debugJS, pod)
	if err != nil {
		return nil, "", fmt.Errorf("error creating patch to add debug container: %v", err)
	}
	klog.V(2).Infof("generated strategic merge patch for debug container: %s", patch)

	pods := o.podClient.Pods(pod.Namespace)
	result, err := pods.Patch(ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "ephemeralcontainers")
	if err != nil {
		// The apiserver will return a 404 when the EphemeralContainers feature is disabled because the `/ephemeralcontainers` subresource
		// is missing. Unlike the 404 returned by a missing pod, the status details will be empty.
		if serr, ok := err.(*errors.StatusError); ok && serr.Status().Reason == metav1.StatusReasonNotFound && serr.ErrStatus.Details.Name == "" {
			return nil, "", fmt.Errorf("ephemeral containers are disabled for this cluster (error from server: %q)", err)
		}

		return nil, "", err
	}

	return result, debugContainer.Name, nil
}

// applyCustomProfile applies given partial container json file on to the profile
// incorporated debug pod.
func (o *DebugOptions) applyCustomProfile(debugPod *corev1.Pod, containerName string) error {
	o.CustomProfile.Name = containerName
	customJS, err := json.Marshal(o.CustomProfile)
	if err != nil {
		return fmt.Errorf("unable to marshall custom profile: %w", err)
	}

	var index int
	found := false
	for i, val := range debugPod.Spec.Containers {
		if val.Name == containerName {
			index = i
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("unable to find the %s container in the pod %s", containerName, debugPod.Name)
	}

	var debugContainerJS []byte
	debugContainerJS, err = json.Marshal(debugPod.Spec.Containers[index])
	if err != nil {
		return fmt.Errorf("unable to marshall container: %w", err)
	}

	patchedContainer, err := strategicpatch.StrategicMergePatch(debugContainerJS, customJS, corev1.Container{})
	if err != nil {
		return fmt.Errorf("error creating three way patch to add debug container: %w", err)
	}

	err = json.Unmarshal(patchedContainer, &debugPod.Spec.Containers[index])
	if err != nil {
		return fmt.Errorf("unable to unmarshall patched container to container: %w", err)
	}

	return nil
}

// applyCustomProfileEphemeral applies given partial container json file on to the profile
// incorporated ephemeral container of the pod.
func (o *DebugOptions) applyCustomProfileEphemeral(debugPod *corev1.Pod, containerName string) error {
	o.CustomProfile.Name = containerName
	customJS, err := json.Marshal(o.CustomProfile)
	if err != nil {
		return fmt.Errorf("unable to marshall custom profile: %w", err)
	}

	var index int
	found := false
	for i, val := range debugPod.Spec.EphemeralContainers {
		if val.Name == containerName {
			index = i
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("unable to find the %s ephemeral container in the pod %s", containerName, debugPod.Name)
	}

	var debugContainerJS []byte
	debugContainerJS, err = json.Marshal(debugPod.Spec.EphemeralContainers[index])
	if err != nil {
		return fmt.Errorf("unable to marshall ephemeral container:%w", err)
	}

	patchedContainer, err := strategicpatch.StrategicMergePatch(debugContainerJS, customJS, corev1.Container{})
	if err != nil {
		return fmt.Errorf("error creating three way patch to add debug container: %w", err)
	}

	err = json.Unmarshal(patchedContainer, &debugPod.Spec.EphemeralContainers[index])
	if err != nil {
		return fmt.Errorf("unable to unmarshall patched container to ephemeral container: %w", err)
	}

	return nil
}

// debugByCopy runs a copy of the target Pod with a debug container added or an original container modified
func (o *DebugOptions) debugByCopy(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, string, error) {
	copied, dc, err := o.generatePodCopyWithDebugContainer(pod)
	if err != nil {
		return nil, "", err
	}
	created, err := o.podClient.Pods(copied.Namespace).Create(ctx, copied, metav1.CreateOptions{})
	if err != nil {
		return nil, "", err
	}
	if o.Replace {
		err := o.podClient.Pods(pod.Namespace).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		if err != nil {
			return nil, "", err
		}
	}
	return created, dc, nil
}

// generateDebugContainer returns a debugging pod and an EphemeralContainer suitable for use as a debug container
// in the given pod.
func (o *DebugOptions) generateDebugContainer(pod *corev1.Pod) (*corev1.Pod, *corev1.EphemeralContainer, error) {
	name := o.computeDebugContainerName(pod)
	ec := &corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:                     name,
			Env:                      o.Env,
			Image:                    o.Image,
			ImagePullPolicy:          o.PullPolicy,
			Stdin:                    o.Interactive,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			TTY:                      o.TTY,
		},
		TargetContainerName: o.TargetContainer,
	}

	if o.ArgsOnly {
		ec.Args = o.Args
	} else {
		ec.Command = o.Args
	}

	copied := pod.DeepCopy()
	copied.Spec.EphemeralContainers = append(copied.Spec.EphemeralContainers, *ec)
	if err := o.Applier.Apply(copied, name, copied); err != nil {
		return nil, nil, err
	}

	if o.CustomProfile != nil {
		err := o.applyCustomProfileEphemeral(copied, ec.Name)
		if err != nil {
			return nil, nil, err
		}
	}

	ec = &copied.Spec.EphemeralContainers[len(copied.Spec.EphemeralContainers)-1]

	return copied, ec, nil
}

// generateNodeDebugPod generates a debugging pod that schedules on the specified node.
// The generated pod will run in the host PID, Network & IPC namespaces, and it will have the node's filesystem mounted at /host.
func (o *DebugOptions) generateNodeDebugPod(node *corev1.Node) (*corev1.Pod, error) {
	cn := "debugger"
	// Setting a user-specified container name doesn't make much difference when there's only one container,
	// but the argument exists for pod debugging so it might be confusing if it didn't work here.
	if len(o.Container) > 0 {
		cn = o.Container
	}

	// The name of the debugging pod is based on the target node, and it's not configurable to
	// limit the number of command line flags. There may be a collision on the name, but this
	// should be rare enough that it's not worth the API round trip to check.
	pn := fmt.Sprintf("node-debugger-%s-%s", node.Name, nameSuffixFunc(5))
	if !o.Quiet {
		fmt.Fprintf(o.Out, "Creating debugging pod %s with container %s on node %s.\n", pn, cn, node.Name)
	}

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: pn,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:                     cn,
					Env:                      o.Env,
					Image:                    o.Image,
					ImagePullPolicy:          o.PullPolicy,
					Stdin:                    o.Interactive,
					TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					TTY:                      o.TTY,
				},
			},
			NodeName:      node.Name,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{
				{
					Operator: corev1.TolerationOpExists,
				},
			},
		},
	}

	if o.ArgsOnly {
		p.Spec.Containers[0].Args = o.Args
	} else {
		p.Spec.Containers[0].Command = o.Args
	}

	if err := o.Applier.Apply(p, cn, node); err != nil {
		return nil, err
	}

	if o.CustomProfile != nil {
		err := o.applyCustomProfile(p, cn)
		if err != nil {
			return nil, err
		}
	}

	return p, nil
}

// generatePodCopyWithDebugContainer takes a Pod and returns a copy and the debug container name of that copy
func (o *DebugOptions) generatePodCopyWithDebugContainer(pod *corev1.Pod) (*corev1.Pod, string, error) {
	copied := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        o.CopyTo,
			Namespace:   pod.Namespace,
			Annotations: pod.Annotations,
			Labels:      pod.Labels,
		},
		Spec: *pod.Spec.DeepCopy(),
	}
	// set EphemeralContainers to nil so that the copy of pod can be created
	copied.Spec.EphemeralContainers = nil
	// change ShareProcessNamespace configuration only when commanded explicitly
	if o.shareProcessedChanged {
		copied.Spec.ShareProcessNamespace = ptr.To(o.ShareProcesses)
	}
	if !o.SameNode {
		copied.Spec.NodeName = ""
	}

	// Apply image mutations
	for i, c := range copied.Spec.Containers {
		override := o.SetImages["*"]
		if img, ok := o.SetImages[c.Name]; ok {
			override = img
		}
		if len(override) > 0 {
			copied.Spec.Containers[i].Image = override
		}
	}

	name, containerByName := o.Container, containerNameToRef(copied)

	c, ok := containerByName[name]
	if !ok {
		// Adding a new debug container
		if len(o.Image) == 0 {
			if len(o.SetImages) > 0 {
				// This was a --set-image only invocation
				return copied, "", nil
			}
			return nil, "", fmt.Errorf("you must specify image when creating new container")
		}

		if len(name) == 0 {
			name = o.computeDebugContainerName(copied)
		}
		copied.Spec.Containers = append(copied.Spec.Containers, corev1.Container{
			Name:                     name,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		})
		c = &copied.Spec.Containers[len(copied.Spec.Containers)-1]
	}

	if len(o.Args) > 0 {
		if o.ArgsOnly {
			c.Args = o.Args
		} else {
			c.Command = o.Args
			c.Args = nil
		}
	}
	if len(o.Env) > 0 {
		c.Env = o.Env
	}
	if len(o.Image) > 0 {
		c.Image = o.Image
	}
	if len(o.PullPolicy) > 0 {
		c.ImagePullPolicy = o.PullPolicy
	}
	c.Stdin = o.Interactive
	c.TTY = o.TTY

	err := o.Applier.Apply(copied, c.Name, pod)
	if err != nil {
		return nil, "", err
	}

	if o.CustomProfile != nil {
		err = o.applyCustomProfile(copied, name)
		if err != nil {
			return nil, "", err
		}
	}

	return copied, name, nil
}

func (o *DebugOptions) computeDebugContainerName(pod *corev1.Pod) string {
	if len(o.Container) > 0 {
		return o.Container
	}

	cn, containerByName := "", containerNameToRef(pod)
	for len(cn) == 0 || (containerByName[cn] != nil) {
		cn = fmt.Sprintf("debugger-%s", nameSuffixFunc(5))
	}
	if !o.Quiet {
		fmt.Fprintf(o.Out, "Defaulting debug container name to %s.\n", cn)
	}
	return cn
}

func containerNameToRef(pod *corev1.Pod) map[string]*corev1.Container {
	names := map[string]*corev1.Container{}
	for i := range pod.Spec.Containers {
		ref := &pod.Spec.Containers[i]
		names[ref.Name] = ref
	}
	for i := range pod.Spec.InitContainers {
		ref := &pod.Spec.InitContainers[i]
		names[ref.Name] = ref
	}
	for i := range pod.Spec.EphemeralContainers {
		ref := (*corev1.Container)(&pod.Spec.EphemeralContainers[i].EphemeralContainerCommon)
		names[ref.Name] = ref
	}
	return names
}

// waitForContainer watches the given pod until the container is running
func (o *DebugOptions) waitForContainer(ctx context.Context, ns, podName, containerName string) (*corev1.Pod, error) {
	// TODO: expose the timeout
	ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx, 0*time.Second)
	defer cancel()

	fieldSelector := fields.OneTermEqualSelector("metadata.name", podName).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = fieldSelector
			return o.podClient.Pods(ns).List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return o.podClient.Pods(ns).Watch(ctx, options)
		},
	}

	intr := interrupt.New(nil, cancel)
	var result *corev1.Pod
	err := intr.Run(func() error {
		ev, err := watchtools.UntilWithSync(ctx, lw, &corev1.Pod{}, nil, func(ev watch.Event) (bool, error) {
			klog.V(2).Infof("watch received event %q with object %T", ev.Type, ev.Object)
			switch ev.Type {
			case watch.Deleted:
				return false, errors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
			}

			p, ok := ev.Object.(*corev1.Pod)
			if !ok {
				return false, fmt.Errorf("watch did not return a pod: %v", ev.Object)
			}

			s := getContainerStatusByName(p, containerName)
			if s == nil {
				return false, nil
			}
			klog.V(2).Infof("debug container status is %v", s)
			if s.State.Running != nil || s.State.Terminated != nil {
				return true, nil
			}
			if !o.Quiet && s.State.Waiting != nil && s.State.Waiting.Message != "" {
				o.WarningPrinter.Print(fmt.Sprintf("container %s: %s", containerName, s.State.Waiting.Message))
			}
			return false, nil
		})
		if ev != nil {
			result = ev.Object.(*corev1.Pod)
		}
		return err
	})

	return result, err
}

func (o *DebugOptions) handleAttachPod(ctx context.Context, restClientGetter genericclioptions.RESTClientGetter, cmdPath string, ns, podName, containerName string) error {
	opts := &attach.AttachOptions{
		StreamOptions: exec.StreamOptions{
			IOStreams: o.IOStreams,
			Stdin:     o.Interactive,
			TTY:       o.TTY,
			Quiet:     o.Quiet,
		},
		CommandName: cmdPath + " attach",

		Attach: &attach.DefaultRemoteAttach{},
	}
	config, err := restClientGetter.ToRESTConfig()
	if err != nil {
		return err
	}
	opts.Config = config
	opts.AttachFunc = attach.DefaultAttachFunc

	pod, err := o.waitForContainer(ctx, ns, podName, containerName)
	if err != nil {
		return err
	}

	opts.Namespace = ns
	opts.Pod = pod
	opts.PodName = podName
	opts.ContainerName = containerName
	if opts.AttachFunc == nil {
		opts.AttachFunc = attach.DefaultAttachFunc
	}

	status := getContainerStatusByName(pod, containerName)
	if status == nil {
		// impossible path
		return fmt.Errorf("error getting container status of container name %q: %+v", containerName, err)
	}
	if status.State.Terminated != nil {
		klog.V(1).Info("Ephemeral container terminated, falling back to logs")
		return logOpts(ctx, restClientGetter, pod, opts)
	}

	if err := opts.Run(); err != nil {
		fmt.Fprintf(opts.ErrOut, "warning: couldn't attach to pod/%s, falling back to streaming logs: %v\n", podName, err)
		return logOpts(ctx, restClientGetter, pod, opts)
	}
	return nil
}

func getContainerStatusByName(pod *corev1.Pod, containerName string) *corev1.ContainerStatus {
	allContainerStatus := [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses}
	for _, statusSlice := range allContainerStatus {
		for i := range statusSlice {
			if statusSlice[i].Name == containerName {
				return &statusSlice[i]
			}
		}
	}
	return nil
}

// logOpts logs output from opts to the pods log.
func logOpts(ctx context.Context, restClientGetter genericclioptions.RESTClientGetter, pod *corev1.Pod, opts *attach.AttachOptions) error {
	ctrName, err := opts.GetContainerName(pod)
	if err != nil {
		return err
	}

	requests, err := polymorphichelpers.LogsForObjectFn(restClientGetter, pod, &corev1.PodLogOptions{Container: ctrName}, opts.GetPodTimeout, false)
	if err != nil {
		return err
	}
	for _, request := range requests {
		if err := logs.DefaultConsumeRequest(ctx, request, opts.Out); err != nil {
			return err
		}
	}

	return nil
}
