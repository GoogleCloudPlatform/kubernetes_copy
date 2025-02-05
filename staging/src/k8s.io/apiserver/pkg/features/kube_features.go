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

package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
)

const (
	// Every feature gate should add method here following this template:
	//
	// // owner: @username
	// MyFeature featuregate.Feature = "MyFeature"
	//
	// Feature gates should be listed in alphabetical, case-sensitive
	// (upper before any lower case character) order. This reduces the risk
	// of code conflicts because changes are more likely to be scattered
	// across the file.

	// owner: @ivelichkovich, @tallclair
	// stable: v1.30
	// kep: https://kep.k8s.io/3716
	//
	// Enables usage of MatchConditions fields to use CEL expressions for matching on admission webhooks
	AdmissionWebhookMatchConditions featuregate.Feature = "AdmissionWebhookMatchConditions"

	// owner: @jefftree @alexzielenski
	// stable: v1.30
	//
	// Enables an single HTTP endpoint /discovery/<version> which supports native HTTP
	// caching with ETags containing all APIResources known to the apiserver.
	AggregatedDiscoveryEndpoint featuregate.Feature = "AggregatedDiscoveryEndpoint"

	// owner: @modulitos
	//
	// Allow user.DefaultInfo.UID to be set from x509 cert during cert auth.
	AllowParsingUserUIDFromCertAuth featuregate.Feature = "AllowParsingUserUIDFromCertAuth"

	// owner: @vinayakankugoyal
	// kep: https://kep.k8s.io/4633
	//
	// Allows us to enable anonymous auth for only certain apiserver endpoints.
	AnonymousAuthConfigurableEndpoints featuregate.Feature = "AnonymousAuthConfigurableEndpoints"

	// owner: @stlaz @tkashem @dgrisonnet
	// kep: https://kep.k8s.io/3926
	//
	// Enables the cluster admin to identify resources that fail to
	// decrypt or fail to be decoded into an object, and introduces
	// a new delete option to allow deletion of such corrupt
	// resources using the Kubernetes API only.
	AllowUnsafeMalformedObjectDeletion featuregate.Feature = "AllowUnsafeMalformedObjectDeletion"

	// owner: @ilackams
	//
	// Enables compression of REST responses (GET and LIST only)
	APIResponseCompression featuregate.Feature = "APIResponseCompression"

	// owner: @roycaihw
	//
	// Assigns each kube-apiserver an ID in a cluster.
	APIServerIdentity featuregate.Feature = "APIServerIdentity"

	// owner: @dashpole
	//
	// Add support for distributed tracing in the API Server
	APIServerTracing featuregate.Feature = "APIServerTracing"

	// owner: @linxiulei
	//
	// Enables serving watch requests in separate goroutines.
	APIServingWithRoutine featuregate.Feature = "APIServingWithRoutine"

	// owner: @deads2k
	// kep: https://kep.k8s.io/4601
	//
	// Allows authorization to use field and label selectors.
	AuthorizeWithSelectors featuregate.Feature = "AuthorizeWithSelectors"

	// owner: @benluddy
	// kep: https://kep.k8s.io/4222
	//
	// Enables CBOR as a supported encoding for requests and responses, and as the
	// preferred storage encoding for custom resources.
	CBORServingAndStorage featuregate.Feature = "CBORServingAndStorage"

	// owner: @serathius
	//
	// Replaces watch cache hashmap implementation with a btree based one, bringing performance improvements.
	BtreeWatchCache featuregate.Feature = "BtreeWatchCache"

	// owner: @serathius
	// Enables concurrent watch object decoding to avoid starving watch cache when conversion webhook is installed.
	ConcurrentWatchObjectDecode featuregate.Feature = "ConcurrentWatchObjectDecode"

	// owner: @jefftree
	// kep: https://kep.k8s.io/4355
	//
	// Enables coordinated leader election in the API server
	CoordinatedLeaderElection featuregate.Feature = "CoordinatedLeaderElection"

	// owner: @aramase
	// kep: https://kep.k8s.io/3299
	// deprecated: v1.28
	//
	// Enables KMS v1 API for encryption at rest.
	KMSv1 featuregate.Feature = "KMSv1"

	// owner: @alexzielenski, @cici37, @jiahuif, @jpbetz
	// kep: https://kep.k8s.io/3962
	//
	// Enables the MutatingAdmissionPolicy in Admission Chain
	MutatingAdmissionPolicy featuregate.Feature = "MutatingAdmissionPolicy"

	// owner: @jiahuif
	// kep: https://kep.k8s.io/2887
	//
	// Enables populating "enum" field of OpenAPI schemas
	// in the spec returned from kube-apiserver.
	OpenAPIEnums featuregate.Feature = "OpenAPIEnums"

	// owner: @caesarxuchao
	// stable: 1.29
	//
	// Allow apiservers to show a count of remaining items in the response
	// to a chunking list request.
	RemainingItemCount featuregate.Feature = "RemainingItemCount"

	// owner: @stlaz
	//
	// Enable kube-apiserver to accept UIDs via request header authentication.
	// This will also make the kube-apiserver's API aggregator add UIDs via standard
	// headers when forwarding requests to the servers serving the aggregated API.
	RemoteRequestHeaderUID featuregate.Feature = "RemoteRequestHeaderUID"

	// owner: @wojtek-t
	//
	// Enables resilient watchcache initialization to avoid controlplane
	// overload.
	ResilientWatchCacheInitialization featuregate.Feature = "ResilientWatchCacheInitialization"

	// owner: @serathius
	//
	// Allow watch cache to create a watch on a dedicated RPC.
	// This prevents watch cache from being starved by other watches.
	SeparateCacheWatchRPC featuregate.Feature = "SeparateCacheWatchRPC"

	// owner: @enj
	//
	// Enables http2 DOS mitigations for unauthenticated clients.
	//
	// Some known reasons to disable these mitigations:
	//
	// An API server that is fronted by an L7 load balancer that is set up
	// to mitigate http2 attacks may opt to disable this protection to prevent
	// unauthenticated clients from disabling connection reuse between the load
	// balancer and the API server (many incoming connections could share the
	// same backend connection).
	//
	// An API server that is on a private network may opt to disable this
	// protection to prevent performance regressions for unauthenticated
	// clients.
	UnauthenticatedHTTP2DOSMitigation featuregate.Feature = "UnauthenticatedHTTP2DOSMitigation"

	// owner: @jpbetz
	// Resource create requests using generateName are retried automatically by the apiserver
	// if the generated name conflicts with an existing resource name, up to a maximum number of 7 retries.
	RetryGenerateName featuregate.Feature = "RetryGenerateName"

	// owner: @cici37
	//
	// StrictCostEnforcementForVAP is used to apply strict CEL cost validation for ValidatingAdmissionPolicy.
	// It will be set to off by default for certain time of period to prevent the impact on the existing users.
	// It is strongly recommended to enable this feature gate as early as possible.
	// The strict cost is specific for the extended libraries whose cost defined under k8s/apiserver/pkg/cel/library.
	StrictCostEnforcementForVAP featuregate.Feature = "StrictCostEnforcementForVAP"

	// owner: @cici37
	//
	// StrictCostEnforcementForWebhooks is used to apply strict CEL cost validation for matchConditions in Webhooks.
	// It will be set to off by default for certain time of period to prevent the impact on the existing users.
	// It is strongly recommended to enable this feature gate as early as possible.
	// The strict cost is specific for the extended libraries whose cost defined under k8s/apiserver/pkg/cel/library.
	StrictCostEnforcementForWebhooks featuregate.Feature = "StrictCostEnforcementForWebhooks"

	// owner: @caesarxuchao @roycaihw
	//
	// Enable the storage version API.
	StorageVersionAPI featuregate.Feature = "StorageVersionAPI"

	// owner: @caesarxuchao
	//
	// Allow apiservers to expose the storage version hash in the discovery
	// document.
	StorageVersionHash featuregate.Feature = "StorageVersionHash"

	// owner: @aramase, @enj, @nabokihms
	// kep: https://kep.k8s.io/3331
	//
	// Enables Structured Authentication Configuration
	StructuredAuthenticationConfiguration featuregate.Feature = "StructuredAuthenticationConfiguration"

	// owner: @palnabarun
	// kep: https://kep.k8s.io/3221
	//
	// Enables Structured Authorization Configuration
	StructuredAuthorizationConfiguration featuregate.Feature = "StructuredAuthorizationConfiguration"

	// owner: @wojtek-t
	//
	// Enables post-start-hook for storage readiness
	WatchCacheInitializationPostStartHook featuregate.Feature = "WatchCacheInitializationPostStartHook"

	// owner: @serathius
	// Enables watches without resourceVersion to be served from storage.
	// Used to prevent https://github.com/kubernetes/kubernetes/issues/123072 until etcd fixes the issue.
	WatchFromStorageWithoutResourceVersion featuregate.Feature = "WatchFromStorageWithoutResourceVersion"

	// owner: @p0lyn0mial
	//
	// Allow the API server to stream individual items instead of chunking
	WatchList featuregate.Feature = "WatchList"

	// owner: @serathius
	// kep: http://kep.k8s.io/2340
	//
	// Allow the API server to serve consistent lists from cache
	ConsistentListFromCache featuregate.Feature = "ConsistentListFromCache"
)

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.Add(defaultKubernetesFeatureGates)) //nolint:forbidigo // TODO(https://github.com/kubernetes/enhancements/tree/master/keps/sig-architecture/4330-compatibility-versions): Remove this once we complete the migration to versioned feature gates
	runtime.Must(utilfeature.DefaultMutableFeatureGate.AddVersioned(defaultVersionedKubernetesFeatureGates))
}

