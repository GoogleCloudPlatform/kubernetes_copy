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

package bootstrap

import (
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	flowcontrol "k8s.io/api/flowcontrol/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/utils/ptr"
)

// The objects that define an apiserver's initial behavior.  The
// registered defaulting procedures make no changes to these
// particular objects (this is verified in the unit tests of the
// internalbootstrap package; it can not be verified in this package
// because that would require importing k8s.io/kubernetes).
var (
	MandatoryPriorityLevelConfigurations = []*flowcontrol.PriorityLevelConfiguration{
		MandatoryPriorityLevelConfigurationCatchAll,
		MandatoryPriorityLevelConfigurationExempt,
	}
	MandatoryFlowSchemas = []*flowcontrol.FlowSchema{
		MandatoryFlowSchemaExempt,
		MandatoryFlowSchemaCatchAll,
	}
)

// The objects that define the current suggested additional configuration
var (
	SuggestedPriorityLevelConfigurations = []*flowcontrol.PriorityLevelConfiguration{
		// "system" priority-level is for the system components that affects self-maintenance of the
		// cluster and the availability of those running pods in the cluster, including kubelet and
		// kube-proxy.
		SuggestedPriorityLevelConfigurationSystem,
		// "node-high" priority-level is for the node health reporting. It is separated from "system"
		// to make sure that nodes are able to report their health even if kube-apiserver is not capable of
		// handling load caused by pod startup (fetching secrets, events etc).
		// NOTE: In large clusters 50% - 90% of all API calls use this priority-level.
		SuggestedPriorityLevelConfigurationNodeHigh,
		// "leader-election" is dedicated for controllers' leader-election, which majorly affects the
		// availability of any controller runs in the cluster.
		SuggestedPriorityLevelConfigurationLeaderElection,
		// "workload-high" is used by those workloads with higher priority but their failure won't directly
		// impact the existing running pods in the cluster, which includes kube-scheduler, and those well-known
		// built-in workloads such as "deployments", "replicasets" and other low-level custom workload which
		// is important for the cluster.
		SuggestedPriorityLevelConfigurationWorkloadHigh,
		// "workload-low" is used by those workloads with lower priority which availability only has a
		// minor impact on the cluster.
		SuggestedPriorityLevelConfigurationWorkloadLow,
		// "global-default" serves the rest traffic not handled by the other suggested flow-schemas above.
		SuggestedPriorityLevelConfigurationGlobalDefault,
	}
	SuggestedFlowSchemas = []*flowcontrol.FlowSchema{
		SuggestedFlowSchemaSystemNodes,               // references "system" priority-level
		SuggestedFlowSchemaSystemNodeHigh,            // references "node-high" priority-level
		SuggestedFlowSchemaProbes,                    // references "exempt" priority-level
		SuggestedFlowSchemaSystemLeaderElection,      // references "leader-election" priority-level
		SuggestedFlowSchemaWorkloadLeaderElection,    // references "leader-election" priority-level
		SuggestedFlowSchemaEndpointsController,       // references "workload-high" priority-level
		SuggestedFlowSchemaKubeControllerManager,     // references "workload-high" priority-level
		SuggestedFlowSchemaKubeScheduler,             // references "workload-high" priority-level
		SuggestedFlowSchemaKubeSystemServiceAccounts, // references "workload-high" priority-level
		SuggestedFlowSchemaServiceAccounts,           // references "workload-low" priority-level
		SuggestedFlowSchemaGlobalDefault,             // references "global-default" priority-level
	}
)

// Mandatory PriorityLevelConfiguration objects
var (
	MandatoryPriorityLevelConfigurationExempt = newPriorityLevelConfiguration(
		flowcontrol.PriorityLevelConfigurationNameExempt,
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementExempt,
			Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(0)),
				LendablePercent:          ptr.To(int32(0)),
			},
		},
	)
	MandatoryPriorityLevelConfigurationCatchAll = newPriorityLevelConfiguration(
		flowcontrol.PriorityLevelConfigurationNameCatchAll,
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(5)),
				LendablePercent:          ptr.To(int32(0)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeReject,
				},
			},
		})
)

