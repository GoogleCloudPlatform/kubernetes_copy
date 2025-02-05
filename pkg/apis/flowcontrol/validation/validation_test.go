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

package validation

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	flowcontrolv1beta3 "k8s.io/api/flowcontrol/v1beta3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/kubernetes/pkg/apis/flowcontrol"
	"k8s.io/kubernetes/pkg/apis/flowcontrol/internalbootstrap"
	"k8s.io/utils/pointer"
)

func TestFlowSchemaValidation(t *testing.T) {
	badExempt := flowcontrol.FlowSchemaSpec{
		MatchingPrecedence: 1,
		PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
			Name: flowcontrol.PriorityLevelConfigurationNameExempt,
		},
		Rules: []flowcontrol.PolicyRulesWithSubjects{{
			Subjects: []flowcontrol.Subject{{
				Kind:  flowcontrol.SubjectKindGroup,
				Group: &flowcontrol.GroupSubject{Name: "system:masters"},
			}},
			ResourceRules: []flowcontrol.ResourcePolicyRule{{
				Verbs:        []string{flowcontrol.VerbAll},
				APIGroups:    []string{flowcontrol.APIGroupAll},
				Resources:    []string{flowcontrol.ResourceAll},
				ClusterScope: true,
				Namespaces:   []string{flowcontrol.NamespaceEvery},
			}},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
				Verbs:           []string{flowcontrol.VerbAll},
				NonResourceURLs: []string{"/"},
			}},
		}},
	}
	badCatchAll := flowcontrol.FlowSchemaSpec{
		MatchingPrecedence: flowcontrol.FlowSchemaMaxMatchingPrecedence,
		PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
			Name: flowcontrol.PriorityLevelConfigurationNameCatchAll,
		},
		DistinguisherMethod: &flowcontrol.FlowDistinguisherMethod{Type: flowcontrol.FlowDistinguisherMethodByUserType},
		Rules: []flowcontrol.PolicyRulesWithSubjects{{
			Subjects: []flowcontrol.Subject{{
				Kind:  flowcontrol.SubjectKindGroup,
				Group: &flowcontrol.GroupSubject{Name: user.AllUnauthenticated},
			}, {
				Kind:  flowcontrol.SubjectKindGroup,
				Group: &flowcontrol.GroupSubject{Name: user.AllAuthenticated},
			}},
			ResourceRules: []flowcontrol.ResourcePolicyRule{{
				Verbs:        []string{flowcontrol.VerbAll},
				APIGroups:    []string{flowcontrol.APIGroupAll},
				Resources:    []string{flowcontrol.ResourceAll},
				ClusterScope: true,
				Namespaces:   []string{flowcontrol.NamespaceEvery},
			}},
			NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
				Verbs:           []string{flowcontrol.VerbAll},
				NonResourceURLs: []string{"/"},
			}},
		}},
	}
	testCases := []struct {
		name           string
		flowSchema     *flowcontrol.FlowSchema
		expectedErrors field.ErrorList
	}{{
		name: "missing both resource and non-resource policy-rule should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Required(field.NewPath("spec").Child("rules").Index(0), "at least one of resourceRules and nonResourceRules has to be non-empty"),
		},
	}, {
		name: "normal flow-schema w/ * verbs/apiGroups/resources should work",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:  flowcontrol.SubjectKindGroup,
						Group: &flowcontrol.GroupSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "malformed Subject union in ServiceAccount case",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:  flowcontrol.SubjectKindServiceAccount,
						User:  &flowcontrol.UserSubject{Name: "fred"},
						Group: &flowcontrol.GroupSubject{Name: "fred"},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Required(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("serviceAccount"), "serviceAccount is required when subject kind is 'ServiceAccount'"),
			field.Forbidden(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("user"), "user is forbidden when subject kind is not 'User'"),
			field.Forbidden(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("group"), "group is forbidden when subject kind is not 'Group'"),
		},
	}, {
		name: "Subject union malformed in User case",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:           flowcontrol.SubjectKindUser,
						Group:          &flowcontrol.GroupSubject{Name: "fred"},
						ServiceAccount: &flowcontrol.ServiceAccountSubject{Namespace: "s", Name: "n"},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Forbidden(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("serviceAccount"), "serviceAccount is forbidden when subject kind is not 'ServiceAccount'"),
			field.Required(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("user"), "user is required when subject kind is 'User'"),
			field.Forbidden(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("group"), "group is forbidden when subject kind is not 'Group'"),
		},
	}, {
		name: "malformed Subject union in Group case",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:           flowcontrol.SubjectKindGroup,
						User:           &flowcontrol.UserSubject{Name: "fred"},
						ServiceAccount: &flowcontrol.ServiceAccountSubject{Namespace: "s", Name: "n"},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Forbidden(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("serviceAccount"), "serviceAccount is forbidden when subject kind is not 'ServiceAccount'"),
			field.Forbidden(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("user"), "user is forbidden when subject kind is not 'User'"),
			field.Required(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("group"), "group is required when subject kind is 'Group'"),
		},
	}, {
		name: "exempt flow-schema should work",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.FlowSchemaNameExempt,
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 1,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: flowcontrol.PriorityLevelConfigurationNameExempt,
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:  flowcontrol.SubjectKindGroup,
						Group: &flowcontrol.GroupSubject{Name: "system:masters"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:        []string{flowcontrol.VerbAll},
						APIGroups:    []string{flowcontrol.APIGroupAll},
						Resources:    []string{flowcontrol.ResourceAll},
						ClusterScope: true,
						Namespaces:   []string{flowcontrol.NamespaceEvery},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "bad exempt flow-schema should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.FlowSchemaNameExempt,
			},
			Spec: badExempt,
		},
		expectedErrors: field.ErrorList{field.Invalid(field.NewPath("spec"), badExempt, "spec of 'exempt' must equal the fixed value")},
	}, {
		name: "bad catch-all flow-schema should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.FlowSchemaNameCatchAll,
			},
			Spec: badCatchAll,
		},
		expectedErrors: field.ErrorList{field.Invalid(field.NewPath("spec"), badCatchAll, "spec of 'catch-all' must equal the fixed value")},
	}, {
		name: "catch-all flow-schema should work",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.FlowSchemaNameCatchAll,
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 10000,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: flowcontrol.PriorityLevelConfigurationNameCatchAll,
				},
				DistinguisherMethod: &flowcontrol.FlowDistinguisherMethod{Type: flowcontrol.FlowDistinguisherMethodByUserType},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:  flowcontrol.SubjectKindGroup,
						Group: &flowcontrol.GroupSubject{Name: user.AllUnauthenticated},
					}, {
						Kind:  flowcontrol.SubjectKindGroup,
						Group: &flowcontrol.GroupSubject{Name: user.AllAuthenticated},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:        []string{flowcontrol.VerbAll},
						APIGroups:    []string{flowcontrol.APIGroupAll},
						Resources:    []string{flowcontrol.ResourceAll},
						ClusterScope: true,
						Namespaces:   []string{flowcontrol.NamespaceEvery},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "non-exempt flow-schema with matchingPrecedence==1 should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fred",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 1,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "exempt",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind:  flowcontrol.SubjectKindGroup,
						Group: &flowcontrol.GroupSubject{Name: "gorp"},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("matchingPrecedence"), int32(1), "only the schema named 'exempt' may have matchingPrecedence 1")},
	}, {
		name: "flow-schema mixes * verbs/apiGroups/resources should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll, "create"},
						APIGroups:  []string{flowcontrol.APIGroupAll, "tak"},
						Resources:  []string{flowcontrol.ResourceAll, "tok"},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("verbs"), []string{"*", "create"}, "if '*' is present, must not specify other verbs"),
			field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("apiGroups"), []string{"*", "tak"}, "if '*' is present, must not specify other api groups"),
			field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("resources"), []string{"*", "tok"}, "if '*' is present, must not specify other resources"),
		},
	}, {
		name: "flow-schema has both resource rules and non-resource rules should work",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{flowcontrol.VerbAll},
						NonResourceURLs: []string{"/apis/*"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "flow-schema mixes * non-resource URLs should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{"*"},
						NonResourceURLs: []string{flowcontrol.NonResourceAll, "tik"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("nonResourceRules").Index(0).Child("nonResourceURLs"), []string{"*", "tik"}, "if '*' is present, must not specify other non-resource URLs"),
		},
	}, {
		name: "invalid subject kind should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: "FooKind",
					}},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
						Verbs:           []string{"*"},
						NonResourceURLs: []string{flowcontrol.NonResourceAll},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.NotSupported(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("kind"), flowcontrol.SubjectKind("FooKind"), supportedSubjectKinds.List()),
		},
	}, {
		name: "flow-schema w/ invalid verb should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{"feed"},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.NotSupported(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("verbs"), []string{"feed"}, supportedVerbs.List()),
		},
	}, {
		name: "flow-schema w/ invalid priority level configuration name should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system+++$$",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("priorityLevelConfiguration").Child("name"), "system+++$$", `a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`),
		},
	}, {
		name: "flow-schema w/ service-account kind missing namespace should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindServiceAccount,
						ServiceAccount: &flowcontrol.ServiceAccountSubject{
							Name: "noxu",
						},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Required(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("serviceAccount").Child("namespace"), "must specify namespace for service account"),
		},
	}, {
		name: "flow-schema missing kind should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: "",
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.NotSupported(field.NewPath("spec").Child("rules").Index(0).Child("subjects").Index(0).Child("kind"), flowcontrol.SubjectKind(""), supportedSubjectKinds.List()),
		},
	}, {
		name: "Omitted ResourceRule.Namespaces should fail",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: nil,
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Required(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("namespaces"), "resource rules that are not cluster scoped must supply at least one namespace"),
		},
	}, {
		name: "ClusterScope is allowed, with no Namespaces",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:        []string{flowcontrol.VerbAll},
						APIGroups:    []string{flowcontrol.APIGroupAll},
						Resources:    []string{flowcontrol.ResourceAll},
						ClusterScope: true,
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "ClusterScope is allowed with NamespaceEvery",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:        []string{flowcontrol.VerbAll},
						APIGroups:    []string{flowcontrol.APIGroupAll},
						Resources:    []string{flowcontrol.ResourceAll},
						ClusterScope: true,
						Namespaces:   []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "NamespaceEvery may not be combined with particulars",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{"foo", flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("namespaces"), []string{"foo", flowcontrol.NamespaceEvery}, "if '*' is present, must not specify other namespaces"),
		},
	}, {
		name: "ResourceRule.Namespaces must be well formed",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 50,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{"-foo"},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("resourceRules").Index(0).Child("namespaces").Index(0), "-foo", nsErrIntro+`a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')`),
		},
	}, {
		name: "MatchingPrecedence must not be greater than 10000",
		flowSchema: &flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: 10001,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: "system-bar",
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{{
					Subjects: []flowcontrol.Subject{{
						Kind: flowcontrol.SubjectKindUser,
						User: &flowcontrol.UserSubject{Name: "noxu"},
					}},
					ResourceRules: []flowcontrol.ResourcePolicyRule{{
						Verbs:      []string{flowcontrol.VerbAll},
						APIGroups:  []string{flowcontrol.APIGroupAll},
						Resources:  []string{flowcontrol.ResourceAll},
						Namespaces: []string{flowcontrol.NamespaceEvery},
					}},
				}},
			},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("matchingPrecedence"), int32(10001), "must not be greater than 10000"),
		},
	}}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errs := ValidateFlowSchema(testCase.flowSchema)
			if !assert.ElementsMatch(t, testCase.expectedErrors, errs) {
				t.Logf("mismatch: %v", cmp.Diff(testCase.expectedErrors, errs))
			}
		})
	}
}

