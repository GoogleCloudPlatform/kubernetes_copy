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

package convert

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/validation"
	scheme "k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
)

var (
	convertLong = templates.LongDesc(i18n.T(`
		Convert config files between different API versions. Both YAML
		and JSON formats are accepted.

		The command takes filename, directory, or URL as input, and convert it into format
		of version specified by --output-version flag. If target version is not specified or
		not supported, convert to latest version.

		The default output will be printed to stdout in YAML format. One can use -o option
		to change to output destination.`))

	convertExample = templates.Examples(i18n.T(`
		# Convert 'pod.yaml' to latest version and print to stdout.
		kubectl convert -f pod.yaml

		# Convert the live state of the resource specified by 'pod.yaml' to the latest version
		# and print to stdout in JSON format.
		kubectl convert -f pod.yaml --local -o json

		# Convert all files under current directory to latest version and create them all.
		kubectl convert -f . | kubectl create -f -`))
)

// ConvertOptions have the data required to perform the convert operation
type ConvertOptions struct {
	PrintFlags *genericclioptions.PrintFlags
	Printer    printers.ResourcePrinter

	OutputVersion string
	Namespace     string

	builder   func() *resource.Builder
	local     bool
	validator func() (validation.Schema, error)

	resource.FilenameOptions
	genericiooptions.IOStreams
}

func NewConvertOptions(ioStreams genericiooptions.IOStreams) *ConvertOptions {
	return &ConvertOptions{
		PrintFlags: genericclioptions.NewPrintFlags("converted").WithTypeSetter(scheme.Scheme).WithDefaultOutput("yaml"),
		local:      true,
		IOStreams:  ioStreams,
	}
}

// NewCmdConvert creates a command object for the generic "convert" action, which
// translates the config file into a given version.
func NewCmdConvert(logger klog.Logger, f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewConvertOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "convert -f FILENAME",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Convert config files between different API versions"),
		Long:                  convertLong,
		Example:               convertExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd))
			cmdutil.CheckErr(o.RunConvert(logger))
		},
	}

	cmd.Flags().BoolVar(&o.local, "local", o.local, "If true, convert will NOT try to contact api-server but run locally.")
	cmd.Flags().StringVar(&o.OutputVersion, "output-version", o.OutputVersion, i18n.T("Output the formatted object with the given group version (for ex: 'extensions/v1beta1')."))
	o.PrintFlags.AddFlags(cmd)

	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, "to need to get converted.")
	return cmd
}

// Complete collects information required to run Convert command from command line.
func (o *ConvertOptions) Complete(f cmdutil.Factory, cmd *cobra.Command) (err error) {
	err = o.FilenameOptions.RequireFilenameOrKustomize()
	if err != nil {
		return err
	}
	o.builder = f.NewBuilder

	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.validator = func() (validation.Schema, error) {
		directive, err := cmdutil.GetValidationDirective(cmd)
		if err != nil {
			return nil, err
		}
		return f.Validator(directive)
	}

	// build the printer
	o.Printer, err = o.PrintFlags.ToPrinter()
	return err
}

// RunConvert implements the generic Convert command
func (o *ConvertOptions) RunConvert(logger klog.Logger) error {
	b := o.builder().
		WithScheme(scheme.Scheme).
		LocalParam(o.local)
	if !o.local {
		schema, err := o.validator()
		if err != nil {
			return err
		}
		b.Schema(schema)
	}

	r := b.NamespaceParam(o.Namespace).
		ContinueOnError().
		FilenameParam(false, &o.FilenameOptions).
		Flatten().
		Do()

	err := r.Err()
	if err != nil {
		return err
	}

	singleItemImplied := false
	infos, err := r.IntoSingleItemImplied(&singleItemImplied).Infos()
	if err != nil {
		return err
	}

	if len(infos) == 0 {
		return fmt.Errorf("no objects passed to convert")
	}

	var specifiedOutputVersion schema.GroupVersion
	if len(o.OutputVersion) > 0 {
		specifiedOutputVersion, err = schema.ParseGroupVersion(o.OutputVersion)
		if err != nil {
			return err
		}
	}

	internalEncoder := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
	internalVersionJSONEncoder := unstructured.NewJSONFallbackEncoder(internalEncoder)
	objects, err := asVersionedObject(logger, infos, !singleItemImplied, specifiedOutputVersion, internalVersionJSONEncoder, o.IOStreams)
	if err != nil {
		return err
	}

	return o.Printer.PrintObj(objects, o.Out)
}

