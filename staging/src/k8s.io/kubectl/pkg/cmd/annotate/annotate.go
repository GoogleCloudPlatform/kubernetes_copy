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

package annotate

import (
	"bytes"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured/unstructuredscheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"

	"k8s.io/client-go/tools/clientcmd"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

// AnnotateFlags directly reflect the information that CLI is gathering via flags.  They will be converted to Options, which
// reflect the runtime requirements for the command.  This structure reduces the transformation to wiring and makes
// the logic itself easy to unit test
type AnnotateFlags struct {
	// Common user flags
	All            bool
	AllNamespaces  bool
	DryRunStrategy cmdutil.DryRunStrategy
	FieldManager   string
	FieldSelector  string
	resource.FilenameOptions
	List            bool
	Local           bool
	OutputFormat    string
	overwrite       bool
	PrintFlags      *genericclioptions.PrintFlags
	RecordFlags     *genericclioptions.RecordFlags
	resourceVersion string
	Selector        string

	genericiooptions.IOStreams
}

// NewAnnotateFlags returns a default AnnotateFlags
func NewAnnotateFlags(streams genericiooptions.IOStreams) *AnnotateFlags {
	return &AnnotateFlags{
		PrintFlags:  genericclioptions.NewPrintFlags("annotated").WithTypeSetter(scheme.Scheme),
		RecordFlags: genericclioptions.NewRecordFlags(),
		IOStreams:   streams,
	}
}

// AnnotateOptions have the data required to perform the annotate operation
type AnnotateOptions struct {
	all           bool
	allNamespaces bool

	builder        *resource.Builder
	dryRunStrategy cmdutil.DryRunStrategy

	enforceNamespace bool
	fieldSelector    string
	fieldManager     string
	resource.FilenameOptions

	genericiooptions.IOStreams

	list           bool
	local          bool
	namespace      string
	newAnnotations map[string]string
	overwrite      bool

	PrintObj printers.ResourcePrinterFunc

	Recorder          genericclioptions.Recorder
	resources         []string
	resourceVersion   string
	removeAnnotations []string
	selector          string

	unstructuredClientForMapping func(mapping *meta.RESTMapping) (resource.RESTClient, error)
}

var (
	annotateLong = templates.LongDesc(i18n.T(`
		Update the annotations on one or more resources.

		All Kubernetes objects support the ability to store additional data with the object as
		annotations. Annotations are key/value pairs that can be larger than labels and include
		arbitrary string values such as structured JSON. Tools and system extensions may use
		annotations to store their own data.

		Attempting to set an annotation that already exists will fail unless --overwrite is set.
		If --resource-version is specified and does not match the current resource version on
		the server the command will fail.`))

	annotateExample = templates.Examples(i18n.T(`
    # Update pod 'foo' with the annotation 'description' and the value 'my frontend'
    # If the same annotation is set multiple times, only the last value will be applied
    kubectl annotate pods foo description='my frontend'

    # Update a pod identified by type and name in "pod.json"
    kubectl annotate -f pod.json description='my frontend'

    # Update pod 'foo' with the annotation 'description' and the value 'my frontend running nginx', overwriting any existing value
    kubectl annotate --overwrite pods foo description='my frontend running nginx'

    # Update all pods in the namespace
    kubectl annotate pods --all description='my frontend running nginx'

    # Update pod 'foo' only if the resource is unchanged from version 1
    kubectl annotate pods foo description='my frontend running nginx' --resource-version=1

    # Update pod 'foo' by removing an annotation named 'description' if it exists
    # Does not require the --overwrite flag
    kubectl annotate pods foo description-`))
)

// NewCmdAnnotate creates the `annotate` command
func NewCmdAnnotate(parent string, f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	flags := NewAnnotateFlags(streams)

	cmd := &cobra.Command{
		Use:                   "annotate [--overwrite] (-f FILENAME | TYPE NAME) KEY_1=VAL_1 ... KEY_N=VAL_N [--resource-version=version]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Update the annotations on a resource"),
		Long:                  annotateLong + "\n\n" + cmdutil.SuggestAPIResources(parent),
		Example:               annotateExample,
		ValidArgsFunction:     completion.ResourceTypeAndNameCompletionFunc(f),
		Run: func(cmd *cobra.Command, args []string) {
			o, err := flags.ToOptions(f, cmd, args)
			cmdutil.CheckErr(err)
			cmdutil.CheckErr(o.RunAnnotate())
		},
	}

	flags.AddFlags(cmd, streams)

	return cmd
}