func TestPriorityLevelConfigurationValidation(t *testing.T) {
	badSpec := flowcontrol.PriorityLevelConfigurationSpec{
		Type: flowcontrol.PriorityLevelEnablementLimited,
		Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
			NominalConcurrencyShares: 42,
			LimitResponse: flowcontrol.LimitResponse{
				Type: flowcontrol.LimitResponseTypeReject},
		},
	}

	badExemptSpec1 := flowcontrol.PriorityLevelConfigurationSpec{
		Type: flowcontrol.PriorityLevelEnablementExempt,
		Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{
			NominalConcurrencyShares: pointer.Int32(-1),
			LendablePercent:          pointer.Int32(101),
		},
	}
	badExemptSpec2 := flowcontrol.PriorityLevelConfigurationSpec{
		Type: flowcontrol.PriorityLevelEnablementExempt,
		Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{
			NominalConcurrencyShares: pointer.Int32(-1),
			LendablePercent:          pointer.Int32(-1),
		},
	}

	badExemptSpec3 := flowcontrol.PriorityLevelConfigurationSpec{
		Type:   flowcontrol.PriorityLevelEnablementExempt,
		Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{},
		Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
			NominalConcurrencyShares: 42,
			LimitResponse: flowcontrol.LimitResponse{
				Type: flowcontrol.LimitResponseTypeReject},
		},
	}

	validChangesInExemptFieldOfExemptPLFn := func() flowcontrol.PriorityLevelConfigurationSpec {
		have, _ := internalbootstrap.MandatoryPriorityLevelConfigurations[flowcontrol.PriorityLevelConfigurationNameExempt]
		return flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementExempt,
			Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{
				NominalConcurrencyShares: pointer.Int32(*have.Spec.Exempt.NominalConcurrencyShares + 10),
				LendablePercent:          pointer.Int32(*have.Spec.Exempt.LendablePercent + 10),
			},
		}
	}

	exemptTypeRepurposed := &flowcontrol.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: flowcontrol.PriorityLevelConfigurationNameExempt,
		},
		Spec: flowcontrol.PriorityLevelConfigurationSpec{
			// changing the type from exempt to limited
			Type:   flowcontrol.PriorityLevelEnablementLimited,
			Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{},
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: 42,
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeReject},
			},
		},
	}

	testCases := []struct {
		name                       string
		priorityLevelConfiguration *flowcontrol.PriorityLevelConfiguration
		requestGV                  *schema.GroupVersion
		expectedErrors             field.ErrorList
	}{{
		name: "exempt should work",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameExempt,
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementExempt,
				Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{
					NominalConcurrencyShares: pointer.Int32(0),
					LendablePercent:          pointer.Int32(0),
				},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "wrong exempt spec should fail",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameExempt,
			},
			Spec: badSpec,
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("type"), flowcontrol.PriorityLevelEnablementLimited, "type must be 'Exempt' if and only if name is 'exempt'"),
			field.Invalid(field.NewPath("spec"), badSpec, "spec of 'exempt' except the 'spec.exempt' field must equal the fixed value"),
		},
	}, {
		name: "exempt priority level should have appropriate values for Exempt field",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameExempt,
			},
			Spec: badExemptSpec1,
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("exempt").Child("nominalConcurrencyShares"), int32(-1), "must be a non-negative integer"),
			field.Invalid(field.NewPath("spec").Child("exempt").Child("lendablePercent"), int32(101), "must be between 0 and 100, inclusive"),
		},
	}, {
		name: "exempt priority level should have appropriate values for Exempt field",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameExempt,
			},
			Spec: badExemptSpec2,
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("exempt").Child("nominalConcurrencyShares"), int32(-1), "must be a non-negative integer"),
			field.Invalid(field.NewPath("spec").Child("exempt").Child("lendablePercent"), int32(-1), "must be between 0 and 100, inclusive"),
		},
	}, {
		name:                       "admins are not allowed to repurpose the 'exempt' pl to a limited type",
		priorityLevelConfiguration: exemptTypeRepurposed,
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("type"), flowcontrol.PriorityLevelEnablementLimited, "type must be 'Exempt' if and only if name is 'exempt'"),
			field.Forbidden(field.NewPath("spec").Child("exempt"), "must be nil if the type is Limited"),
			field.Invalid(field.NewPath("spec"), exemptTypeRepurposed.Spec, "spec of 'exempt' except the 'spec.exempt' field must equal the fixed value"),
		},
	}, {
		name: "admins are not allowed to change any field of the 'exempt' pl except 'Exempt'",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameExempt,
			},
			Spec: badExemptSpec3,
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec"), badExemptSpec3, "spec of 'exempt' except the 'spec.exempt' field must equal the fixed value"),
			field.Forbidden(field.NewPath("spec").Child("limited"), "must be nil if the type is not Limited"),
		},
	}, {
		name: "admins are allowed to change the Exempt field of the 'exempt' pl",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameExempt,
			},
			Spec: validChangesInExemptFieldOfExemptPLFn(),
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "limited must not set exempt priority level configuration for borrowing",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "broken-limited",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type:   flowcontrol.PriorityLevelEnablementLimited,
				Exempt: &flowcontrol.ExemptPriorityLevelConfiguration{},
			},
		},
		expectedErrors: field.ErrorList{
			field.Forbidden(field.NewPath("spec").Child("exempt"), "must be nil if the type is Limited"),
			field.Required(field.NewPath("spec").Child("limited"), "must not be empty when type is Limited"),
		},
	}, {
		name: "limited requires more details",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "broken-limited",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
			},
		},
		expectedErrors: field.ErrorList{field.Required(field.NewPath("spec").Child("limited"), "must not be empty when type is Limited")},
	}, {
		name: "max-in-flight should work",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "max-in-flight",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 42,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeReject},
				},
			},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "forbid queuing details when not queuing",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeReject,
						Queuing: &flowcontrol.QueuingConfiguration{
							Queues:           512,
							HandSize:         4,
							QueueLengthLimit: 100,
						}}}},
		},
		expectedErrors: field.ErrorList{field.Forbidden(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing"), "must be nil if limited.limitResponse.type is not Limited")},
	}, {
		name: "wrong backstop spec should fail",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameCatchAll,
			},
			Spec: badSpec,
		},
		expectedErrors: field.ErrorList{field.Invalid(field.NewPath("spec"), badSpec, "spec of 'catch-all' must equal the fixed value")},
	}, {
		name: "backstop should work",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowcontrol.PriorityLevelConfigurationNameCatchAll,
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 5,
					LendablePercent:          pointer.Int32(0),
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeReject,
					},
					Weight: pointer.Int32(10)}},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "missing QueuingConfiguration should fail",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
					}}},
		},
		expectedErrors: field.ErrorList{field.Required(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing"), "must not be empty if limited.limitResponse.type is Limited")},
	}, {
		name: "normal customized priority level should work",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
						Queuing: &flowcontrol.QueuingConfiguration{
							Queues:           512,
							HandSize:         4,
							QueueLengthLimit: 100,
						}}}},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "customized priority level w/ overflowing handSize/queues should fail 1",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
						Queuing: &flowcontrol.QueuingConfiguration{
							QueueLengthLimit: 100,
							Queues:           512,
							HandSize:         8,
						}}}},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing").Child("handSize"), int32(8), "required entropy bits of deckSize 512 and handSize 8 should not be greater than 60"),
		},
	}, {
		name: "customized priority level w/ overflowing handSize/queues should fail 2",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
						Queuing: &flowcontrol.QueuingConfiguration{
							QueueLengthLimit: 100,
							Queues:           128,
							HandSize:         10,
						}}}},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing").Child("handSize"), int32(10), "required entropy bits of deckSize 128 and handSize 10 should not be greater than 60"),
		},
	}, {
		name: "customized priority level w/ overflowing handSize/queues should fail 3",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
						Queuing: &flowcontrol.QueuingConfiguration{
							QueueLengthLimit: 100,
							Queues:           math.MaxInt32,
							HandSize:         3,
						}}}},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing").Child("handSize"), int32(3), "required entropy bits of deckSize 2147483647 and handSize 3 should not be greater than 60"),
			field.Invalid(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing").Child("queues"), int32(math.MaxInt32), "must not be greater than 10000000"),
		},
	}, {
		name: "customized priority level w/ handSize=2 and queues=10^7 should work",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
						Queuing: &flowcontrol.QueuingConfiguration{
							QueueLengthLimit: 100,
							Queues:           10 * 1000 * 1000, // 10^7
							HandSize:         2,
						}}}},
		},
		expectedErrors: field.ErrorList{},
	}, {
		name: "customized priority level w/ handSize greater than queues should fail",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-foo",
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 100,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeQueue,
						Queuing: &flowcontrol.QueuingConfiguration{
							QueueLengthLimit: 100,
							Queues:           7,
							HandSize:         8,
						}}}},
		},
		expectedErrors: field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child("limitResponse").Child("queuing").Child("handSize"), int32(8), "should not be greater than queues (7)"),
		},
	}, {
		name: "the roundtrip annotation is forbidden",
		priorityLevelConfiguration: &flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "with-forbidden-annotation",
				Annotations: map[string]string{
					flowcontrolv1beta3.PriorityLevelPreserveZeroConcurrencySharesKey: "",
				},
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: 42,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeReject},
				},
			},
		},
		// the internal object should never have the round trip annotation
		requestGV: &schema.GroupVersion{},
		expectedErrors: field.ErrorList{
			field.Forbidden(field.NewPath("metadata").Child("annotations"), fmt.Sprintf("annotation '%s' is forbidden", flowcontrolv1beta3.PriorityLevelPreserveZeroConcurrencySharesKey)),
		},
	}}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gv := flowcontrolv1beta3.SchemeGroupVersion
			if testCase.requestGV != nil {
				gv = *testCase.requestGV
			}
			errs := ValidatePriorityLevelConfiguration(testCase.priorityLevelConfiguration, gv, PriorityLevelValidationOptions{})
			if !assert.ElementsMatch(t, testCase.expectedErrors, errs) {
				t.Logf("mismatch: %v", cmp.Diff(testCase.expectedErrors, errs))
			}
		})
	}
}