// asVersionedObject converts a list of infos into a single object - either a List containing
// the objects as children, or if only a single Object is present, as that object. The provided
// version will be preferred as the conversion target, but the Object's mapping version will be
// used if that version is not present.
func asVersionedObject(logger klog.Logger, infos []*resource.Info, forceList bool, specifiedOutputVersion schema.GroupVersion, encoder runtime.Encoder, iostream genericclioptions.IOStreams) (runtime.Object, error) {
	objects, err := asVersionedObjects(infos, specifiedOutputVersion, encoder, iostream)
	if err != nil {
		return nil, err
	}

	var object runtime.Object
	if len(objects) == 1 && !forceList {
		object = objects[0]
	} else {
		object = &api.List{Items: objects}
		targetVersions := []schema.GroupVersion{}
		if !specifiedOutputVersion.Empty() {
			targetVersions = append(targetVersions, specifiedOutputVersion)
		}
		targetVersions = append(targetVersions, schema.GroupVersion{Group: "", Version: "v1"})

		converted, err := tryConvert(scheme.Scheme, object, targetVersions...)
		if err != nil {
			return nil, err
		}
		object = converted
	}

	actualVersion := object.GetObjectKind().GroupVersionKind()
	if actualVersion.Version != specifiedOutputVersion.Version {
		defaultVersionInfo := ""
		if len(actualVersion.Version) > 0 {
			defaultVersionInfo = fmt.Sprintf("Defaulting to %q", actualVersion.Version)
		}
		logger.V(1).Info("The output version specified is invalid.", "defaultVersionInfo", defaultVersionInfo)
	}
	return object, nil
}

// asVersionedObjects converts a list of infos into versioned objects. The provided
// version will be preferred as the conversion target, but the Object's mapping version will be
// used if that version is not present.
func asVersionedObjects(infos []*resource.Info, specifiedOutputVersion schema.GroupVersion, encoder runtime.Encoder, iostream genericclioptions.IOStreams) ([]runtime.Object, error) {
	objects := []runtime.Object{}
	for _, info := range infos {
		if info.Object == nil {
			continue
		}

		targetVersions := []schema.GroupVersion{}
		// objects that are not part of api.Scheme must be converted to JSON
		// TODO: convert to map[string]interface{}, attach to runtime.Unknown?
		if !specifiedOutputVersion.Empty() {
			if _, _, err := scheme.Scheme.ObjectKinds(info.Object); runtime.IsNotRegisteredError(err) {
				// TODO: ideally this would encode to version, but we don't expose multiple codecs here.
				data, err := runtime.Encode(encoder, info.Object)
				if err != nil {
					return nil, err
				}
				// TODO: Set ContentEncoding and ContentType.
				objects = append(objects, &runtime.Unknown{Raw: data})
				continue
			}
			targetVersions = append(targetVersions, specifiedOutputVersion)
		} else {
			gvks, _, err := scheme.Scheme.ObjectKinds(info.Object)
			if err == nil {
				for _, gvk := range gvks {
					targetVersions = append(targetVersions, scheme.Scheme.PrioritizedVersionsForGroup(gvk.Group)...)
				}
			}
		}

		converted, err := tryConvert(scheme.Scheme, info.Object, targetVersions...)
		if err != nil {
			// Dont fail on not registered error converting objects.
			// Simply warn the user with the error returned from api-machinery and continue with the rest of the file
			// fail on all other errors
			if runtime.IsNotRegisteredError(err) {
				fmt.Fprintln(iostream.ErrOut, err.Error())
				continue
			}

			return nil, err
		}
		objects = append(objects, converted)
	}
	return objects, nil
}

// tryConvert attempts to convert the given object to the provided versions in order. This function assumes
// the object is in internal version.
func tryConvert(converter runtime.ObjectConvertor, object runtime.Object, versions ...schema.GroupVersion) (runtime.Object, error) {
	var last error
	for _, version := range versions {
		if version.Empty() {
			return object, nil
		}
		obj, err := converter.ConvertToVersion(object, version)
		if err != nil {
			last = err
			continue
		}
		return obj, nil
	}
	return nil, last
}
