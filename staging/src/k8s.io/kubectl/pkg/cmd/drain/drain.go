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

package drain

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/klog/v2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/drain"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/util/term"
)

type DrainCmdOptions struct {
	PrintFlags *genericclioptions.PrintFlags
	ToPrinter  func(string) (printers.ResourcePrinterFunc, error)

	Namespace string

	drainer   *drain.Helper
	nodeInfos []*resource.Info

	genericiooptions.IOStreams
	WarningPrinter *printers.WarningPrinter
}

var (
	cordonLong = templates.LongDesc(i18n.T(`
		Mark node as unschedulable.`))

	cordonExample = templates.Examples(i18n.T(`
		# Mark node "foo" as unschedulable
		kubectl cordon foo`))
)

func NewCmdCordon(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewDrainCmdOptions(f, ioStreams)

	cmd := &cobra.Command{
		Use:                   "cordon NODE",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Mark node as unschedulable"),
		Long:                  cordonLong,
		Example:               cordonExample,
		ValidArgsFunction:     completion.ResourceNameCompletionFunc(f, "node"),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.RunCordonOrUncordon(true))
		},
	}
	cmdutil.AddLabelSelectorFlagVar(cmd, &o.drainer.Selector)
	cmdutil.AddDryRunFlag(cmd)
	return cmd
}

var (
	uncordonLong = templates.LongDesc(i18n.T(`
		Mark node as schedulable.`))

	uncordonExample = templates.Examples(i18n.T(`
		# Mark node "foo" as schedulable
		kubectl uncordon foo`))
)

func NewCmdUncordon(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewDrainCmdOptions(f, ioStreams)

	cmd := &cobra.Command{
		Use:                   "uncordon NODE",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Mark node as schedulable"),
		Long:                  uncordonLong,
		Example:               uncordonExample,
		ValidArgsFunction:     completion.ResourceNameCompletionFunc(f, "node"),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.RunCordonOrUncordon(false))
		},
	}
	cmdutil.AddLabelSelectorFlagVar(cmd, &o.drainer.Selector)
	cmdutil.AddDryRunFlag(cmd)
	return cmd
}

var (
	drainLong = templates.LongDesc(i18n.T(`
		Drain node in preparation for maintenance.

		The given node will be marked unschedulable to prevent new pods from arriving.
		'drain' evicts the pods if the API server supports
		[eviction](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/). Otherwise, it will use normal
		DELETE to delete the pods.
		The 'drain' evicts or deletes all pods except mirror pods (which cannot be deleted through
		the API server).  If there are daemon set-managed pods, drain will not proceed
		without --ignore-daemonsets, and regardless it will not delete any
		daemon set-managed pods, because those pods would be immediately replaced by the
		daemon set controller, which ignores unschedulable markings.  If there are any
		pods that are neither mirror pods nor managed by a replication controller,
		replica set, daemon set, stateful set, or job, then drain will not delete any pods unless you
		use --force.  --force will also allow deletion to proceed if the managing resource of one
		or more pods is missing.

		'drain' waits for graceful termination. You should not operate on the machine until
		the command completes.

		When you are ready to put the node back into service, use kubectl uncordon, which
		will make the node schedulable again.

		![Workflow](https://kubernetes.io/images/docs/kubectl_drain.svg)`))

	drainExample = templates.Examples(i18n.T(`
		# Drain node "foo", even if there are pods not managed by a replication controller, replica set, job, daemon set, or stateful set on it
		kubectl drain foo --force

		# As above, but abort if there are pods not managed by a replication controller, replica set, job, daemon set, or stateful set, and use a grace period of 15 minutes
		kubectl drain foo --grace-period=900`))
)

