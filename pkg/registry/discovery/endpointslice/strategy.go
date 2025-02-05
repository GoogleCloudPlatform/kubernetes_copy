/*
Copyright 2019 The Kubernetes Authors.

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

package endpointslice

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	discoveryv1beta1 "k8s.io/api/discovery/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage/names"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/apis/discovery"
	"k8s.io/kubernetes/pkg/apis/discovery/validation"
	"k8s.io/kubernetes/pkg/features"
)

// endpointSliceStrategy implements verification logic for Replication.
type endpointSliceStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating Replication EndpointSlice objects.
var Strategy = endpointSliceStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped returns true because all EndpointSlices need to be within a namespace.
func (endpointSliceStrategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears the status of an EndpointSlice before creation.
func (endpointSliceStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	endpointSlice := obj.(*discovery.EndpointSlice)
	endpointSlice.Generation = 1

	dropDisabledFieldsOnCreate(endpointSlice)
	dropTopologyOnV1(ctx, nil, endpointSlice)
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (endpointSliceStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newEPS := obj.(*discovery.EndpointSlice)
	oldEPS := old.(*discovery.EndpointSlice)

	// Increment generation if anything other than meta changed
	// This needs to be changed if a status attribute is added to EndpointSlice
	ogNewMeta := newEPS.ObjectMeta
	ogOldMeta := oldEPS.ObjectMeta
	newEPS.ObjectMeta = metav1.ObjectMeta{}
	oldEPS.ObjectMeta = metav1.ObjectMeta{}

	if !apiequality.Semantic.DeepEqual(newEPS, oldEPS) || !apiequality.Semantic.DeepEqual(ogNewMeta.Labels, ogOldMeta.Labels) {
		ogNewMeta.Generation = ogOldMeta.Generation + 1
	}

	newEPS.ObjectMeta = ogNewMeta
	oldEPS.ObjectMeta = ogOldMeta

	dropDisabledFieldsOnUpdate(oldEPS, newEPS)
	dropTopologyOnV1(ctx, oldEPS, newEPS)
}

// Validate validates a new EndpointSlice.
func (endpointSliceStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	endpointSlice := obj.(*discovery.EndpointSlice)
	err := validation.ValidateEndpointSliceCreate(endpointSlice)
	return err
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (endpointSliceStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	eps := obj.(*discovery.EndpointSlice)
	if eps == nil {
		return nil
	}
	var warnings []string
	warnings = append(warnings, warnOnDeprecatedAddressType(eps.AddressType)...)
	warnings = append(warnings, warnOnBadIPs(eps)...)
	return warnings
}

// Canonicalize normalizes the object after validation.
func (endpointSliceStrategy) Canonicalize(obj runtime.Object) {
}

// AllowCreateOnUpdate is false for EndpointSlice; this means POST is needed to create one.
func (endpointSliceStrategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate is the default update validation for an end user.
func (endpointSliceStrategy) ValidateUpdate(ctx context.Context, new, old runtime.Object) field.ErrorList {
	newEPS := new.(*discovery.EndpointSlice)
	oldEPS := old.(*discovery.EndpointSlice)
	return validation.ValidateEndpointSliceUpdate(newEPS, oldEPS)
}

// WarningsOnUpdate returns warnings for the given update.
func (endpointSliceStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	eps := obj.(*discovery.EndpointSlice)
	if eps == nil {
		return nil
	}
	var warnings []string
	warnings = append(warnings, warnOnBadIPs(eps)...)
	return warnings
}

// AllowUnconditionalUpdate is the default update policy for EndpointSlice objects.
func (endpointSliceStrategy) AllowUnconditionalUpdate() bool {
	return true
}

// dropDisabledConditionsOnCreate will drop any fields that are disabled.
func dropDisabledFieldsOnCreate(endpointSlice *discovery.EndpointSlice) {
	dropHints := !utilfeature.DefaultFeatureGate.Enabled(features.TopologyAwareHints)

	if dropHints {
		for i := range endpointSlice.Endpoints {
			if dropHints {
				endpointSlice.Endpoints[i].Hints = nil
			}
		}
	}
}

// dropDisabledFieldsOnUpdate will drop any disable fields that have not already
// been set on the EndpointSlice.
func dropDisabledFieldsOnUpdate(oldEPS, newEPS *discovery.EndpointSlice) {
	dropHints := !utilfeature.DefaultFeatureGate.Enabled(features.TopologyAwareHints)
	if dropHints {
		for _, ep := range oldEPS.Endpoints {
			if ep.Hints != nil {
				dropHints = false
				break
			}
		}
	}

	if dropHints {
		for i := range newEPS.Endpoints {
			if dropHints {
				newEPS.Endpoints[i].Hints = nil
			}
		}
	}
}

// dropTopologyOnV1 on V1 request wipes the DeprecatedTopology field  and copies
// the NodeName value into DeprecatedTopology
func dropTopologyOnV1(ctx context.Context, oldEPS, newEPS *discovery.EndpointSlice) {
	if info, ok := genericapirequest.RequestInfoFrom(ctx); ok {
		requestGV := schema.GroupVersion{Group: info.APIGroup, Version: info.APIVersion}
		if requestGV == discoveryv1beta1.SchemeGroupVersion {
			return
		}

		// do not drop topology if endpoints have not been changed
		if oldEPS != nil && apiequality.Semantic.DeepEqual(oldEPS.Endpoints, newEPS.Endpoints) {
			return
		}

		// Only node names that exist in previous version of the EndpointSlice
		// deprecatedTopology fields may be retained in new version of the
		// EndpointSlice.
		prevNodeNames := getDeprecatedTopologyNodeNames(oldEPS)

		for i := range newEPS.Endpoints {
			ep := &newEPS.Endpoints[i]

			newTopologyNodeName, ok := ep.DeprecatedTopology[corev1.LabelHostname]
			if ep.NodeName == nil && ok && prevNodeNames.Has(newTopologyNodeName) && len(apivalidation.ValidateNodeName(newTopologyNodeName, false)) == 0 {
				// Copy the label previously used to store the node name into the nodeName field,
				// in order to make partial updates preserve previous node info
				ep.NodeName = &newTopologyNodeName
			}
			// Drop writes to this field via the v1 API as documented
			ep.DeprecatedTopology = nil
		}
	}
}

// getDeprecatedTopologyNodeNames returns a set of node names present in
// deprecatedTopology fields within the provided EndpointSlice.
func getDeprecatedTopologyNodeNames(eps *discovery.EndpointSlice) sets.String {
	if eps == nil {
		return nil
	}
	var names sets.String
	for _, ep := range eps.Endpoints {
		if nodeName, ok := ep.DeprecatedTopology[corev1.LabelHostname]; ok && len(nodeName) > 0 {
			if names == nil {
				names = sets.NewString()
			}
			names.Insert(nodeName)
		}
	}
	return names
}

// warnOnDeprecatedAddressType returns a warning for endpointslices with FQDN AddressType
func warnOnDeprecatedAddressType(addressType discovery.AddressType) []string {
	if addressType == discovery.AddressTypeFQDN {
		return []string{fmt.Sprintf("%s: FQDN endpoints are deprecated", field.NewPath("spec").Child("addressType"))}
	}
	return nil
}

// warnOnBadIPs returns warnings for bad IP address formats
func warnOnBadIPs(eps *discovery.EndpointSlice) []string {
	// Don't bother checking for bad IPs if it looks like the request is coming from
	// one of our controllers.
	if eps.Labels[discoveryv1.LabelManagedBy] == "endpointslice-controller.k8s.io" ||
		eps.Labels[discoveryv1.LabelManagedBy] == "endpointslicemirroring-controller.k8s.io" {
		return nil
	}

	var warnings []string
	for i := range eps.Endpoints {
		for j, addr := range eps.Endpoints[i].Addresses {
			fldPath := field.NewPath("endpoints").Index(i).Child("addresses").Index(j)
			warnings = append(warnings, utilvalidation.GetWarningsForIP(fldPath, addr)...)
		}
	}
	return warnings
}
