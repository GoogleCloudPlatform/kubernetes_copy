/*
Copyright 2017 The Kubernetes Authors.

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

package validation

import (
	"fmt"
	"net/netip"
	"strings"

	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	unversionedvalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	api "k8s.io/kubernetes/pkg/apis/core"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/apis/networking"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

const (
	annotationIngressClass       = "kubernetes.io/ingress.class"
	maxLenIngressClassController = 250
)

var (
	supportedPathTypes = sets.NewString(
		string(networking.PathTypeExact),
		string(networking.PathTypePrefix),
		string(networking.PathTypeImplementationSpecific),
	)
	invalidPathSequences = []string{"//", "/./", "/../", "%2f", "%2F"}
	invalidPathSuffixes  = []string{"/..", "/."}

	supportedIngressClassParametersReferenceScopes = sets.NewString(
		networking.IngressClassParametersReferenceScopeNamespace,
		networking.IngressClassParametersReferenceScopeCluster,
	)
)

type NetworkPolicyValidationOptions struct {
	AllowInvalidLabelValueInSelector bool
}

// ValidateNetworkPolicyName can be used to check whether the given networkpolicy
// name is valid.
func ValidateNetworkPolicyName(name string, prefix bool) []string {
	return apimachineryvalidation.NameIsDNSSubdomain(name, prefix)
}

// ValidateNetworkPolicyPort validates a NetworkPolicyPort
func ValidateNetworkPolicyPort(port *networking.NetworkPolicyPort, portPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if port.Protocol != nil && *port.Protocol != api.ProtocolTCP && *port.Protocol != api.ProtocolUDP && *port.Protocol != api.ProtocolSCTP {
		allErrs = append(allErrs, field.NotSupported(portPath.Child("protocol"), *port.Protocol, []string{string(api.ProtocolTCP), string(api.ProtocolUDP), string(api.ProtocolSCTP)}))
	}
	if port.Port != nil {
		if port.Port.Type == intstr.Int {
			for _, msg := range validation.IsValidPortNum(int(port.Port.IntVal)) {
				allErrs = append(allErrs, field.Invalid(portPath.Child("port"), port.Port.IntVal, msg))
			}
			if port.EndPort != nil {
				if *port.EndPort < port.Port.IntVal {
					allErrs = append(allErrs, field.Invalid(portPath.Child("endPort"), port.Port.IntVal, "must be greater than or equal to `port`"))
				}
				for _, msg := range validation.IsValidPortNum(int(*port.EndPort)) {
					allErrs = append(allErrs, field.Invalid(portPath.Child("endPort"), *port.EndPort, msg))
				}
			}
		} else {
			if port.EndPort != nil {
				allErrs = append(allErrs, field.Invalid(portPath.Child("endPort"), *port.EndPort, "may not be specified when `port` is non-numeric"))
			}
			for _, msg := range validation.IsValidPortName(port.Port.StrVal) {
				allErrs = append(allErrs, field.Invalid(portPath.Child("port"), port.Port.StrVal, msg))
			}
		}
	} else {
		if port.EndPort != nil {
			allErrs = append(allErrs, field.Invalid(portPath.Child("endPort"), *port.EndPort, "may not be specified when `port` is not specified"))
		}
	}

	return allErrs
}

// ValidateNetworkPolicyPeer validates a NetworkPolicyPeer
func ValidateNetworkPolicyPeer(peer *networking.NetworkPolicyPeer, opts NetworkPolicyValidationOptions, peerPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	numPeers := 0
	labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector,
	}

	if peer.PodSelector != nil {
		numPeers++
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(peer.PodSelector, labelSelectorValidationOpts, peerPath.Child("podSelector"))...)
	}
	if peer.NamespaceSelector != nil {
		numPeers++
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(peer.NamespaceSelector, labelSelectorValidationOpts, peerPath.Child("namespaceSelector"))...)
	}
	if peer.IPBlock != nil {
		numPeers++
		allErrs = append(allErrs, ValidateIPBlock(peer.IPBlock, peerPath.Child("ipBlock"))...)
	}

	if numPeers == 0 {
		allErrs = append(allErrs, field.Required(peerPath, "must specify a peer"))
	} else if numPeers > 1 && peer.IPBlock != nil {
		allErrs = append(allErrs, field.Forbidden(peerPath, "may not specify both ipBlock and another peer"))
	}

	return allErrs
}

// ValidateNetworkPolicySpec tests if required fields in the networkpolicy spec are set.
func ValidateNetworkPolicySpec(spec *networking.NetworkPolicySpec, opts NetworkPolicyValidationOptions, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector,
	}
	allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(
		&spec.PodSelector,
		labelSelectorValidationOpts,
		fldPath.Child("podSelector"),
	)...)

	// Validate ingress rules.
	for i, ingress := range spec.Ingress {
		ingressPath := fldPath.Child("ingress").Index(i)
		for i, port := range ingress.Ports {
			portPath := ingressPath.Child("ports").Index(i)
			allErrs = append(allErrs, ValidateNetworkPolicyPort(&port, portPath)...)
		}
		for i, from := range ingress.From {
			fromPath := ingressPath.Child("from").Index(i)
			allErrs = append(allErrs, ValidateNetworkPolicyPeer(&from, opts, fromPath)...)
		}
	}
	// Validate egress rules
	for i, egress := range spec.Egress {
		egressPath := fldPath.Child("egress").Index(i)
		for i, port := range egress.Ports {
			portPath := egressPath.Child("ports").Index(i)
			allErrs = append(allErrs, ValidateNetworkPolicyPort(&port, portPath)...)
		}
		for i, to := range egress.To {
			toPath := egressPath.Child("to").Index(i)
			allErrs = append(allErrs, ValidateNetworkPolicyPeer(&to, opts, toPath)...)
		}
	}
	// Validate PolicyTypes
	allowed := sets.NewString(string(networking.PolicyTypeIngress), string(networking.PolicyTypeEgress))
	if len(spec.PolicyTypes) > len(allowed) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("policyTypes"), &spec.PolicyTypes, "may not specify more than two policyTypes"))
		return allErrs
	}
	for i, pType := range spec.PolicyTypes {
		policyPath := fldPath.Child("policyTypes").Index(i)
		if !allowed.Has(string(pType)) {
			allErrs = append(allErrs, field.NotSupported(policyPath, pType, []string{string(networking.PolicyTypeIngress), string(networking.PolicyTypeEgress)}))
		}
	}
	return allErrs
}

// ValidateNetworkPolicy validates a networkpolicy.
func ValidateNetworkPolicy(np *networking.NetworkPolicy, opts NetworkPolicyValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&np.ObjectMeta, true, ValidateNetworkPolicyName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateNetworkPolicySpec(&np.Spec, opts, field.NewPath("spec"))...)
	return allErrs
}

// ValidationOptionsForNetworking generates NetworkPolicyValidationOptions for Networking
func ValidationOptionsForNetworking(new, old *networking.NetworkPolicy) NetworkPolicyValidationOptions {
	opts := NetworkPolicyValidationOptions{
		AllowInvalidLabelValueInSelector: false,
	}
	if old != nil {
		labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{
			AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector,
		}
		if len(unversionedvalidation.ValidateLabelSelector(&old.Spec.PodSelector, labelSelectorValidationOpts, nil)) > 0 {
			opts.AllowInvalidLabelValueInSelector = true
		}
	}
	return opts
}

// ValidateNetworkPolicyUpdate tests if an update to a NetworkPolicy is valid.
func ValidateNetworkPolicyUpdate(update, old *networking.NetworkPolicy, opts NetworkPolicyValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateObjectMetaUpdate(&update.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))...)
	allErrs = append(allErrs, ValidateNetworkPolicySpec(&update.Spec, opts, field.NewPath("spec"))...)
	return allErrs
}

// ValidateIPBlock validates a cidr and the except fields of an IpBlock NetworkPolicyPeer
func ValidateIPBlock(ipb *networking.IPBlock, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if ipb.CIDR == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("cidr"), ""))
		return allErrs
	}
	allErrs = append(allErrs, validation.IsValidCIDR(fldPath.Child("cidr"), ipb.CIDR)...)
	_, cidrIPNet, err := netutils.ParseCIDRSloppy(ipb.CIDR)
	if err != nil {
		// Implies validation would have failed so we already added errors for it.
		return allErrs
	}

	for i, exceptCIDRStr := range ipb.Except {
		exceptPath := fldPath.Child("except").Index(i)
		allErrs = append(allErrs, validation.IsValidCIDR(exceptPath, exceptCIDRStr)...)
		_, exceptCIDR, err := netutils.ParseCIDRSloppy(exceptCIDRStr)
		if err != nil {
			// Implies validation would have failed so we already added errors for it.
			continue
		}

		cidrMaskLen, _ := cidrIPNet.Mask.Size()
		exceptMaskLen, _ := exceptCIDR.Mask.Size()
		if !cidrIPNet.Contains(exceptCIDR.IP) || cidrMaskLen >= exceptMaskLen {
			allErrs = append(allErrs, field.Invalid(exceptPath, exceptCIDRStr, "must be a strict subset of `cidr`"))
		}
	}
	return allErrs
}

// ValidateIngressName validates that the given name can be used as an Ingress
// name.
var ValidateIngressName = apimachineryvalidation.NameIsDNSSubdomain

// IngressValidationOptions cover beta to GA transitions for HTTP PathType
type IngressValidationOptions struct {
	// AllowInvalidSecretName indicates whether spec.tls[*].secretName values that are not valid Secret names should be allowed
	AllowInvalidSecretName bool

	// AllowInvalidWildcardHostRule indicates whether invalid rule values are allowed in rules with wildcard hostnames
	AllowInvalidWildcardHostRule bool
}

// ValidateIngress validates Ingresses on create and update.
func validateIngress(ingress *networking.Ingress, opts IngressValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&ingress.ObjectMeta, true, ValidateIngressName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateIngressSpec(&ingress.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateIngressCreate validates Ingresses on create.
func ValidateIngressCreate(ingress *networking.Ingress) field.ErrorList {
	allErrs := field.ErrorList{}
	opts := IngressValidationOptions{
		AllowInvalidSecretName:       false,
		AllowInvalidWildcardHostRule: false,
	}
	allErrs = append(allErrs, validateIngress(ingress, opts)...)
	annotationVal, annotationIsSet := ingress.Annotations[annotationIngressClass]
	if annotationIsSet && ingress.Spec.IngressClassName != nil && annotationVal != *ingress.Spec.IngressClassName {
		annotationPath := field.NewPath("annotations").Child(annotationIngressClass)
		allErrs = append(allErrs, field.Invalid(annotationPath, annotationVal, "must match `ingressClassName` when both are specified"))
	}
	return allErrs
}

// ValidateIngressUpdate validates ingresses on update.
func ValidateIngressUpdate(ingress, oldIngress *networking.Ingress) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&ingress.ObjectMeta, &oldIngress.ObjectMeta, field.NewPath("metadata"))
	opts := IngressValidationOptions{
		AllowInvalidSecretName:       allowInvalidSecretName(oldIngress),
		AllowInvalidWildcardHostRule: allowInvalidWildcardHostRule(oldIngress),
	}

	allErrs = append(allErrs, validateIngress(ingress, opts)...)
	return allErrs
}

func validateIngressTLS(spec *networking.IngressSpec, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	// TODO: Perform a more thorough validation of spec.TLS.Hosts that takes
	// the wildcard spec from RFC 6125 into account.
	for tlsIndex, itls := range spec.TLS {
		for i, host := range itls.Hosts {
			if strings.Contains(host, "*") {
				for _, msg := range validation.IsWildcardDNS1123Subdomain(host) {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(tlsIndex).Child("hosts").Index(i), host, msg))
				}
				continue
			}
			for _, msg := range validation.IsDNS1123Subdomain(host) {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(tlsIndex).Child("hosts").Index(i), host, msg))
			}
		}

		if !opts.AllowInvalidSecretName {
			for _, msg := range validateTLSSecretName(itls.SecretName) {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(tlsIndex).Child("secretName"), itls.SecretName, msg))
			}
		}
	}

	return allErrs
}

// ValidateIngressSpec tests if required fields in the IngressSpec are set.
func ValidateIngressSpec(spec *networking.IngressSpec, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(spec.Rules) == 0 && spec.DefaultBackend == nil {
		errMsg := fmt.Sprintf("either `%s` or `rules` must be specified", "defaultBackend")
		allErrs = append(allErrs, field.Invalid(fldPath, spec.Rules, errMsg))
	}
	if spec.DefaultBackend != nil {
		allErrs = append(allErrs, validateIngressBackend(spec.DefaultBackend, fldPath.Child("defaultBackend"), opts)...)
	}
	if len(spec.Rules) > 0 {
		allErrs = append(allErrs, validateIngressRules(spec.Rules, fldPath.Child("rules"), opts)...)
	}
	if len(spec.TLS) > 0 {
		allErrs = append(allErrs, validateIngressTLS(spec, fldPath.Child("tls"), opts)...)
	}
	if spec.IngressClassName != nil {
		for _, msg := range ValidateIngressClassName(*spec.IngressClassName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("ingressClassName"), *spec.IngressClassName, msg))
		}
	}
	return allErrs
}

// ValidateIngressStatusUpdate tests if required fields in the Ingress are set when updating status.
func ValidateIngressStatusUpdate(ingress, oldIngress *networking.Ingress) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&ingress.ObjectMeta, &oldIngress.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateIngressLoadBalancerStatus(&ingress.Status.LoadBalancer, field.NewPath("status", "loadBalancer"))...)
	return allErrs
}

// ValidateIngressLoadBalancerStatus validates required fields on an IngressLoadBalancerStatus
func ValidateIngressLoadBalancerStatus(status *networking.IngressLoadBalancerStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, ingress := range status.Ingress {
		idxPath := fldPath.Child("ingress").Index(i)
		if len(ingress.IP) > 0 {
			allErrs = append(allErrs, validation.IsValidIP(idxPath.Child("ip"), ingress.IP)...)
		}
		if len(ingress.Hostname) > 0 {
			for _, msg := range validation.IsDNS1123Subdomain(ingress.Hostname) {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("hostname"), ingress.Hostname, msg))
			}
			if isIP := (netutils.ParseIPSloppy(ingress.Hostname) != nil); isIP {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("hostname"), ingress.Hostname, "must be a DNS name, not an IP address"))
			}
		}
	}
	return allErrs
}

func validateIngressRules(ingressRules []networking.IngressRule, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(ingressRules) == 0 {
		return append(allErrs, field.Required(fldPath, ""))
	}
	for i, ih := range ingressRules {
		wildcardHost := false
		if len(ih.Host) > 0 {
			if isIP := (netutils.ParseIPSloppy(ih.Host) != nil); isIP {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("host"), ih.Host, "must be a DNS name, not an IP address"))
			}
			// TODO: Ports and ips are allowed in the host part of a url
			// according to RFC 3986, consider allowing them.
			if strings.Contains(ih.Host, "*") {
				for _, msg := range validation.IsWildcardDNS1123Subdomain(ih.Host) {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("host"), ih.Host, msg))
				}
				wildcardHost = true
			} else {
				for _, msg := range validation.IsDNS1123Subdomain(ih.Host) {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("host"), ih.Host, msg))
				}
			}
		}

		if !wildcardHost || !opts.AllowInvalidWildcardHostRule {
			allErrs = append(allErrs, validateIngressRuleValue(&ih.IngressRuleValue, fldPath.Index(i), opts)...)
		}
	}
	return allErrs
}

func validateIngressRuleValue(ingressRule *networking.IngressRuleValue, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if ingressRule.HTTP != nil {
		allErrs = append(allErrs, validateHTTPIngressRuleValue(ingressRule.HTTP, fldPath.Child("http"), opts)...)
	}
	return allErrs
}

func validateHTTPIngressRuleValue(httpIngressRuleValue *networking.HTTPIngressRuleValue, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(httpIngressRuleValue.Paths) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("paths"), ""))
	}
	for i, path := range httpIngressRuleValue.Paths {
		allErrs = append(allErrs, validateHTTPIngressPath(&path, fldPath.Child("paths").Index(i), opts)...)
	}
	return allErrs
}

func validateHTTPIngressPath(path *networking.HTTPIngressPath, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if path.PathType == nil {
		return append(allErrs, field.Required(fldPath.Child("pathType"), "pathType must be specified"))
	}

	switch *path.PathType {
	case networking.PathTypeExact, networking.PathTypePrefix:
		if !strings.HasPrefix(path.Path, "/") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("path"), path.Path, "must be an absolute path"))
		}
		if len(path.Path) > 0 {
			for _, invalidSeq := range invalidPathSequences {
				if strings.Contains(path.Path, invalidSeq) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("path"), path.Path, fmt.Sprintf("must not contain '%s'", invalidSeq)))
				}
			}

			for _, invalidSuff := range invalidPathSuffixes {
				if strings.HasSuffix(path.Path, invalidSuff) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("path"), path.Path, fmt.Sprintf("cannot end with '%s'", invalidSuff)))
				}
			}
		}
	case networking.PathTypeImplementationSpecific:
		if len(path.Path) > 0 {
			if !strings.HasPrefix(path.Path, "/") {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("path"), path.Path, "must be an absolute path"))
			}
		}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("pathType"), *path.PathType, supportedPathTypes.List()))
	}
	allErrs = append(allErrs, validateIngressBackend(&path.Backend, fldPath.Child("backend"), opts)...)
	return allErrs
}

// validateIngressBackend tests if a given backend is valid.
func validateIngressBackend(backend *networking.IngressBackend, fldPath *field.Path, opts IngressValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	hasResourceBackend := backend.Resource != nil
	hasServiceBackend := backend.Service != nil

	switch {
	case hasResourceBackend && hasServiceBackend:
		return append(allErrs, field.Invalid(fldPath, "", "cannot set both resource and service backends"))
	case hasResourceBackend:
		allErrs = append(allErrs, validateIngressTypedLocalObjectReference(backend.Resource, fldPath.Child("resource"))...)
	case hasServiceBackend:

		if len(backend.Service.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("service", "name"), ""))
		} else {
			for _, msg := range apivalidation.ValidateServiceName(backend.Service.Name, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("service", "name"), backend.Service.Name, msg))
			}
		}

		hasPortName := len(backend.Service.Port.Name) > 0
		hasPortNumber := backend.Service.Port.Number != 0
		if hasPortName && hasPortNumber {
			allErrs = append(allErrs, field.Invalid(fldPath, "", "cannot set both port name & port number"))
		} else if hasPortName {
			for _, msg := range validation.IsValidPortName(backend.Service.Port.Name) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("service", "port", "name"), backend.Service.Port.Name, msg))
			}
		} else if hasPortNumber {
			for _, msg := range validation.IsValidPortNum(int(backend.Service.Port.Number)) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("service", "port", "number"), backend.Service.Port.Number, msg))
			}
		} else {
			allErrs = append(allErrs, field.Required(fldPath, "port name or number is required"))
		}
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, "", "resource or service backend is required"))
	}
	return allErrs
}

// ValidateIngressClassName validates that the given name can be used as an
// IngressClass name.
var ValidateIngressClassName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateIngressClass ensures that IngressClass resources are valid.
func ValidateIngressClass(ingressClass *networking.IngressClass) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&ingressClass.ObjectMeta, false, ValidateIngressClassName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateIngressClassSpec(&ingressClass.Spec, field.NewPath("spec"))...)
	return allErrs
}

// ValidateIngressClassUpdate ensures that IngressClass updates are valid.
func ValidateIngressClassUpdate(newIngressClass, oldIngressClass *networking.IngressClass) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&newIngressClass.ObjectMeta, &oldIngressClass.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateIngressClassSpecUpdate(&newIngressClass.Spec, &oldIngressClass.Spec, field.NewPath("spec"))...)
	allErrs = append(allErrs, ValidateIngressClass(newIngressClass)...)
	return allErrs
}

// validateIngressClassSpec ensures that IngressClassSpec fields are valid.
func validateIngressClassSpec(spec *networking.IngressClassSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(spec.Controller) > maxLenIngressClassController {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("controller"), "" /*unused*/, maxLenIngressClassController))
	}
	allErrs = append(allErrs, validation.IsDomainPrefixedPath(fldPath.Child("controller"), spec.Controller)...)
	allErrs = append(allErrs, validateIngressClassParametersReference(spec.Parameters, fldPath.Child("parameters"))...)
	return allErrs
}