func NewDrainCmdOptions(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *DrainCmdOptions {
	o := &DrainCmdOptions{
		PrintFlags: genericclioptions.NewPrintFlags("drained").WithTypeSetter(scheme.Scheme),
		IOStreams:  ioStreams,
		drainer: &drain.Helper{
			GracePeriodSeconds: -1,
			Out:                ioStreams.Out,
			ErrOut:             ioStreams.ErrOut,
			ChunkSize:          cmdutil.DefaultChunkSize,
		},
	}
	o.drainer.OnPodDeletionOrEvictionFinished = o.onPodDeletionOrEvictionFinished
	o.drainer.OnPodDeletionOrEvictionStarted = o.onPodDeletionOrEvictionStarted
	return o
}

// onPodDeletionOrEvictionFinished is called by drain.Helper, when eviction/deletetion of the pod is finished
func (o *DrainCmdOptions) onPodDeletionOrEvictionFinished(pod *corev1.Pod, usingEviction bool, err error) {
	var verbStr string
	if usingEviction {
		if err != nil {
			verbStr = "eviction failed"
		} else {
			verbStr = "evicted"
		}
	} else {
		if err != nil {
			verbStr = "deletion failed"
		} else {
			verbStr = "deleted"
		}
	}
	printObj, err := o.ToPrinter(verbStr)
	if err != nil {
		fmt.Fprintf(o.ErrOut, "error building printer: %v\n", err)
		fmt.Fprintf(o.Out, "pod %s/%s %s\n", pod.Namespace, pod.Name, verbStr)
	} else {
		_ = printObj(pod, o.Out)
	}
}

// onPodDeletionOrEvictionStarted is called by drain.Helper, when eviction/deletion of the pod is started
func (o *DrainCmdOptions) onPodDeletionOrEvictionStarted(pod *corev1.Pod, usingEviction bool) {
	if !klog.V(2).Enabled() {
		return
	}
	var verbStr string
	if usingEviction {
		verbStr = "eviction started"
	} else {
		verbStr = "deletion started"
	}
	printObj, err := o.ToPrinter(verbStr)
	if err != nil {
		fmt.Fprintf(o.ErrOut, "error building printer: %v\n", err)
		fmt.Fprintf(o.Out, "pod %s/%s %s\n", pod.Namespace, pod.Name, verbStr)
	} else {
		_ = printObj(pod, o.Out)
	}
}

func NewCmdDrain(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewDrainCmdOptions(f, ioStreams)

	cmd := &cobra.Command{
		Use:                   "drain NODE",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Drain node in preparation for maintenance"),
		Long:                  drainLong,
		Example:               drainExample,
		ValidArgsFunction:     completion.ResourceNameCompletionFunc(f, "node"),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.RunDrain())
		},
	}
	cmd.Flags().BoolVar(&o.drainer.Force, "force", o.drainer.Force, "Continue even if there are pods that do not declare a controller.")
	cmd.Flags().BoolVar(&o.drainer.IgnoreAllDaemonSets, "ignore-daemonsets", o.drainer.IgnoreAllDaemonSets, "Ignore DaemonSet-managed pods.")
	cmd.Flags().BoolVar(&o.drainer.DeleteEmptyDirData, "delete-emptydir-data", o.drainer.DeleteEmptyDirData, "Continue even if there are pods using emptyDir (local data that will be deleted when the node is drained).")
	cmd.Flags().IntVar(&o.drainer.GracePeriodSeconds, "grace-period", o.drainer.GracePeriodSeconds, "Period of time in seconds given to each pod to terminate gracefully. If negative, the default value specified in the pod will be used.")
	cmd.Flags().DurationVar(&o.drainer.Timeout, "timeout", o.drainer.Timeout, "The length of time to wait before giving up, zero means infinite")
	cmd.Flags().StringVarP(&o.drainer.PodSelector, "pod-selector", "", o.drainer.PodSelector, "Label selector to filter pods on the node")
	cmd.Flags().BoolVar(&o.drainer.DisableEviction, "disable-eviction", o.drainer.DisableEviction, "Force drain to use delete, even if eviction is supported. This will bypass checking PodDisruptionBudgets, use with caution.")
	cmd.Flags().IntVar(&o.drainer.SkipWaitForDeleteTimeoutSeconds, "skip-wait-for-delete-timeout", o.drainer.SkipWaitForDeleteTimeoutSeconds, "If pod DeletionTimestamp older than N seconds, skip waiting for the pod.  Seconds must be greater than 0 to skip.")
	cmd.Flags().IntVar(&o.drainer.DelayBetweenNodeSeconds, "delay-between-nodes", o.drainer.DelayBetweenNodeSeconds, "When more than one node is targeted (e.g. via labels) this is the period of seconds to wait after each node completes draining. For workloads without PDBs this can be used as a grace period allowing replacements to become ready before all nodes they reside on are drained.")

	cmdutil.AddChunkSizeFlag(cmd, &o.drainer.ChunkSize)
	cmdutil.AddDryRunFlag(cmd)
	cmdutil.AddLabelSelectorFlagVar(cmd, &o.drainer.Selector)
	return cmd
}

