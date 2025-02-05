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

package delete

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	cmdwait "k8s.io/kubectl/pkg/cmd/wait"
	"k8s.io/kubectl/pkg/rawhttp"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/util/term"
)

var (
	deleteLong = templates.LongDesc(i18n.T(`
		Delete resources by file names, stdin, resources and names, or by resources and label selector.

		JSON and YAML formats are accepted. Only one type of argument may be specified: file names,
		resources and names, or resources and label selector.

		Some resources, such as pods, support graceful deletion. These resources define a default period
		before they are forcibly terminated (the grace period) but you may override that value with
		the --grace-period flag, or pass --now to set a grace-period of 1. Because these resources often
		represent entities in the cluster, deletion may not be acknowledged immediately. If the node
		hosting a pod is down or cannot reach the API server, termination may take significantly longer
		than the grace period. To force delete a resource, you must specify the --force flag.
		Note: only a subset of resources support graceful deletion. In absence of the support,
		the --grace-period flag is ignored.

		IMPORTANT: Force deleting pods does not wait for confirmation that the pod's processes have been
		terminated, which can leave those processes running until the node detects the deletion and
		completes graceful deletion. If your processes use shared storage or talk to a remote API and
		depend on the name of the pod to identify themselves, force deleting those pods may result in
		multiple processes running on different machines using the same identification which may lead
		to data corruption or inconsistency. Only force delete pods when you are sure the pod is
		terminated, or if your application can tolerate multiple copies of the same pod running at once.
		Also, if you force delete pods, the scheduler may place new pods on those nodes before the node
		has released those resources and causing those pods to be evicted immediately.

		Note that the delete command does NOT do resource version checks, so if someone submits an
		update to a resource right when you submit a delete, their update will be lost along with the
		rest of the resource.

		After a CustomResourceDefinition is deleted, invalidation of discovery cache may take up
		to 6 hours. If you don't want to wait, you might want to run "kubectl api-resources" to refresh
		the discovery cache.`))

	deleteExample = templates.Examples(i18n.T(`
		# Delete a pod using the type and name specified in pod.json
		kubectl delete -f ./pod.json

		# Delete resources from a directory containing kustomization.yaml - e.g. dir/kustomization.yaml
		kubectl delete -k dir

		# Delete resources from all files that end with '.json'
		kubectl delete -f '*.json'

		# Delete a pod based on the type and name in the JSON passed into stdin
		cat pod.json | kubectl delete -f -

		# Delete pods and services with same names "baz" and "foo"
		kubectl delete pod,service baz foo

		# Delete pods and services with label name=myLabel
		kubectl delete pods,services -l name=myLabel

		# Delete a pod with minimal delay
		kubectl delete pod foo --now

		# Force delete a pod on a dead node
		kubectl delete pod foo --force

		# Delete all pods
		kubectl delete pods --all

		# Delete all pods only if the user confirms the deletion
		kubectl delete pods --all --interactive`))
)

type DeleteOptions struct {
	resource.FilenameOptions

	LabelSelector       string
	FieldSelector       string
	DeleteAll           bool
	DeleteAllNamespaces bool
	CascadingStrategy   metav1.DeletionPropagation
	IgnoreNotFound      bool
	DeleteNow           bool
	ForceDeletion       bool
	WaitForDeletion     bool
	Quiet               bool
	WarnClusterScope    bool
	Raw                 string
	Interactive         bool
	Concurrent          bool

	GracePeriod int
	Timeout     time.Duration

	DryRunStrategy cmdutil.DryRunStrategy

	Output string

	DynamicClient      dynamic.Interface
	Mapper             meta.RESTMapper
	Result             *resource.Result
	PreviewResult      *resource.Result
	previewResourceMap map[cmdwait.ResourceLocation]struct{}

	genericiooptions.IOStreams
	WarningPrinter *printers.WarningPrinter
}

func NewCmdDelete(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	deleteFlags := NewDeleteCommandFlags("containing the resource to delete.")

	cmd := &cobra.Command{
		Use:                   "delete ([-f FILENAME] | [-k DIRECTORY] | TYPE [(NAME | -l label | --all)])",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Delete resources by file names, stdin, resources and names, or by resources and label selector"),
		Long:                  deleteLong,
		Example:               deleteExample,
		ValidArgsFunction:     completion.ResourceTypeAndNameCompletionFunc(f),
		Run: func(cmd *cobra.Command, args []string) {
			o, err := deleteFlags.ToOptions(nil, streams)
			cmdutil.CheckErr(err)
			cmdutil.CheckErr(o.Complete(f, args, cmd))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunDelete(f))
		},
		SuggestFor: []string{"rm"},
	}

	deleteFlags.AddFlags(cmd)
	cmdutil.AddDryRunFlag(cmd)

	return cmd
}

