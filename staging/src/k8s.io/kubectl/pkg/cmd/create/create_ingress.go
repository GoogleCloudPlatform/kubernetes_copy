/*
Copyright 2020 The Kubernetes Authors.

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

package create

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	networkingv1client "k8s.io/client-go/kubernetes/typed/networking/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	// Explaining the Regex below:
	// ^(?P<host>[\w\*\-\.]*) -> Indicates the host - 0-N characters of letters, number, underscore, '-', '.' and '*'
	// (?P<path>/.*) -> Indicates the path and MUST start with '/' - / + 0-N characters
	// Separator from host/path to svcname:svcport -> "="
	// (?P<svcname>[\w\-]+) -> Service Name (letters, numbers, '-') -> 1-N characters
	// Separator from svcname to svcport -> ":"
	// (?P<svcport>[\w\-]+) -> Service Port (letters, numbers, '-') -> 1-N characters
	regexHostPathSvc = `^(?P<host>[\w\*\-\.]*)(?P<path>/.*)=(?P<svcname>[\w\-]+):(?P<svcport>[\w\-]+)`

	// This Regex is optional -> (....)?
	// (?P<istls>tls) -> Verify if the argument after "," is 'tls'
	// Optional Separator from tls to the secret name -> "=?"
	// (?P<secretname>[\w\-]+)? -> Optional secret name after the separator -> 1-N characters
	regexTLS = `(,(?P<istls>tls)=?(?P<secretname>[\w\-]+)?)?`

	// The validation Regex is the concatenation of hostPathSvc validation regex
	// and the TLS validation regex
	ruleRegex = regexHostPathSvc + regexTLS

	ingressLong = templates.LongDesc(i18n.T(`
	Create an ingress with the specified name.`))

	ingressExample = templates.Examples(i18n.T(`
		# Create a single ingress called 'simple' that directs requests to foo.com/bar to svc
		# svc1:8080 with a TLS secret "my-cert"
		kubectl create ingress simple --rule="foo.com/bar=svc1:8080,tls=my-cert"

		# Create a catch all ingress of "/path" pointing to service svc:port and Ingress Class as "otheringress"
		kubectl create ingress catch-all --class=otheringress --rule="/path=svc:port"

		# Create an ingress with two annotations: ingress.annotation1 and ingress.annotations2
		kubectl create ingress annotated --class=default --rule="foo.com/bar=svc:port" \
			--annotation ingress.annotation1=foo \
			--annotation ingress.annotation2=bla

		# Create an ingress with the same host and multiple paths
		kubectl create ingress multipath --class=default \
			--rule="foo.com/=svc:port" \
			--rule="foo.com/admin/=svcadmin:portadmin"

		# Create an ingress with multiple hosts and the pathType as Prefix
		kubectl create ingress ingress1 --class=default \
			--rule="foo.com/path*=svc:8080" \
			--rule="bar.com/admin*=svc2:http"

		# Create an ingress with TLS enabled using the default ingress certificate and different path types
		kubectl create ingress ingtls --class=default \
		   --rule="foo.com/=svc:https,tls" \
		   --rule="foo.com/path/subpath*=othersvc:8080"

		# Create an ingress with TLS enabled using a specific secret and pathType as Prefix
		kubectl create ingress ingsecret --class=default \
		   --rule="foo.com/*=svc:8080,tls=secret1"

		# Create an ingress with a default backend
		kubectl create ingress ingdefault --class=default \
		   --default-backend=defaultsvc:http \
		   --rule="foo.com/*=svc:8080,tls=secret1"

		`))
)

// CreateIngressOptions is returned by NewCmdCreateIngress
type CreateIngressOptions struct {
	PrintFlags *genericclioptions.PrintFlags

	PrintObj func(obj runtime.Object) error

	Name             string
	IngressClass     string
	Rules            []string
	Annotations      []string
	DefaultBackend   string
	Namespace        string
	EnforceNamespace bool
	CreateAnnotation bool

	Client              networkingv1client.NetworkingV1Interface
	DryRunStrategy      cmdutil.DryRunStrategy
	ValidationDirective string

	FieldManager string

	genericiooptions.IOStreams
}

// NewCreateIngressOptions creates the CreateIngressOptions to be used later
func NewCreateIngressOptions(ioStreams genericiooptions.IOStreams) *CreateIngressOptions {
	return &CreateIngressOptions{
		PrintFlags: genericclioptions.NewPrintFlags("created").WithTypeSetter(scheme.Scheme),
		IOStreams:  ioStreams,
	}
}

// NewCmdCreateIngress is a macro command to create a new ingress.
// This command is better known to users as `kubectl create ingress`.
func NewCmdCreateIngress(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewCreateIngressOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "ingress NAME --rule=host/path=service:port[,tls[=secret]] ",
		DisableFlagsInUseLine: true,
		Aliases:               []string{"ing"},
		Short:                 i18n.T("Create an ingress with the specified name"),
		Long:                  ingressLong,
		Example:               ingressExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}

	o.PrintFlags.AddFlags(cmd)

	cmdutil.AddApplyAnnotationFlags(cmd)
	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddDryRunFlag(cmd)
	cmd.Flags().StringVar(&o.IngressClass, "class", o.IngressClass, "Ingress Class to be used")
	cmd.Flags().StringArrayVar(&o.Rules, "rule", o.Rules, "Rule in format host/path=service:port[,tls=secretname]. Paths containing the trailing character '*' are considered pathType=Prefix. tls argument is optional.")
	cmd.Flags().StringVar(&o.DefaultBackend, "default-backend", o.DefaultBackend, "Default service for backend, in format of svcname:port")
	cmd.Flags().StringArrayVar(&o.Annotations, "annotation", o.Annotations, "Annotation to insert in the ingress object, in the format annotation=value")
	cmdutil.AddFieldManagerFlagVar(cmd, &o.FieldManager, "kubectl-create")

	return cmd
}

// Complete completes all the options
func (o *CreateIngressOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	name, err := NameFromCommandArgs(cmd, args)
	if err != nil {
		return err
	}
	o.Name = name

	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.Client, err = networkingv1client.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	o.Namespace, o.EnforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.CreateAnnotation = cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)

	o.DryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}
	cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.DryRunStrategy)

	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}
	o.PrintObj = func(obj runtime.Object) error {
		return printer.PrintObj(obj, o.Out)
	}

	o.ValidationDirective, err = cmdutil.GetValidationDirective(cmd)
	return err
}

// Validate validates the Ingress object to be created
func (o *CreateIngressOptions) Validate() error {
	if len(o.DefaultBackend) == 0 && len(o.Rules) == 0 {
		return fmt.Errorf("not enough information provided: every ingress has to either specify a default-backend (which catches all traffic) or a list of rules (which catch specific paths)")
	}

	rulevalidation, err := regexp.Compile(ruleRegex)
	if err != nil {
		return fmt.Errorf("failed to compile the regex")
	}

	for _, rule := range o.Rules {
		if match := rulevalidation.MatchString(rule); !match {
			return fmt.Errorf("rule %s is invalid and should be in format host/path=svcname:svcport[,tls[=secret]]", rule)
		}
	}

	for _, annotation := range o.Annotations {
		if an := strings.SplitN(annotation, "=", 2); len(an) != 2 {
			return fmt.Errorf("annotation %s is invalid and should be in format key=[value]", annotation)
		}
	}

	if len(o.DefaultBackend) > 0 && len(strings.Split(o.DefaultBackend, ":")) != 2 {
		return fmt.Errorf("default-backend should be in format servicename:serviceport")
	}

	return nil
}

// Run performs the execution of 'create ingress' sub command
func (o *CreateIngressOptions) Run() error {
	ingress := o.createIngress()

	if err := util.CreateOrUpdateAnnotation(o.CreateAnnotation, ingress, scheme.DefaultJSONEncoder()); err != nil {
		return err
	}

	if o.DryRunStrategy != cmdutil.DryRunClient {
		createOptions := metav1.CreateOptions{}
		if o.FieldManager != "" {
			createOptions.FieldManager = o.FieldManager
		}
		createOptions.FieldValidation = o.ValidationDirective
		if o.DryRunStrategy == cmdutil.DryRunServer {
			createOptions.DryRun = []string{metav1.DryRunAll}
		}
		var err error
		ingress, err = o.Client.Ingresses(o.Namespace).Create(context.TODO(), ingress, createOptions)
		if err != nil {
			return fmt.Errorf("failed to create ingress: %v", err)
		}
	}
	return o.PrintObj(ingress)
}

func (o *CreateIngressOptions) createIngress() *networkingv1.Ingress {
	namespace := ""
	if o.EnforceNamespace {
		namespace = o.Namespace
	}

	annotations := o.buildAnnotations()
	spec := o.buildIngressSpec()

	ingress := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        o.Name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: spec,
	}
	return ingress
}

func (o *CreateIngressOptions) buildAnnotations() map[string]string {

	var annotations = make(map[string]string)

	for _, annotation := range o.Annotations {
		an := strings.SplitN(annotation, "=", 2)
		annotations[an[0]] = an[1]
	}
	return annotations
}

// buildIngressSpec builds the .spec from the diverse arguments passed to kubectl
func (o *CreateIngressOptions) buildIngressSpec() networkingv1.IngressSpec {
	var ingressSpec networkingv1.IngressSpec

	if len(o.IngressClass) > 0 {
		ingressSpec.IngressClassName = &o.IngressClass
	}

	if len(o.DefaultBackend) > 0 {
		defaultbackend := buildIngressBackendSvc(o.DefaultBackend)
		ingressSpec.DefaultBackend = &defaultbackend
	}
	ingressSpec.TLS = o.buildTLSRules()
	ingressSpec.Rules = o.buildIngressRules()

	return ingressSpec
}

func (o *CreateIngressOptions) buildTLSRules() []networkingv1.IngressTLS {
	hostAlreadyPresent := make(map[string]struct{})

	ingressTLSs := []networkingv1.IngressTLS{}
	var secret string

	for _, rule := range o.Rules {
		tls := strings.Split(rule, ",")

		if len(tls) == 2 {
			ingressTLS := networkingv1.IngressTLS{}
			host := strings.SplitN(rule, "/", 2)[0]
			secret = ""
			secretName := strings.Split(tls[1], "=")

			if len(secretName) > 1 {
				secret = secretName[1]
			}

			idxSecret := getIndexSecret(secret, ingressTLSs)
			// We accept the same host into TLS secrets only once
			if _, ok := hostAlreadyPresent[host]; !ok {
				if idxSecret > -1 {
					ingressTLSs[idxSecret].Hosts = append(ingressTLSs[idxSecret].Hosts, host)
					hostAlreadyPresent[host] = struct{}{}
					continue
				}
				if host != "" {
					ingressTLS.Hosts = append(ingressTLS.Hosts, host)
				}
				if secret != "" {
					ingressTLS.SecretName = secret
				}
				if len(ingressTLS.SecretName) > 0 || len(ingressTLS.Hosts) > 0 {
					ingressTLSs = append(ingressTLSs, ingressTLS)
				}
				hostAlreadyPresent[host] = struct{}{}
			}
		}
	}
	return ingressTLSs
}

// buildIngressRules builds the .spec.rules for an ingress object.
func (o *CreateIngressOptions) buildIngressRules() []networkingv1.IngressRule {
	ingressRules := []networkingv1.IngressRule{}

	for _, rule := range o.Rules {
		removeTLS := strings.Split(rule, ",")[0]
		hostSplit := strings.SplitN(removeTLS, "/", 2)
		host := hostSplit[0]
		ingressPath := buildHTTPIngressPath(hostSplit[1])
		ingressRule := networkingv1.IngressRule{}

		if host != "" {
			ingressRule.Host = host
		}

		idxHost := getIndexHost(ingressRule.Host, ingressRules)
		if idxHost > -1 {
			ingressRules[idxHost].IngressRuleValue.HTTP.Paths = append(ingressRules[idxHost].IngressRuleValue.HTTP.Paths, ingressPath)
			continue
		}

		ingressRule.IngressRuleValue = networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					ingressPath,
				},
			},
		}
		ingressRules = append(ingressRules, ingressRule)
	}
	return ingressRules
}

func buildHTTPIngressPath(pathsvc string) networkingv1.HTTPIngressPath {
	pathsvcsplit := strings.Split(pathsvc, "=")
	path := "/" + pathsvcsplit[0]
	service := pathsvcsplit[1]

	var pathType networkingv1.PathType
	pathType = "Exact"

	// If * in the End, turn pathType=Prefix but remove the * from the end
	if path[len(path)-1:] == "*" {
		pathType = "Prefix"
		path = path[0 : len(path)-1]
	}

	httpIngressPath := networkingv1.HTTPIngressPath{
		Path:     path,
		PathType: &pathType,
		Backend:  buildIngressBackendSvc(service),
	}
	return httpIngressPath
}

func buildIngressBackendSvc(service string) networkingv1.IngressBackend {
	svcname := strings.Split(service, ":")[0]
	svcport := strings.Split(service, ":")[1]

	ingressBackend := networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: svcname,
			Port: parseServiceBackendPort(svcport),
		},
	}
	return ingressBackend
}

func parseServiceBackendPort(port string) networkingv1.ServiceBackendPort {
	var backendPort networkingv1.ServiceBackendPort
	portIntOrStr := intstr.Parse(port)

	if portIntOrStr.Type == intstr.Int {
		backendPort.Number = portIntOrStr.IntVal
	}

	if portIntOrStr.Type == intstr.String {
		backendPort.Name = portIntOrStr.StrVal
	}
	return backendPort
}

func getIndexHost(host string, rules []networkingv1.IngressRule) int {
	for index, v := range rules {
		if v.Host == host {
			return index
		}
	}
	return -1
}

func getIndexSecret(secretname string, tls []networkingv1.IngressTLS) int {
	for index, v := range tls {
		if v.SecretName == secretname {
			return index
		}
	}
	return -1
}