// Complete populates some fields from the factory, grabs command line
// arguments and looks up the node using Builder
func (o *DrainCmdOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error

	if len(args) == 0 && !cmd.Flags().Changed("selector") {
		return cmdutil.UsageErrorf(cmd, "USAGE: %s [flags]", cmd.Use)
	}
	if len(args) > 0 && len(o.drainer.Selector) > 0 {
		return cmdutil.UsageErrorf(cmd, "error: cannot specify both a node name and a --selector option")
	}

	o.drainer.DryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}

	if o.drainer.Client, err = f.KubernetesClientSet(); err != nil {
		return err
	}

	if len(o.drainer.PodSelector) > 0 {
		if _, err := labels.Parse(o.drainer.PodSelector); err != nil {
			return errors.New("--pod-selector=<pod_selector> must be a valid label selector")
		}
	}

	o.nodeInfos = []*resource.Info{}

	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.ToPrinter = func(operation string) (printers.ResourcePrinterFunc, error) {
		o.PrintFlags.NamePrintFlags.Operation = operation
		cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.drainer.DryRunStrategy)

		printer, err := o.PrintFlags.ToPrinter()
		if err != nil {
			return nil, err
		}

		return printer.PrintObj, nil
	}

	// Set default WarningPrinter if not already set.
	if o.WarningPrinter == nil {
		o.WarningPrinter = printers.NewWarningPrinter(o.ErrOut, printers.WarningPrinterOptions{Color: term.AllowsColorOutput(o.ErrOut)})
	}

	builder := f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(o.Namespace).DefaultNamespace().
		RequestChunksOf(o.drainer.ChunkSize).
		ResourceNames("nodes", args...).
		SingleResourceType().
		Flatten()

	if len(o.drainer.Selector) > 0 {
		builder = builder.LabelSelectorParam(o.drainer.Selector).
			ResourceTypes("nodes")
	}

	r := builder.Do()

	if err = r.Err(); err != nil {
		return err
	}

	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		if info.Mapping.Resource.GroupResource() != (schema.GroupResource{Group: "", Resource: "nodes"}) {
			return fmt.Errorf("error: expected resource of type node, got %q", info.Mapping.Resource)
		}

		o.nodeInfos = append(o.nodeInfos, info)
		return nil
	})
}

// RunDrain runs the 'drain' command
func (o *DrainCmdOptions) RunDrain() error {
	if err := o.RunCordonOrUncordon(true); err != nil {
		return err
	}

	drainedNodes := sets.New[string]()
	var fatal []error

	remainingNodes := []string{}
	nodeCount := len(o.nodeInfos)
	for idx, info := range o.nodeInfos {
		if err := o.deleteOrEvictPodsSimple(info); err == nil {
			drainedNodes.Insert(info.Name)

			printObj, err := o.ToPrinter("drained")
			if err != nil {
				return err
			}

			printObj(info.Object, o.Out)
		} else {
			fmt.Fprintf(o.ErrOut, "error: unable to drain node %q due to error: %s, continuing command...\n", info.Name, err)

			if !drainedNodes.Has(info.Name) {
				fatal = append(fatal, err)
				remainingNodes = append(remainingNodes, info.Name)
			}

			continue
		}

		if o.drainer.DelayBetweenNodeSeconds > 0 && idx+1 < nodeCount {
			fmt.Fprintf(o.Out, "Waiting %d seconds before draining next node...\n", o.drainer.DelayBetweenNodeSeconds)
			time.Sleep(time.Duration(o.drainer.DelayBetweenNodeSeconds) * time.Second)
		}
	}

	if len(remainingNodes) > 0 {
		fmt.Fprintf(o.ErrOut, "There are pending nodes to be drained:\n")
		for _, nodeName := range remainingNodes {
			fmt.Fprintf(o.ErrOut, " %s\n", nodeName)
		}
	}

	return utilerrors.NewAggregate(fatal)
}