func (o *DeleteOptions) Complete(f cmdutil.Factory, args []string, cmd *cobra.Command) error {
	cmdNamespace, enforceNamespace, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.WarnClusterScope = enforceNamespace && !o.DeleteAllNamespaces

	if o.DeleteAll || len(o.LabelSelector) > 0 || len(o.FieldSelector) > 0 {
		if f := cmd.Flags().Lookup("ignore-not-found"); f != nil && !f.Changed {
			// If the user didn't explicitly set the option, default to ignoring NotFound errors when used with --all, -l, or --field-selector
			o.IgnoreNotFound = true
		}
	}
	if o.DeleteNow {
		if o.GracePeriod != -1 {
			return fmt.Errorf("--now and --grace-period cannot be specified together")
		}
		o.GracePeriod = 1
	}
	if o.GracePeriod == 0 && !o.ForceDeletion {
		// To preserve backwards compatibility, but prevent accidental data loss, we convert --grace-period=0
		// into --grace-period=1. Users may provide --force to bypass this conversion.
		o.GracePeriod = 1
	}
	if o.ForceDeletion && o.GracePeriod < 0 {
		o.GracePeriod = 0
	}

	o.DryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}

	// Set default WarningPrinter if not already set.
	if o.WarningPrinter == nil {
		o.WarningPrinter = printers.NewWarningPrinter(o.ErrOut, printers.WarningPrinterOptions{Color: term.AllowsColorOutput(o.ErrOut)})
	}

	if len(o.Raw) != 0 {
		return nil
	}

	r := f.NewBuilder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, &o.FilenameOptions).
		LabelSelectorParam(o.LabelSelector).
		FieldSelectorParam(o.FieldSelector).
		SelectAllParam(o.DeleteAll).
		AllNamespaces(o.DeleteAllNamespaces).
		ResourceTypeOrNameArgs(false, args...).RequireObject(false).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}
	o.Result = r

	if o.Interactive {
		// preview result will be used to list resources for confirmation prior to actual delete.
		// We can not use r as result object because it can only be used once. But we need to traverse
		// twice. Parameters in preview result must be equal to genuine result.
		previewr := f.NewBuilder().
			Unstructured().
			ContinueOnError().
			NamespaceParam(cmdNamespace).DefaultNamespace().
			FilenameParam(enforceNamespace, &o.FilenameOptions).
			LabelSelectorParam(o.LabelSelector).
			FieldSelectorParam(o.FieldSelector).
			SelectAllParam(o.DeleteAll).
			AllNamespaces(o.DeleteAllNamespaces).
			ResourceTypeOrNameArgs(false, args...).RequireObject(false).
			Flatten().
			Do()
		err = previewr.Err()
		if err != nil {
			return err
		}
		o.PreviewResult = previewr
		o.previewResourceMap = make(map[cmdwait.ResourceLocation]struct{})
	}

	o.Mapper, err = f.ToRESTMapper()
	if err != nil {
		return err
	}

	o.DynamicClient, err = f.DynamicClient()
	if err != nil {
		return err
	}

	return nil
}

func (o *DeleteOptions) Validate() error {
	if o.Output != "" && o.Output != "name" {
		return fmt.Errorf("unexpected -o output mode: %v. We only support '-o name'", o.Output)
	}

	if o.DeleteAll && len(o.LabelSelector) > 0 {
		return fmt.Errorf("cannot set --all and --selector at the same time")
	}
	if o.DeleteAll && len(o.FieldSelector) > 0 {
		return fmt.Errorf("cannot set --all and --field-selector at the same time")
	}
	if o.WarningPrinter == nil {
		return fmt.Errorf("WarningPrinter can not be used without initialization")
	}
	if o.Concurrent && o.Interactive {
		return fmt.Errorf("cannot set --concurrent and --interactive at the same time")
	}

	switch {
	case o.GracePeriod == 0 && o.ForceDeletion:
		o.WarningPrinter.Print("Immediate deletion does not wait for confirmation that the running resource has been terminated. The resource may continue to run on the cluster indefinitely.")
	case o.GracePeriod > 0 && o.ForceDeletion:
		return fmt.Errorf("--force and --grace-period greater than 0 cannot be specified together")
	}

	if len(o.Raw) == 0 {
		return nil
	}

	if o.Interactive {
		return fmt.Errorf("--interactive can not be used with --raw")
	}
	if len(o.FilenameOptions.Filenames) > 1 {
		return fmt.Errorf("--raw can only use a single local file or stdin")
	} else if len(o.FilenameOptions.Filenames) == 1 {
		if strings.Index(o.FilenameOptions.Filenames[0], "http://") == 0 || strings.Index(o.FilenameOptions.Filenames[0], "https://") == 0 {
			return fmt.Errorf("--raw cannot read from a url")
		}
	}

	if o.FilenameOptions.Recursive {
		return fmt.Errorf("--raw and --recursive are mutually exclusive")
	}
	if len(o.Output) > 0 {
		return fmt.Errorf("--raw and --output are mutually exclusive")
	}
	if _, err := url.ParseRequestURI(o.Raw); err != nil {
		return fmt.Errorf("--raw must be a valid URL path: %v", err)
	}
	return nil
}

