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

package patch

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/yaml"
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

var patchTypes = map[string]types.PatchType{"json": types.JSONPatchType, "merge": types.MergePatchType, "strategic": types.StrategicMergePatchType}

// PatchOptions is the start of the data required to perform the operation.  As new fields are added, add them here instead of
// referencing the cmd.Flags()
type PatchOptions struct {
	resource.FilenameOptions

	RecordFlags *genericclioptions.RecordFlags
	PrintFlags  *genericclioptions.PrintFlags
	ToPrinter   func(string) (printers.ResourcePrinter, error)
	Recorder    genericclioptions.Recorder

	Local       bool
	PatchType   string
	Patch       string
	PatchFile   string
	Subresource string

	namespace                    string
	enforceNamespace             bool
	dryRunStrategy               cmdutil.DryRunStrategy
	outputFormat                 string
	args                         []string
	builder                      *resource.Builder
	unstructuredClientForMapping func(mapping *meta.RESTMapping) (resource.RESTClient, error)
	fieldManager                 string

	genericiooptions.IOStreams
}

var (
	patchLong = templates.LongDesc(i18n.T(`
		Update fields of a resource using strategic merge patch, a JSON merge patch, or a JSON patch.

		JSON and YAML formats are accepted.

		Note: Strategic merge patch is not supported for custom resources.`))

	patchExample = templates.Examples(i18n.T(`
		# Partially update a node using a strategic merge patch, specifying the patch as JSON
		kubectl patch node k8s-node-1 -p '{"spec":{"unschedulable":true}}'

		# Partially update a node using a strategic merge patch, specifying the patch as YAML
		kubectl patch node k8s-node-1 -p $'spec:\n unschedulable: true'

		# Partially update a node identified by the type and name specified in "node.json" using strategic merge patch
		kubectl patch -f node.json -p '{"spec":{"unschedulable":true}}'

		# Update a container's image; spec.containers[*].name is required because it's a merge key
		kubectl patch pod valid-pod -p '{"spec":{"containers":[{"name":"kubernetes-serve-hostname","image":"new image"}]}}'

		# Update a container's image using a JSON patch with positional arrays
		kubectl patch pod valid-pod --type='json' -p='[{"op": "replace", "path": "/spec/containers/0/image", "value":"new image"}]'

		# Update a deployment's replicas through the 'scale' subresource using a merge patch
		kubectl patch deployment nginx-deployment --subresource='scale' --type='merge' -p '{"spec":{"replicas":2}}'`))
)

func NewPatchOptions(ioStreams genericiooptions.IOStreams) *PatchOptions {
	return &PatchOptions{
		RecordFlags: genericclioptions.NewRecordFlags(),
		Recorder:    genericclioptions.NoopRecorder{},
		PrintFlags:  genericclioptions.NewPrintFlags("patched").WithTypeSetter(scheme.Scheme),
		IOStreams:   ioStreams,
	}
}

func NewCmdPatch(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewPatchOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "patch (-f FILENAME | TYPE NAME) [-p PATCH|--patch-file FILE]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Update fields of a resource"),
		Long:                  patchLong,
		Example:               patchExample,
		ValidArgsFunction:     completion.ResourceTypeAndNameCompletionFunc(f),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunPatch())
		},
	}

	o.RecordFlags.AddFlags(cmd)
	o.PrintFlags.AddFlags(cmd)

	cmd.Flags().StringVarP(&o.Patch, "patch", "p", "", "The patch to be applied to the resource JSON file.")
	cmd.Flags().StringVar(&o.PatchFile, "patch-file", "", "A file containing a patch to be applied to the resource.")
	cmd.Flags().StringVar(&o.PatchType, "type", "strategic", fmt.Sprintf("The type of patch being provided; one of %v", sets.List(sets.KeySet(patchTypes))))
	cmdutil.AddDryRunFlag(cmd)
	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, "identifying the resource to update")
	cmd.Flags().BoolVar(&o.Local, "local", o.Local, "If true, patch will operate on the content of the file, not the server-side resource.")
	cmdutil.AddFieldManagerFlagVar(cmd, &o.fieldManager, "kubectl-patch")
	cmdutil.AddSubresourceFlags(cmd, &o.Subresource, "If specified, patch will operate on the subresource of the requested object.")

	return cmd
}

func (o *PatchOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
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

	cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.dryRunStrategy)
	o.ToPrinter = func(operation string) (printers.ResourcePrinter, error) {
		o.PrintFlags.NamePrintFlags.Operation = operation

		return o.PrintFlags.ToPrinter()
	}

	o.namespace, o.enforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil && !(o.Local && clientcmd.IsEmptyConfig(err)) {
		return err
	}
	o.args = args
	o.builder = f.NewBuilder()
	o.unstructuredClientForMapping = f.UnstructuredClientForMapping

	return nil
}

func (o *PatchOptions) Validate() error {
	if len(o.Patch) > 0 && len(o.PatchFile) > 0 {
		return fmt.Errorf("cannot specify --patch and --patch-file together")
	}
	if len(o.Patch) == 0 && len(o.PatchFile) == 0 {
		return fmt.Errorf("must specify --patch or --patch-file containing the contents of the patch")
	}
	if o.Local && len(o.args) != 0 {
		return fmt.Errorf("cannot specify --local and server resources")
	}
	if o.Local && o.dryRunStrategy == cmdutil.DryRunServer {
		return fmt.Errorf("cannot specify --local and --dry-run=server - did you mean --dry-run=client?")
	}
	if len(o.PatchType) != 0 {
		if _, ok := patchTypes[strings.ToLower(o.PatchType)]; !ok {
			return fmt.Errorf("--type must be one of %v, not %q", sets.List(sets.KeySet(patchTypes)), o.PatchType)
		}
	}
	return nil
}

