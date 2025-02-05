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

package label

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured/unstructuredscheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

const (
	MsgNotLabeled = "not labeled"
	MsgLabeled    = "labeled"
	MsgUnLabeled  = "unlabeled"
)

// LabelOptions have the data required to perform the label operation
type LabelOptions struct {
	// Filename options
	resource.FilenameOptions
	RecordFlags *genericclioptions.RecordFlags

	PrintFlags *genericclioptions.PrintFlags
	ToPrinter  func(string) (printers.ResourcePrinter, error)

	// Common user flags
	overwrite       bool
	list            bool
	local           bool
	dryRunStrategy  cmdutil.DryRunStrategy
	all             bool
	allNamespaces   bool
	resourceVersion string
	selector        string
	fieldSelector   string
	outputFormat    string
	fieldManager    string

	// results of arg parsing
	resources    []string
	newLabels    map[string]string
	removeLabels []string

	Recorder genericclioptions.Recorder

	namespace                    string
	enforceNamespace             bool
	builder                      *resource.Builder
	unstructuredClientForMapping func(mapping *meta.RESTMapping) (resource.RESTClient, error)

	// Common shared fields
	genericiooptions.IOStreams
}

var (
	labelLong = templates.LongDesc(i18n.T(`
		Update the labels on a resource.

		* A label key and value must begin with a letter or number, and may contain letters, numbers, hyphens, dots, and underscores, up to %[1]d characters each.
		* Optionally, the key can begin with a DNS subdomain prefix and a single '/', like example.com/my-app.
		* If --overwrite is true, then existing labels can be overwritten, otherwise attempting to overwrite a label will result in an error.
		* If --resource-version is specified, then updates will use this resource version, otherwise the existing resource-version will be used.`))

	labelExample = templates.Examples(i18n.T(`
		# Update pod 'foo' with the label 'unhealthy' and the value 'true'
		kubectl label pods foo unhealthy=true

		# Update pod 'foo' with the label 'status' and the value 'unhealthy', overwriting any existing value
		kubectl label --overwrite pods foo status=unhealthy

		# Update all pods in the namespace
		kubectl label pods --all status=unhealthy

		# Update a pod identified by the type and name in "pod.json"
		kubectl label -f pod.json status=unhealthy

		# Update pod 'foo' only if the resource is unchanged from version 1
		kubectl label pods foo status=unhealthy --resource-version=1

		# Update pod 'foo' by removing a label named 'bar' if it exists
		# Does not require the --overwrite flag
		kubectl label pods foo bar-`))
)

func NewLabelOptions(ioStreams genericiooptions.IOStreams) *LabelOptions {
	return &LabelOptions{
		RecordFlags: genericclioptions.NewRecordFlags(),
		Recorder:    genericclioptions.NoopRecorder{},

		PrintFlags: genericclioptions.NewPrintFlags("labeled").WithTypeSetter(scheme.Scheme),

		IOStreams: ioStreams,
	}
}

func NewCmdLabel(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewLabelOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "label [--overwrite] (-f FILENAME | TYPE NAME) KEY_1=VAL_1 ... KEY_N=VAL_N [--resource-version=version]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Update the labels on a resource"),
		Long:                  fmt.Sprintf(labelLong, validation.LabelValueMaxLength),
		Example:               labelExample,
		ValidArgsFunction:     completion.ResourceTypeAndNameCompletionFunc(f),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunLabel())
		},
	}

	o.RecordFlags.AddFlags(cmd)
	o.PrintFlags.AddFlags(cmd)

	cmd.Flags().BoolVar(&o.overwrite, "overwrite", o.overwrite, "If true, allow labels to be overwritten, otherwise reject label updates that overwrite existing labels.")
	cmd.Flags().BoolVar(&o.list, "list", o.list, "If true, display the labels for a given resource.")
	cmd.Flags().BoolVar(&o.local, "local", o.local, "If true, label will NOT contact api-server but run locally.")
	cmd.Flags().StringVarP(&o.fieldSelector, "field-selector", "F", o.fieldSelector, "Selector (field query) to filter on, supports '=', '==', and '!='.(e.g. --field-selector key1=value1,key2=value2). The server only supports a limited number of field queries per type.")
	cmd.Flags().BoolVar(&o.all, "all", o.all, "Select all resources, in the namespace of the specified resource types")
	cmd.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", o.allNamespaces, "If true, check the specified action in all namespaces.")
	cmd.Flags().StringVar(&o.resourceVersion, "resource-version", o.resourceVersion, i18n.T("If non-empty, the labels update will only succeed if this is the current resource-version for the object. Only valid when specifying a single resource."))
	usage := "identifying the resource to update the labels"
	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, usage)
	cmdutil.AddDryRunFlag(cmd)
	cmdutil.AddFieldManagerFlagVar(cmd, &o.fieldManager, "kubectl-label")
	cmdutil.AddLabelSelectorFlagVar(cmd, &o.selector)

	return cmd
}

