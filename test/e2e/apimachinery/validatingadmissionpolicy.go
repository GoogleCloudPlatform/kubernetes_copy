/*
Copyright 2022 The Kubernetes Authors.

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

package apimachinery

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	applyadmissionregistrationv1 "k8s.io/client-go/applyconfigurations/admissionregistration/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/openapi3"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("ValidatingAdmissionPolicy [Privileged:ClusterAdmin]", func() {
	f := framework.NewDefaultFramework("validating-admission-policy")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	var client clientset.Interface
	var extensionsClient apiextensionsclientset.Interface

	ginkgo.BeforeEach(func() {
		var err error
		client, err = clientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "initializing client")
		extensionsClient, err = apiextensionsclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "initializing api-extensions client")
	})

	ginkgo.BeforeEach(func(ctx context.Context) {
		// Make sure the namespace created for the test is labeled to be selected
		// in binding.spec.matchResources.namespaceSelector.matchLabels
		// By containing the tests within the marked namespace, they will not
		// disturb concurrent tests that run in other namespaces.
		labelNamespace(ctx, f, f.Namespace.Name)
	})

	/*
	   Release: v1.30
	   Testname: ValidatingAdmissionPolicy
	   Description:
	   The ValidatingAdmissionPolicy should validate a deployment as the expression defined inside the policy.
	*/
	framework.ConformanceIt("should validate against a Deployment", func(ctx context.Context) {
		ginkgo.By("creating the policy", func() {
			policy := newValidatingAdmissionPolicyBuilder(f.UniqueName+".policy.example.com").
				MatchUniqueNamespace(f.UniqueName).
				StartResourceRule().
				MatchResource([]string{"apps"}, []string{"v1"}, []string{"deployments"}).
				EndResourceRule().
				WithValidation(admissionregistrationv1.Validation{
					Expression:        "object.spec.replicas > 1",
					MessageExpression: "'wants replicas > 1, got ' + object.spec.replicas",
				}).
				WithValidation(admissionregistrationv1.Validation{
					Expression: "namespaceObject.metadata.name == '" + f.UniqueName + "'",
					Message:    "Internal error! Other namespace should not be allowed.",
				}).
				Build()
			policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
			}, policy.Name)
			binding := createBinding(f.UniqueName+".binding.example.com", f.UniqueName, policy.Name)
			binding, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy binding")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(ctx, name, metav1.DeleteOptions{})
			}, binding.Name)
		})
		ginkgo.By("waiting until the marker is denied", func() {
			deployment := basicDeployment("marker-deployment", 1)
			err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				_, err = client.AppsV1().Deployments(f.Namespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
				defer client.AppsV1().Deployments(f.Namespace.Name).Delete(ctx, deployment.Name, metav1.DeleteOptions{})
				if err != nil {
					if apierrors.IsInvalid(err) {
						return true, nil
					}
					return false, err
				}
				return false, nil
			})
			framework.ExpectNoError(err, "wait for marker")
		})
		ginkgo.By("testing a replicated Deployment to be allowed", func() {
			deployment := basicDeployment("replicated", 2)
			deployment, err := client.AppsV1().Deployments(f.Namespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
			defer client.AppsV1().Deployments(f.Namespace.Name).Delete(ctx, deployment.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "create replicated Deployment")
		})
		ginkgo.By("testing a non-replicated ReplicaSet not to be denied", func() {
			replicaSet := basicReplicaSet("non-replicated", 1)
			replicaSet, err := client.AppsV1().ReplicaSets(f.Namespace.Name).Create(ctx, replicaSet, metav1.CreateOptions{})
			defer client.AppsV1().ReplicaSets(f.Namespace.Name).Delete(ctx, replicaSet.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "create non-replicated ReplicaSet")
		})
	})

	/*
	   Release: v1.30
	   Testname: ValidatingAdmissionPolicy
	   Description:
	   The ValidatingAdmissionPolicy should type check the expressions defined inside policy.
	*/
	framework.It("should type check validation expressions", func(ctx context.Context) {
		var policy *admissionregistrationv1.ValidatingAdmissionPolicy
		ginkgo.By("creating the policy with correct types", func() {
			policy = newValidatingAdmissionPolicyBuilder(f.UniqueName+".correct-policy.example.com").
				MatchUniqueNamespace(f.UniqueName).
				StartResourceRule().
				MatchResource([]string{"apps"}, []string{"v1"}, []string{"deployments"}).
				EndResourceRule().
				WithValidation(admissionregistrationv1.Validation{
					Expression: "object.spec.replicas > 1",
				}).
				Build()
			var err error
			policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
			}, policy.Name)
		})
		ginkgo.By("waiting for the type check to finish without any warnings", func() {
			err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if policy.Status.TypeChecking != nil { // non-nil TypeChecking indicates its completion
					return true, nil
				}
				return false, nil
			})
			framework.ExpectNoError(err, "wait for type checking")
			gomega.Expect(policy.Status.TypeChecking.ExpressionWarnings).To(gomega.BeEmpty())
		})
		ginkgo.By("creating the policy with type confusion", func() {
			policy = newValidatingAdmissionPolicyBuilder(f.UniqueName+".confused-policy.example.com").
				MatchUniqueNamespace(f.UniqueName).
				StartResourceRule().
				MatchResource([]string{"apps"}, []string{"v1"}, []string{"deployments"}).
				EndResourceRule().
				WithValidation(admissionregistrationv1.Validation{
					Expression:        "object.spec.replicas > '1'",                        // confusion: int > string
					MessageExpression: "'wants replicas > 1, got ' + object.spec.replicas", // confusion: string + int
				}).
				Build()
			var err error
			policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
			}, policy.Name)
		})
		ginkgo.By("waiting for the type check to finish with warnings", func() {
			err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if policy.Status.TypeChecking != nil { // non-nil TypeChecking indicates its completion
					return true, nil
				}
				return false, nil
			})
			framework.ExpectNoError(err, "wait for type checking")

			// assert it to contain 2 warnings, first for expression and second for messageExpression
			gomega.Expect(policy.Status.TypeChecking.ExpressionWarnings).To(gomega.HaveLen(2))
			warning := policy.Status.TypeChecking.ExpressionWarnings[0]
			gomega.Expect(warning.FieldRef).To(gomega.Equal("spec.validations[0].expression"))
			gomega.Expect(warning.Warning).To(gomega.ContainSubstring("found no matching overload for '_>_' applied to '(int, string)'"))
			warning = policy.Status.TypeChecking.ExpressionWarnings[1]
			gomega.Expect(warning.FieldRef).To(gomega.Equal("spec.validations[0].messageExpression"))
			gomega.Expect(warning.Warning).To(gomega.ContainSubstring("found no matching overload for '_+_' applied to '(string, int)'"))
		})
	})

	/*
	   Release: v1.30
	   Testname: ValidatingAdmissionPolicy
	   Description:
	   The ValidatingAdmissionPolicy should allow expressions to refer variables.
	*/
	framework.ConformanceIt("should allow expressions to refer variables.", func(ctx context.Context) {
		ginkgo.By("creating a policy with variables", func() {
			policy := newValidatingAdmissionPolicyBuilder(f.UniqueName+".policy.example.com").
				MatchUniqueNamespace(f.UniqueName).
				StartResourceRule().
				MatchResource([]string{"apps"}, []string{"v1"}, []string{"deployments"}).
				EndResourceRule().
				WithVariable(admissionregistrationv1.Variable{
					Name:       "replicas",
					Expression: "object.spec.replicas",
				}).
				WithVariable(admissionregistrationv1.Variable{
					Name:       "oddReplicas",
					Expression: "variables.replicas % 2 == 1",
				}).
				WithValidation(admissionregistrationv1.Validation{
					Expression: "variables.replicas > 1",
				}).
				WithValidation(admissionregistrationv1.Validation{
					Expression: "variables.oddReplicas",
				}).
				Build()
			policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
			}, policy.Name)
			binding := createBinding(f.UniqueName+".binding.example.com", f.UniqueName, policy.Name)
			binding, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy binding")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(ctx, name, metav1.DeleteOptions{})
			}, binding.Name)
		})
		ginkgo.By("waiting until the marker is denied", func() {
			deployment := basicDeployment("marker-deployment", 1)
			err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				_, err = client.AppsV1().Deployments(f.Namespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
				defer client.AppsV1().Deployments(f.Namespace.Name).Delete(ctx, deployment.Name, metav1.DeleteOptions{})
				if err != nil {
					if apierrors.IsInvalid(err) {
						return true, nil
					}
					return false, err
				}
				return false, nil
			})
			framework.ExpectNoError(err, "wait for marker")
		})
		ginkgo.By("testing a replicated Deployment to be allowed", func() {
			deployment := basicDeployment("replicated", 3)
			deployment, err := client.AppsV1().Deployments(f.Namespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
			defer client.AppsV1().Deployments(f.Namespace.Name).Delete(ctx, deployment.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "create replicated Deployment")
		})
		ginkgo.By("testing a non-replicated ReplicaSet not to be denied", func() {
			replicaSet := basicReplicaSet("non-replicated", 1)
			replicaSet, err := client.AppsV1().ReplicaSets(f.Namespace.Name).Create(ctx, replicaSet, metav1.CreateOptions{})
			defer client.AppsV1().ReplicaSets(f.Namespace.Name).Delete(ctx, replicaSet.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "create non-replicated ReplicaSet")
		})
	})

	/*
	   Release: v1.30
	   Testname: ValidatingAdmissionPolicy
	   Description:
	   The ValidatingAdmissionPolicy should type check a CRD.
	*/
	framework.It("should type check a CRD", func(ctx context.Context) {
		crd := crontabExampleCRD()
		crd.Spec.Group = "stable." + f.UniqueName
		crd.Name = crd.Spec.Names.Plural + "." + crd.Spec.Group
		var policy *admissionregistrationv1.ValidatingAdmissionPolicy
		ginkgo.By("creating the CRD", func() {
			var err error
			crd, err = extensionsClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create CRD")
			err = wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				// wait for the CRD to be published.
				root := openapi3.NewRoot(client.Discovery().OpenAPIV3())
				_, err = root.GVSpec(schema.GroupVersion{Group: crd.Spec.Group, Version: "v1"})
				return err == nil, nil
			})
			framework.ExpectNoError(err, "wait for CRD.")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return extensionsClient.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, name, metav1.DeleteOptions{})
			}, crd.Name)
		})
		ginkgo.By("creating a valid policy for crontabs", func() {
			policy = newValidatingAdmissionPolicyBuilder(f.UniqueName+".correct-crd-policy.example.com").
				MatchUniqueNamespace(f.UniqueName).
				StartResourceRule().
				MatchResource([]string{crd.Spec.Group}, []string{"v1"}, []string{"crontabs"}).
				EndResourceRule().
				WithValidation(admissionregistrationv1.Validation{
					Expression: "object.spec.replicas > 1",
				}).
				Build()
			policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
			}, policy.Name)
		})
		ginkgo.By("waiting for the type check to finish without warnings", func() {
			err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if policy.Status.TypeChecking != nil {
					return true, nil
				}
				return false, nil
			})
			framework.ExpectNoError(err, "wait for type checking")
			gomega.Expect(policy.Status.TypeChecking.ExpressionWarnings).To(gomega.BeEmpty(), "expect no warnings")
		})
		ginkgo.By("creating a policy with type-confused expressions for crontabs", func() {
			policy = newValidatingAdmissionPolicyBuilder(f.UniqueName+".confused-crd-policy.example.com").
				MatchUniqueNamespace(f.UniqueName).
				StartResourceRule().
				MatchResource([]string{crd.Spec.Group}, []string{"v1"}, []string{"crontabs"}).
				EndResourceRule().
				WithValidation(admissionregistrationv1.Validation{
					Expression: "object.spec.replicas > '1'", // type confusion
				}).
				WithValidation(admissionregistrationv1.Validation{
					Expression: "object.spec.maxRetries < 10", // not yet existing field
				}).
				Build()
			policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create policy")
			ginkgo.DeferCleanup(func(ctx context.Context, name string) error {
				return client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
			}, policy.Name)
		})
		ginkgo.By("waiting for the type check to finish with warnings", func() {
			err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
				policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if policy.Status.TypeChecking != nil {
					// TODO(#123829) Remove once the schema watcher is merged.
					// If the warnings are empty, touch the policy to retry type checking
					if len(policy.Status.TypeChecking.ExpressionWarnings) == 0 {
						randomValue := fmt.Sprintf(`"%d"`, rand.Int())
						// Use a fixed key and change only the value
						applyConfig := applyadmissionregistrationv1.ValidatingAdmissionPolicy(policy.Name).
							WithSpec(applyadmissionregistrationv1.ValidatingAdmissionPolicySpec().
								WithAuditAnnotations(applyadmissionregistrationv1.AuditAnnotation().
									WithKey("key").
									WithValueExpression(randomValue),
								),
							)
						_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Apply(ctx, applyConfig, metav1.ApplyOptions{FieldManager: "validatingadmissionpolicy-e2e"})
						return false, err
					}
					return true, nil
				}
				return false, nil
			})
			framework.ExpectNoError(err, "wait for type checking")

			gomega.Expect(policy.Status.TypeChecking.ExpressionWarnings).To(gomega.HaveLen(2))
			warning := policy.Status.TypeChecking.ExpressionWarnings[0]
			gomega.Expect(warning.FieldRef).To(gomega.Equal("spec.validations[0].expression"))
			gomega.Expect(warning.Warning).To(gomega.ContainSubstring("found no matching overload for '_>_' applied to '(int, string)'"))
			warning = policy.Status.TypeChecking.ExpressionWarnings[1]
			gomega.Expect(warning.FieldRef).To(gomega.Equal("spec.validations[1].expression"))
			gomega.Expect(warning.Warning).To(gomega.ContainSubstring("undefined field 'maxRetries'"))
		})
	})

	/*
	   Release: v1.30
	   Testname: ValidatingAdmissionPolicy API
	   Description:
	   The admissionregistration.k8s.io API group MUST exist in the
	     /apis discovery document.
	   The admissionregistration.k8s.io/v1 API group/version MUST exist
	     in the /apis/admissionregistration.k8s.io discovery document.
	   The validatingadmisionpolicy and validatingadmissionpolicy/status
	     resources MUST exist in the
	     /apis/admissionregistration.k8s.io/v1 discovery document.
	   The validatingadmisionpolicy resource must support create, get,
	     list, watch, update, patch, delete, and deletecollection.
	*/
	framework.ConformanceIt("should support ValidatingAdmissionPolicy API operations", func(ctx context.Context) {
		vapVersion := "v1"
		ginkgo.By("getting /apis")
		{
			discoveryGroups, err := f.ClientSet.Discovery().ServerGroups()
			framework.ExpectNoError(err)
			found := false
			for _, group := range discoveryGroups.Groups {
				if group.Name == admissionregistrationv1.GroupName {
					for _, version := range group.Versions {
						if version.Version == vapVersion {
							found = true
							break
						}
					}
				}
			}
			if !found {
				framework.Failf("expected ValidatingAdmissionPolicy API group/version, got %#v", discoveryGroups.Groups)
			}
		}

		ginkgo.By("getting /apis/admissionregistration.k8s.io")
		{
			group := &metav1.APIGroup{}
			err := f.ClientSet.Discovery().RESTClient().Get().AbsPath("/apis/admissionregistration.k8s.io").Do(ctx).Into(group)
			framework.ExpectNoError(err)
			found := false
			for _, version := range group.Versions {
				if version.Version == vapVersion {
					found = true
					break
				}
			}
			if !found {
				framework.Failf("expected ValidatingAdmissionPolicy API version, got %#v", group.Versions)
			}
		}

		ginkgo.By("getting /apis/admissionregistration.k8s.io/" + vapVersion)
		{
			resources, err := f.ClientSet.Discovery().ServerResourcesForGroupVersion(admissionregistrationv1.SchemeGroupVersion.String())
			framework.ExpectNoError(err)
			foundVAP, foundVAPStatus := false, false
			for _, resource := range resources.APIResources {
				switch resource.Name {
				case "validatingadmissionpolicies":
					foundVAP = true
				case "validatingadmissionpolicies/status":
					foundVAPStatus = true
				}
			}
			if !foundVAP {
				framework.Failf("expected validatingadmissionpolicies, got %#v", resources.APIResources)
			}
			if !foundVAPStatus {
				framework.Failf("expected validatingadmissionpolicies/status, got %#v", resources.APIResources)
			}
		}

		client := f.ClientSet.AdmissionregistrationV1().ValidatingAdmissionPolicies()
		labelKey, labelValue := "example-e2e-vap-label", utilrand.String(8)
		label := fmt.Sprintf("%s=%s", labelKey, labelValue)

		template := &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "e2e-example-vap-",
				Labels: map[string]string{
					labelKey: labelValue,
				},
			},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
				Validations: []admissionregistrationv1.Validation{
					{
						Expression: "object.spec.replicas <= 100",
					},
				},
				MatchConstraints: &admissionregistrationv1.MatchResources{
					ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
						{
							RuleWithOperations: admissionregistrationv1.RuleWithOperations{
								Operations: []admissionregistrationv1.OperationType{"CREATE"},
								Rule: admissionregistrationv1.Rule{
									APIGroups:   []string{"apps"},
									APIVersions: []string{"v1"},
									Resources:   []string{"deployments"},
								},
							},
						},
					},
				},
			},
		}

		ginkgo.DeferCleanup(func(ctx context.Context) {
			err := client.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: label})
			framework.ExpectNoError(err)
		})

		ginkgo.By("creating")
		_, err := client.Create(ctx, template, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		_, err = client.Create(ctx, template, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		vapCreated, err := client.Create(ctx, template, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("getting")
		vapRead, err := client.Get(ctx, vapCreated.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(vapRead.UID).To(gomega.Equal(vapCreated.UID))

		ginkgo.By("listing")
		list, err := client.List(ctx, metav1.ListOptions{LabelSelector: label})
		framework.ExpectNoError(err)

		ginkgo.By("watching")
		framework.Logf("starting watch")
		vapWatch, err := client.Watch(ctx, metav1.ListOptions{ResourceVersion: list.ResourceVersion, LabelSelector: label})
		framework.ExpectNoError(err)

		ginkgo.By("patching")
		patchBytes := []byte(`{"metadata":{"annotations":{"patched":"true"}},"spec":{"failurePolicy":"Ignore"}}`)
		vapPatched, err := client.Patch(ctx, vapCreated.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(vapPatched.Annotations).To(gomega.HaveKeyWithValue("patched", "true"), "patched object should have the applied annotation")
		gomega.Expect(vapPatched.Spec.FailurePolicy).To(gomega.HaveValue(gomega.Equal(admissionregistrationv1.Ignore)), "patched object should have the applied spec")

		ginkgo.By("updating")
		var vapUpdated *admissionregistrationv1.ValidatingAdmissionPolicy
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vap, err := client.Get(ctx, vapCreated.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			vapToUpdate := vap.DeepCopy()
			vapToUpdate.Annotations["updated"] = "true"
			fail := admissionregistrationv1.Fail
			vapToUpdate.Spec.FailurePolicy = &fail

			vapUpdated, err = client.Update(ctx, vapToUpdate, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err, "failed to update validatingadmissionpolicy %q", vapCreated.Name)
		gomega.Expect(vapUpdated.Annotations).To(gomega.HaveKeyWithValue("updated", "true"), "updated object should have the applied annotation")
		gomega.Expect(vapUpdated.Spec.FailurePolicy).To(gomega.HaveValue(gomega.Equal(admissionregistrationv1.Fail)), "updated object should have the applied spec")

		framework.Logf("waiting for watch events with expected annotations")
		for sawAnnotation := false; !sawAnnotation; {
			select {
			case evt, ok := <-vapWatch.ResultChan():
				if !ok {
					framework.Fail("watch channel should not close")
				}
				gomega.Expect(evt.Type).To(gomega.Equal(watch.Modified))
				vapWatched, isFS := evt.Object.(*admissionregistrationv1.ValidatingAdmissionPolicy)
				if !isFS {
					framework.Failf("expected an object of type: %T, but got %T", &admissionregistrationv1.ValidatingAdmissionPolicy{}, evt.Object)
				}
				if vapWatched.Annotations["patched"] == "true" {
					sawAnnotation = true
					vapWatch.Stop()
				} else {
					framework.Logf("missing expected annotations, waiting: %#v", vapWatched.Annotations)
				}
			case <-time.After(wait.ForeverTestTimeout):
				framework.Fail("timed out waiting for watch event")
			}
		}

		ginkgo.By("getting /status")
		resource := admissionregistrationv1.SchemeGroupVersion.WithResource("validatingadmissionpolicies")
		vapStatusRead, err := f.DynamicClient.Resource(resource).Get(ctx, vapCreated.Name, metav1.GetOptions{}, "status")
		framework.ExpectNoError(err)
		gomega.Expect(vapStatusRead.GetObjectKind().GroupVersionKind()).To(gomega.Equal(admissionregistrationv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicy")))
		gomega.Expect(vapStatusRead.GetUID()).To(gomega.Equal(vapCreated.UID))

		ginkgo.By("patching /status")
		patchBytes = []byte(`{"status":{"conditions":[{"type":"PatchStatusFailed","status":"False","reason":"e2e","message":"Set from an e2e test","lastTransitionTime":"2024-01-01T00:00:00Z"}]}}`)
		vapStatusPatched, err := client.Patch(ctx, vapCreated.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
		framework.ExpectNoError(err)
		hasCondition := false
		for i := range vapStatusPatched.Status.Conditions {
			if vapStatusPatched.Status.Conditions[i].Type == "PatchStatusFailed" {
				hasCondition = true
			}
		}
		gomega.Expect(hasCondition).To(gomega.BeTrueBecause("expect the patched status exist"))

		ginkgo.By("updating /status")
		var vapStatusUpdated *admissionregistrationv1.ValidatingAdmissionPolicy
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vap, err := client.Get(ctx, vapCreated.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			vapStatusToUpdate := vap.DeepCopy()
			vapStatusToUpdate.Status.Conditions = append(vapStatusToUpdate.Status.Conditions, metav1.Condition{
				Type:               "StatusUpdateFailed",
				Status:             metav1.ConditionFalse,
				Reason:             "E2E",
				Message:            "Set from an e2e test",
				LastTransitionTime: metav1.NewTime(time.Now()),
			})
			vapStatusUpdated, err = client.UpdateStatus(ctx, vapStatusToUpdate, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err, "failed to update status of validatingadmissionpolicy %q", vapCreated.Name)
		hasCondition = false
		for i := range vapStatusUpdated.Status.Conditions {
			if vapStatusUpdated.Status.Conditions[i].Type == "StatusUpdateFailed" {
				hasCondition = true
			}
		}
		gomega.Expect(hasCondition).To(gomega.BeTrueBecause("expect the updated status exist"))

		ginkgo.By("deleting")
		err = client.Delete(ctx, vapCreated.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)
		vapTmp, err := client.Get(ctx, vapCreated.Name, metav1.GetOptions{})
		switch {
		case err == nil && vapTmp.GetDeletionTimestamp() != nil && len(vapTmp.GetFinalizers()) > 0:
			// deletion requested successfully, object is blocked by finalizers
		case err == nil:
			framework.Failf("expected deleted object, got %#v", vapTmp)
		case apierrors.IsNotFound(err):
			// deleted successfully
		default:
			framework.Failf("expected 404, got %#v", err)
		}

		list, err = client.List(ctx, metav1.ListOptions{LabelSelector: label})
		var itemsWithoutFinalizer []admissionregistrationv1.ValidatingAdmissionPolicy
		for _, item := range list.Items {
			if len(item.GetFinalizers()) == 0 {
				itemsWithoutFinalizer = append(itemsWithoutFinalizer, item)
			}
		}
		framework.ExpectNoError(err)
		gomega.Expect(itemsWithoutFinalizer).To(gomega.HaveLen(2), "filtered list should have 2 items")

		ginkgo.By("deleting a collection")
		err = client.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: label})
		framework.ExpectNoError(err)

		list, err = client.List(ctx, metav1.ListOptions{LabelSelector: label})
		var itemsColWithoutFinalizer []admissionregistrationv1.ValidatingAdmissionPolicy
		for _, item := range list.Items {
			if !(item.GetDeletionTimestamp() != nil && len(item.GetFinalizers()) > 0) {
				itemsColWithoutFinalizer = append(itemsColWithoutFinalizer, item)
			}
		}
		framework.ExpectNoError(err)
		gomega.Expect(itemsColWithoutFinalizer).To(gomega.BeEmpty(), "filtered list should have 0 items")
	})

	/*
	   Release: v1.30
	   Testname: ValidatingadmissionPolicyBinding API
	   Description:
	   The admissionregistration.k8s.io API group MUST exist in the
	     /apis discovery document.
	   The admissionregistration.k8s.io/v1 API group/version MUST exist
	     in the /apis/admissionregistration.k8s.io discovery document.
	   The ValidatingadmissionPolicyBinding resources MUST exist in the
	     /apis/admissionregistration.k8s.io/v1 discovery document.
	   The ValidatingadmissionPolicyBinding resource must support create, get,
	     list, watch, update, patch, delete, and deletecollection.
	*/
	framework.ConformanceIt("should support ValidatingAdmissionPolicyBinding API operations", func(ctx context.Context) {
		vapbVersion := "v1"
		ginkgo.By("getting /apis")
		{
			discoveryGroups, err := f.ClientSet.Discovery().ServerGroups()
			framework.ExpectNoError(err)
			found := false
			for _, group := range discoveryGroups.Groups {
				if group.Name == admissionregistrationv1.GroupName {
					for _, version := range group.Versions {
						if version.Version == vapbVersion {
							found = true
							break
						}
					}
				}
			}
			if !found {
				framework.Failf("expected ValidatingAdmissionPolicyBinding API group/version, got %#v", discoveryGroups.Groups)
			}
		}

		ginkgo.By("getting /apis/admissionregistration.k8s.io")
		{
			group := &metav1.APIGroup{}
			err := f.ClientSet.Discovery().RESTClient().Get().AbsPath("/apis/admissionregistration.k8s.io").Do(ctx).Into(group)
			framework.ExpectNoError(err)
			found := false
			for _, version := range group.Versions {
				if version.Version == vapbVersion {
					found = true
					break
				}
			}
			if !found {
				framework.Failf("expected ValidatingAdmissionPolicyBinding API version, got %#v", group.Versions)
			}
		}

		ginkgo.By("getting /apis/admissionregistration.k8s.io/" + vapbVersion)
		{
			resources, err := f.ClientSet.Discovery().ServerResourcesForGroupVersion(admissionregistrationv1.SchemeGroupVersion.String())
			framework.ExpectNoError(err)
			foundVAPB := false
			for _, resource := range resources.APIResources {
				switch resource.Name {
				case "validatingadmissionpolicybindings":
					foundVAPB = true
				}
			}
			if !foundVAPB {
				framework.Failf("expected validatingadmissionpolicybindings, got %#v", resources.APIResources)
			}
		}

		client := f.ClientSet.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings()
		labelKey, labelValue := "example-e2e-vapb-label", utilrand.String(8)
		label := fmt.Sprintf("%s=%s", labelKey, labelValue)

		template := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "e2e-example-vapb-",
				Labels: map[string]string{
					labelKey: labelValue,
				},
			},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        "replicalimit-policy.example.com",
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			},
		}

		ginkgo.DeferCleanup(func(ctx context.Context) {
			err := client.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: label})
			framework.ExpectNoError(err)
		})

		ginkgo.By("creating")
		_, err := client.Create(ctx, template, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		_, err = client.Create(ctx, template, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		vapbCreated, err := client.Create(ctx, template, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("getting")
		vapbRead, err := client.Get(ctx, vapbCreated.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(vapbRead.UID).To(gomega.Equal(vapbCreated.UID))

		ginkgo.By("listing")
		list, err := client.List(ctx, metav1.ListOptions{LabelSelector: label})
		framework.ExpectNoError(err)

		ginkgo.By("watching")
		framework.Logf("starting watch")
		vapbWatch, err := client.Watch(ctx, metav1.ListOptions{ResourceVersion: list.ResourceVersion, LabelSelector: label})
		framework.ExpectNoError(err)

		ginkgo.By("patching")
		patchBytes := []byte(`{"metadata":{"annotations":{"patched":"true"}},"spec":{"validationActions":["Warn"]}}`)
		vapbPatched, err := client.Patch(ctx, vapbCreated.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(vapbPatched.Annotations).To(gomega.HaveKeyWithValue("patched", "true"), "patched object should have the applied annotation")
		gomega.Expect(vapbPatched.Spec.ValidationActions).To(gomega.Equal([]admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn}), "patched object should have the applied spec")

		ginkgo.By("updating")
		var vapbUpdated *admissionregistrationv1.ValidatingAdmissionPolicyBinding
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vap, err := client.Get(ctx, vapbCreated.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			vapbToUpdate := vap.DeepCopy()
			vapbToUpdate.Annotations["updated"] = "true"
			vapbToUpdate.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}

			vapbUpdated, err = client.Update(ctx, vapbToUpdate, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err, "failed to update validatingadmissionpolicybinding %q", vapbCreated.Name)
		gomega.Expect(vapbUpdated.Annotations).To(gomega.HaveKeyWithValue("updated", "true"), "updated object should have the applied annotation")
		gomega.Expect(vapbUpdated.Spec.ValidationActions).To(gomega.Equal([]admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}), "updated object should have the applied spec")

		framework.Logf("waiting for watch events with expected annotations")
		for sawAnnotation := false; !sawAnnotation; {
			select {
			case evt, ok := <-vapbWatch.ResultChan():
				if !ok {
					framework.Fail("watch channel should not close")
				}
				gomega.Expect(evt.Type).To(gomega.Equal(watch.Modified))
				vapbWatched, isFS := evt.Object.(*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
				if !isFS {
					framework.Failf("expected an object of type: %T, but got %T", &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}, evt.Object)
				}
				if vapbWatched.Annotations["patched"] == "true" {
					sawAnnotation = true
					vapbWatch.Stop()
				} else {
					framework.Logf("missing expected annotations, waiting: %#v", vapbWatched.Annotations)
				}
			case <-time.After(wait.ForeverTestTimeout):
				framework.Fail("timed out waiting for watch event")
			}
		}
		ginkgo.By("deleting")
		err = client.Delete(ctx, vapbCreated.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)
		vapbTmp, err := client.Get(ctx, vapbCreated.Name, metav1.GetOptions{})
		switch {
		case err == nil && vapbTmp.GetDeletionTimestamp() != nil && len(vapbTmp.GetFinalizers()) > 0:
			// deletion requested successfully, object is blocked by finalizers
		case err == nil:
			framework.Failf("expected deleted object, got %#v", vapbTmp)
		case apierrors.IsNotFound(err):
			// deleted successfully
		default:
			framework.Failf("expected 404, got %#v", err)
		}

		list, err = client.List(ctx, metav1.ListOptions{LabelSelector: label})
		var itemsWithoutFinalizer []admissionregistrationv1.ValidatingAdmissionPolicyBinding
		for _, item := range list.Items {
			if len(item.GetFinalizers()) == 0 {
				itemsWithoutFinalizer = append(itemsWithoutFinalizer, item)
			}
		}
		framework.ExpectNoError(err)
		gomega.Expect(itemsWithoutFinalizer).To(gomega.HaveLen(2), "filtered list should have 2 items")

		ginkgo.By("deleting a collection")
		err = client.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: label})
		framework.ExpectNoError(err)

		list, err = client.List(ctx, metav1.ListOptions{LabelSelector: label})
		var itemsColWithoutFinalizer []admissionregistrationv1.ValidatingAdmissionPolicyBinding
		for _, item := range list.Items {
			if !(item.GetDeletionTimestamp() != nil && len(item.GetFinalizers()) > 0) {
				itemsColWithoutFinalizer = append(itemsColWithoutFinalizer, item)
			}
		}
		framework.ExpectNoError(err)
		gomega.Expect(itemsColWithoutFinalizer).To(gomega.BeEmpty(), "filtered list should have 0 items")
	})
})