func TestValidateFlowSchemaStatus(t *testing.T) {
	testCases := []struct {
		name           string
		status         *flowcontrol.FlowSchemaStatus
		expectedErrors field.ErrorList
	}{{
		name:           "empty status should work",
		status:         &flowcontrol.FlowSchemaStatus{},
		expectedErrors: field.ErrorList{},
	}, {
		name: "duplicate key should fail",
		status: &flowcontrol.FlowSchemaStatus{
			Conditions: []flowcontrol.FlowSchemaCondition{{
				Type: "1",
			}, {
				Type: "1",
			}},
		},
		expectedErrors: field.ErrorList{
			field.Duplicate(field.NewPath("status").Child("conditions").Index(1).Child("type"), flowcontrol.FlowSchemaConditionType("1")),
		},
	}, {
		name: "missing key should fail",
		status: &flowcontrol.FlowSchemaStatus{
			Conditions: []flowcontrol.FlowSchemaCondition{{
				Type: "",
			}},
		},
		expectedErrors: field.ErrorList{
			field.Required(field.NewPath("status").Child("conditions").Index(0).Child("type"), "must not be empty"),
		},
	}}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errs := ValidateFlowSchemaStatus(testCase.status, field.NewPath("status"))
			if !assert.ElementsMatch(t, testCase.expectedErrors, errs) {
				t.Logf("mismatch: %v", cmp.Diff(testCase.expectedErrors, errs))
			}
		})
	}
}