func (o *PatchOptions) RunPatch() error {
	patchType := types.StrategicMergePatchType
	if len(o.PatchType) != 0 {
		patchType = patchTypes[strings.ToLower(o.PatchType)]
	}

	var patchBytes []byte
	if len(o.PatchFile) > 0 {
		var err error
		patchBytes, err = os.ReadFile(o.PatchFile)
		if err != nil {
			return fmt.Errorf("unable to read patch file: %v", err)
		}
	} else {
		patchBytes = []byte(o.Patch)
	}

	patchBytes, err := yaml.ToJSON(patchBytes)
	if err != nil {
		return fmt.Errorf("unable to parse %q: %v", o.Patch, err)
	}

	r := o.builder.
		Unstructured().
		ContinueOnError().
		LocalParam(o.Local).
		NamespaceParam(o.namespace).DefaultNamespace().
		FilenameParam(o.enforceNamespace, &o.FilenameOptions).
		Subresource(o.Subresource).
		ResourceTypeOrNameArgs(false, o.args...).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	count := 0
	err = r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		count++
		name, namespace := info.Name, info.Namespace

		if !o.Local && o.dryRunStrategy != cmdutil.DryRunClient {
			mapping := info.ResourceMapping()
			client, err := o.unstructuredClientForMapping(mapping)
			if err != nil {
				return err
			}

			helper := resource.
				NewHelper(client, mapping).
				DryRun(o.dryRunStrategy == cmdutil.DryRunServer).
				WithFieldManager(o.fieldManager).
				WithSubresource(o.Subresource)
			patchedObj, err := helper.Patch(namespace, name, patchType, patchBytes, nil)
			if err != nil {
				if apierrors.IsUnsupportedMediaType(err) {
					return errors.Wrap(err, fmt.Sprintf("%s is not supported by %s", patchType, mapping.GroupVersionKind))
				}
				return err
			}

			didPatch := !reflect.DeepEqual(info.Object, patchedObj)

			// if the recorder makes a change, compute and create another patch
			if mergePatch, err := o.Recorder.MakeRecordMergePatch(patchedObj); err != nil {
				klog.V(4).Infof("error recording current command: %v", err)
			} else if len(mergePatch) > 0 {
				if recordedObj, err := helper.Patch(namespace, name, types.MergePatchType, mergePatch, nil); err != nil {
					klog.V(4).Infof("error recording reason: %v", err)
				} else {
					patchedObj = recordedObj
				}
			}

			printer, err := o.ToPrinter(patchOperation(didPatch))
			if err != nil {
				return err
			}
			return printer.PrintObj(patchedObj, o.Out)
		}

		originalObjJS, err := runtime.Encode(unstructured.UnstructuredJSONScheme, info.Object)
		if err != nil {
			return err
		}

		originalPatchedObjJS, err := getPatchedJSON(patchType, originalObjJS, patchBytes, info.Object.GetObjectKind().GroupVersionKind(), scheme.Scheme)
		if err != nil {
			return err
		}

		targetObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, originalPatchedObjJS)
		if err != nil {
			return err
		}

		didPatch := !reflect.DeepEqual(info.Object, targetObj)
		printer, err := o.ToPrinter(patchOperation(didPatch))
		if err != nil {
			return err
		}
		return printer.PrintObj(targetObj, o.Out)
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no objects passed to patch")
	}
	return nil
}

func getPatchedJSON(patchType types.PatchType, originalJS, patchJS []byte, gvk schema.GroupVersionKind, creater runtime.ObjectCreater) ([]byte, error) {
	switch patchType {
	case types.JSONPatchType:
		patchObj, err := jsonpatch.DecodePatch(patchJS)
		if err != nil {
			return nil, err
		}
		bytes, err := patchObj.Apply(originalJS)
		// TODO: This is pretty hacky, we need a better structured error from the json-patch
		if err != nil && strings.Contains(err.Error(), "doc is missing key") {
			msg := err.Error()
			ix := strings.Index(msg, "key:")
			key := msg[ix+5:]
			return bytes, fmt.Errorf("Object to be patched is missing field (%s)", key)
		}
		return bytes, err

	case types.MergePatchType:
		return jsonpatch.MergePatch(originalJS, patchJS)

	case types.StrategicMergePatchType:
		// get a typed object for this GVK if we need to apply a strategic merge patch
		obj, err := creater.New(gvk)
		if err != nil {
			return nil, fmt.Errorf("strategic merge patch is not supported for %s locally, try --type merge", gvk.String())
		}
		return strategicpatch.StrategicMergePatch(originalJS, patchJS, obj)

	default:
		// only here as a safety net - go-restful filters content-type
		return nil, fmt.Errorf("unknown Content-Type header for patch: %v", patchType)
	}
}

func patchOperation(didPatch bool) string {
	if didPatch {
		return "patched"
	}
	return "patched (no change)"
}