func createBinding(bindingName string, uniqueLabel string, policyName string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: policyName,
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{uniqueLabel: "true"},
				},
			},
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func basicDeployment(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "nginx"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "nginx"},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "nginx"},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "nginx",
							Image: "nginx:latest",
						},
					},
				},
			},
		}}
}

func basicReplicaSet(name string, replicas int32) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "nginx"},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "nginx"},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "nginx"},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "nginx",
							Image: "nginx:latest",
						},
					},
				},
			},
		}}
}

type validatingAdmissionPolicyBuilder struct {
	policy *admissionregistrationv1.ValidatingAdmissionPolicy
}

type resourceRuleBuilder struct {
	policyBuilder *validatingAdmissionPolicyBuilder
	resourceRule  *admissionregistrationv1.NamedRuleWithOperations
}

func newValidatingAdmissionPolicyBuilder(policyName string) *validatingAdmissionPolicyBuilder {
	return &validatingAdmissionPolicyBuilder{
		policy: &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName},
		},
	}
}

func (b *validatingAdmissionPolicyBuilder) MatchUniqueNamespace(uniqueLabel string) *validatingAdmissionPolicyBuilder {
	if b.policy.Spec.MatchConstraints == nil {
		b.policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{}
	}
	b.policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			uniqueLabel: "true",
		},
	}
	return b
}