func TestValidatePriorityLevelConfigurationStatus(t *testing.T) {
	testCases := []struct {
		name           string
		status         *flowcontrol.PriorityLevelConfigurationStatus
		expectedErrors field.ErrorList
	}{{
		name:           "empty status should work",
		status:         &flowcontrol.PriorityLevelConfigurationStatus{},
		expectedErrors: field.ErrorList{},
	}, {
		name: "duplicate key should fail",
		status: &flowcontrol.PriorityLevelConfigurationStatus{
			Conditions: []flowcontrol.PriorityLevelConfigurationCondition{{
				Type: "1",
			}, {
				Type: "1",
			}},
		},
		expectedErrors: field.ErrorList{
			field.Duplicate(field.NewPath("status").Child("conditions").Index(1).Child("type"), flowcontrol.PriorityLevelConfigurationConditionType("1")),
		},
	}, {
		name: "missing key should fail",
		status: &flowcontrol.PriorityLevelConfigurationStatus{
			Conditions: []flowcontrol.PriorityLevelConfigurationCondition{{
				Type: "",
			}},
		},
		expectedErrors: field.ErrorList{
			field.Required(field.NewPath("status").Child("conditions").Index(0).Child("type"), "must not be empty"),
		},
	}}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errs := ValidatePriorityLevelConfigurationStatus(testCase.status, field.NewPath("status"))
			if !assert.ElementsMatch(t, testCase.expectedErrors, errs) {
				t.Logf("mismatch: %v", cmp.Diff(testCase.expectedErrors, errs))
			}
		})
	}
}