func (o *DeleteOptions) RunDelete(f cmdutil.Factory) error {
	if len(o.Raw) > 0 {
		restClient, err := f.RESTClient()
		if err != nil {
			return err
		}
		if len(o.Filenames) == 0 {
			return rawhttp.RawDelete(restClient, o.IOStreams, o.Raw, "")
		}
		return rawhttp.RawDelete(restClient, o.IOStreams, o.Raw, o.Filenames[0])
	}

	if o.Interactive {
		previewInfos := []*resource.Info{}
		if o.IgnoreNotFound {
			o.PreviewResult = o.PreviewResult.IgnoreErrors(apierrors.IsNotFound)
		}
		err := o.PreviewResult.Visit(func(info *resource.Info, err error) error {
			if err != nil {
				return err
			}
			previewInfos = append(previewInfos, info)
			o.previewResourceMap[cmdwait.ResourceLocation{
				GroupResource: info.Mapping.Resource.GroupResource(),
				Namespace:     info.Namespace,
				Name:          info.Name,
			}] = struct{}{}

			return nil
		})
		if err != nil {
			return err
		}
		if len(previewInfos) == 0 {
			fmt.Fprintf(o.Out, "No resources found\n")
			return nil
		}

		if !o.confirmation(previewInfos) {
			fmt.Fprintf(o.Out, "deletion is cancelled\n")
			return nil
		}
	}

	return o.DeleteResult(o.Result)
}

func (o *DeleteOptions) DeleteResult(r *resource.Result) error {
	found := 0
	if o.IgnoreNotFound {
		r = r.IgnoreErrors(apierrors.IsNotFound)
	}
	if o.Concurrent {
		o.WarningPrinter.Print("concurrent deletion of resources, not guaranteed to be in order")
	}
	warnClusterScope := o.WarnClusterScope
	deletedInfos := []*resource.Info{}
	uidMap := cmdwait.UIDMap{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errMutex sync.Mutex
	var errorList []error
	visitFunc := func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		if o.Concurrent {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := o.processResource(info, &mu, &deletedInfos, &found, &warnClusterScope, uidMap); err != nil {
					errMutex.Lock()
					errorList = append(errorList, err)
					errMutex.Unlock()
				}
			}()
		} else {
			if err := o.processResource(info, &mu, &deletedInfos, &found, &warnClusterScope, uidMap); err != nil {
				return err
			}
		}
		return nil
	}
	err := r.Visit(visitFunc)
	if o.Concurrent {
		wg.Wait()
		if len(errorList) > 0 {
			mergedError := errors.Join(errorList...)
			return mergedError
		}
	}
	if err != nil {
		return err
	}
	if found == 0 {
		fmt.Fprintf(o.Out, "No resources found\n")
		return nil
	}
	if !o.WaitForDeletion {
		return nil
	}
	// if we don't have a dynamic client, we don't want to wait.  Eventually when delete is cleaned up, this will likely
	// drop out.
	if o.DynamicClient == nil {
		return nil
	}

	// If we are dry-running, then we don't want to wait
	if o.DryRunStrategy != cmdutil.DryRunNone {
		return nil
	}

	effectiveTimeout := o.Timeout
	if effectiveTimeout == 0 {
		// if we requested to wait forever, set it to a week.
		effectiveTimeout = 168 * time.Hour
	}
	waitOptions := cmdwait.WaitOptions{
		ResourceFinder: genericclioptions.ResourceFinderForResult(resource.InfoListVisitor(deletedInfos)),
		UIDMap:         uidMap,
		DynamicClient:  o.DynamicClient,
		Timeout:        effectiveTimeout,

		Printer:     printers.NewDiscardingPrinter(),
		ConditionFn: cmdwait.IsDeleted,
		IOStreams:   o.IOStreams,
	}
	err = waitOptions.RunWait()
	if apierrors.IsForbidden(err) || apierrors.IsMethodNotSupported(err) {
		// if we're forbidden from waiting, we shouldn't fail.
		// if the resource doesn't support a verb we need, we shouldn't fail.
		klog.V(1).Info(err)
		return nil
	}
	return err
}