// validateIngressClassSpecUpdate ensures that IngressClassSpec updates are
// valid.
func validateIngressClassSpecUpdate(newSpec, oldSpec *networking.IngressClassSpec, fldPath *field.Path) field.ErrorList {
	return apivalidation.ValidateImmutableField(newSpec.Controller, oldSpec.Controller, fldPath.Child("controller"))
}

// validateIngressTypedLocalObjectReference ensures that Parameters fields are valid.
func validateIngressTypedLocalObjectReference(params *api.TypedLocalObjectReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if params == nil {
		return allErrs
	}

	if params.APIGroup != nil {
		for _, msg := range validation.IsDNS1123Subdomain(*params.APIGroup) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("apiGroup"), *params.APIGroup, msg))
		}
	}

	if params.Kind == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("kind"), "kind is required"))
	} else {
		for _, msg := range pathvalidation.IsValidPathSegmentName(params.Kind) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("kind"), params.Kind, msg))
		}
	}

	if params.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "name is required"))
	} else {
		for _, msg := range pathvalidation.IsValidPathSegmentName(params.Name) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), params.Name, msg))
		}
	}

	return allErrs
}

// validateIngressClassParametersReference ensures that Parameters fields are valid.
// Parameters was previously of type `TypedLocalObjectReference` and used
// `validateIngressTypedLocalObjectReference()`. This function extends validation
// for additional fields introduced for namespace-scoped references.
func validateIngressClassParametersReference(params *networking.IngressClassParametersReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if params == nil {
		return allErrs
	}

	allErrs = append(allErrs, validateIngressTypedLocalObjectReference(&api.TypedLocalObjectReference{
		APIGroup: params.APIGroup,
		Kind:     params.Kind,
		Name:     params.Name,
	}, fldPath)...)

	if params.Scope == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("scope"), ""))
		return allErrs
	}

	scope := ptr.Deref(params.Scope, "")

	if !supportedIngressClassParametersReferenceScopes.Has(scope) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("scope"), scope,
			supportedIngressClassParametersReferenceScopes.List()))
	} else {
		if scope == networking.IngressClassParametersReferenceScopeNamespace {
			if params.Namespace == nil {
				allErrs = append(allErrs, field.Required(fldPath.Child("namespace"), "`parameters.scope` is set to 'Namespace'"))
			} else {
				for _, msg := range apivalidation.ValidateNamespaceName(*params.Namespace, false) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), *params.Namespace, msg))
				}
			}
		}

		if scope == networking.IngressClassParametersReferenceScopeCluster && params.Namespace != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("namespace"), "`parameters.scope` is set to 'Cluster'"))
		}
	}

	return allErrs
}