// Complete adapts from the command line args and factory to the data required.
func (o *LabelOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error

	o.RecordFlags.Complete(cmd)
	o.Recorder, err = o.RecordFlags.ToRecorder()
	if err != nil {
		return err
	}

	o.outputFormat = cmdutil.GetFlagString(cmd, "output")
	o.dryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}

	o.ToPrinter = func(operation string) (printers.ResourcePrinter, error) {
		o.PrintFlags.NamePrintFlags.Operation = operation
		// PrintFlagsWithDryRunStrategy must be done after NamePrintFlags.Operation is set
		cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.dryRunStrategy)
		return o.PrintFlags.ToPrinter()
	}

	resources, labelArgs, err := cmdutil.GetResourcesAndPairs(args, "label")
	if err != nil {
		return err
	}
	o.resources = resources
	o.newLabels, o.removeLabels, err = parseLabels(labelArgs)
	if err != nil {
		return err
	}

	if o.list && len(o.outputFormat) > 0 {
		return fmt.Errorf("--list and --output may not be specified together")
	}

	o.namespace, o.enforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil && !(o.local && clientcmd.IsEmptyConfig(err)) {
		return err
	}
	o.builder = f.NewBuilder()
	o.unstructuredClientForMapping = f.UnstructuredClientForMapping

	return nil
}

// Validate checks to the LabelOptions to see if there is sufficient information run the command.
func (o *LabelOptions) Validate() error {
	if o.all && len(o.selector) > 0 {
		return fmt.Errorf("cannot set --all and --selector at the same time")
	}
	if o.all && len(o.fieldSelector) > 0 {
		return fmt.Errorf("cannot set --all and --field-selector at the same time")
	}
	if o.local {
		if o.dryRunStrategy == cmdutil.DryRunServer {
			return fmt.Errorf("cannot specify --local and --dry-run=server - did you mean --dry-run=client?")
		}
		if len(o.resources) > 0 {
			return fmt.Errorf("can only use local files by -f pod.yaml or --filename=pod.json when --local=true is set")
		}
		if cmdutil.IsFilenameSliceEmpty(o.FilenameOptions.Filenames, o.FilenameOptions.Kustomize) {
			return fmt.Errorf("one or more files must be specified as -f pod.yaml or --filename=pod.json")
		}
	} else {
		if len(o.resources) < 1 && cmdutil.IsFilenameSliceEmpty(o.FilenameOptions.Filenames, o.FilenameOptions.Kustomize) {
			return fmt.Errorf("one or more resources must be specified as <resource> <name> or <resource>/<name>")
		}
	}
	if len(o.newLabels) < 1 && len(o.removeLabels) < 1 && !o.list {
		return fmt.Errorf("at least one label update is required")
	}
	return nil
}