func (o *DeleteOptions) processResource(info *resource.Info, mu *sync.Mutex, deletedInfos *[]*resource.Info, found *int, warnClusterScope *bool, uidMap cmdwait.UIDMap) error {
	if o.Interactive {
		if _, ok := o.previewResourceMap[cmdwait.ResourceLocation{
			GroupResource: info.Mapping.Resource.GroupResource(),
			Namespace:     info.Namespace,
			Name:          info.Name,
		}]; !ok {
			// resource not in the list of previewed resources based on resourceLocation
			return nil
		}
	}
	mu.Lock()
	*deletedInfos = append(*deletedInfos, info)
	*found++
	mu.Unlock()
	options := &metav1.DeleteOptions{}
	if o.GracePeriod >= 0 {
		options = metav1.NewDeleteOptions(int64(o.GracePeriod))
	}
	options.PropagationPolicy = &o.CascadingStrategy

	if *warnClusterScope && info.Mapping.Scope.Name() == meta.RESTScopeNameRoot {
		o.WarningPrinter.Print("deleting cluster-scoped resources, not scoped to the provided namespace")
		*warnClusterScope = false
	}
	if o.DryRunStrategy == cmdutil.DryRunClient {
		if !o.Quiet {
			o.PrintObj(info)
		}
		return nil
	}
	response, err := o.deleteResource(info, options)
	if err != nil {
		return err
	}
	resourceLocation := cmdwait.ResourceLocation{
		GroupResource: info.Mapping.Resource.GroupResource(),
		Namespace:     info.Namespace,
		Name:          info.Name,
	}
	mu.Lock()
	defer mu.Unlock()
	if status, ok := response.(*metav1.Status); ok && status.Details != nil {
		uidMap[resourceLocation] = status.Details.UID
	} else {
		responseMetadata, err := meta.Accessor(response)
		if err != nil {
			return err
		} else {
			uidMap[resourceLocation] = responseMetadata.GetUID()
		}
	}
	return nil
}

func (o *DeleteOptions) deleteResource(info *resource.Info, deleteOptions *metav1.DeleteOptions) (runtime.Object, error) {
	deleteResponse, err := resource.
		NewHelper(info.Client, info.Mapping).
		DryRun(o.DryRunStrategy == cmdutil.DryRunServer).
		DeleteWithOptions(info.Namespace, info.Name, deleteOptions)
	if err != nil {
		return nil, cmdutil.AddSourceToErr("deleting", info.Source, err)
	}

	if !o.Quiet {
		o.PrintObj(info)
	}
	return deleteResponse, nil
}

// PrintObj for deleted objects is special because we do not have an object to print.
// This mirrors name printer behavior
func (o *DeleteOptions) PrintObj(info *resource.Info) {
	operation := "deleted"
	groupKind := info.Mapping.GroupVersionKind
	kindString := fmt.Sprintf("%s.%s", strings.ToLower(groupKind.Kind), groupKind.Group)
	if len(groupKind.Group) == 0 {
		kindString = strings.ToLower(groupKind.Kind)
	}

	if o.GracePeriod == 0 {
		operation = "force deleted"
	}

	switch o.DryRunStrategy {
	case cmdutil.DryRunClient:
		operation = fmt.Sprintf("%s (dry run)", operation)
	case cmdutil.DryRunServer:
		operation = fmt.Sprintf("%s (server dry run)", operation)
	}

	if o.Output == "name" {
		// -o name: prints resource/name
		fmt.Fprintf(o.Out, "%s/%s\n", kindString, info.Name)
		return
	}

	// understandable output by default
	fmt.Fprintf(o.Out, "%s \"%s\" %s\n", kindString, info.Name, operation)
}

func (o *DeleteOptions) confirmation(infos []*resource.Info) bool {
	fmt.Fprintf(o.Out, i18n.T("You are about to delete the following %d resource(s):\n"), len(infos))
	for _, info := range infos {
		groupKind := info.Mapping.GroupVersionKind
		kindString := fmt.Sprintf("%s.%s", strings.ToLower(groupKind.Kind), groupKind.Group)
		if len(groupKind.Group) == 0 {
			kindString = strings.ToLower(groupKind.Kind)
		}

		fmt.Fprintf(o.Out, "%s/%s\n", kindString, info.Name)
	}
	fmt.Fprint(o.Out, i18n.T("Do you want to continue?")+" (y/n): ")
	var input string
	_, err := fmt.Fscan(o.In, &input)
	if err != nil {
		return false
	}

	return strings.EqualFold(input, "y")
}
