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

package util

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	utilexec "k8s.io/utils/exec"
)

const (
	ApplyAnnotationsFlag = "save-config"
	DefaultErrorExitCode = 1
	DefaultChunkSize     = 500
)

type debugError interface {
	DebugError() (msg string, args []interface{})
}

// AddSourceToErr adds handleResourcePrefix and source string to error message.
// verb is the string like "creating", "deleting" etc.
// source is the filename or URL to the template file(*.json or *.yaml), or stdin to use to handle the resource.
func AddSourceToErr(verb string, source string, err error) error {
	if source != "" {
		if statusError, ok := err.(apierrors.APIStatus); ok {
			status := statusError.Status()
			status.Message = fmt.Sprintf("error when %s %q: %v", verb, source, status.Message)
			return &apierrors.StatusError{ErrStatus: status}
		}
		return fmt.Errorf("error when %s %q: %v", verb, source, err)
	}
	return err
}

var fatalErrHandler = fatal

// BehaviorOnFatal allows you to override the default behavior when a fatal
// error occurs, which is to call os.Exit(code). You can pass 'panic' as a function
// here if you prefer the panic() over os.Exit(1).
func BehaviorOnFatal(f func(string, int)) {
	fatalErrHandler = f
}

// DefaultBehaviorOnFatal allows you to undo any previous override.  Useful in
// tests.
func DefaultBehaviorOnFatal() {
	fatalErrHandler = fatal
}

// fatal prints the message (if provided) and then exits. If V(99) or greater,
// klog.Fatal is invoked for extended information. This is intended for maintainer
// debugging and out of a reasonable range for users.
func fatal(msg string, code int) {
	// nolint:logcheck // Not using the result of klog.V(99) inside the if
	// branch is okay, we just use it to determine how to terminate.
	if klog.V(99).Enabled() {
		klog.FatalDepth(2, msg)
	}
	if len(msg) > 0 {
		// add newline if needed
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		fmt.Fprint(os.Stderr, msg)
	}
	os.Exit(code)
}

// ErrExit may be passed to CheckError to instruct it to output nothing but exit with
// status code 1.
var ErrExit = fmt.Errorf("exit")

// CheckErr prints a user friendly error to STDERR and exits with a non-zero
// exit code. Unrecognized errors will be printed with an "error: " prefix.
//
// This method is generic to the command in use and may be used by non-Kubectl
// commands.
func CheckErr(err error) {
	checkErr(err, fatalErrHandler)
}

// CheckDiffErr prints a user friendly error to STDERR and exits with a
// non-zero and non-one exit code. Unrecognized errors will be printed
// with an "error: " prefix.
//
// This method is meant specifically for `kubectl diff` and may be used
// by other commands.
func CheckDiffErr(err error) {
	checkErr(err, func(msg string, code int) {
		fatalErrHandler(msg, code+1)
	})
}

// isInvalidReasonStatusError returns true if this is an API Status error with reason=Invalid.
// This is distinct from generic 422 errors we want to fall back to generic error handling.
func isInvalidReasonStatusError(err error) bool {
	if !apierrors.IsInvalid(err) {
		return false
	}
	statusError, isStatusError := err.(*apierrors.StatusError)
	if !isStatusError {
		return false
	}
	status := statusError.Status()
	return status.Reason == metav1.StatusReasonInvalid
}

