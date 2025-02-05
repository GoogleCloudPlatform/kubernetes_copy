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

package explain

import (
	"fmt"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	openapiclient "k8s.io/client-go/openapi"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/explain"
	openapiv3explain "k8s.io/kubectl/pkg/explain/v2"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/openapi"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	explainLong = templates.LongDesc(i18n.T(`
		Describe (sub-)fields and structure of various resources.

		This command describes the (sub-)fields associated with each supported API resource.
		Fields are identified via a simple JSONPath accessor:

			<type>.<fieldName>

		Subfield of each field is accessed through the same dotted notation:

			<type>.<fieldName>[.<subfieldName>]
			<type>.<fieldName>.<subfieldName>[.<subfieldName>]

		Information about each field is retrieved from the server in OpenAPI format.`))

	explainExamples = templates.Examples(i18n.T(`
		# Get the documentation of the resource and its fields
		kubectl explain pods

		# Get all the fields in the resource
		kubectl explain pods --recursive

		# Get the explanation for deployment in supported api versions
		kubectl explain deployments --api-version=apps/v1

		# Get the documentation of a specific field of a resource
		kubectl explain pods.spec.containers

		# Get the documentation of resources in different format
		kubectl explain deployment --output=plaintext-openapiv2`))
)

const (
	plaintextTemplateName          = "plaintext"
	plaintextOpenAPIV2TemplateName = "plaintext-openapiv2"
)

// ExplainFlags directly reflect the information that CLI is gathering via flags.
// They will be converted to Options, which reflect the runtime requirements for
// the command.
type ExplainFlags struct {
	APIVersion   string
	OutputFormat string
	Recursive    bool

	genericiooptions.IOStreams
}

// NewExplainFlags returns a default ExplainFlags
func NewExplainFlags(streams genericiooptions.IOStreams) *ExplainFlags {
	return &ExplainFlags{
		OutputFormat: plaintextTemplateName,
		IOStreams:    streams,
	}
}

// AddFlags registers flags for a cli
func (flags *ExplainFlags) AddFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&flags.Recursive, "recursive", flags.Recursive, "Print the fields of fields (Currently only 1 level deep)")
	cmd.Flags().StringVar(&flags.APIVersion, "api-version", flags.APIVersion, "Get different explanations for particular API version (API group/version)")
	cmd.Flags().StringVar(&flags.OutputFormat, "output", plaintextTemplateName, "Format in which to render the schema (plaintext, plaintext-openapiv2)")
}

// ToOptions converts from CLI inputs to runtime input
func (flags *ExplainFlags) ToOptions(f cmdutil.Factory, parent string, args []string) (*ExplainOptions, error) {
	mapper, err := f.ToRESTMapper()
	if err != nil {
		return nil, err
	}

	// Only openapi v3 needs the discovery client.
	openAPIV3Client, err := f.OpenAPIV3Client()
	if err != nil {
		return nil, err
	}

	o := &ExplainOptions{
		IOStreams: flags.IOStreams,

		Recursive:    flags.Recursive,
		APIVersion:   flags.APIVersion,
		OutputFormat: flags.OutputFormat,

		CmdParent: parent,
		args:      args,

		Mapper:        mapper,
		openAPIGetter: f,

		OpenAPIV3Client: openAPIV3Client,
	}

	return o, nil
}

// NewCmdExplain returns a cobra command for swagger docs
func NewCmdExplain(parent string, f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	flags := NewExplainFlags(streams)

	cmd := &cobra.Command{
		Use:                   "explain TYPE [--recursive=FALSE|TRUE] [--api-version=api-version-group] [-o|--output=plaintext|plaintext-openapiv2]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Get documentation for a resource"),
		Long:                  explainLong + "\n\n" + cmdutil.SuggestAPIResources(parent),
		Example:               explainExamples,
		Run: func(cmd *cobra.Command, args []string) {
			o, err := flags.ToOptions(f, parent, args)
			cmdutil.CheckErr(err)
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}

	flags.AddFlags(cmd)

	return cmd
}

type ExplainOptions struct {
	genericiooptions.IOStreams

	Recursive  bool
	APIVersion string
	// Name of the template to use with the openapiv3 template renderer.
	OutputFormat string

	CmdParent string
	args      []string

	Mapper        meta.RESTMapper
	openAPIGetter openapi.OpenAPIResourcesGetter

	// Client capable of fetching openapi documents from the user's cluster
	OpenAPIV3Client openapiclient.Client
}

func (o *ExplainOptions) Validate() error {
	if len(o.args) == 0 {
		return fmt.Errorf("You must specify the type of resource to explain. %s\n", cmdutil.SuggestAPIResources(o.CmdParent))
	}
	if len(o.args) > 1 {
		return fmt.Errorf("We accept only this format: explain RESOURCE\n")
	}

	return nil
}

// Run executes the appropriate steps to print a model's documentation
func (o *ExplainOptions) Run() error {
	var fullySpecifiedGVR schema.GroupVersionResource
	var fieldsPath []string
	var err error
	if len(o.APIVersion) == 0 {
		fullySpecifiedGVR, fieldsPath, err = explain.SplitAndParseResourceRequestWithMatchingPrefix(o.args[0], o.Mapper)
		if err != nil {
			return err
		}
	} else {
		// TODO: After we figured out the new syntax to separate group and resource, allow
		// the users to use it in explain (kubectl explain <group><syntax><resource>).
		// Refer to issue #16039 for why we do this. Refer to PR #15808 that used "/" syntax.
		fullySpecifiedGVR, fieldsPath, err = explain.SplitAndParseResourceRequest(o.args[0], o.Mapper)
		if err != nil {
			return err
		}
	}

	// Fallback to openapiv2 implementation using special template name
	switch o.OutputFormat {
	case plaintextOpenAPIV2TemplateName:
		return o.renderOpenAPIV2(fullySpecifiedGVR, fieldsPath)
	case plaintextTemplateName:
		// Check whether the server reponds to OpenAPIV3.
		if _, err := o.OpenAPIV3Client.Paths(); err != nil {
			// Use v2 renderer if server does not support v3
			return o.renderOpenAPIV2(fullySpecifiedGVR, fieldsPath)
		}

		fallthrough
	default:
		if len(o.APIVersion) > 0 {
			apiVersion, err := schema.ParseGroupVersion(o.APIVersion)
			if err != nil {
				return err
			}
			fullySpecifiedGVR.Group = apiVersion.Group
			fullySpecifiedGVR.Version = apiVersion.Version
		}

		return openapiv3explain.PrintModelDescription(
			fieldsPath,
			o.Out,
			o.OpenAPIV3Client,
			fullySpecifiedGVR,
			o.Recursive,
			o.OutputFormat,
		)
	}
}

func (o *ExplainOptions) renderOpenAPIV2(
	fullySpecifiedGVR schema.GroupVersionResource,
	fieldsPath []string,
) error {
	var err error

	gvk, _ := o.Mapper.KindFor(fullySpecifiedGVR)
	if gvk.Empty() {
		gvk, err = o.Mapper.KindFor(fullySpecifiedGVR.GroupResource().WithVersion(""))
		if err != nil {
			return err
		}
	}

	if len(o.APIVersion) != 0 {
		apiVersion, err := schema.ParseGroupVersion(o.APIVersion)
		if err != nil {
			return err
		}
		gvk = apiVersion.WithKind(gvk.Kind)
	}

	resources, err := o.openAPIGetter.OpenAPISchema()
	if err != nil {
		return err
	}
	schema := resources.LookupResource(gvk)
	if schema == nil {
		return fmt.Errorf("couldn't find resource for %q", gvk)
	}

	return explain.PrintModelDescription(fieldsPath, o.Out, schema, gvk, o.Recursive)
}