// AddFlags registers flags for a cli.
func (flags *AnnotateFlags) AddFlags(cmd *cobra.Command, ioStreams genericiooptions.IOStreams) {
	flags.PrintFlags.AddFlags(cmd)
	flags.RecordFlags.AddFlags(cmd)

	cmdutil.AddDryRunFlag(cmd)

	usage := "identifying the resource to update the annotation"
	cmdutil.AddFilenameOptionFlags(cmd, &flags.FilenameOptions, usage)
	cmdutil.AddFieldManagerFlagVar(cmd, &flags.FieldManager, "kubectl-annotate")
	cmdutil.AddLabelSelectorFlagVar(cmd, &flags.Selector)

	cmd.Flags().BoolVar(&flags.overwrite, "overwrite", flags.overwrite, "If true, allow annotations to be overwritten, otherwise reject annotation updates that overwrite existing annotations.")
	cmd.Flags().BoolVar(&flags.List, "list", flags.List, "If true, display the annotations for a given resource.")
	cmd.Flags().BoolVar(&flags.Local, "local", flags.Local, "If true, annotation will NOT contact api-server but run locally.")
	cmd.Flags().StringVarP(&flags.FieldSelector, "field-selector", "F", flags.FieldSelector, "Selector (field query) to filter on, supports '=', '==', and '!='.(e.g. --field-selector key1=value1,key2=value2). The server only supports a limited number of field queries per type.")
	cmd.Flags().BoolVar(&flags.All, "all", flags.All, "Select all resources, in the namespace of the specified resource types.")
	cmd.Flags().BoolVarP(&flags.AllNamespaces, "all-namespaces", "A", flags.AllNamespaces, "If true, check the specified action in all namespaces.")
	cmd.Flags().StringVar(&flags.resourceVersion, "resource-version", flags.resourceVersion, i18n.T("If non-empty, the annotation update will only succeed if this is the current resource-version for the object. Only valid when specifying a single resource."))
}

// ToOptions converts from CLI inputs to runtime inputs.
func (flags *AnnotateFlags) ToOptions(f cmdutil.Factory, cmd *cobra.Command, args []string) (*AnnotateOptions, error) {
	options := &AnnotateOptions{
		all:             flags.All,
		allNamespaces:   flags.AllNamespaces,
		FilenameOptions: flags.FilenameOptions,
		fieldSelector:   flags.FieldSelector,
		fieldManager:    flags.FieldManager,
		IOStreams:       flags.IOStreams,
		local:           flags.Local,
		list:            flags.List,
		overwrite:       flags.overwrite,
		resourceVersion: flags.resourceVersion,
		Recorder:        genericclioptions.NoopRecorder{},
		selector:        flags.Selector,
	}

	var err error

	flags.RecordFlags.Complete(cmd)
	options.Recorder, err = flags.RecordFlags.ToRecorder()
	if err != nil {
		return nil, err
	}

	options.dryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return nil, err
	}

	cmdutil.PrintFlagsWithDryRunStrategy(flags.PrintFlags, options.dryRunStrategy)
	printer, err := flags.PrintFlags.ToPrinter()
	if err != nil {
		return nil, err
	}
	options.PrintObj = func(obj runtime.Object, out io.Writer) error {
		return printer.PrintObj(obj, out)
	}

	options.namespace, options.enforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil && !(options.local && clientcmd.IsEmptyConfig(err)) {
		return nil, err
	}
	options.builder = f.NewBuilder()
	options.unstructuredClientForMapping = f.UnstructuredClientForMapping

	// retrieves resource and annotation args from args
	// also checks args to verify that all resources are specified before annotations
	resources, annotationArgs, err := cmdutil.GetResourcesAndPairs(args, "annotation")
	if err != nil {
		return nil, err
	}
	options.resources = resources
	options.newAnnotations, options.removeAnnotations, err = parseAnnotations(annotationArgs)
	if err != nil {
		return nil, err
	}

	// Checks the options and flags to see if there is sufficient information run the command.
	if flags.List && len(flags.OutputFormat) > 0 {
		return nil, fmt.Errorf("--list and --output may not be specified together")
	}
	if flags.All && len(flags.Selector) > 0 {
		return nil, fmt.Errorf("cannot set --all and --selector at the same time")
	}
	if flags.All && len(flags.FieldSelector) > 0 {
		return nil, fmt.Errorf("cannot set --all and --field-selector at the same time")
	}

	if !flags.Local {
		if len(options.resources) < 1 && cmdutil.IsFilenameSliceEmpty(flags.Filenames, flags.Kustomize) {
			return nil, fmt.Errorf("one or more resources must be specified as <resource> <name> or <resource>/<name>")
		}
	} else {
		if options.dryRunStrategy == cmdutil.DryRunServer {
			return nil, fmt.Errorf("cannot specify --local and --dry-run=server - did you mean --dry-run=client?")
		}
		if len(options.resources) > 0 {
			return nil, fmt.Errorf("can only use local files by -f rsrc.yaml or --filename=rsrc.json when --local=true is set")
		}
		if cmdutil.IsFilenameSliceEmpty(flags.Filenames, flags.Kustomize) {
			return nil, fmt.Errorf("one or more files must be specified as -f rsrc.yaml or --filename=rsrc.json")
		}
	}
	if len(options.newAnnotations) < 1 && len(options.removeAnnotations) < 1 && !flags.List {
		return nil, fmt.Errorf("at least one annotation update is required")
	}
	err = validateAnnotations(options.removeAnnotations, options.newAnnotations)
	if err != nil {
		return nil, err
	}

	return options, nil
}