// checkErr formats a given error as a string and calls the passed handleErr
// func with that string and an kubectl exit code.
func checkErr(err error, handleErr func(string, int)) {
	// unwrap aggregates of 1
	if agg, ok := err.(utilerrors.Aggregate); ok && len(agg.Errors()) == 1 {
		err = agg.Errors()[0]
	}

	if err == nil {
		return
	}

	switch {
	case err == ErrExit:
		handleErr("", DefaultErrorExitCode)
	case isInvalidReasonStatusError(err):
		status := err.(*apierrors.StatusError).Status()
		details := status.Details
		s := "The request is invalid"
		if details == nil {
			// if we have no other details, include the message from the server if present
			if len(status.Message) > 0 {
				s += ": " + status.Message
			}
			handleErr(s, DefaultErrorExitCode)
			return
		}
		if len(details.Kind) != 0 || len(details.Name) != 0 {
			s = fmt.Sprintf("The %s %q is invalid", details.Kind, details.Name)
		} else if len(status.Message) > 0 && len(details.Causes) == 0 {
			// only append the message if we have no kind/name details and no causes,
			// since default invalid error constructors duplicate that information in the message
			s += ": " + status.Message
		}

		if len(details.Causes) > 0 {
			errs := statusCausesToAggrError(details.Causes)
			handleErr(MultilineError(s+": ", errs), DefaultErrorExitCode)
		} else {
			handleErr(s, DefaultErrorExitCode)
		}
	case clientcmd.IsConfigurationInvalid(err):
		handleErr(MultilineError("Error in configuration: ", err), DefaultErrorExitCode)
	default:
		switch err := err.(type) {
		case *meta.NoResourceMatchError:
			switch {
			case len(err.PartialResource.Group) > 0 && len(err.PartialResource.Version) > 0:
				handleErr(fmt.Sprintf("the server doesn't have a resource type %q in group %q and version %q", err.PartialResource.Resource, err.PartialResource.Group, err.PartialResource.Version), DefaultErrorExitCode)
			case len(err.PartialResource.Group) > 0:
				handleErr(fmt.Sprintf("the server doesn't have a resource type %q in group %q", err.PartialResource.Resource, err.PartialResource.Group), DefaultErrorExitCode)
			case len(err.PartialResource.Version) > 0:
				handleErr(fmt.Sprintf("the server doesn't have a resource type %q in version %q", err.PartialResource.Resource, err.PartialResource.Version), DefaultErrorExitCode)
			default:
				handleErr(fmt.Sprintf("the server doesn't have a resource type %q", err.PartialResource.Resource), DefaultErrorExitCode)
			}
		case utilerrors.Aggregate:
			handleErr(MultipleErrors(``, err.Errors()), DefaultErrorExitCode)
		case utilexec.ExitError:
			handleErr(err.Error(), err.ExitStatus())
		default: // for any other error type
			msg, ok := StandardErrorMessage(err)
			if !ok {
				msg = err.Error()
				if !strings.HasPrefix(msg, "error: ") {
					msg = fmt.Sprintf("error: %s", msg)
				}
			}
			handleErr(msg, DefaultErrorExitCode)
		}
	}
}

func statusCausesToAggrError(scs []metav1.StatusCause) utilerrors.Aggregate {
	errs := make([]error, 0, len(scs))
	errorMsgs := sets.New[string]()
	for _, sc := range scs {
		// check for duplicate error messages and skip them
		msg := fmt.Sprintf("%s: %s", sc.Field, sc.Message)
		if errorMsgs.Has(msg) {
			continue
		}
		errorMsgs.Insert(msg)
		errs = append(errs, errors.New(msg))
	}
	return utilerrors.NewAggregate(errs)
}

// StandardErrorMessage translates common errors into a human readable message, or returns
// false if the error is not one of the recognized types. It may also log extended
// information to klog.
//
// This method is generic to the command in use and may be used by non-Kubectl
// commands.
func StandardErrorMessage(err error) (string, bool) {
	if debugErr, ok := err.(debugError); ok {
		klog.V(4).Info(debugErr.DebugError())
	}
	status, isStatus := err.(apierrors.APIStatus)
	switch {
	case isStatus:
		switch s := status.Status(); {
		case s.Reason == metav1.StatusReasonUnauthorized:
			return fmt.Sprintf("error: You must be logged in to the server (%s)", s.Message), true
		case len(s.Reason) > 0:
			return fmt.Sprintf("Error from server (%s): %s", s.Reason, err.Error()), true
		default:
			return fmt.Sprintf("Error from server: %s", err.Error()), true
		}
	case apierrors.IsUnexpectedObjectError(err):
		return fmt.Sprintf("Server returned an unexpected response: %s", err.Error()), true
	}
	switch t := err.(type) {
	case *url.Error:
		klog.V(4).Infof("Connection error: %s %s: %v", t.Op, t.URL, t.Err)
		switch {
		case strings.Contains(t.Err.Error(), "connection refused"):
			host := t.URL
			if server, err := url.Parse(t.URL); err == nil {
				host = server.Host
			}
			return fmt.Sprintf("The connection to the server %s was refused - did you specify the right host or port?", host), true
		}
		return fmt.Sprintf("Unable to connect to the server: %v", t.Err), true
	}
	return "", false
}

