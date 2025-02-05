/*
Copyright 2021 The Kubernetes Authors.

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

package completion

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/kubectl/pkg/cmd/apiresources"
	"k8s.io/kubectl/pkg/cmd/get"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
)

var factory cmdutil.Factory

// SetFactoryForCompletion Store the factory which is needed by the completion functions.
// Not all commands have access to the factory, so cannot pass it to the completion functions.
func SetFactoryForCompletion(f cmdutil.Factory) {
	factory = f
}

// ResourceTypeAndNameCompletionFunc Returns a completion function that completes resource types
// and resource names that match the toComplete prefix.  It supports the <type>/<name> form.
func ResourceTypeAndNameCompletionFunc(f cmdutil.Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return resourceTypeAndNameCompletionFunc(f, nil, true)
}

// SpecifiedResourceTypeAndNameCompletionFunc Returns a completion function that completes resource
// types limited to the specified allowedTypes, and resource names that match the toComplete prefix.
// It allows for multiple resources. It supports the <type>/<name> form.
func SpecifiedResourceTypeAndNameCompletionFunc(f cmdutil.Factory, allowedTypes []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return resourceTypeAndNameCompletionFunc(f, allowedTypes, true)
}

// SpecifiedResourceTypeAndNameNoRepeatCompletionFunc Returns a completion function that completes resource
// types limited to the specified allowedTypes, and resource names that match the toComplete prefix.
// It only allows for one resource. It supports the <type>/<name> form.
func SpecifiedResourceTypeAndNameNoRepeatCompletionFunc(f cmdutil.Factory, allowedTypes []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return resourceTypeAndNameCompletionFunc(f, allowedTypes, false)
}

// ResourceNameCompletionFunc Returns a completion function that completes as a first argument
// the resource names specified by the resourceType parameter, and which match the toComplete prefix.
// This function does NOT support the <type>/<name> form: it is meant to be used by commands
// that don't support that form.  For commands that apply to pods and that support the <type>/<name>
// form, please use PodResourceNameCompletionFunc()
func ResourceNameCompletionFunc(f cmdutil.Factory, resourceType string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var comps []string
		if len(args) == 0 {
			comps = CompGetResource(f, resourceType, toComplete)
		}
		return comps, cobra.ShellCompDirectiveNoFileComp
	}
}

// PodResourceNameCompletionFunc Returns a completion function that completes:
// 1- pod names that match the toComplete prefix
// 2- resource types containing pods which match the toComplete prefix
func PodResourceNameCompletionFunc(f cmdutil.Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var comps []string
		directive := cobra.ShellCompDirectiveNoFileComp
		if len(args) == 0 {
			comps, directive = doPodResourceCompletion(f, toComplete)
		}
		return comps, directive
	}
}

// ResourceAndPortCompletionFunc Returns a completion function that completes, as a first argument:
// 1- resources that match the toComplete prefix
// 2- the ports of the specific resource. i.e: container ports for pod resources and port for services
func ResourceAndPortCompletionFunc(f cmdutil.Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var comps []string
		var resourceType string
		directive := cobra.ShellCompDirectiveNoFileComp
		if len(args) == 0 {
			comps, directive = doPodResourceCompletion(f, toComplete)
		} else if len(args) == 1 {
			t := strings.Split(args[0], "/")
			// if we specify directly the pod, then resource type is pod, otherwise it would be the resource type passed
			// by the user
			if len(t) == 1 {
				resourceType = "pod"
			} else {
				resourceType = t[0]
			}
			if resourceType == "service" || resourceType == "services" || resourceType == "svc" {
				comps = CompGetServicePorts(f, t[1], toComplete)
			} else {
				podName := convertResourceNameToPodName(f, args[0])
				comps = CompGetPodContainerPorts(f, podName, toComplete)
			}
		}
		return comps, directive
	}
}

// PodResourceNameAndContainerCompletionFunc Returns a completion function that completes, as a first argument:
// 1- pod names that match the toComplete prefix
// 2- resource types containing pods which match the toComplete prefix
// and as a second argument the containers within the specified pod.
func PodResourceNameAndContainerCompletionFunc(f cmdutil.Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var comps []string
		directive := cobra.ShellCompDirectiveNoFileComp
		if len(args) == 0 {
			comps, directive = doPodResourceCompletion(f, toComplete)
		} else if len(args) == 1 {
			podName := convertResourceNameToPodName(f, args[0])
			comps = CompGetContainers(f, podName, toComplete)
		}
		return comps, directive
	}
}

// ContainerCompletionFunc Returns a completion function that completes the containers within the
// pod specified by the first argument.  The resource containing the pod can be specified in
// the <type>/<name> form.
func ContainerCompletionFunc(f cmdutil.Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var comps []string
		// We need the pod name to be able to complete the container names, it must be in args[0].
		// That first argument can also be of the form <type>/<name> so we need to convert it.
		if len(args) > 0 {
			podName := convertResourceNameToPodName(f, args[0])
			comps = CompGetContainers(f, podName, toComplete)
		}
		return comps, cobra.ShellCompDirectiveNoFileComp
	}
}

// ContextCompletionFunc is a completion function that completes as a first argument the
// context names that match the toComplete prefix
func ContextCompletionFunc(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return ListContextsInConfig(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// ClusterCompletionFunc is a completion function that completes as a first argument the
// cluster names that match the toComplete prefix
func ClusterCompletionFunc(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return ListClustersInConfig(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// UserCompletionFunc is a completion function that completes as a first argument the
// user names that match the toComplete prefix
func UserCompletionFunc(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return ListUsersInConfig(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// CompGetResource gets the list of the resource specified which begin with `toComplete`.
func CompGetResource(f cmdutil.Factory, resourceName string, toComplete string) []string {
	template := "{{ range .items  }}{{ .metadata.name }} {{ end }}"
	return CompGetFromTemplate(&template, f, "", []string{resourceName}, toComplete)
}

// CompGetContainers gets the list of containers of the specified pod which begin with `toComplete`.
func CompGetContainers(f cmdutil.Factory, podName string, toComplete string) []string {
	template := "{{ range .spec.initContainers }}{{ .name }} {{end}}{{ range .spec.containers  }}{{ .name }} {{ end }}"
	return CompGetFromTemplate(&template, f, "", []string{"pod", podName}, toComplete)
}

// CompGetPodContainerPorts retrieves the list of ports for containers within the specified pod that start with `toComplete`.
func CompGetPodContainerPorts(f cmdutil.Factory, podName string, toComplete string) []string {
	var template string
	exposedPort := strings.Split(toComplete, ":")[0]

	if toComplete == "" {
		exposedPort = "{{ .containerPort }}"
	}
	template = fmt.Sprintf("{{ range .spec.containers }}{{ range .ports }}%s:{{ .containerPort }} {{ end }}{{ end }}", exposedPort)
	return CompGetFromTemplate(&template, f, "", []string{"pod", podName}, toComplete)
}

// CompGetServicePorts gets the list of ports of the specified service which begin with `toComplete`.
func CompGetServicePorts(f cmdutil.Factory, serviceName string, toComplete string) []string {
	var template string
	exposedPort := strings.Split(toComplete, ":")[0]

	if toComplete == "" {
		exposedPort = "{{ .port }}"
	}
	template = fmt.Sprintf("{{ range .spec.ports }}%s:{{ .port }} {{ end }}", exposedPort)
	return CompGetFromTemplate(&template, f, "", []string{"service", serviceName}, toComplete)
}

// CompGetFromTemplate executes a Get operation using the specified template and args and returns the results
// which begin with `toComplete`.
func CompGetFromTemplate(template *string, f cmdutil.Factory, namespace string, args []string, toComplete string) []string {
	buf := new(bytes.Buffer)
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: buf, ErrOut: io.Discard}
	o := get.NewGetOptions("kubectl", streams)

	// Get the list of names of the specified resource
	o.PrintFlags.TemplateFlags.GoTemplatePrintFlags.TemplateArgument = template
	format := "go-template"
	o.PrintFlags.OutputFormat = &format

	// Do the steps Complete() would have done.
	// We cannot actually call Complete() or Validate() as these function check for
	// the presence of flags, which, in our case won't be there
	if namespace != "" {
		o.Namespace = namespace
		o.ExplicitNamespace = true
	} else {
		var err error
		o.Namespace, o.ExplicitNamespace, err = f.ToRawKubeConfigLoader().Namespace()
		if err != nil {
			return nil
		}
	}

	o.ToPrinter = func(mapping *meta.RESTMapping, outputObjects *bool, withNamespace bool, withKind bool) (printers.ResourcePrinterFunc, error) {
		printer, err := o.PrintFlags.ToPrinter()
		if err != nil {
			return nil, err
		}
		return printer.PrintObj, nil
	}

	o.Run(f, args)

	var comps []string
	resources := strings.Split(buf.String(), " ")
	for _, res := range resources {
		if res != "" && strings.HasPrefix(res, toComplete) {
			comps = append(comps, res)
		}
	}
	return comps
}

// ListContextsInConfig returns a list of context names which begin with `toComplete`
func ListContextsInConfig(toComplete string) []string {
	config, err := factory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil
	}
	var ret []string
	for name := range config.Contexts {
		if strings.HasPrefix(name, toComplete) {
			ret = append(ret, name)
		}
	}
	return ret
}

// ListClustersInConfig returns a list of cluster names which begin with `toComplete`
func ListClustersInConfig(toComplete string) []string {
	config, err := factory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil
	}
	var ret []string
	for name := range config.Clusters {
		if strings.HasPrefix(name, toComplete) {
			ret = append(ret, name)
		}
	}
	return ret
}

// ListUsersInConfig returns a list of user names which begin with `toComplete`
func ListUsersInConfig(toComplete string) []string {
	config, err := factory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil
	}
	var ret []string
	for name := range config.AuthInfos {
		if strings.HasPrefix(name, toComplete) {
			ret = append(ret, name)
		}
	}
	return ret
}

// compGetResourceList returns the list of api resources which begin with `toComplete`.
func compGetResourceList(restClientGetter genericclioptions.RESTClientGetter, cmd *cobra.Command, toComplete string) []string {
	buf := new(bytes.Buffer)
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: buf, ErrOut: io.Discard}
	o := apiresources.NewAPIResourceOptions(streams)

	o.Complete(restClientGetter, cmd, nil)

	// Get the list of resources
	*o.PrintFlags.OutputFormat = "name"
	o.Cached = true
	o.Verbs = []string{"get"}
	// TODO:Should set --request-timeout=5s

	// Ignore errors as the output may still be valid
	o.RunAPIResources()

	// Resources can be a comma-separated list.  The last element is then
	// the one we should complete.  For example if toComplete=="pods,secre"
	// we should return "pods,secrets"
	prefix := ""
	suffix := toComplete
	lastIdx := strings.LastIndex(toComplete, ",")
	if lastIdx != -1 {
		prefix = toComplete[0 : lastIdx+1]
		suffix = toComplete[lastIdx+1:]
	}
	var comps []string
	resources := strings.Split(buf.String(), "\n")
	for _, res := range resources {
		if res != "" && strings.HasPrefix(res, suffix) {
			comps = append(comps, fmt.Sprintf("%s%s", prefix, res))
		}
	}
	return comps
}

// resourceTypeAndNameCompletionFunc Returns a completion function that completes resource types
// and resource names that match the toComplete prefix.  It supports the <type>/<name> form.
func resourceTypeAndNameCompletionFunc(f cmdutil.Factory, allowedTypes []string, allowRepeat bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var comps []string
		directive := cobra.ShellCompDirectiveNoFileComp

		if len(args) > 0 && !strings.Contains(args[0], "/") {
			// The first argument is of the form <type> (e.g., pods)
			// All following arguments should be a resource name.
			if allowRepeat || len(args) == 1 {
				comps = CompGetResource(f, args[0], toComplete)

				// Remove choices already on the command-line
				if len(args) > 1 {
					comps = cmdutil.Difference(comps, args[1:])
				}
			}
		} else {
			slashIdx := strings.Index(toComplete, "/")
			if slashIdx == -1 {
				if len(args) == 0 {
					// We are completing the first argument.  We default to the normal
					// <type> form (not the form <type>/<name>).
					// So we suggest resource types and let the shell add a space after
					// the completion.
					if len(allowedTypes) == 0 {
						comps = compGetResourceList(f, cmd, toComplete)
					} else {
						for _, c := range allowedTypes {
							if strings.HasPrefix(c, toComplete) {
								comps = append(comps, c)
							}
						}
					}
				} else {
					// Here we know the first argument contains a / (<type>/<name>).
					// All other arguments must also use that form.
					if allowRepeat {
						// Since toComplete does not already contain a / we know we are completing a
						// resource type. Disable adding a space after the completion, and add the /
						directive |= cobra.ShellCompDirectiveNoSpace

						if len(allowedTypes) == 0 {
							typeComps := compGetResourceList(f, cmd, toComplete)
							for _, c := range typeComps {
								comps = append(comps, fmt.Sprintf("%s/", c))
							}
						} else {
							for _, c := range allowedTypes {
								if strings.HasPrefix(c, toComplete) {
									comps = append(comps, fmt.Sprintf("%s/", c))
								}
							}
						}
					}
				}
			} else {
				// We are completing an argument of the form <type>/<name>
				// and since the / is already present, we are completing the resource name.
				if allowRepeat || len(args) == 0 {
					resourceType := toComplete[:slashIdx]
					toComplete = toComplete[slashIdx+1:]
					nameComps := CompGetResource(f, resourceType, toComplete)
					for _, c := range nameComps {
						comps = append(comps, fmt.Sprintf("%s/%s", resourceType, c))
					}

					// Remove choices already on the command-line.
					if len(args) > 0 {
						comps = cmdutil.Difference(comps, args[0:])
					}
				}
			}
		}
		return comps, directive
	}
}

// doPodResourceCompletion Returns completions of:
// 1- pod names that match the toComplete prefix
// 2- resource types containing pods which match the toComplete prefix
func doPodResourceCompletion(f cmdutil.Factory, toComplete string) ([]string, cobra.ShellCompDirective) {
	var comps []string
	directive := cobra.ShellCompDirectiveNoFileComp
	slashIdx := strings.Index(toComplete, "/")
	if slashIdx == -1 {
		// Standard case, complete pod names
		comps = CompGetResource(f, "pod", toComplete)

		// Also include resource choices for the <type>/<name> form,
		// but only for resources that contain pods
		resourcesWithPods := []string{
			"daemonsets",
			"deployments",
			"pods",
			"jobs",
			"replicasets",
			"replicationcontrollers",
			"services",
			"statefulsets"}

		if len(comps) == 0 {
			// If there are no pods to complete, we will only be completing
			// <type>/.  We should disable adding a space after the /.
			directive |= cobra.ShellCompDirectiveNoSpace
		}

		for _, resource := range resourcesWithPods {
			if strings.HasPrefix(resource, toComplete) {
				comps = append(comps, fmt.Sprintf("%s/", resource))
			}
		}
	} else {
		// Dealing with the <type>/<name> form, use the specified resource type
		resourceType := toComplete[:slashIdx]
		toComplete = toComplete[slashIdx+1:]
		nameComps := CompGetResource(f, resourceType, toComplete)
		for _, c := range nameComps {
			comps = append(comps, fmt.Sprintf("%s/%s", resourceType, c))
		}
	}
	return comps, directive
}

// convertResourceNameToPodName Converts a resource name to a pod name.
// If the resource name is of the form <type>/<name>, we use
// polymorphichelpers.AttachablePodForObjectFn(), if not, the resource name
// is already a pod name.
func convertResourceNameToPodName(f cmdutil.Factory, resourceName string) string {
	var podName string
	if !strings.Contains(resourceName, "/") {
		// When we don't have the <type>/<name> form, the resource name is the pod name
		podName = resourceName
	} else {
		// if the resource name is of the form <type>/<name>, we need to convert it to a pod name
		ns, _, err := f.ToRawKubeConfigLoader().Namespace()
		if err != nil {
			return ""
		}

		resourceWithPod, err := f.NewBuilder().
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			ContinueOnError().
			NamespaceParam(ns).DefaultNamespace().
			ResourceNames("pods", resourceName).
			Do().Object()
		if err != nil {
			return ""
		}

		// For shell completion, use a short timeout
		forwardablePod, err := polymorphichelpers.AttachablePodForObjectFn(f, resourceWithPod, 100*time.Millisecond)
		if err != nil {
			return ""
		}
		podName = forwardablePod.Name
	}
	return podName
}
