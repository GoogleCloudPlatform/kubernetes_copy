/*
Copyright 2018 The Kubernetes Authors.

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

package apiresources

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/discovery"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	apiresourcesExample = templates.Examples(`
		# Print the supported API resources
		kubectl api-resources

		# Print the supported API resources with more information
		kubectl api-resources -o wide

		# Print the supported API resources sorted by a column
		kubectl api-resources --sort-by=name

		# Print the supported namespaced resources
		kubectl api-resources --namespaced=true

		# Print the supported non-namespaced resources
		kubectl api-resources --namespaced=false

		# Print the supported API resources with a specific APIGroup
		kubectl api-resources --api-group=rbac.authorization.k8s.io`)
)

// APIResourceOptions is the start of the data required to perform the operation.
// As new fields are added, add them here instead of referencing the cmd.Flags()
type APIResourceOptions struct {
	SortBy     string
	APIGroup   string
	Namespaced bool
	Verbs      []string
	NoHeaders  bool
	Cached     bool
	Categories []string
	PrintFlags *genericclioptions.PrintFlags
	PrintObj   printers.ResourcePrinterFunc

	groupChanged bool
	nsChanged    bool

	discoveryClient discovery.CachedDiscoveryInterface

	genericiooptions.IOStreams
}

// groupResource contains the APIGroup and APIResource
type groupResource struct {
	APIGroup        string
	APIGroupVersion string
	APIResource     metav1.APIResource
}

// NewAPIResourceOptions creates the options for APIResource
func NewAPIResourceOptions(ioStreams genericiooptions.IOStreams) *APIResourceOptions {
	return &APIResourceOptions{
		IOStreams:  ioStreams,
		Namespaced: true,
		PrintFlags: genericclioptions.NewPrintFlags(""),
	}
}

// NewCmdAPIResources creates the `api-resources` command
func NewCmdAPIResources(restClientGetter genericclioptions.RESTClientGetter, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewAPIResourceOptions(ioStreams)

	cmd := &cobra.Command{
		Use:     "api-resources",
		Short:   i18n.T("Print the supported API resources on the server"),
		Long:    i18n.T("Print the supported API resources on the server."),
		Example: apiresourcesExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(restClientGetter, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunAPIResources())
		},
	}

	cmd.Flags().BoolVar(&o.NoHeaders, "no-headers", o.NoHeaders, "When using the default or custom-column output format, don't print headers (default print headers).")
	o.PrintFlags.AddFlags(cmd)

	cmd.Flags().StringVar(&o.APIGroup, "api-group", o.APIGroup, "Limit to resources in the specified API group.")
	cmd.Flags().BoolVar(&o.Namespaced, "namespaced", o.Namespaced, "If false, non-namespaced resources will be returned, otherwise returning namespaced resources by default.")
	cmd.Flags().StringSliceVar(&o.Verbs, "verbs", o.Verbs, "Limit to resources that support the specified verbs.")
	cmd.Flags().StringVar(&o.SortBy, "sort-by", o.SortBy, "If non-empty, sort list of resources using specified field. The field can be either 'name' or 'kind'.")
	cmd.Flags().BoolVar(&o.Cached, "cached", o.Cached, "Use the cached list of resources if available.")
	cmd.Flags().StringSliceVar(&o.Categories, "categories", o.Categories, "Limit to resources that belong to the specified categories.")
	return cmd
}

// Validate checks to the APIResourceOptions to see if there is sufficient information run the command
func (o *APIResourceOptions) Validate() error {
	supportedOutputTypes := sets.NewString("", "wide", "name", "json", "yaml")
	if !supportedOutputTypes.Has(*o.PrintFlags.OutputFormat) {
		return fmt.Errorf("--output %v is not available", *o.PrintFlags.OutputFormat)
	}
	supportedSortTypes := sets.New[string]("", "name", "kind")
	if len(o.SortBy) > 0 {
		if !supportedSortTypes.Has(o.SortBy) {
			return fmt.Errorf("--sort-by accepts only name or kind")
		}
	}
	return nil
}

// Complete adapts from the command line args and validates them
func (o *APIResourceOptions) Complete(restClientGetter genericclioptions.RESTClientGetter, cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		return cmdutil.UsageErrorf(cmd, "unexpected arguments: %v", args)
	}

	discoveryClient, err := restClientGetter.ToDiscoveryClient()
	if err != nil {
		return err
	}
	o.discoveryClient = discoveryClient

	o.groupChanged = cmd.Flags().Changed("api-group")
	o.nsChanged = cmd.Flags().Changed("namespaced")

	var printer printers.ResourcePrinter
	if o.PrintFlags.OutputFormat != nil && len(*o.PrintFlags.OutputFormat) > 0 &&
		(*o.PrintFlags.OutputFormat == "json" || *o.PrintFlags.OutputFormat == "yaml") {
		printer, err = o.PrintFlags.ToPrinter()
		if err != nil {
			return err
		}
		o.PrintObj = func(object runtime.Object, out io.Writer) error {
			return printer.PrintObj(object, out)
		}
	}
	return nil
}

// RunAPIResources does the work
func (o *APIResourceOptions) RunAPIResources() error {
	w := printers.GetNewTabWriter(o.Out)
	defer w.Flush()

	if !o.Cached {
		// Always request fresh data from the server
		o.discoveryClient.Invalidate()
	}

	errs := []error{}
	lists, err := o.discoveryClient.ServerPreferredResources()
	if err != nil {
		errs = append(errs, err)
	}

	resources := []groupResource{}
	var filteredList []*metav1.APIResourceList

	for _, list := range lists {
		if len(list.APIResources) == 0 {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		apiList := &metav1.APIResourceList{
			TypeMeta: metav1.TypeMeta{
				Kind:       "APIResourceList",
				APIVersion: "v1",
			},
			GroupVersion: gv.String(),
		}
		var apiResources []metav1.APIResource
		for _, resource := range list.APIResources {
			if len(resource.Verbs) == 0 {
				continue
			}
			// filter apiGroup
			if o.groupChanged && o.APIGroup != gv.Group {
				continue
			}
			// filter namespaced
			if o.nsChanged && o.Namespaced != resource.Namespaced {
				continue
			}
			// filter to resources that support the specified verbs
			if len(o.Verbs) > 0 && !sets.New[string](resource.Verbs...).HasAll(o.Verbs...) {
				continue
			}
			// filter to resources that belong to the specified categories
			if len(o.Categories) > 0 && !sets.New[string](resource.Categories...).HasAll(o.Categories...) {
				continue
			}
			resources = append(resources, groupResource{
				APIGroup:        gv.Group,
				APIGroupVersion: gv.String(),
				APIResource:     resource,
			})
			apiResources = append(apiResources, resource)
		}
		if len(apiResources) > 0 {
			apiList.APIResources = apiResources
			filteredList = append(filteredList, apiList)
		}
	}

	if !o.NoHeaders && (*o.PrintFlags.OutputFormat == "" || *o.PrintFlags.OutputFormat == "wide") {
		if err = printContextHeaders(w, *o.PrintFlags.OutputFormat); err != nil {
			return err
		}
	}

	if *o.PrintFlags.OutputFormat == "json" || *o.PrintFlags.OutputFormat == "yaml" {
		flatList := &metav1.APIResourceList{
			TypeMeta: metav1.TypeMeta{
				APIVersion: filteredList[0].APIVersion,
				Kind:       filteredList[0].Kind,
			},
		}
		for _, resource := range filteredList {
			flatList.APIResources = append(flatList.APIResources, resource.APIResources...)
		}
		return o.PrintObj(flatList, w)
	}

	sort.Stable(sortableResource{resources, o.SortBy})
	for _, r := range resources {
		switch *o.PrintFlags.OutputFormat {
		case "name":
			name := r.APIResource.Name
			if len(r.APIGroup) > 0 {
				name += "." + r.APIGroup
			}
			if _, err := fmt.Fprintf(w, "%s\n", name); err != nil {
				errs = append(errs, err)
			}
		case "wide":
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%v\t%v\n",
				r.APIResource.Name,
				strings.Join(r.APIResource.ShortNames, ","),
				r.APIGroupVersion,
				r.APIResource.Namespaced,
				r.APIResource.Kind,
				strings.Join(r.APIResource.Verbs, ","),
				strings.Join(r.APIResource.Categories, ",")); err != nil {
				errs = append(errs, err)
			}
		case "":
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\n",
				r.APIResource.Name,
				strings.Join(r.APIResource.ShortNames, ","),
				r.APIGroupVersion,
				r.APIResource.Namespaced,
				r.APIResource.Kind); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return errors.NewAggregate(errs)
	}
	return nil
}

func printContextHeaders(out io.Writer, output string) error {
	columnNames := []string{"NAME", "SHORTNAMES", "APIVERSION", "NAMESPACED", "KIND"}
	if output == "wide" {
		columnNames = append(columnNames, "VERBS", "CATEGORIES")
	}
	_, err := fmt.Fprintf(out, "%s\n", strings.Join(columnNames, "\t"))
	return err
}

type sortableResource struct {
	resources []groupResource
	sortBy    string
}

func (s sortableResource) Len() int { return len(s.resources) }
func (s sortableResource) Swap(i, j int) {
	s.resources[i], s.resources[j] = s.resources[j], s.resources[i]
}
func (s sortableResource) Less(i, j int) bool {
	ret := strings.Compare(s.compareValues(i, j))
	if ret > 0 {
		return false
	} else if ret == 0 {
		return strings.Compare(s.resources[i].APIResource.Name, s.resources[j].APIResource.Name) < 0
	}
	return true
}

func (s sortableResource) compareValues(i, j int) (string, string) {
	switch s.sortBy {
	case "name":
		return s.resources[i].APIResource.Name, s.resources[j].APIResource.Name
	case "kind":
		return s.resources[i].APIResource.Kind, s.resources[j].APIResource.Kind
	}
	return s.resources[i].APIGroup, s.resources[j].APIGroup
}