// RunLabel does the work
func (o *LabelOptions) RunLabel() error {
	b := o.builder.
		Unstructured().
		LocalParam(o.local).
		ContinueOnError().
		NamespaceParam(o.namespace).DefaultNamespace().
		FilenameParam(o.enforceNamespace, &o.FilenameOptions).
		Flatten()

	if !o.local {
		b = b.LabelSelectorParam(o.selector).
			FieldSelectorParam(o.fieldSelector).
			AllNamespaces(o.allNamespaces).
			ResourceTypeOrNameArgs(o.all, o.resources...).
			Latest()
	}

	one := false
	r := b.Do().IntoSingleItemImplied(&one)
	if err := r.Err(); err != nil {
		return err
	}

	// only apply resource version locking on a single resource
	if !one && len(o.resourceVersion) > 0 {
		return fmt.Errorf("--resource-version may only be used with a single resource")
	}

	// TODO: support bulk generic output a la Get
	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		var outputObj runtime.Object
		var dataChangeMsg string
		obj := info.Object

		if len(o.resourceVersion) != 0 {
			// ensure resourceVersion is always sent in the patch by clearing it from the starting JSON
			accessor, err := meta.Accessor(obj)
			if err != nil {
				return err
			}
			accessor.SetResourceVersion("")
		}

		oldData, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if o.dryRunStrategy == cmdutil.DryRunClient || o.local || o.list {
			err = labelFunc(obj, o.overwrite, o.resourceVersion, o.newLabels, o.removeLabels)
			if err != nil {
				return err
			}
			newObj, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			dataChangeMsg = updateDataChangeMsg(oldData, newObj, o.overwrite)
			outputObj = info.Object
		} else {
			name, namespace := info.Name, info.Namespace
			if err != nil {
				return err
			}
			accessor, err := meta.Accessor(obj)
			if err != nil {
				return err
			}
			for _, label := range o.removeLabels {
				if _, ok := accessor.GetLabels()[label]; !ok {
					fmt.Fprintf(o.Out, "label %q not found.\n", label)
				}
			}

			if err := labelFunc(obj, o.overwrite, o.resourceVersion, o.newLabels, o.removeLabels); err != nil {
				return err
			}
			if err := o.Recorder.Record(obj); err != nil {
				klog.V(4).Infof("error recording current command: %v", err)
			}
			newObj, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			dataChangeMsg = updateDataChangeMsg(oldData, newObj, o.overwrite)
			patchBytes, err := jsonpatch.CreateMergePatch(oldData, newObj)
			createdPatch := err == nil
			if err != nil {
				klog.V(2).Infof("couldn't compute patch: %v", err)
			}

			mapping := info.ResourceMapping()
			client, err := o.unstructuredClientForMapping(mapping)
			if err != nil {
				return err
			}
			helper := resource.NewHelper(client, mapping).
				DryRun(o.dryRunStrategy == cmdutil.DryRunServer).
				WithFieldManager(o.fieldManager)

			if createdPatch {
				outputObj, err = helper.Patch(namespace, name, types.MergePatchType, patchBytes, nil)
			} else {
				outputObj, err = helper.Replace(namespace, name, false, obj)
			}
			if err != nil {
				return err
			}
		}

		if o.list {
			accessor, err := meta.Accessor(outputObj)
			if err != nil {
				return err
			}

			indent := ""
			if !one {
				indent = " "
				gvks, _, err := unstructuredscheme.NewUnstructuredObjectTyper().ObjectKinds(info.Object)
				if err != nil {
					return err
				}
				fmt.Fprintf(o.Out, "Listing labels for %s.%s/%s:\n", gvks[0].Kind, gvks[0].Group, info.Name)
			}
			for k, v := range accessor.GetLabels() {
				fmt.Fprintf(o.Out, "%s%s=%s\n", indent, k, v)
			}

			return nil
		}

		printer, err := o.ToPrinter(dataChangeMsg)
		if err != nil {
			return err
		}
		return printer.PrintObj(info.Object, o.Out)
	})
}

func updateDataChangeMsg(oldObj []byte, newObj []byte, overwrite bool) string {
	msg := MsgNotLabeled
	if !reflect.DeepEqual(oldObj, newObj) {
		msg = MsgLabeled
		if !overwrite && len(newObj) < len(oldObj) {
			msg = MsgUnLabeled
		}
	}
	return msg
}

func validateNoOverwrites(accessor metav1.Object, labels map[string]string) error {
	allErrs := []error{}
	for key, value := range labels {
		if currValue, found := accessor.GetLabels()[key]; found && currValue != value {
			allErrs = append(allErrs, fmt.Errorf("'%s' already has a value (%s), and --overwrite is false", key, currValue))
		}
	}
	return utilerrors.NewAggregate(allErrs)
}

func parseLabels(spec []string) (map[string]string, []string, error) {
	labels := map[string]string{}
	var remove []string
	for _, labelSpec := range spec {
		if strings.Contains(labelSpec, "=") {
			parts := strings.Split(labelSpec, "=")
			if len(parts) != 2 {
				return nil, nil, fmt.Errorf("invalid label spec: %v", labelSpec)
			}
			if errs := validation.IsValidLabelValue(parts[1]); len(errs) != 0 {
				return nil, nil, fmt.Errorf("invalid label value: %q: %s", labelSpec, strings.Join(errs, ";"))
			}
			labels[parts[0]] = parts[1]
		} else if strings.HasSuffix(labelSpec, "-") {
			remove = append(remove, labelSpec[:len(labelSpec)-1])
		} else {
			return nil, nil, fmt.Errorf("unknown label spec: %v", labelSpec)
		}
	}
	for _, removeLabel := range remove {
		if _, found := labels[removeLabel]; found {
			return nil, nil, fmt.Errorf("can not both modify and remove a label in the same command")
		}
	}
	return labels, remove, nil
}

func labelFunc(obj runtime.Object, overwrite bool, resourceVersion string, labels map[string]string, remove []string) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if !overwrite {
		if err := validateNoOverwrites(accessor, labels); err != nil {
			return err
		}
	}

	objLabels := accessor.GetLabels()
	if objLabels == nil {
		objLabels = make(map[string]string)
	}

	for key, value := range labels {
		objLabels[key] = value
	}
	for _, label := range remove {
		delete(objLabels, label)
	}
	accessor.SetLabels(objLabels)

	if len(resourceVersion) != 0 {
		accessor.SetResourceVersion(resourceVersion)
	}
	return nil
}