// MultilineError returns a string representing an error that splits sub errors into their own
// lines. The returned string will end with a newline.
func MultilineError(prefix string, err error) string {
	if agg, ok := err.(utilerrors.Aggregate); ok {
		errs := utilerrors.Flatten(agg).Errors()
		buf := &bytes.Buffer{}
		switch len(errs) {
		case 0:
			return fmt.Sprintf("%s%v\n", prefix, err)
		case 1:
			return fmt.Sprintf("%s%v\n", prefix, messageForError(errs[0]))
		default:
			fmt.Fprintln(buf, prefix)
			for _, err := range errs {
				fmt.Fprintf(buf, "* %v\n", messageForError(err))
			}
			return buf.String()
		}
	}
	return fmt.Sprintf("%s%s\n", prefix, err)
}

// PrintErrorWithCauses prints an error's kind, name, and each of the error's causes in a new line.
// The returned string will end with a newline.
// Returns true if a case exists to handle the error type, or false otherwise.
func PrintErrorWithCauses(err error, errOut io.Writer) bool {
	switch t := err.(type) {
	case *apierrors.StatusError:
		errorDetails := t.Status().Details
		if errorDetails != nil {
			fmt.Fprintf(errOut, "error: %s %q is invalid\n\n", errorDetails.Kind, errorDetails.Name)
			for _, cause := range errorDetails.Causes {
				fmt.Fprintf(errOut, "* %s: %s\n", cause.Field, cause.Message)
			}
			return true
		}
	}

	fmt.Fprintf(errOut, "error: %v\n", err)
	return false
}

// MultipleErrors returns a newline delimited string containing
// the prefix and referenced errors in standard form.
func MultipleErrors(prefix string, errs []error) string {
	buf := &bytes.Buffer{}
	for _, err := range errs {
		fmt.Fprintf(buf, "%s%v\n", prefix, messageForError(err))
	}
	return buf.String()
}

// messageForError returns the string representing the error.
func messageForError(err error) string {
	msg, ok := StandardErrorMessage(err)
	if !ok {
		msg = err.Error()
	}
	return msg
}

func UsageErrorf(cmd *cobra.Command, format string, args ...interface{}) error {
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s\nSee '%s -h' for help and examples", msg, cmd.CommandPath())
}

func IsFilenameSliceEmpty(filenames []string, directory string) bool {
	return len(filenames) == 0 && directory == ""
}