// Mandatory FlowSchema objects
var (
	// "exempt" priority-level is used for preventing priority inversion and ensuring that sysadmin
	// requests are always possible.
	MandatoryFlowSchemaExempt = newFlowSchema(
		"exempt",
		flowcontrol.PriorityLevelConfigurationNameExempt,
		1,  // matchingPrecedence
		"", // distinguisherMethodType
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(user.SystemPrivilegedGroup),
			ResourceRules: []flowcontrol.ResourcePolicyRule{
				resourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.APIGroupAll},
					[]string{flowcontrol.ResourceAll},
					[]string{flowcontrol.NamespaceEvery},
					true,
				),
			},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll},
				),
			},
		},
	)
	// "catch-all" priority-level only gets a minimal positive share of concurrency and won't be reaching
	// ideally unless you intentionally deleted the suggested "global-default".
	MandatoryFlowSchemaCatchAll = newFlowSchema(
		flowcontrol.FlowSchemaNameCatchAll,
		flowcontrol.PriorityLevelConfigurationNameCatchAll,
		10000, // matchingPrecedence
		flowcontrol.FlowDistinguisherMethodByUserType, // distinguisherMethodType
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(user.AllUnauthenticated, user.AllAuthenticated),
			ResourceRules: []flowcontrol.ResourcePolicyRule{
				resourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.APIGroupAll},
					[]string{flowcontrol.ResourceAll},
					[]string{flowcontrol.NamespaceEvery},
					true,
				),
			},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll},
				),
			},
		},
	)
)

// Suggested PriorityLevelConfiguration objects
var (
	// system priority-level
	SuggestedPriorityLevelConfigurationSystem = newPriorityLevelConfiguration(
		"system",
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(30)),
				LendablePercent:          ptr.To(int32(33)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeQueue,
					Queuing: &flowcontrol.QueuingConfiguration{
						Queues:           64,
						HandSize:         6,
						QueueLengthLimit: 50,
					},
				},
			},
		})
	SuggestedPriorityLevelConfigurationNodeHigh = newPriorityLevelConfiguration(
		"node-high",
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(40)),
				LendablePercent:          ptr.To(int32(25)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeQueue,
					Queuing: &flowcontrol.QueuingConfiguration{
						Queues:           64,
						HandSize:         6,
						QueueLengthLimit: 50,
					},
				},
			},
		})
	// leader-election priority-level
	SuggestedPriorityLevelConfigurationLeaderElection = newPriorityLevelConfiguration(
		"leader-election",
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(40)),
				LendablePercent:          ptr.To(int32(0)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeQueue,
					Queuing: &flowcontrol.QueuingConfiguration{
						Queues:           16,
						HandSize:         4,
						QueueLengthLimit: 50,
					},
				},
			},
		})
	// workload-high priority-level
	SuggestedPriorityLevelConfigurationWorkloadHigh = newPriorityLevelConfiguration(
		"workload-high",
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(40)),
				LendablePercent:          ptr.To(int32(50)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeQueue,
					Queuing: &flowcontrol.QueuingConfiguration{
						Queues:           128,
						HandSize:         6,
						QueueLengthLimit: 50,
					},
				},
			},
		})
	// workload-low priority-level
	SuggestedPriorityLevelConfigurationWorkloadLow = newPriorityLevelConfiguration(
		"workload-low",
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(100)),
				LendablePercent:          ptr.To(int32(90)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeQueue,
					Queuing: &flowcontrol.QueuingConfiguration{
						Queues:           128,
						HandSize:         6,
						QueueLengthLimit: 50,
					},
				},
			},
		})
	// global-default priority-level
	SuggestedPriorityLevelConfigurationGlobalDefault = newPriorityLevelConfiguration(
		"global-default",
		flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(20)),
				LendablePercent:          ptr.To(int32(50)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeQueue,
					Queuing: &flowcontrol.QueuingConfiguration{
						Queues:           128,
						HandSize:         6,
						QueueLengthLimit: 50,
					},
				},
			},
		})
)

