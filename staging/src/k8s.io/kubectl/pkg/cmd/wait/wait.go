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

package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/jsonpath"
	cmdget "k8s.io/kubectl/pkg/cmd/get"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	waitLong = templates.LongDesc(i18n.T(`
		Experimental: Wait for a specific condition on one or many resources.

		The command takes multiple resources and waits until the specified condition
		is seen in the Status field of every given resource.

		Alternatively, the command can wait for the given set of resources to be created or
		deleted by providing the "create" or "delete" keyword as the value to the --for flag.

		A successful message will be printed to stdout indicating when the specified
        condition has been met. You can use -o option to change to output destination.`))

	waitExample = templates.Examples(i18n.T(`
		# Wait for the pod "busybox1" to contain the status condition of type "Ready"
		kubectl wait --for=condition=Ready pod/busybox1

		# The default value of status condition is true; you can wait for other targets after an equal delimiter (compared after Unicode simple case folding, which is a more general form of case-insensitivity)
		kubectl wait --for=condition=Ready=false pod/busybox1

		# Wait for the pod "busybox1" to contain the status phase to be "Running"
		kubectl wait --for=jsonpath='{.status.phase}'=Running pod/busybox1

		# Wait for pod "busybox1" to be Ready
		kubectl wait --for='jsonpath={.status.conditions[?(@.type=="Ready")].status}=True' pod/busybox1

		# Wait for the service "loadbalancer" to have ingress
		kubectl wait --for=jsonpath='{.status.loadBalancer.ingress}' service/loadbalancer

		# Wait for the secret "busybox1" to be created, with a timeout of 30s
		kubectl create secret generic busybox1
		kubectl wait --for=create secret/busybox1 --timeout=30s

		# Wait for the pod "busybox1" to be deleted, with a timeout of 60s, after having issued the "delete" command
		kubectl delete pod/busybox1
		kubectl wait --for=delete pod/busybox1 --timeout=60s`))
)

// errNoMatchingResources is returned when there is no resources matching a query.
var errNoMatchingResources = errors.New("no matching resources found")

// WaitFlags directly reflect the information that CLI is gathering via flags.  They will be converted to Options, which
// reflect the runtime requirements for the command.  This structure reduces the transformation to wiring and makes
// the logic itself easy to unit test
type WaitFlags struct {
	RESTClientGetter     genericclioptions.RESTClientGetter
	PrintFlags           *genericclioptions.PrintFlags
	ResourceBuilderFlags *genericclioptions.ResourceBuilderFlags

	Timeout      time.Duration
	ForCondition string

	genericiooptions.IOStreams
}

// NewWaitFlags returns a default WaitFlags
func NewWaitFlags(restClientGetter genericclioptions.RESTClientGetter, streams genericiooptions.IOStreams) *WaitFlags {
	return &WaitFlags{
		RESTClientGetter: restClientGetter,
		PrintFlags:       genericclioptions.NewPrintFlags("condition met"),
		ResourceBuilderFlags: genericclioptions.NewResourceBuilderFlags().
			WithLabelSelector("").
			WithFieldSelector("").
			WithAll(false).
			WithAllNamespaces(false).
			WithLocal(false).
			WithLatest(),

		Timeout: 30 * time.Second,

		IOStreams: streams,
	}
}

// NewCmdWait returns a cobra command for waiting
func NewCmdWait(restClientGetter genericclioptions.RESTClientGetter, streams genericiooptions.IOStreams) *cobra.Command {
	flags := NewWaitFlags(restClientGetter, streams)

	cmd := &cobra.Command{
		Use:     "wait ([-f FILENAME] | resource.group/resource.name | resource.group [(-l label | --all)]) [--for=create|--for=delete|--for condition=available|--for=jsonpath='{}'[=value]]",
		Short:   i18n.T("Experimental: Wait for a specific condition on one or many resources"),
		Long:    waitLong,
		Example: waitExample,

		DisableFlagsInUseLine: true,
		Run: func(cmd *cobra.Command, args []string) {
			o, err := flags.ToOptions(args)
			cmdutil.CheckErr(err)
			cmdutil.CheckErr(o.RunWait())
		},
		SuggestFor: []string{"list", "ps"},
	}

	flags.AddFlags(cmd)

	return cmd
}