func TestValidateNonResourceURLPath(t *testing.T) {
	testCases := []struct {
		name           string
		path           string
		expectingError bool
	}{{
		name:           "empty string should fail",
		path:           "",
		expectingError: true,
	}, {
		name:           "no slash should fail",
		path:           "foo",
		expectingError: true,
	}, {
		name:           "single slash should work",
		path:           "/",
		expectingError: false,
	}, {
		name:           "continuous slash should fail",
		path:           "//",
		expectingError: true,
	}, {
		name:           "/foo slash should work",
		path:           "/foo",
		expectingError: false,
	}, {
		name:           "multiple continuous slashes should fail",
		path:           "/////",
		expectingError: true,
	}, {
		name:           "ending up with slash should work",
		path:           "/apis/",
		expectingError: false,
	}, {
		name:           "ending up with wildcard should work",
		path:           "/healthz/*",
		expectingError: false,
	}, {
		name:           "single wildcard inside the path should fail",
		path:           "/healthz/*/foo",
		expectingError: true,
	}, {
		name:           "white-space in the path should fail",
		path:           "/healthz/foo bar",
		expectingError: true,
	}, {
		name:           "wildcard plus plain path should fail",
		path:           "/health*",
		expectingError: true,
	}, {
		name:           "wildcard plus plain path should fail 2",
		path:           "/health*/foo",
		expectingError: true,
	}, {
		name:           "multiple wildcard internal and suffix should fail",
		path:           "/*/*",
		expectingError: true,
	}}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateNonResourceURLPath(testCase.path, field.NewPath(""))
			assert.Equal(t, testCase.expectingError, err != nil,
				"actual error: %v", err)
		})
	}
}