func allowInvalidSecretName(oldIngress *networking.Ingress) bool {
	if oldIngress != nil {
		for _, tls := range oldIngress.Spec.TLS {
			if len(validateTLSSecretName(tls.SecretName)) > 0 {
				// backwards compatibility with existing persisted object
				return true
			}
		}
	}
	return false
}

func validateTLSSecretName(name string) []string {
	if len(name) == 0 {
		return nil
	}
	return apivalidation.ValidateSecretName(name, false)
}

func allowInvalidWildcardHostRule(oldIngress *networking.Ingress) bool {
	if oldIngress != nil {
		for _, rule := range oldIngress.Spec.Rules {
			if strings.Contains(rule.Host, "*") && len(validateIngressRuleValue(&rule.IngressRuleValue, nil, IngressValidationOptions{})) > 0 {
				// backwards compatibility with existing invalid data
				return true
			}
		}
	}
	return false
}

// ValidateIPAddressName validates that the name is the decimal representation of an IP address.
// IPAddress does not support generating names, prefix is not considered.
func ValidateIPAddressName(name string, prefix bool) []string {
	var errs []string
	ip, err := netip.ParseAddr(name)
	if err != nil {
		errs = append(errs, err.Error())
	} else if ip.String() != name {
		errs = append(errs, "must be a canonical format IP address")

	}
	return errs
}