// AddFlags registers flags for a cli
func (flags *WaitFlags) AddFlags(cmd *cobra.Command) {
	flags.PrintFlags.AddFlags(cmd)
	flags.ResourceBuilderFlags.AddFlags(cmd.Flags())

	cmd.Flags().DurationVar(&flags.Timeout, "timeout", flags.Timeout, "The length of time to wait before giving up. Zero means check once and don't wait, negative means wait for a week.")
	cmd.Flags().StringVar(&flags.ForCondition, "for", flags.ForCondition, "The condition to wait on: [create|delete|condition=condition-name[=condition-value]|jsonpath='{JSONPath expression}'=[JSONPath value]]. The default condition-value is True. Condition values are compared after Unicode simple case folding, which is a more general form of case-insensitivity.  Conditions that have an observed generation lower than the resource's generation are ignored.")
}

// ToOptions converts from CLI inputs to runtime inputs
func (flags *WaitFlags) ToOptions(args []string) (*WaitOptions, error) {
	printer, err := flags.PrintFlags.ToPrinter()
	if err != nil {
		return nil, err
	}
	builder := flags.ResourceBuilderFlags.ToBuilder(flags.RESTClientGetter, args)
	clientConfig, err := flags.RESTClientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}
	conditionFn, err := conditionFuncFor(flags.ForCondition, flags.ErrOut)
	if err != nil {
		return nil, err
	}

	effectiveTimeout := flags.Timeout
	if effectiveTimeout < 0 {
		effectiveTimeout = 168 * time.Hour
	}

	o := &WaitOptions{
		ResourceFinder: builder,
		DynamicClient:  dynamicClient,
		Timeout:        effectiveTimeout,
		ForCondition:   flags.ForCondition,

		Printer:     printer,
		ConditionFn: conditionFn,
		IOStreams:   flags.IOStreams,
	}

	return o, nil
}

func conditionFuncFor(condition string, errOut io.Writer) (ConditionFunc, error) {
	lowercaseCond := strings.ToLower(condition)
	switch {
	case lowercaseCond == "delete":
		return IsDeleted, nil

	case lowercaseCond == "create":
		return IsCreated, nil

	case strings.HasPrefix(condition, "condition="):
		conditionName := strings.TrimPrefix(condition, "condition=")
		conditionValue := "true"
		if equalsIndex := strings.Index(conditionName, "="); equalsIndex != -1 {
			conditionValue = conditionName[equalsIndex+1:]
			conditionName = conditionName[0:equalsIndex]
		}

		return ConditionalWait{
			conditionName:   conditionName,
			conditionStatus: conditionValue,
			errOut:          errOut,
		}.IsConditionMet, nil

	case strings.HasPrefix(condition, "jsonpath="):
		jsonPathInput := strings.TrimPrefix(condition, "jsonpath=")
		jsonPathExp, jsonPathValue, err := processJSONPathInput(jsonPathInput)
		if err != nil {
			return nil, err
		}
		j, err := newJSONPathParser(jsonPathExp)
		if err != nil {
			return nil, err
		}
		return JSONPathWait{
			matchAnyValue:  jsonPathValue == "",
			jsonPathValue:  jsonPathValue,
			jsonPathParser: j,
			errOut:         errOut,
		}.IsJSONPathConditionMet, nil
	}

	return nil, fmt.Errorf("unrecognized condition: %q", condition)
}

// newJSONPathParser will create a new JSONPath parser based on the jsonPathExpression
func newJSONPathParser(jsonPathExpression string) (*jsonpath.JSONPath, error) {
	j := jsonpath.New("wait").AllowMissingKeys(true)
	if jsonPathExpression == "" {
		return nil, errors.New("jsonpath expression cannot be empty")
	}
	if err := j.Parse(jsonPathExpression); err != nil {
		return nil, err
	}
	return j, nil
}