func TestValidateLimitedPriorityLevelConfiguration(t *testing.T) {
	errExpectedFn := func(fieldName string, v int32, msg string) field.ErrorList {
		return field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child(fieldName), int32(v), msg),
		}
	}

	tests := []struct {
		requestVersion    schema.GroupVersion
		concurrencyShares int32
		errExpected       field.ErrorList
	}{{
		requestVersion:    flowcontrolv1beta3.SchemeGroupVersion,
		concurrencyShares: 0,
		errExpected:       nil,
	}, {
		requestVersion:    flowcontrolv1beta3.SchemeGroupVersion,
		concurrencyShares: -1,
		errExpected:       errExpectedFn("nominalConcurrencyShares", -1, "must be a non-negative integer"),
	}, {
		requestVersion:    flowcontrolv1beta3.SchemeGroupVersion,
		concurrencyShares: 1,
		errExpected:       nil,
	}, {
		requestVersion:    flowcontrolv1.SchemeGroupVersion,
		concurrencyShares: 0,
		errExpected:       nil,
	}, {
		requestVersion:    flowcontrolv1.SchemeGroupVersion,
		concurrencyShares: -1,
		errExpected:       errExpectedFn("nominalConcurrencyShares", -1, "must be a non-negative integer"),
	}, {
		requestVersion:    flowcontrolv1.SchemeGroupVersion,
		concurrencyShares: 1,
		errExpected:       nil,
	}}

	for _, test := range tests {
		t.Run(test.requestVersion.String(), func(t *testing.T) {
			configuration := &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: test.concurrencyShares,
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeReject,
				},
			}
			specPath := field.NewPath("spec").Child("limited")

			errGot := ValidateLimitedPriorityLevelConfiguration(configuration, test.requestVersion, specPath, PriorityLevelValidationOptions{})
			if !cmp.Equal(test.errExpected, errGot) {
				t.Errorf("Expected error: %v, diff: %s", test.errExpected, cmp.Diff(test.errExpected, errGot))
			}
		})
	}
}