func ValidateIPAddress(ipAddress *networking.IPAddress) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&ipAddress.ObjectMeta, false, ValidateIPAddressName, field.NewPath("metadata"))
	errs := validateIPAddressParentReference(ipAddress.Spec.ParentRef, field.NewPath("spec"))
	allErrs = append(allErrs, errs...)
	return allErrs

}

// validateIPAddressParentReference ensures that the IPAddress ParenteReference exists and is valid.
func validateIPAddressParentReference(params *networking.ParentReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if params == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("parentRef"), ""))
		return allErrs
	}

	fldPath = fldPath.Child("parentRef")
	// group is required but the Core group used by Services is the empty value, so it can not be enforced
	if params.Group != "" {
		for _, msg := range validation.IsDNS1123Subdomain(params.Group) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("group"), params.Group, msg))
		}
	}

	// resource is required
	if params.Resource == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("resource"), ""))
	} else {
		for _, msg := range pathvalidation.IsValidPathSegmentName(params.Resource) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("resource"), params.Resource, msg))
		}
	}

	// name is required
	if params.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else {
		for _, msg := range pathvalidation.IsValidPathSegmentName(params.Name) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), params.Name, msg))
		}
	}

	// namespace is optional
	if params.Namespace != "" {
		for _, msg := range pathvalidation.IsValidPathSegmentName(params.Namespace) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), params.Namespace, msg))
		}
	}
	return allErrs
}

