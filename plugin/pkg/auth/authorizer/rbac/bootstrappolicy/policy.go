/*
Copyright 2016 The Kubernetes Authors.

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

package bootstrappolicy

import (
	capi "k8s.io/api/certificates/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/apiserver/pkg/authentication/user"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	zpagesfeatures "k8s.io/component-base/zpages/features"

	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"
	"k8s.io/kubernetes/pkg/features"
)

// Write and other vars are slices of the allowed verbs.
// Label and Annotation are default maps of bootstrappolicy.
var (
	Write      = []string{"create", "update", "patch", "delete", "deletecollection"}
	ReadWrite  = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	Read       = []string{"get", "list", "watch"}
	ReadUpdate = []string{"get", "list", "watch", "update", "patch"}

	Label      = map[string]string{"kubernetes.io/bootstrapping": "rbac-defaults"}
	Annotation = map[string]string{rbacv1.AutoUpdateAnnotationKey: "true"}
)

const (
	legacyGroup                  = ""
	appsGroup                    = "apps"
	authenticationGroup          = "authentication.k8s.io"
	authorizationGroup           = "authorization.k8s.io"
	autoscalingGroup             = "autoscaling"
	batchGroup                   = "batch"
	certificatesGroup            = "certificates.k8s.io"
	coordinationGroup            = "coordination.k8s.io"
	discoveryGroup               = "discovery.k8s.io"
	extensionsGroup              = "extensions"
	policyGroup                  = "policy"
	rbacGroup                    = "rbac.authorization.k8s.io"
	resourceGroup                = "resource.k8s.io"
	storageGroup                 = "storage.k8s.io"
	resMetricsGroup              = "metrics.k8s.io"
	customMetricsGroup           = "custom.metrics.k8s.io"
	externalMetricsGroup         = "external.metrics.k8s.io"
	networkingGroup              = "networking.k8s.io"
	eventsGroup                  = "events.k8s.io"
	internalAPIServerGroup       = "internal.apiserver.k8s.io"
	admissionRegistrationGroup   = "admissionregistration.k8s.io"
	storageVersionMigrationGroup = "storagemigration.k8s.io"
)

func addDefaultMetadata(obj runtime.Object) {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		// if this happens, then some static code is broken
		panic(err)
	}

	labels := metadata.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range Label {
		labels[k] = v
	}
	metadata.SetLabels(labels)

	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range Annotation {
		annotations[k] = v
	}
	metadata.SetAnnotations(annotations)
}

func addClusterRoleLabel(roles []rbacv1.ClusterRole) {
	for i := range roles {
		addDefaultMetadata(&roles[i])
	}
	return
}

func addClusterRoleBindingLabel(rolebindings []rbacv1.ClusterRoleBinding) {
	for i := range rolebindings {
		addDefaultMetadata(&rolebindings[i])
	}
	return
}

// NodeRules returns node policy rules, it is slice of rbacv1.PolicyRule.
func NodeRules() []rbacv1.PolicyRule {
	nodePolicyRules := []rbacv1.PolicyRule{
		// Needed to check API access.  These creates are non-mutating
		rbacv1helpers.NewRule("create").Groups(authenticationGroup).Resources("tokenreviews").RuleOrDie(),
		rbacv1helpers.NewRule("create").Groups(authorizationGroup).Resources("subjectaccessreviews", "localsubjectaccessreviews").RuleOrDie(),

		// Needed to build serviceLister, to populate env vars for services
		rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("services").RuleOrDie(),

		// Nodes can register Node API objects and report status.
		// Use the NodeRestriction admission plugin to limit a node to creating/updating its own API object.
		rbacv1helpers.NewRule("create", "get", "list", "watch").Groups(legacyGroup).Resources("nodes").RuleOrDie(),
		rbacv1helpers.NewRule("update", "patch").Groups(legacyGroup).Resources("nodes/status").RuleOrDie(),
		rbacv1helpers.NewRule("update", "patch").Groups(legacyGroup).Resources("nodes").RuleOrDie(),

		// TODO: restrict to the bound node as creator in the NodeRestrictions admission plugin
		rbacv1helpers.NewRule("create", "update", "patch").Groups(legacyGroup).Resources("events").RuleOrDie(),

		// Use the Node authorizer to limit get to pods related to the node, and to limit list/watch to field selectors related to the node.
		rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("pods").RuleOrDie(),

		// Needed for the node to create/delete mirror pods.
		// Use the NodeRestriction admission plugin to limit a node to creating/deleting mirror pods bound to itself.
		rbacv1helpers.NewRule("create", "delete").Groups(legacyGroup).Resources("pods").RuleOrDie(),
		// Needed for the node to report status of pods it is running.
		// Use the NodeRestriction admission plugin to limit a node to updating status of pods bound to itself.
		rbacv1helpers.NewRule("update", "patch").Groups(legacyGroup).Resources("pods/status").RuleOrDie(),
		// Needed for the node to create pod evictions.
		// Use the NodeRestriction admission plugin to limit a node to creating evictions for pods bound to itself.
		rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("pods/eviction").RuleOrDie(),

		// Needed for imagepullsecrets, rbd/ceph and secret volumes, and secrets in envs
		// Needed for configmap volume and envs
		// Use the Node authorization mode to limit a node to get secrets/configmaps referenced by pods bound to itself.
		rbacv1helpers.NewRule("get", "list", "watch").Groups(legacyGroup).Resources("secrets", "configmaps").RuleOrDie(),
		// Needed for persistent volumes
		// Use the Node authorization mode to limit a node to get pv/pvc objects referenced by pods bound to itself.
		rbacv1helpers.NewRule("get").Groups(legacyGroup).Resources("persistentvolumeclaims", "persistentvolumes").RuleOrDie(),

		// TODO: add to the Node authorizer and restrict to endpoints referenced by pods or PVs bound to the node
		// Needed for glusterfs volumes
		rbacv1helpers.NewRule("get").Groups(legacyGroup).Resources("endpoints").RuleOrDie(),
		// Used to create a certificatesigningrequest for a node-specific client certificate, and watch
		// for it to be signed. This allows the kubelet to rotate it's own certificate.
		rbacv1helpers.NewRule("create", "get", "list", "watch").Groups(certificatesGroup).Resources("certificatesigningrequests").RuleOrDie(),

		// Leases
		rbacv1helpers.NewRule("get", "create", "update", "patch", "delete").Groups("coordination.k8s.io").Resources("leases").RuleOrDie(),

		// CSI
		rbacv1helpers.NewRule("get").Groups(storageGroup).Resources("volumeattachments").RuleOrDie(),

		// Use the Node authorization to limit a node to create tokens for service accounts running on that node
		// Use the NodeRestriction admission plugin to limit a node to create tokens bound to pods on that node
		rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("serviceaccounts/token").RuleOrDie(),
	}

	// Use the Node authorization mode to limit a node to update status of pvc objects referenced by pods bound to itself.
	// Use the NodeRestriction admission plugin to limit a node to just update the status stanza.
	pvcStatusPolicyRule := rbacv1helpers.NewRule("get", "update", "patch").Groups(legacyGroup).Resources("persistentvolumeclaims/status").RuleOrDie()
	nodePolicyRules = append(nodePolicyRules, pvcStatusPolicyRule)

	// CSI
	csiDriverRule := rbacv1helpers.NewRule("get", "watch", "list").Groups("storage.k8s.io").Resources("csidrivers").RuleOrDie()
	nodePolicyRules = append(nodePolicyRules, csiDriverRule)
	csiNodeInfoRule := rbacv1helpers.NewRule("get", "create", "update", "patch", "delete").Groups("storage.k8s.io").Resources("csinodes").RuleOrDie()
	nodePolicyRules = append(nodePolicyRules, csiNodeInfoRule)

	// RuntimeClass
	nodePolicyRules = append(nodePolicyRules, rbacv1helpers.NewRule("get", "list", "watch").Groups("node.k8s.io").Resources("runtimeclasses").RuleOrDie())

	// DRA Resource Claims
	if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		nodePolicyRules = append(nodePolicyRules, rbacv1helpers.NewRule("get").Groups(resourceGroup).Resources("resourceclaims").RuleOrDie())
		nodePolicyRules = append(nodePolicyRules, rbacv1helpers.NewRule("deletecollection").Groups(resourceGroup).Resources("resourceslices").RuleOrDie())
	}
	// Kubelet needs access to ClusterTrustBundles to support the pemTrustAnchors volume type.
	if utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundle) {
		nodePolicyRules = append(nodePolicyRules, rbacv1helpers.NewRule("get", "list", "watch").Groups(certificatesGroup).Resources("clustertrustbundles").RuleOrDie())
	}
	// Kubelet needs access to ServiceAccounts to support sending service account tokens to the credential provider.
	// Use the Node authorizer to limit get to service accounts related to the node.
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletServiceAccountTokenForCredentialProviders) {
		nodePolicyRules = append(nodePolicyRules, rbacv1helpers.NewRule("get").Groups(legacyGroup).Resources("serviceaccounts").RuleOrDie())
	}

	return nodePolicyRules
}

// ClusterRoles returns the cluster roles to bootstrap an API server with
func ClusterRoles() []rbacv1.ClusterRole {
	monitoringRules := []rbacv1.PolicyRule{
		rbacv1helpers.NewRule("get").URLs(
			"/metrics", "/metrics/slis",
			"/livez", "/readyz", "/healthz",
			"/livez/*", "/readyz/*", "/healthz/*",
		).RuleOrDie(),
	}

	if utilfeature.DefaultFeatureGate.Enabled(zpagesfeatures.ComponentFlagz) {
		monitoringRules = append(monitoringRules, rbacv1helpers.NewRule("get").URLs("/flagz").RuleOrDie())
	}

	if utilfeature.DefaultFeatureGate.Enabled(zpagesfeatures.ComponentStatusz) {
		monitoringRules = append(monitoringRules, rbacv1helpers.NewRule("get").URLs("/statusz").RuleOrDie())
	}

	roles := []rbacv1.ClusterRole{
		{
			// a "root" role which can do absolutely anything
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("*").Groups("*").Resources("*").RuleOrDie(),
				rbacv1helpers.NewRule("*").URLs("*").RuleOrDie(),
			},
		},
		{
			// a role which provides just enough power to determine if the server is
			// ready and discover API versions for negotiation
			ObjectMeta: metav1.ObjectMeta{Name: "system:discovery"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("get").URLs(
					"/livez", "/readyz", "/healthz",
					"/version", "/version/",
					"/openapi", "/openapi/*",
					"/api", "/api/*",
					"/apis", "/apis/*",
				).RuleOrDie(),
			},
		},
		{
			// a role which provides minimal read access to the monitoring endpoints
			// (i.e. /metrics, /livez/*, /readyz/*, /healthz/*, /livez, /readyz, /healthz)
			// The splatted health check endpoints allow read access to individual health check
			// endpoints which may contain more sensitive cluster information information
			ObjectMeta: metav1.ObjectMeta{Name: "system:monitoring"},
			Rules:      monitoringRules,
		},
	}

	basicUserRules := []rbacv1.PolicyRule{
		rbacv1helpers.NewRule("create").Groups(authorizationGroup).Resources("selfsubjectaccessreviews", "selfsubjectrulesreviews").RuleOrDie(),
		rbacv1helpers.NewRule("create").Groups(authenticationGroup).Resources("selfsubjectreviews").RuleOrDie(),
	}

	roles = append(roles, []rbacv1.ClusterRole{
		{
			// a role which provides minimal resource access to allow a "normal" user to learn information about themselves
			ObjectMeta: metav1.ObjectMeta{Name: "system:basic-user"},
			Rules:      basicUserRules,
		},
		{
			// a role which provides just enough power read insensitive cluster information
			ObjectMeta: metav1.ObjectMeta{Name: "system:public-info-viewer"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("get").URLs(
					"/livez", "/readyz", "/healthz", "/version", "/version/",
				).RuleOrDie(),
			},
		},
		{
			// a role for a namespace level admin.  It is `edit` plus the power to grant permissions to other users.
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			AggregationRule: &rbacv1.AggregationRule{
				ClusterRoleSelectors: []metav1.LabelSelector{
					{MatchLabels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true"}},
				},
			},
		},
		{
			// a role for a namespace level editor.  It grants access to all user level actions in a namespace.
			// It does not grant powers for "privileged" resources which are domain of the system: `/status`
			// subresources or `quota`/`limits` which are used to control namespaces
			ObjectMeta: metav1.ObjectMeta{Name: "edit", Labels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true"}},
			AggregationRule: &rbacv1.AggregationRule{
				ClusterRoleSelectors: []metav1.LabelSelector{
					{MatchLabels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-edit": "true"}},
				},
			},
		},
		{
			// a role for namespace level viewing.  It grants Read-only access to non-escalating resources in
			// a namespace.
			ObjectMeta: metav1.ObjectMeta{Name: "view", Labels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-edit": "true"}},
			AggregationRule: &rbacv1.AggregationRule{
				ClusterRoleSelectors: []metav1.LabelSelector{
					{MatchLabels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-view": "true"}},
				},
			},
		},
		{
			// a role for a namespace level admin.  It is `edit` plus the power to grant permissions to other users.
			ObjectMeta: metav1.ObjectMeta{Name: "system:aggregate-to-admin", Labels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true"}},
			Rules: []rbacv1.PolicyRule{
				// additional admin powers
				rbacv1helpers.NewRule("create").Groups(authorizationGroup).Resources("localsubjectaccessreviews").RuleOrDie(),
				rbacv1helpers.NewRule(ReadWrite...).Groups(rbacGroup).Resources("roles", "rolebindings").RuleOrDie(),
			},
		},
		{
			// a role for a namespace level editor.  It grants access to all user level actions in a namespace.
			// It does not grant powers for "privileged" resources which are domain of the system: `/status`
			// subresources or `quota`/`limits` which are used to control namespaces
			ObjectMeta: metav1.ObjectMeta{Name: "system:aggregate-to-edit", Labels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-edit": "true"}},
			Rules: []rbacv1.PolicyRule{
				// Allow read on escalating resources
				rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("pods/attach", "pods/proxy", "pods/exec", "pods/portforward", "secrets", "services/proxy").RuleOrDie(),
				rbacv1helpers.NewRule("impersonate").Groups(legacyGroup).Resources("serviceaccounts").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(legacyGroup).Resources("pods", "pods/attach", "pods/proxy", "pods/exec", "pods/portforward").RuleOrDie(),
				rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("pods/eviction").RuleOrDie(),
				rbacv1helpers.NewRule(Write...).Groups(legacyGroup).Resources("replicationcontrollers", "replicationcontrollers/scale", "serviceaccounts",
					"services", "services/proxy", "persistentvolumeclaims", "configmaps", "secrets", "events").RuleOrDie(),
				rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("serviceaccounts/token").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(appsGroup).Resources(
					"statefulsets", "statefulsets/scale",
					"daemonsets",
					"deployments", "deployments/scale", "deployments/rollback",
					"replicasets", "replicasets/scale").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(autoscalingGroup).Resources("horizontalpodautoscalers").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(batchGroup).Resources("jobs", "cronjobs").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(extensionsGroup).Resources("daemonsets",
					"deployments", "deployments/scale", "deployments/rollback", "ingresses",
					"replicasets", "replicasets/scale", "replicationcontrollers/scale",
					"networkpolicies").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(policyGroup).Resources("poddisruptionbudgets").RuleOrDie(),

				rbacv1helpers.NewRule(Write...).Groups(networkingGroup).Resources("networkpolicies", "ingresses").RuleOrDie(),

				rbacv1helpers.NewRule(ReadWrite...).Groups(coordinationGroup).Resources("leases").RuleOrDie(),
			},
		},
		{
			// a role for namespace level viewing.  It grants Read-only access to non-escalating resources in
			// a namespace.
			ObjectMeta: metav1.ObjectMeta{Name: "system:aggregate-to-view", Labels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-view": "true"}},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("pods", "replicationcontrollers", "replicationcontrollers/scale", "serviceaccounts",
					"services", "services/status", "endpoints", "persistentvolumeclaims", "persistentvolumeclaims/status", "configmaps").RuleOrDie(),
				rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("limitranges", "resourcequotas", "bindings", "events",
					"pods/status", "resourcequotas/status", "namespaces/status", "replicationcontrollers/status", "pods/log").RuleOrDie(),
				// read access to namespaces at the namespace scope means you can read *this* namespace.  This can be used as an
				// indicator of which namespaces you have access to.
				rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("namespaces").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(discoveryGroup).Resources("endpointslices").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(appsGroup).Resources(
					"controllerrevisions",
					"statefulsets", "statefulsets/status", "statefulsets/scale",
					"daemonsets", "daemonsets/status",
					"deployments", "deployments/status", "deployments/scale",
					"replicasets", "replicasets/status", "replicasets/scale").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(autoscalingGroup).Resources("horizontalpodautoscalers", "horizontalpodautoscalers/status").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(batchGroup).Resources("jobs", "cronjobs", "cronjobs/status", "jobs/status").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(extensionsGroup).Resources("daemonsets", "daemonsets/status", "deployments", "deployments/scale", "deployments/status",
					"ingresses", "ingresses/status", "replicasets", "replicasets/scale", "replicasets/status", "replicationcontrollers/scale",
					"networkpolicies").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(policyGroup).Resources("poddisruptionbudgets", "poddisruptionbudgets/status").RuleOrDie(),

				rbacv1helpers.NewRule(Read...).Groups(networkingGroup).Resources("networkpolicies", "ingresses", "ingresses/status").RuleOrDie(),
			},
		},
		{
			// a role to use for heapster's connections back to the API server
			ObjectMeta: metav1.ObjectMeta{Name: "system:heapster"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("events", "pods", "nodes", "namespaces").RuleOrDie(),
				rbacv1helpers.NewRule(Read...).Groups(extensionsGroup).Resources("deployments").RuleOrDie(),
			},
		},
		{
			// a role for nodes to use to have the access they need for running pods
			ObjectMeta: metav1.ObjectMeta{Name: systemNodeRoleName},
			Rules:      NodeRules(),
		},
		{
			// a role to use for node-problem-detector access.  It does not get bound to default location since
			// deployment locations can reasonably vary.
			ObjectMeta: metav1.ObjectMeta{Name: "system:node-problem-detector"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("get").Groups(legacyGroup).Resources("nodes").RuleOrDie(),
				rbacv1helpers.NewRule("patch").Groups(legacyGroup).Resources("nodes/status").RuleOrDie(),
				eventsRule(),
			},
		},
		{
			// a role to use for bootstrapping a node's client certificates
			ObjectMeta: metav1.ObjectMeta{Name: "system:node-bootstrapper"},
			Rules: []rbacv1.PolicyRule{
				// used to create a certificatesigningrequest for a node-specific client certificate, and watch for it to be signed
				rbacv1helpers.NewRule("create", "get", "list", "watch").Groups(certificatesGroup).Resources("certificatesigningrequests").RuleOrDie(),
			},
		},
		{
			// a role to use for allowing authentication and authorization delegation
			ObjectMeta: metav1.ObjectMeta{Name: "system:auth-delegator"},
			Rules: []rbacv1.PolicyRule{
				// These creates are non-mutating
				rbacv1helpers.NewRule("create").Groups(authenticationGroup).Resources("tokenreviews").RuleOrDie(),
				rbacv1helpers.NewRule("create").Groups(authorizationGroup).Resources("subjectaccessreviews").RuleOrDie(),
			},
		},
		{
			// a role to use for the API registry, summarization, and proxy handling
			ObjectMeta: metav1.ObjectMeta{Name: "system:kube-aggregator"},
			Rules: []rbacv1.PolicyRule{
				// it needs to see all services so that it knows whether the ones it points to exist or not
				rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("services", "endpoints").RuleOrDie(),
			},
		},
		{
			// a role to use for bootstrapping the kube-controller-manager so it can create the shared informers
			// service accounts, and secrets that we need to create separate identities for other controllers
			ObjectMeta: metav1.ObjectMeta{Name: "system:kube-controller-manager"},
			Rules: []rbacv1.PolicyRule{
				eventsRule(),
				// Needed for leader election.
				rbacv1helpers.NewRule("create").Groups(coordinationGroup).Resources("leases").RuleOrDie(),
				rbacv1helpers.NewRule("get", "update").Groups(coordinationGroup).Resources("leases").Names("kube-controller-manager").RuleOrDie(),
				// Fundamental resources.
				rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("secrets", "serviceaccounts").RuleOrDie(),
				rbacv1helpers.NewRule("delete").Groups(legacyGroup).Resources("secrets").RuleOrDie(),
				rbacv1helpers.NewRule("get").Groups(legacyGroup).Resources("namespaces", "secrets", "serviceaccounts", "configmaps").RuleOrDie(),
				rbacv1helpers.NewRule("update").Groups(legacyGroup).Resources("secrets", "serviceaccounts").RuleOrDie(),
				// Needed to check API access.  These creates are non-mutating
				rbacv1helpers.NewRule("create").Groups(authenticationGroup).Resources("tokenreviews").RuleOrDie(),
				rbacv1helpers.NewRule("create").Groups(authorizationGroup).Resources("subjectaccessreviews").RuleOrDie(),
				// Needed for all shared informers
				rbacv1helpers.NewRule("list", "watch").Groups("*").Resources("*").RuleOrDie(),
				rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("serviceaccounts/token").RuleOrDie(),
			},
		},
		{
			// a role to use for the kube-dns pod
			ObjectMeta: metav1.ObjectMeta{Name: "system:kube-dns"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("list", "watch").Groups(legacyGroup).Resources("endpoints", "services").RuleOrDie(),
			},
		},
		{
			// a role for an external/out-of-tree persistent volume provisioner
			ObjectMeta: metav1.ObjectMeta{Name: "system:persistent-volume-provisioner"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("get", "list", "watch", "create", "delete").Groups(legacyGroup).Resources("persistentvolumes").RuleOrDie(),
				// update is needed in addition to read access for setting lock annotations on PVCs
				rbacv1helpers.NewRule("get", "list", "watch", "update").Groups(legacyGroup).Resources("persistentvolumeclaims").RuleOrDie(),
				rbacv1helpers.NewRule(Read...).Groups(storageGroup).Resources("storageclasses").RuleOrDie(),

				// Needed for watching provisioning success and failure events
				rbacv1helpers.NewRule("watch").Groups(legacyGroup).Resources("events").RuleOrDie(),

				eventsRule(),
			},
		},
		{
			// a role making the csrapprover controller approve a node client CSR
			ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("create").Groups(certificatesGroup).Resources("certificatesigningrequests/nodeclient").RuleOrDie(),
			},
		},
		{
			// a role making the csrapprover controller approve a node client CSR requested by the node itself
			ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:certificatesigningrequests:selfnodeclient"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("create").Groups(certificatesGroup).Resources("certificatesigningrequests/selfnodeclient").RuleOrDie(),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "system:volume-scheduler"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule(ReadUpdate...).Groups(legacyGroup).Resources("persistentvolumes").RuleOrDie(),
				rbacv1helpers.NewRule(Read...).Groups(storageGroup).Resources("storageclasses").RuleOrDie(),
				rbacv1helpers.NewRule(ReadUpdate...).Groups(legacyGroup).Resources("persistentvolumeclaims").RuleOrDie(),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:legacy-unknown-approver"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("approve").Groups(certificatesGroup).Resources("signers").Names(capi.LegacyUnknownSignerName).RuleOrDie(),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:kubelet-serving-approver"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("approve").Groups(certificatesGroup).Resources("signers").Names(capi.KubeletServingSignerName).RuleOrDie(),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:kube-apiserver-client-approver"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("approve").Groups(certificatesGroup).Resources("signers").Names(capi.KubeAPIServerClientSignerName).RuleOrDie(),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:kube-apiserver-client-kubelet-approver"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("approve").Groups(certificatesGroup).Resources("signers").Names(capi.KubeAPIServerClientKubeletSignerName).RuleOrDie(),
			},
		},
	}...)

	// Add the cluster role for reading the ServiceAccountIssuerDiscovery endpoints
	// Also allow slash-ended URLs to allow clients generated from published openapi docs prior to fixing the trailing slash to work properly
	roles = append(roles, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "system:service-account-issuer-discovery"},
		Rules: []rbacv1.PolicyRule{
			rbacv1helpers.NewRule("get").URLs(
				"/.well-known/openid-configuration",
				"/.well-known/openid-configuration/",
				"/openid/v1/jwks",
				"/openid/v1/jwks/",
			).RuleOrDie(),
		},
	})

	// Add the cluster role system:kubelet-api-admin
	kubeletAPIAdminRules := []rbacv1.PolicyRule{
		// Allow read-only access to the Node API objects
		rbacv1helpers.NewRule("get", "list", "watch").Groups(legacyGroup).Resources("nodes").RuleOrDie(),
		// Allow all API calls to the nodes
		rbacv1helpers.NewRule("proxy").Groups(legacyGroup).Resources("nodes").RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups(legacyGroup).Resources("nodes/proxy", "nodes/metrics", "nodes/stats", "nodes/log").RuleOrDie(),
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletFineGrainedAuthz) {
		kubeletAPIAdminRules = append(kubeletAPIAdminRules, rbacv1helpers.NewRule("*").Groups(legacyGroup).Resources("nodes/pods", "nodes/healthz", "nodes/configz").RuleOrDie())
	}

	roles = append(roles, rbacv1.ClusterRole{
		// a role to use for full access to the kubelet API
		ObjectMeta: metav1.ObjectMeta{Name: "system:kubelet-api-admin"},
		Rules:      kubeletAPIAdminRules,
	})

	// node-proxier role is used by kube-proxy.
	nodeProxierRules := []rbacv1.PolicyRule{
		rbacv1helpers.NewRule("list", "watch").Groups(legacyGroup).Resources("services", "endpoints").RuleOrDie(),
		rbacv1helpers.NewRule("get", "list", "watch").Groups(legacyGroup).Resources("nodes").RuleOrDie(),

		eventsRule(),
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.MultiCIDRServiceAllocator) {
		nodeProxierRules = append(nodeProxierRules, rbacv1helpers.NewRule("list", "watch").Groups(networkingGroup).Resources("servicecidrs").RuleOrDie())
	}

	nodeProxierRules = append(nodeProxierRules, rbacv1helpers.NewRule("list", "watch").Groups(discoveryGroup).Resources("endpointslices").RuleOrDie())
	roles = append(roles, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "system:node-proxier"},
		Rules:      nodeProxierRules,
	})

	kubeSchedulerRules := []rbacv1.PolicyRule{
		eventsRule(),
		// This is for leaderlease access
		// TODO: scope this to the kube-system namespace
		rbacv1helpers.NewRule("create").Groups(coordinationGroup).Resources("leases").RuleOrDie(),
		rbacv1helpers.NewRule("get", "update", "list", "watch").Groups(coordinationGroup).Resources("leases").Names("kube-scheduler").RuleOrDie(),
		rbacv1helpers.NewRule(ReadWrite...).Groups(coordinationGroup).Resources("leasecandidates").RuleOrDie(),

		// Fundamental resources
		rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("nodes").RuleOrDie(),
		rbacv1helpers.NewRule("get", "list", "watch", "delete").Groups(legacyGroup).Resources("pods").RuleOrDie(),
		rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("pods/binding", "bindings").RuleOrDie(),
		rbacv1helpers.NewRule("patch", "update").Groups(legacyGroup).Resources("pods/status").RuleOrDie(),
		// Things that select pods
		rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("services", "replicationcontrollers").RuleOrDie(),
		rbacv1helpers.NewRule(Read...).Groups(appsGroup, extensionsGroup).Resources("replicasets").RuleOrDie(),
		rbacv1helpers.NewRule(Read...).Groups(appsGroup).Resources("statefulsets").RuleOrDie(),
		// Things that pods use or applies to them
		rbacv1helpers.NewRule(Read...).Groups(policyGroup).Resources("poddisruptionbudgets").RuleOrDie(),
		rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("persistentvolumeclaims", "persistentvolumes").RuleOrDie(),
		// Needed to check API access. These creates are non-mutating
		rbacv1helpers.NewRule("create").Groups(authenticationGroup).Resources("tokenreviews").RuleOrDie(),
		rbacv1helpers.NewRule("create").Groups(authorizationGroup).Resources("subjectaccessreviews").RuleOrDie(),
		// Needed for volume limits
		rbacv1helpers.NewRule(Read...).Groups(storageGroup).Resources("csinodes").RuleOrDie(),
		rbacv1helpers.NewRule("get", "list", "watch").Groups(storageGroup).Resources("volumeattachments").RuleOrDie(),
		// Needed for namespaceSelector feature in pod affinity
		rbacv1helpers.NewRule(Read...).Groups(legacyGroup).Resources("namespaces").RuleOrDie(),
		rbacv1helpers.NewRule(Read...).Groups(storageGroup).Resources("csidrivers").RuleOrDie(),
		rbacv1helpers.NewRule(Read...).Groups(storageGroup).Resources("csistoragecapacities").RuleOrDie(),
	}
	// Needed for dynamic resource allocation.
	if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		kubeSchedulerRules = append(kubeSchedulerRules,
			rbacv1helpers.NewRule(Read...).Groups(resourceGroup).Resources("deviceclasses").RuleOrDie(),
			rbacv1helpers.NewRule(ReadUpdate...).Groups(resourceGroup).Resources("resourceclaims").RuleOrDie(),
			rbacv1helpers.NewRule(ReadUpdate...).Groups(resourceGroup).Resources("resourceclaims/status").RuleOrDie(),
			rbacv1helpers.NewRule(ReadUpdate...).Groups(legacyGroup).Resources("pods/finalizers").RuleOrDie(),
			rbacv1helpers.NewRule(Read...).Groups(resourceGroup).Resources("resourceslices").RuleOrDie(),
		)
	}
	roles = append(roles, rbacv1.ClusterRole{
		// a role to use for the kube-scheduler
		ObjectMeta: metav1.ObjectMeta{Name: "system:kube-scheduler"},
		Rules:      kubeSchedulerRules,
	})

	// Default ClusterRole to allow reading ClusterTrustBundle objects
	if utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundle) {
		roles = append(roles, rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "system:cluster-trust-bundle-discovery"},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule(Read...).Groups(certificatesGroup).Resources("clustertrustbundles").RuleOrDie(),
			},
		})
	}

	addClusterRoleLabel(roles)
	return roles
}

const systemNodeRoleName = "system:node"

// ClusterRoleBindings return default rolebindings to the default roles
func ClusterRoleBindings() []rbacv1.ClusterRoleBinding {
	rolebindings := []rbacv1.ClusterRoleBinding{
		rbacv1helpers.NewClusterBinding("cluster-admin").Groups(user.SystemPrivilegedGroup).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:monitoring").Groups(user.MonitoringGroup).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:discovery").Groups(user.AllAuthenticated).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:basic-user").Groups(user.AllAuthenticated).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:public-info-viewer").Groups(user.AllAuthenticated, user.AllUnauthenticated).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:node-proxier").Users(user.KubeProxy).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:kube-controller-manager").Users(user.KubeControllerManager).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:kube-dns").SAs("kube-system", "kube-dns").BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:kube-scheduler").Users(user.KubeScheduler).BindingOrDie(),
		rbacv1helpers.NewClusterBinding("system:volume-scheduler").Users(user.KubeScheduler).BindingOrDie(),

		// This default binding of the system:node role to the system:nodes group is deprecated in 1.7 with the availability of the Node authorizer.
		// This leaves the binding, but with an empty set of subjects, so that tightening reconciliation can remove the subject.
		{
			ObjectMeta: metav1.ObjectMeta{Name: systemNodeRoleName},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: systemNodeRoleName},
		},
	}

	// Allow all in-cluster workloads (via their service accounts) to read the OIDC discovery endpoints.
	// Users with certain forms of write access (create pods, create secrets, create service accounts, etc)
	// can gain access to a service account identity which would allow them to access this information.
	// This includes the issuer URL, which is already present in the SA token JWT.  Similarly, SAs can
	// already gain this same info via introspection of their own token.  Since this discovery endpoint
	// points to what issued all service account tokens, it seems fitting for SAs to have this access.
	// Defer to the cluster admin with regard to binding directly to all authenticated and/or
	// unauthenticated users.
	rolebindings = append(rolebindings,
		rbacv1helpers.NewClusterBinding("system:service-account-issuer-discovery").Groups(serviceaccount.AllServiceAccountsGroup).BindingOrDie(),
	)

	// Service accounts can read ClusterTrustBundle objects.
	if utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundle) {
		rolebindings = append(rolebindings, rbacv1helpers.NewClusterBinding("system:cluster-trust-bundle-discovery").Groups(serviceaccount.AllServiceAccountsGroup).BindingOrDie())
	}

	addClusterRoleBindingLabel(rolebindings)

	return rolebindings
}

// ClusterRolesToAggregate maps from previous clusterrole name to the new clusterrole name
func ClusterRolesToAggregate() map[string]string {
	return map[string]string{
		"admin": "system:aggregate-to-admin",
		"edit":  "system:aggregate-to-edit",
		"view":  "system:aggregate-to-view",
	}
}

// ClusterRoleBindingsToSplit returns a map of Names of source ClusterRoleBindings
// to copy Subjects, Annotations, and Labels to destination ClusterRoleBinding templates.
func ClusterRoleBindingsToSplit() map[string]rbacv1.ClusterRoleBinding {
	bindingsToSplit := map[string]rbacv1.ClusterRoleBinding{}
	for _, defaultClusterRoleBinding := range ClusterRoleBindings() {
		switch defaultClusterRoleBinding.Name {
		case "system:public-info-viewer":
			bindingsToSplit["system:discovery"] = defaultClusterRoleBinding
		}
	}
	return bindingsToSplit
}