// processJSONPathInput will parse and process the provided JSONPath input containing a JSON expression and optionally
// a value for the matching condition.
func processJSONPathInput(input string) (string, string, error) {
	jsonPathInput := splitJSONPathInput(input)
	if numOfArgs := len(jsonPathInput); numOfArgs < 1 || numOfArgs > 2 {
		return "", "", fmt.Errorf("jsonpath wait format must be --for=jsonpath='{.status.readyReplicas}'=3 or --for=jsonpath='{.status.readyReplicas}'")
	}
	relaxedJSONPathExp, err := cmdget.RelaxedJSONPathExpression(jsonPathInput[0])
	if err != nil {
		return "", "", err
	}
	if len(jsonPathInput) == 1 {
		return relaxedJSONPathExp, "", nil
	}
	jsonPathValue := strings.Trim(jsonPathInput[1], `'"`)
	if jsonPathValue == "" {
		return "", "", errors.New("jsonpath wait has to have a value after equal sign, like --for=jsonpath='{.status.readyReplicas}'=3")
	}
	return relaxedJSONPathExp, jsonPathValue, nil
}

// splitJSONPathInput splits the provided input string on single '='. Double '==' will not cause the string to be
// split. E.g., "a.b.c====d.e.f===g.h.i===" will split to ["a.b.c====d.e.f==","g.h.i==",""].
func splitJSONPathInput(input string) []string {
	var output []string
	var element strings.Builder
	for i := 0; i < len(input); i++ {
		if input[i] == '=' {
			if i < len(input)-1 && input[i+1] == '=' {
				element.WriteString("==")
				i++
				continue
			}
			output = append(output, element.String())
			element.Reset()
			continue
		}
		element.WriteByte(input[i])
	}
	return append(output, element.String())
}

// ResourceLocation holds the location of a resource
type ResourceLocation struct {
	GroupResource schema.GroupResource
	Namespace     string
	Name          string
}

// UIDMap maps ResourceLocation with UID
type UIDMap map[ResourceLocation]types.UID

// WaitOptions is a set of options that allows you to wait.  This is the object reflects the runtime needs of a wait
// command, making the logic itself easy to unit test with our existing mocks.
type WaitOptions struct {
	ResourceFinder genericclioptions.ResourceFinder
	// UIDMap maps a resource location to a UID.  It is optional, but ConditionFuncs may choose to use it to make the result
	// more reliable.  For instance, delete can look for UID consistency during delegated calls.
	UIDMap        UIDMap
	DynamicClient dynamic.Interface
	Timeout       time.Duration
	ForCondition  string

	Printer     printers.ResourcePrinter
	ConditionFn ConditionFunc
	genericiooptions.IOStreams
}

// ConditionFunc is the interface for providing condition checks
type ConditionFunc func(ctx context.Context, info *resource.Info, o *WaitOptions) (finalObject runtime.Object, done bool, err error)

// RunWait runs the waiting logic
func (o *WaitOptions) RunWait() error {
	ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), o.Timeout)
	defer cancel()

	if strings.ToLower(o.ForCondition) == "create" {
		// TODO(soltysh): this is not ideal solution, because we're polling every .5s,
		// and we have to use ResourceFinder, which contains the resource name.
		// In the long run, we should expose resource information from ResourceFinder,
		// or functions from ResourceBuilder for parsing those. Lastly, this poll
		// should be replaced with a ListWatch cache.
		if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, o.Timeout, true, func(context.Context) (done bool, err error) {
			foundResource := false
			visitErr := o.ResourceFinder.Do().Visit(func(info *resource.Info, err error) error {
				foundResource = true
				return nil
			})
			if apierrors.IsNotFound(visitErr) {
				return false, nil
			}
			if visitErr != nil {
				return false, visitErr
			}
			return foundResource, nil
		}); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%s", wait.ErrWaitTimeout.Error()) // nolint:staticcheck // SA1019
			}
			return err
		}
	}

	visitCount := 0
	visitFunc := func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		visitCount++
		finalObject, success, err := o.ConditionFn(ctx, info, o)
		if success {
			o.Printer.PrintObj(finalObject, o.Out)
			return nil
		}
		if err == nil {
			return fmt.Errorf("%v unsatisfied for unknown reason", finalObject)
		}
		return err
	}
	visitor := o.ResourceFinder.Do()
	isForDelete := strings.ToLower(o.ForCondition) == "delete"
	if visitor, ok := visitor.(*resource.Result); ok && isForDelete {
		visitor.IgnoreErrors(apierrors.IsNotFound)
	}

	err := visitor.Visit(visitFunc)
	if err != nil {
		return err
	}
	if visitCount == 0 && !isForDelete {
		return errNoMatchingResources
	}
	return err
}