func (b *validatingAdmissionPolicyBuilder) StartResourceRule() *resourceRuleBuilder {
	return &resourceRuleBuilder{
		policyBuilder: b,
		resourceRule: &admissionregistrationv1.NamedRuleWithOperations{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{"apps"},
					APIVersions: []string{"v1"},
					Resources:   []string{"deployments"},
				},
			},
		},
	}
}

func (rb *resourceRuleBuilder) CreateAndUpdate() *resourceRuleBuilder {
	rb.resourceRule.Operations = []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update}
	return rb
}

func (rb *resourceRuleBuilder) MatchResource(groups []string, versions []string, resources []string) *resourceRuleBuilder {
	rb.resourceRule.Rule = admissionregistrationv1.Rule{
		APIGroups:   groups,
		APIVersions: versions,
		Resources:   resources,
	}
	return rb
}

func (rb *resourceRuleBuilder) EndResourceRule() *validatingAdmissionPolicyBuilder {
	b := rb.policyBuilder
	if b.policy.Spec.MatchConstraints == nil {
		b.policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{}
	}
	b.policy.Spec.MatchConstraints.ResourceRules = append(b.policy.Spec.MatchConstraints.ResourceRules, *rb.resourceRule)
	return b
}

func (b *validatingAdmissionPolicyBuilder) WithValidation(validation admissionregistrationv1.Validation) *validatingAdmissionPolicyBuilder {
	b.policy.Spec.Validations = append(b.policy.Spec.Validations, validation)
	return b
}

func (b *validatingAdmissionPolicyBuilder) WithVariable(variable admissionregistrationv1.Variable) *validatingAdmissionPolicyBuilder {
	b.policy.Spec.Variables = append(b.policy.Spec.Variables, variable)
	return b
}

func (b *validatingAdmissionPolicyBuilder) Build() *admissionregistrationv1.ValidatingAdmissionPolicy {
	return b.policy
}

func crontabExampleCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crontabs.stable.example.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "stable.example.com",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"cronSpec": {
											Type: "string",
										},
										"image": {
											Type: "string",
										},
										"replicas": {
											Type: "integer",
										},
									},
								},
							},
						}},
				},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:     "crontabs",
				Singular:   "crontab",
				Kind:       "CronTab",
				ShortNames: []string{"ct"},
			},
		},
	}
}