// Suggested FlowSchema objects.
// Ordered by matching precedence, so that their interactions are easier
// to follow while reading this source.
var (
	// the following flow schema exempts probes
	SuggestedFlowSchemaProbes = newFlowSchema(
		"probes", "exempt", 2,
		"", // distinguisherMethodType
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(user.AllUnauthenticated, user.AllAuthenticated),
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{"get"},
					[]string{"/healthz", "/readyz", "/livez"}),
			},
		},
	)
	SuggestedFlowSchemaSystemLeaderElection = newFlowSchema(
		"system-leader-election", "leader-election", 100,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: append(
				users(user.KubeControllerManager, user.KubeScheduler),
				kubeSystemServiceAccount(flowcontrol.NameAll)...),
			ResourceRules: []flowcontrol.ResourcePolicyRule{
				resourceRule(
					[]string{"get", "create", "update"},
					[]string{coordinationv1.GroupName},
					[]string{"leases"},
					[]string{flowcontrol.NamespaceEvery},
					false),
			},
		},
	)
	// We add an explicit rule for endpoint-controller with high precedence
	// to ensure that those calls won't get caught by the following
	// <workload-leader-election> flow-schema.
	//
	// TODO(#80289): Get rid of this rule once we get rid of support for
	//   using endpoints and configmaps objects for leader election.
	SuggestedFlowSchemaEndpointsController = newFlowSchema(
		"endpoint-controller", "workload-high", 150,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: append(
				users(user.KubeControllerManager),
				kubeSystemServiceAccount("endpoint-controller", "endpointslicemirroring-controller")...),
			ResourceRules: []flowcontrol.ResourcePolicyRule{
				resourceRule(
					[]string{"get", "create", "update"},
					[]string{corev1.GroupName},
					[]string{"endpoints"},
					[]string{flowcontrol.NamespaceEvery},
					false),
			},
		},
	)
	// TODO(#80289): Get rid of this rule once we get rid of support for
	//   using endpoints and configmaps objects for leader election.
	SuggestedFlowSchemaWorkloadLeaderElection = newFlowSchema(
		"workload-leader-election", "leader-election", 200,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: kubeSystemServiceAccount(flowcontrol.NameAll),
			ResourceRules: []flowcontrol.ResourcePolicyRule{
				resourceRule(
					[]string{"get", "create", "update"},
					[]string{corev1.GroupName},
					[]string{"endpoints", "configmaps"},
					[]string{flowcontrol.NamespaceEvery},
					false),
				resourceRule(
					[]string{"get", "create", "update"},
					[]string{coordinationv1.GroupName},
					[]string{"leases"},
					[]string{flowcontrol.NamespaceEvery},
					false),
			},
		},
	)
	SuggestedFlowSchemaSystemNodeHigh = newFlowSchema(
		"system-node-high", "node-high", 400,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(user.NodesGroup), // the nodes group
			ResourceRules: []flowcontrol.ResourcePolicyRule{
				resourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{corev1.GroupName},
					[]string{"nodes", "nodes/status"},
					[]string{flowcontrol.NamespaceEvery},
					true),
				resourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{coordinationv1.GroupName},
					[]string{"leases"},
					[]string{flowcontrol.NamespaceEvery},
					false),
			},
		},
	)
	SuggestedFlowSchemaSystemNodes = newFlowSchema(
		"system-nodes", "system", 500,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(user.NodesGroup), // the nodes group
			ResourceRules: []flowcontrol.ResourcePolicyRule{resourceRule(
				[]string{flowcontrol.VerbAll},
				[]string{flowcontrol.APIGroupAll},
				[]string{flowcontrol.ResourceAll},
				[]string{flowcontrol.NamespaceEvery},
				true)},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll}),
			},
		},
	)
	SuggestedFlowSchemaKubeControllerManager = newFlowSchema(
		"kube-controller-manager", "workload-high", 800,
		flowcontrol.FlowDistinguisherMethodByNamespaceType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: users(user.KubeControllerManager),
			ResourceRules: []flowcontrol.ResourcePolicyRule{resourceRule(
				[]string{flowcontrol.VerbAll},
				[]string{flowcontrol.APIGroupAll},
				[]string{flowcontrol.ResourceAll},
				[]string{flowcontrol.NamespaceEvery},
				true)},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll}),
			},
		},
	)
	SuggestedFlowSchemaKubeScheduler = newFlowSchema(
		"kube-scheduler", "workload-high", 800,
		flowcontrol.FlowDistinguisherMethodByNamespaceType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: users(user.KubeScheduler),
			ResourceRules: []flowcontrol.ResourcePolicyRule{resourceRule(
				[]string{flowcontrol.VerbAll},
				[]string{flowcontrol.APIGroupAll},
				[]string{flowcontrol.ResourceAll},
				[]string{flowcontrol.NamespaceEvery},
				true)},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll}),
			},
		},
	)
	SuggestedFlowSchemaKubeSystemServiceAccounts = newFlowSchema(
		"kube-system-service-accounts", "workload-high", 900,
		flowcontrol.FlowDistinguisherMethodByNamespaceType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: kubeSystemServiceAccount(flowcontrol.NameAll),
			ResourceRules: []flowcontrol.ResourcePolicyRule{resourceRule(
				[]string{flowcontrol.VerbAll},
				[]string{flowcontrol.APIGroupAll},
				[]string{flowcontrol.ResourceAll},
				[]string{flowcontrol.NamespaceEvery},
				true)},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll}),
			},
		},
	)
	SuggestedFlowSchemaServiceAccounts = newFlowSchema(
		"service-accounts", "workload-low", 9000,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(serviceaccount.AllServiceAccountsGroup),
			ResourceRules: []flowcontrol.ResourcePolicyRule{resourceRule(
				[]string{flowcontrol.VerbAll},
				[]string{flowcontrol.APIGroupAll},
				[]string{flowcontrol.ResourceAll},
				[]string{flowcontrol.NamespaceEvery},
				true)},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll}),
			},
		},
	)
	SuggestedFlowSchemaGlobalDefault = newFlowSchema(
		"global-default", "global-default", 9900,
		flowcontrol.FlowDistinguisherMethodByUserType,
		flowcontrol.PolicyRulesWithSubjects{
			Subjects: groups(user.AllUnauthenticated, user.AllAuthenticated),
			ResourceRules: []flowcontrol.ResourcePolicyRule{resourceRule(
				[]string{flowcontrol.VerbAll},
				[]string{flowcontrol.APIGroupAll},
				[]string{flowcontrol.ResourceAll},
				[]string{flowcontrol.NamespaceEvery},
				true)},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{
				nonResourceRule(
					[]string{flowcontrol.VerbAll},
					[]string{flowcontrol.NonResourceAll}),
			},
		},
	)
)