func TestValidateLimitedPriorityLevelConfigurationWithBorrowing(t *testing.T) {
	errLendablePercentFn := func(v int32) field.ErrorList {
		return field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child("lendablePercent"), v, "must be between 0 and 100, inclusive"),
		}
	}
	errBorrowingLimitPercentFn := func(v int32) field.ErrorList {
		return field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("limited").Child("borrowingLimitPercent"), v, "if specified, must be a non-negative integer"),
		}
	}

	makeTestNameFn := func(lendablePercent *int32, borrowingLimitPercent *int32) string {
		formatFn := func(v *int32) string {
			if v == nil {
				return "<nil>"
			}
			return fmt.Sprintf("%d", *v)
		}
		return fmt.Sprintf("lendablePercent %s, borrowingLimitPercent %s", formatFn(lendablePercent), formatFn(borrowingLimitPercent))
	}

	tests := []struct {
		lendablePercent       *int32
		borrowingLimitPercent *int32
		errExpected           field.ErrorList
	}{{
		lendablePercent: nil,
		errExpected:     nil,
	}, {
		lendablePercent: pointer.Int32(0),
		errExpected:     nil,
	}, {
		lendablePercent: pointer.Int32(100),
		errExpected:     nil,
	}, {
		lendablePercent: pointer.Int32(101),
		errExpected:     errLendablePercentFn(101),
	}, {
		lendablePercent: pointer.Int32(-1),
		errExpected:     errLendablePercentFn(-1),
	}, {
		borrowingLimitPercent: nil,
		errExpected:           nil,
	}, {
		borrowingLimitPercent: pointer.Int32(1),
		errExpected:           nil,
	}, {
		borrowingLimitPercent: pointer.Int32(100),
		errExpected:           nil,
	}, {
		borrowingLimitPercent: pointer.Int32(0),
		errExpected:           nil,
	}, {
		borrowingLimitPercent: pointer.Int32(-1),
		errExpected:           errBorrowingLimitPercentFn(-1),
	}}

	for _, test := range tests {
		t.Run(makeTestNameFn(test.lendablePercent, test.borrowingLimitPercent), func(t *testing.T) {
			configuration := &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: 1,
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeReject,
				},
				LendablePercent:       test.lendablePercent,
				BorrowingLimitPercent: test.borrowingLimitPercent,
			}
			specPath := field.NewPath("spec").Child("limited")

			errGot := ValidateLimitedPriorityLevelConfiguration(configuration, flowcontrolv1.SchemeGroupVersion, specPath, PriorityLevelValidationOptions{})
			if !cmp.Equal(test.errExpected, errGot) {
				t.Errorf("Expected error: %v, diff: %s", test.errExpected, cmp.Diff(test.errExpected, errGot))
			}
		})
	}
}