func (o *DrainCmdOptions) deleteOrEvictPodsSimple(nodeInfo *resource.Info) error {
	list, errs := o.drainer.GetPodsForDeletion(nodeInfo.Name)
	if errs != nil {
		return utilerrors.NewAggregate(errs)
	}
	if warnings := list.Warnings(); warnings != "" {
		o.WarningPrinter.Print(warnings)
	}
	if o.drainer.DryRunStrategy == cmdutil.DryRunClient {
		for _, pod := range list.Pods() {
			fmt.Fprintf(o.Out, "evicting pod %s/%s (dry run)\n", pod.Namespace, pod.Name)
		}
		return nil
	}

	if err := o.drainer.DeleteOrEvictPods(list.Pods()); err != nil {
		pendingList, newErrs := o.drainer.GetPodsForDeletion(nodeInfo.Name)
		if pendingList != nil {
			pods := pendingList.Pods()
			if len(pods) != 0 {
				fmt.Fprintf(o.ErrOut, "There are pending pods in node %q when an error occurred: %v\n", nodeInfo.Name, err)
				for _, pendingPod := range pods {
					fmt.Fprintf(o.ErrOut, "%s/%s\n", "pod", pendingPod.Name)
				}
			}
		}
		if newErrs != nil {
			fmt.Fprintf(o.ErrOut, "Following errors occurred while getting the list of pods to delete:\n%s", utilerrors.NewAggregate(newErrs))
		}
		return err
	}
	return nil
}

// RunCordonOrUncordon runs either Cordon or Uncordon.  The desired value for
// "Unschedulable" is passed as the first arg.
func (o *DrainCmdOptions) RunCordonOrUncordon(desired bool) error {
	cordonOrUncordon := "cordon"
	if !desired {
		cordonOrUncordon = "un" + cordonOrUncordon
	}

	for _, nodeInfo := range o.nodeInfos {

		printError := func(err error) {
			fmt.Fprintf(o.ErrOut, "error: unable to %s node %q: %v\n", cordonOrUncordon, nodeInfo.Name, err)
		}

		gvk := nodeInfo.ResourceMapping().GroupVersionKind
		if gvk.Kind == "Node" {
			c, err := drain.NewCordonHelperFromRuntimeObject(nodeInfo.Object, scheme.Scheme, gvk)
			if err != nil {
				printError(err)
				continue
			}

			if updateRequired := c.UpdateIfRequired(desired); !updateRequired {
				printObj, err := o.ToPrinter(already(desired))
				if err != nil {
					fmt.Fprintf(o.ErrOut, "error: %v\n", err)
					continue
				}
				printObj(nodeInfo.Object, o.Out)
			} else {
				if o.drainer.DryRunStrategy != cmdutil.DryRunClient {
					err, patchErr := c.PatchOrReplace(o.drainer.Client, o.drainer.DryRunStrategy == cmdutil.DryRunServer)
					if patchErr != nil {
						printError(patchErr)
					}
					if err != nil {
						printError(err)
						continue
					}
				}
				printObj, err := o.ToPrinter(changed(desired))
				if err != nil {
					fmt.Fprintf(o.ErrOut, "%v\n", err)
					continue
				}
				printObj(nodeInfo.Object, o.Out)
			}
		} else {
			printObj, err := o.ToPrinter("skipped")
			if err != nil {
				fmt.Fprintf(o.ErrOut, "%v\n", err)
				continue
			}
			printObj(nodeInfo.Object, o.Out)
		}
	}

	return nil
}

// already() and changed() return suitable strings for {un,}cordoning

func already(desired bool) string {
	if desired {
		return "already cordoned"
	}
	return "already uncordoned"
}

func changed(desired bool) string {
	if desired {
		return "cordoned"
	}
	return "uncordoned"
}