func newPriorityLevelConfiguration(name string, spec flowcontrol.PriorityLevelConfigurationSpec) *flowcontrol.PriorityLevelConfiguration {
	return &flowcontrol.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				flowcontrol.AutoUpdateAnnotationKey: "true",
			},
		},
		Spec: spec,
	}
}

func newFlowSchema(name, plName string, matchingPrecedence int32, dmType flowcontrol.FlowDistinguisherMethodType, rules ...flowcontrol.PolicyRulesWithSubjects) *flowcontrol.FlowSchema {
	var dm *flowcontrol.FlowDistinguisherMethod
	if dmType != "" {
		dm = &flowcontrol.FlowDistinguisherMethod{Type: dmType}
	}
	return &flowcontrol.FlowSchema{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				flowcontrol.AutoUpdateAnnotationKey: "true",
			},
		},
		Spec: flowcontrol.FlowSchemaSpec{
			PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
				Name: plName,
			},
			MatchingPrecedence:  matchingPrecedence,
			DistinguisherMethod: dm,
			Rules:               rules},
	}

}

func groups(names ...string) []flowcontrol.Subject {
	ans := make([]flowcontrol.Subject, len(names))
	for idx, name := range names {
		ans[idx] = flowcontrol.Subject{
			Kind: flowcontrol.SubjectKindGroup,
			Group: &flowcontrol.GroupSubject{
				Name: name,
			},
		}
	}
	return ans
}

func users(names ...string) []flowcontrol.Subject {
	ans := make([]flowcontrol.Subject, len(names))
	for idx, name := range names {
		ans[idx] = flowcontrol.Subject{
			Kind: flowcontrol.SubjectKindUser,
			User: &flowcontrol.UserSubject{
				Name: name,
			},
		}
	}
	return ans
}

func kubeSystemServiceAccount(names ...string) []flowcontrol.Subject {
	subjects := []flowcontrol.Subject{}
	for _, name := range names {
		subjects = append(subjects, flowcontrol.Subject{
			Kind: flowcontrol.SubjectKindServiceAccount,
			ServiceAccount: &flowcontrol.ServiceAccountSubject{
				Name:      name,
				Namespace: metav1.NamespaceSystem,
			},
		})
	}
	return subjects
}

func resourceRule(verbs []string, groups []string, resources []string, namespaces []string, clusterScoped bool) flowcontrol.ResourcePolicyRule {
	return flowcontrol.ResourcePolicyRule{
		Verbs:        verbs,
		APIGroups:    groups,
		Resources:    resources,
		Namespaces:   namespaces,
		ClusterScope: clusterScoped,
	}
}

func nonResourceRule(verbs []string, nonResourceURLs []string) flowcontrol.NonResourcePolicyRule {
	return flowcontrol.NonResourcePolicyRule{Verbs: verbs, NonResourceURLs: nonResourceURLs}
}