// ValidateIPAddressUpdate tests if an update to an IPAddress is valid.
func ValidateIPAddressUpdate(update, old *networking.IPAddress) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, apivalidation.ValidateObjectMetaUpdate(&update.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))...)
	allErrs = append(allErrs, apivalidation.ValidateImmutableField(update.Spec.ParentRef, old.Spec.ParentRef, field.NewPath("spec").Child("parentRef"))...)
	return allErrs
}

var ValidateServiceCIDRName = apimachineryvalidation.NameIsDNSSubdomain

func ValidateServiceCIDR(cidrConfig *networking.ServiceCIDR) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&cidrConfig.ObjectMeta, false, ValidateServiceCIDRName, field.NewPath("metadata"))
	fieldPath := field.NewPath("spec", "cidrs")

	if len(cidrConfig.Spec.CIDRs) == 0 {
		allErrs = append(allErrs, field.Required(fieldPath, "at least one CIDR required"))
		return allErrs
	}

	if len(cidrConfig.Spec.CIDRs) > 2 {
		allErrs = append(allErrs, field.Invalid(fieldPath, cidrConfig.Spec, "may only hold up to 2 values"))
		return allErrs
	}
	// validate cidrs are dual stack, one of each IP family
	if len(cidrConfig.Spec.CIDRs) == 2 {
		isDual, err := netutils.IsDualStackCIDRStrings(cidrConfig.Spec.CIDRs)
		if err != nil || !isDual {
			allErrs = append(allErrs, field.Invalid(fieldPath, cidrConfig.Spec, "may specify no more than one IP for each IP family, i.e 192.168.0.0/24 and 2001:db8::/64"))
			return allErrs
		}
	}

	for i, cidr := range cidrConfig.Spec.CIDRs {
		allErrs = append(allErrs, validateCIDR(cidr, fieldPath.Index(i))...)
	}

	return allErrs
}

func validateCIDR(cidr string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, cidr, err.Error()))
	} else {
		if prefix.Addr() != prefix.Masked().Addr() {
			allErrs = append(allErrs, field.Invalid(fldPath, cidr, "wrong CIDR format, IP doesn't match network IP address"))
		}
		if prefix.String() != cidr {
			allErrs = append(allErrs, field.Invalid(fldPath, cidr, "CIDR not in RFC 5952 canonical format"))
		}
	}
	return allErrs
}

// ValidateServiceCIDRUpdate tests if an update to a ServiceCIDR is valid.
func ValidateServiceCIDRUpdate(update, old *networking.ServiceCIDR) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, apivalidation.ValidateObjectMetaUpdate(&update.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))...)
	allErrs = append(allErrs, apivalidation.ValidateImmutableField(update.Spec.CIDRs, old.Spec.CIDRs, field.NewPath("spec").Child("cidrs"))...)

	return allErrs
}

// ValidateServiceCIDRStatusUpdate tests if if an update to a ServiceCIDR Status is valid.
func ValidateServiceCIDRStatusUpdate(update, old *networking.ServiceCIDR) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&update.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))
	return allErrs
}