// defaultVersionedKubernetesFeatureGates consists of all known Kubernetes-specific feature keys with VersionedSpecs.
// To add a new feature, define a key for it above and add it here. The features will be
// available throughout Kubernetes binaries.
//
// Entries are alphabetized and separated from each other with blank lines to avoid sweeping gofmt changes
// when adding or removing one entry.
var defaultVersionedKubernetesFeatureGates = map[featuregate.Feature]featuregate.VersionedSpecs{
	AdmissionWebhookMatchConditions: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	AggregatedDiscoveryEndpoint: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	AllowParsingUserUIDFromCertAuth: {
		{Version: version.MustParse("1.33"), Default: true, PreRelease: featuregate.Beta},
	},

	AllowUnsafeMalformedObjectDeletion: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	AnonymousAuthConfigurableEndpoints: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	APIResponseCompression: {
		{Version: version.MustParse("1.8"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.16"), Default: true, PreRelease: featuregate.Beta},
	},

	APIServerIdentity: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.Beta},
	},

	APIServerTracing: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
	},

	APIServingWithRoutine: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
	},

	BtreeWatchCache: {
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	AuthorizeWithSelectors: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	CBORServingAndStorage: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	ConcurrentWatchObjectDecode: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	ConsistentListFromCache: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	CoordinatedLeaderElection: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
	},

	KMSv1: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Deprecated},
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Deprecated},
	},

	MutatingAdmissionPolicy: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	OpenAPIEnums: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.24"), Default: true, PreRelease: featuregate.Beta},
	},

	RemainingItemCount: {
		{Version: version.MustParse("1.15"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.16"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	RemoteRequestHeaderUID: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	ResilientWatchCacheInitialization: {
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	RetryGenerateName: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, LockToDefault: true, PreRelease: featuregate.GA},
	},

	SeparateCacheWatchRPC: {
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
	},

	StorageVersionAPI: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
	},

	StorageVersionHash: {
		{Version: version.MustParse("1.14"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.15"), Default: true, PreRelease: featuregate.Beta},
	},

	StrictCostEnforcementForVAP: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	StrictCostEnforcementForWebhooks: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	StructuredAuthenticationConfiguration: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	StructuredAuthorizationConfiguration: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	UnauthenticatedHTTP2DOSMitigation: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},

	WatchCacheInitializationPostStartHook: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	WatchFromStorageWithoutResourceVersion: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.33"), Default: false, PreRelease: featuregate.Deprecated, LockToDefault: true},
	},

	WatchList: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},
}

// defaultKubernetesFeatureGates consists of legacy unversioned Kubernetes-specific feature keys.
// Please do not add to this struct and use defaultVersionedKubernetesFeatureGates instead.
var defaultKubernetesFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{}