// RunAnnotate does the work
func (o AnnotateOptions) RunAnnotate() error {
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

	r := b.Do()
	if err := r.Err(); err != nil {
		return err
	}

	var singleItemImpliedResource bool
	r.IntoSingleItemImplied(&singleItemImpliedResource)

	// only apply resource version locking on a single resource.
	// we must perform this check after o.builder.Do() as
	// []o.resources can not accurately return the proper number
	// of resources when they are not passed in "resource/name" format.
	if !singleItemImpliedResource && len(o.resourceVersion) > 0 {
		return fmt.Errorf("--resource-version may only be used with a single resource")
	}

	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		var outputObj runtime.Object
		obj := info.Object

		if o.dryRunStrategy == cmdutil.DryRunClient || o.local || o.list {
			if err := o.updateAnnotations(obj); err != nil {
				return err
			}
			outputObj = obj
		} else {
			mapping := info.ResourceMapping()
			name, namespace := info.Name, info.Namespace

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
			if err := o.Recorder.Record(info.Object); err != nil {
				klog.V(4).Infof("error recording current command: %v", err)
			}
			if err := o.updateAnnotations(obj); err != nil {
				return err
			}
			newData, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
			createdPatch := err == nil
			if err != nil {
				klog.V(2).Infof("couldn't compute patch: %v", err)
			}

			client, err := o.unstructuredClientForMapping(mapping)
			if err != nil {
				return err
			}
			helper := resource.
				NewHelper(client, mapping).
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
			if !singleItemImpliedResource {
				indent = " "
				gvks, _, err := unstructuredscheme.NewUnstructuredObjectTyper().ObjectKinds(info.Object)
				if err != nil {
					return err
				}
				fmt.Fprintf(o.Out, "Listing annotations for %s.%s/%s:\n", gvks[0].Kind, gvks[0].Group, info.Name)
			}
			for k, v := range accessor.GetAnnotations() {
				fmt.Fprintf(o.Out, "%s%s=%s\n", indent, k, v)
			}

			return nil
		}

		return o.PrintObj(outputObj, o.Out)
	})
}

// parseAnnotations retrieves new and remove annotations from annotation args
func parseAnnotations(annotationArgs []string) (map[string]string, []string, error) {
	return cmdutil.ParsePairs(annotationArgs, "annotation", true)
}

// validateAnnotations checks the format of annotation args and checks removed annotations aren't in the new annotations map
func validateAnnotations(removeAnnotations []string, newAnnotations map[string]string) error {
	var modifyRemoveBuf bytes.Buffer
	for _, removeAnnotation := range removeAnnotations {
		if _, found := newAnnotations[removeAnnotation]; found {
			if modifyRemoveBuf.Len() > 0 {
				modifyRemoveBuf.WriteString(", ")
			}
			modifyRemoveBuf.WriteString(fmt.Sprint(removeAnnotation))
		}
	}
	if modifyRemoveBuf.Len() > 0 {
		return fmt.Errorf("can not both modify and remove the following annotation(s) in the same command: %s", modifyRemoveBuf.String())
	}

	return nil
}

// validateNoAnnotationOverwrites validates that when overwrite is false, to-be-updated annotations don't exist in the object annotation map (yet)
func validateNoAnnotationOverwrites(accessor metav1.Object, annotations map[string]string) error {
	var buf bytes.Buffer
	for key, value := range annotations {
		// change-cause annotation can always be overwritten
		if key == polymorphichelpers.ChangeCauseAnnotation {
			continue
		}
		if currValue, found := accessor.GetAnnotations()[key]; found && currValue != value {
			if buf.Len() > 0 {
				buf.WriteString("; ")
			}
			buf.WriteString(fmt.Sprintf("'%s' already has a value (%s)", key, currValue))
		}
	}
	if buf.Len() > 0 {
		return fmt.Errorf("--overwrite is false but found the following declared annotation(s): %s", buf.String())
	}
	return nil
}

// updateAnnotations updates annotations of obj
func (o AnnotateOptions) updateAnnotations(obj runtime.Object) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if !o.overwrite {
		if err := validateNoAnnotationOverwrites(accessor, o.newAnnotations); err != nil {
			return err
		}
	}

	annotations := accessor.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	for key, value := range o.newAnnotations {
		annotations[key] = value
	}
	for _, annotation := range o.removeAnnotations {
		delete(annotations, annotation)
	}
	accessor.SetAnnotations(annotations)

	if len(o.resourceVersion) != 0 {
		accessor.SetResourceVersion(o.resourceVersion)
	}
	return nil
}