func GetFlagString(cmd *cobra.Command, flag string) string {
	s, err := cmd.Flags().GetString(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return s
}

// GetFlagStringSlice can be used to accept multiple argument with flag repetition (e.g. -f arg1,arg2 -f arg3 ...)
func GetFlagStringSlice(cmd *cobra.Command, flag string) []string {
	s, err := cmd.Flags().GetStringSlice(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return s
}

// GetFlagStringArray can be used to accept multiple argument with flag repetition (e.g. -f arg1 -f arg2 ...)
func GetFlagStringArray(cmd *cobra.Command, flag string) []string {
	s, err := cmd.Flags().GetStringArray(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return s
}

func GetFlagBool(cmd *cobra.Command, flag string) bool {
	b, err := cmd.Flags().GetBool(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return b
}

// Assumes the flag has a default value.
func GetFlagInt(cmd *cobra.Command, flag string) int {
	i, err := cmd.Flags().GetInt(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return i
}

// Assumes the flag has a default value.
func GetFlagInt32(cmd *cobra.Command, flag string) int32 {
	i, err := cmd.Flags().GetInt32(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return i
}

// Assumes the flag has a default value.
func GetFlagInt64(cmd *cobra.Command, flag string) int64 {
	i, err := cmd.Flags().GetInt64(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return i
}

func GetFlagDuration(cmd *cobra.Command, flag string) time.Duration {
	d, err := cmd.Flags().GetDuration(flag)
	if err != nil {
		klog.Fatalf("error accessing flag %s for command %s: %v", flag, cmd.Name(), err)
	}
	return d
}

func GetPodRunningTimeoutFlag(cmd *cobra.Command) (time.Duration, error) {
	timeout := GetFlagDuration(cmd, "pod-running-timeout")
	if timeout <= 0 {
		return timeout, fmt.Errorf("--pod-running-timeout must be higher than zero")
	}
	return timeout, nil
}

type FeatureGate string

const (
	ApplySet                FeatureGate = "KUBECTL_APPLYSET"
	CmdPluginAsSubcommand   FeatureGate = "KUBECTL_ENABLE_CMD_SHADOW"
	OpenAPIV3Patch          FeatureGate = "KUBECTL_OPENAPIV3_PATCH"
	RemoteCommandWebsockets FeatureGate = "KUBECTL_REMOTE_COMMAND_WEBSOCKETS"
	PortForwardWebsockets   FeatureGate = "KUBECTL_PORT_FORWARD_WEBSOCKETS"
	// DebugCustomProfile should be dropped in 1.34
	DebugCustomProfile FeatureGate = "KUBECTL_DEBUG_CUSTOM_PROFILE"
	KubeRC             FeatureGate = "KUBECTL_KUBERC"
)

// IsEnabled returns true iff environment variable is set to true.
// All other cases, it returns false.
func (f FeatureGate) IsEnabled() bool {
	return strings.ToLower(os.Getenv(string(f))) == "true"
}

// IsDisabled returns true iff environment variable is set to false.
// All other cases, it returns true.
// This function is used for the cases where feature is enabled by default,
// but it may be needed to provide a way to ability to disable this feature.
func (f FeatureGate) IsDisabled() bool {
	return strings.ToLower(os.Getenv(string(f))) == "false"
}

func AddValidateFlags(cmd *cobra.Command) {
	cmd.Flags().String(
		"validate",
		"strict",
		`Must be one of: strict (or true), warn, ignore (or false). "true" or "strict" will use a schema to validate the input and fail the request if invalid. It will perform server side validation if ServerSideFieldValidation is enabled on the api-server, but will fall back to less reliable client-side validation if not. "warn" will warn about unknown or duplicate fields without blocking the request if server-side field validation is enabled on the API server, and behave as "ignore" otherwise. "false" or "ignore" will not perform any schema validation, silently dropping any unknown or duplicate fields.`,
	)

	cmd.Flags().Lookup("validate").NoOptDefVal = "strict"
}

func AddFilenameOptionFlags(cmd *cobra.Command, options *resource.FilenameOptions, usage string) {
	AddJsonFilenameFlag(cmd.Flags(), &options.Filenames, "Filename, directory, or URL to files "+usage)
	AddKustomizeFlag(cmd.Flags(), &options.Kustomize)
	cmd.Flags().BoolVarP(&options.Recursive, "recursive", "R", options.Recursive, "Process the directory used in -f, --filename recursively. Useful when you want to manage related manifests organized within the same directory.")
}

func AddJsonFilenameFlag(flags *pflag.FlagSet, value *[]string, usage string) {
	flags.StringSliceVarP(value, "filename", "f", *value, usage)
	annotations := make([]string, 0, len(resource.FileExtensions))
	for _, ext := range resource.FileExtensions {
		annotations = append(annotations, strings.TrimLeft(ext, "."))
	}
	flags.SetAnnotation("filename", cobra.BashCompFilenameExt, annotations)
}

// AddKustomizeFlag adds kustomize flag to a command
func AddKustomizeFlag(flags *pflag.FlagSet, value *string) {
	flags.StringVarP(value, "kustomize", "k", *value, "Process the kustomization directory. This flag can't be used together with -f or -R.")
}

// AddDryRunFlag adds dry-run flag to a command. Usually used by mutations.
func AddDryRunFlag(cmd *cobra.Command) {
	cmd.Flags().String(
		"dry-run",
		"none",
		`Must be "none", "server", or "client". If client strategy, only print the object that would be sent, without sending it. If server strategy, submit server-side request without persisting the resource.`,
	)
	cmd.Flags().Lookup("dry-run").NoOptDefVal = "unchanged"
}

func AddFieldManagerFlagVar(cmd *cobra.Command, p *string, defaultFieldManager string) {
	cmd.Flags().StringVar(p, "field-manager", defaultFieldManager, "Name of the manager used to track field ownership.")
}

func AddContainerVarFlags(cmd *cobra.Command, p *string, containerName string) {
	cmd.Flags().StringVarP(p, "container", "c", containerName, "Container name. If omitted, use the kubectl.kubernetes.io/default-container annotation for selecting the container to be attached or the first container in the pod will be chosen")
}

func AddServerSideApplyFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("server-side", false, "If true, apply runs in the server instead of the client.")
	cmd.Flags().Bool("force-conflicts", false, "If true, server-side apply will force the changes against conflicts.")
}

func AddPodRunningTimeoutFlag(cmd *cobra.Command, defaultTimeout time.Duration) {
	cmd.Flags().Duration("pod-running-timeout", defaultTimeout, "The length of time (like 5s, 2m, or 3h, higher than zero) to wait until at least one pod is running")
}

func AddApplyAnnotationFlags(cmd *cobra.Command) {
	cmd.Flags().Bool(ApplyAnnotationsFlag, false, "If true, the configuration of current object will be saved in its annotation. Otherwise, the annotation will be unchanged. This flag is useful when you want to perform kubectl apply on this object in the future.")
}

func AddApplyAnnotationVarFlags(cmd *cobra.Command, applyAnnotation *bool) {
	cmd.Flags().BoolVar(applyAnnotation, ApplyAnnotationsFlag, *applyAnnotation, "If true, the configuration of current object will be saved in its annotation. Otherwise, the annotation will be unchanged. This flag is useful when you want to perform kubectl apply on this object in the future.")
}

func AddChunkSizeFlag(cmd *cobra.Command, value *int64) {
	cmd.Flags().Int64Var(value, "chunk-size", *value,
		"Return large lists in chunks rather than all at once. Pass 0 to disable. This flag is beta and may change in the future.")
}

func AddLabelSelectorFlagVar(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVarP(p, "selector", "l", *p, "Selector (label query) to filter on, supports '=', '==', '!=', 'in', 'notin'.(e.g. -l key1=value1,key2=value2,key3 in (value3)). Matching objects must satisfy all of the specified label constraints.")
}

func AddPruningFlags(cmd *cobra.Command, prune *bool, pruneAllowlist *[]string, all *bool, applySetRef *string) {
	// Flags associated with the original allowlist-based alpha
	cmd.Flags().StringArrayVar(pruneAllowlist, "prune-allowlist", *pruneAllowlist, "Overwrite the default allowlist with <group/version/kind> for --prune")
	cmd.Flags().BoolVar(all, "all", *all, "Select all resources in the namespace of the specified resource types.")

	// Flags associated with the new ApplySet-based alpha
	if ApplySet.IsEnabled() {
		cmd.Flags().StringVar(applySetRef, "applyset", *applySetRef, "[alpha] The name of the ApplySet that tracks which resources are being managed, for the purposes of determining what to prune. Live resources that are part of the ApplySet but have been removed from the provided configs will be deleted. Format: [RESOURCE][.GROUP]/NAME. A Secret will be used if no resource or group is specified.")
		cmd.Flags().BoolVar(prune, "prune", *prune, "Automatically delete previously applied resource objects that do not appear in the provided configs. For alpha1, use with either -l or --all. For alpha2, use with --applyset.")
	} else {
		// different docs for the shared --prune flag if only alpha1 is enabled
		cmd.Flags().BoolVar(prune, "prune", *prune, "Automatically delete resource objects, that do not appear in the configs and are created by either apply or create --save-config. Should be used with either -l or --all.")
	}
}

func AddSubresourceFlags(cmd *cobra.Command, subresource *string, usage string) {
	cmd.Flags().StringVar(subresource, "subresource", "", fmt.Sprintf("%s This flag is beta and may change in the future.", usage))
	CheckErr(cmd.RegisterFlagCompletionFunc("subresource", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		var commonSubresources = []string{"status", "scale", "resize"}
		return commonSubresources, cobra.ShellCompDirectiveNoFileComp
	}))
}

type ValidateOptions struct {
	ValidationDirective string
}

// Merge converts the passed in object to JSON, merges the fragment into it using an RFC7396 JSON Merge Patch,
// and returns the resulting object
// TODO: merge assumes JSON serialization, and does not properly abstract API retrieval
func Merge(codec runtime.Codec, dst runtime.Object, fragment string) (runtime.Object, error) {
	// encode dst into versioned json and apply fragment directly too it
	target, err := runtime.Encode(codec, dst)
	if err != nil {
		return nil, err
	}
	patched, err := jsonpatch.MergePatch(target, []byte(fragment))
	if err != nil {
		return nil, err
	}
	out, err := runtime.Decode(codec, patched)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StrategicMerge converts the passed in object to JSON, merges the fragment into it using a Strategic Merge Patch,
// and returns the resulting object
func StrategicMerge(codec runtime.Codec, dst runtime.Object, fragment string, dataStruct runtime.Object) (runtime.Object, error) {
	target, err := runtime.Encode(codec, dst)
	if err != nil {
		return nil, err
	}
	patched, err := strategicpatch.StrategicMergePatch(target, []byte(fragment), dataStruct)
	if err != nil {
		return nil, err
	}
	out, err := runtime.Decode(codec, patched)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// JSONPatch converts the passed in object to JSON, performs an RFC6902 JSON Patch using operations specified in the
// fragment, and returns the resulting object
func JSONPatch(codec runtime.Codec, dst runtime.Object, fragment string) (runtime.Object, error) {
	target, err := runtime.Encode(codec, dst)
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.DecodePatch([]byte(fragment))
	if err != nil {
		return nil, err
	}
	patched, err := patch.Apply(target)
	if err != nil {
		return nil, err
	}
	out, err := runtime.Decode(codec, patched)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DumpReaderToFile writes all data from the given io.Reader to the specified file
// (usually for temporary use).
func DumpReaderToFile(reader io.Reader, filename string) error {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	buffer := make([]byte, 1024)
	for {
		count, err := reader.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		_, err = f.Write(buffer[:count])
		if err != nil {
			return err
		}
	}
	return nil
}

func GetServerSideApplyFlag(cmd *cobra.Command) bool {
	return GetFlagBool(cmd, "server-side")
}

func GetForceConflictsFlag(cmd *cobra.Command) bool {
	return GetFlagBool(cmd, "force-conflicts")
}

func GetFieldManagerFlag(cmd *cobra.Command) string {
	return GetFlagString(cmd, "field-manager")
}

func GetValidationDirective(cmd *cobra.Command) (string, error) {
	var validateFlag = GetFlagString(cmd, "validate")
	b, err := strconv.ParseBool(validateFlag)
	if err != nil {
		switch validateFlag {
		case "strict":
			return metav1.FieldValidationStrict, nil
		case "warn":
			return metav1.FieldValidationWarn, nil
		case "ignore":
			return metav1.FieldValidationIgnore, nil
		default:
			return metav1.FieldValidationStrict, fmt.Errorf(`invalid - validate option %q; must be one of: strict (or true), warn, ignore (or false)`, validateFlag)
		}
	}
	// The flag was a boolean
	if b {
		return metav1.FieldValidationStrict, nil
	}
	return metav1.FieldValidationIgnore, nil
}

type DryRunStrategy int

const (
	// DryRunNone indicates the client will make all mutating calls
	DryRunNone DryRunStrategy = iota

	// DryRunClient, or client-side dry-run, indicates the client will prevent
	// making mutating calls such as CREATE, PATCH, and DELETE
	DryRunClient

	// DryRunServer, or server-side dry-run, indicates the client will send
	// mutating calls to the APIServer with the dry-run parameter to prevent
	// persisting changes.
	//
	// Note that clients sending server-side dry-run calls should verify that
	// the APIServer and the resource supports server-side dry-run, and otherwise
	// clients should fail early.
	//
	// If a client sends a server-side dry-run call to an APIServer that doesn't
	// support server-side dry-run, then the APIServer will persist changes inadvertently.
	DryRunServer
)

func GetDryRunStrategy(cmd *cobra.Command) (DryRunStrategy, error) {
	var dryRunFlag = GetFlagString(cmd, "dry-run")
	b, err := strconv.ParseBool(dryRunFlag)
	// The flag is not a boolean
	if err != nil {
		switch dryRunFlag {
		case cmd.Flag("dry-run").NoOptDefVal:
			klog.Warning(`--dry-run is deprecated and can be replaced with --dry-run=client.`)
			return DryRunClient, nil
		case "client":
			return DryRunClient, nil
		case "server":
			return DryRunServer, nil
		case "none":
			return DryRunNone, nil
		default:
			return DryRunNone, fmt.Errorf(`Invalid dry-run value (%v). Must be "none", "server", or "client".`, dryRunFlag)
		}
	}
	// The flag was a boolean
	if b {
		klog.Warningf(`--dry-run=%v is deprecated (boolean value) and can be replaced with --dry-run=%s.`, dryRunFlag, "client")
		return DryRunClient, nil
	}
	klog.Warningf(`--dry-run=%v is deprecated (boolean value) and can be replaced with --dry-run=%s.`, dryRunFlag, "none")
	return DryRunNone, nil
}

// PrintFlagsWithDryRunStrategy sets a success message at print time for the dry run strategy
//
// TODO(juanvallejo): This can be cleaned up even further by creating
// a PrintFlags struct that binds the --dry-run flag, and whose
// ToPrinter method returns a printer that understands how to print
// this success message.
func PrintFlagsWithDryRunStrategy(printFlags *genericclioptions.PrintFlags, dryRunStrategy DryRunStrategy) *genericclioptions.PrintFlags {
	switch dryRunStrategy {
	case DryRunClient:
		printFlags.Complete("%s (dry run)")
	case DryRunServer:
		printFlags.Complete("%s (server dry run)")
	}
	return printFlags
}

// GetResourcesAndPairs retrieves resources and "KEY=VALUE or KEY-" pair args from given args
func GetResourcesAndPairs(args []string, pairType string) (resources []string, pairArgs []string, err error) {
	foundPair := false
	for _, s := range args {
		nonResource := (strings.Contains(s, "=") && s[0] != '=') || (strings.HasSuffix(s, "-") && s != "-")
		switch {
		case !foundPair && nonResource:
			foundPair = true
			fallthrough
		case foundPair && nonResource:
			pairArgs = append(pairArgs, s)
		case !foundPair && !nonResource:
			resources = append(resources, s)
		case foundPair && !nonResource:
			err = fmt.Errorf("all resources must be specified before %s changes: %s", pairType, s)
			return
		}
	}
	return
}

// ParsePairs retrieves new and remove pairs (if supportRemove is true) from "KEY=VALUE or KEY-" pair args
func ParsePairs(pairArgs []string, pairType string, supportRemove bool) (newPairs map[string]string, removePairs []string, err error) {
	newPairs = map[string]string{}
	if supportRemove {
		removePairs = []string{}
	}
	var invalidBuf bytes.Buffer
	var invalidBufNonEmpty bool
	for _, pairArg := range pairArgs {
		if strings.Contains(pairArg, "=") && pairArg[0] != '=' {
			parts := strings.SplitN(pairArg, "=", 2)
			if len(parts) != 2 {
				if invalidBufNonEmpty {
					invalidBuf.WriteString(", ")
				}
				invalidBuf.WriteString(pairArg)
				invalidBufNonEmpty = true
			} else {
				newPairs[parts[0]] = parts[1]
			}
		} else if supportRemove && strings.HasSuffix(pairArg, "-") && pairArg != "-" {
			removePairs = append(removePairs, pairArg[:len(pairArg)-1])
		} else {
			if invalidBufNonEmpty {
				invalidBuf.WriteString(", ")
			}
			invalidBuf.WriteString(pairArg)
			invalidBufNonEmpty = true
		}
	}
	if invalidBufNonEmpty {
		err = fmt.Errorf("invalid %s format: %s", pairType, invalidBuf.String())
		return
	}

	return
}

// IsSiblingCommandExists receives a pointer to a cobra command and a target string.
// Returns true if the target string is found in the list of sibling commands.
func IsSiblingCommandExists(cmd *cobra.Command, targetCmdName string) bool {
	for _, c := range cmd.Parent().Commands() {
		if c.Name() == targetCmdName {
			return true
		}
	}

	return false
}

// DefaultSubCommandRun prints a command's help string to the specified output if no
// arguments (sub-commands) are provided, or a usage error otherwise.
func DefaultSubCommandRun(out io.Writer) func(c *cobra.Command, args []string) {
	return func(c *cobra.Command, args []string) {
		c.SetOut(out)
		c.SetErr(out)
		RequireNoArguments(c, args)
		c.Help()
		CheckErr(ErrExit)
	}
}

// RequireNoArguments exits with a usage error if extra arguments are provided.
func RequireNoArguments(c *cobra.Command, args []string) {
	if len(args) > 0 {
		CheckErr(UsageErrorf(c, "unknown command %q", strings.Join(args, " ")))
	}
}

// StripComments will transform a YAML file into JSON, thus dropping any comments
// in it. Note that if the given file has a syntax error, the transformation will
// fail and we will manually drop all comments from the file.
func StripComments(file []byte) []byte {
	stripped := file
	stripped, err := yaml.ToJSON(stripped)
	if err != nil {
		stripped = ManualStrip(file)
	}
	return stripped
}

// ManualStrip is used for dropping comments from a YAML file
func ManualStrip(file []byte) []byte {
	stripped := []byte{}
	lines := bytes.Split(file, []byte("\n"))
	for i, line := range lines {
		trimline := bytes.TrimSpace(line)

		if bytes.HasPrefix(trimline, []byte("#")) && !bytes.HasPrefix(trimline, []byte("#!")) {
			continue
		}
		stripped = append(stripped, line...)
		if i < len(lines)-1 {
			stripped = append(stripped, '\n')
		}
	}
	return stripped
}

// ScaleClientFunc provides a ScalesGetter
type ScaleClientFunc func(genericclioptions.RESTClientGetter) (scale.ScalesGetter, error)

// ScaleClientFn gives a way to easily override the function for unit testing if needed.
var ScaleClientFn ScaleClientFunc = scaleClient

// scaleClient gives you back scale getter
func scaleClient(restClientGetter genericclioptions.RESTClientGetter) (scale.ScalesGetter, error) {
	discoveryClient, err := restClientGetter.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	clientConfig, err := restClientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	setKubernetesDefaults(clientConfig)
	restClient, err := rest.RESTClientFor(clientConfig)
	if err != nil {
		return nil, err
	}
	resolver := scale.NewDiscoveryScaleKindResolver(discoveryClient)
	mapper, err := restClientGetter.ToRESTMapper()
	if err != nil {
		return nil, err
	}

	return scale.New(restClient, mapper, dynamic.LegacyAPIPathResolverFunc, resolver), nil
}

func Warning(cmdErr io.Writer, newGeneratorName, oldGeneratorName string) {
	fmt.Fprintf(cmdErr, "WARNING: New generator %q specified, "+
		"but it isn't available. "+
		"Falling back to %q.\n",
		newGeneratorName,
		oldGeneratorName,
	)
}

// Difference removes any elements of subArray from fullArray and returns the result
func Difference(fullArray []string, subArray []string) []string {
	exclude := make(map[string]bool, len(subArray))
	for _, elem := range subArray {
		exclude[elem] = true
	}
	var result []string
	for _, elem := range fullArray {
		if _, found := exclude[elem]; !found {
			result = append(result, elem)
		}
	}
	return result
}
