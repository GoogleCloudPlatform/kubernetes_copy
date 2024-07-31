/*
Copyright 2023 The Kubernetes Authors.

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

package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	mathrand "math/rand/v2"
	"os"
	"regexp"
	"strings"
	"time"

	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
)

const (
	testSignerOneName = "test.test/signer-one"
	testSignerTwoName = "test.test/signer-two"
	aliveSignersKey   = "signer.alive=true"
	deadSignersKey    = "signer.alive=false"
	noSignerKey       = "no-signer"
)

var _ = SIGDescribe(feature.ClusterTrustBundle, feature.ClusterTrustBundleProjection, func() {
	f := framework.NewDefaultFramework("projected-clustertrustbundle")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	initCTBs, pemMapping := initCTBData()

	ginkgo.BeforeEach(func(ctx context.Context) {
		cleanup := mustInitCTBs(ctx, f, initCTBs)
		ginkgo.DeferCleanup(cleanup)
	})

	ginkgo.It("should be able to mount a single ClusterTrustBundle by name", func(ctx context.Context) {

		for _, tt := range []struct {
			name           string
			ctbName        string
			optional       *bool
			expectedOutput []string
		}{
			{
				name:           "name of an existing CTB",
				ctbName:        "test.test.signer-one.4" + f.UniqueName,
				expectedOutput: expectedRegexFromPEMs(initCTBs[4].Spec.TrustBundle),
			},
			{
				name:           "name of a CTB that does not exist + optional=true",
				ctbName:        "does-not-exist.at.all",
				optional:       ptr.To(true),
				expectedOutput: []string{"content of file \"/var/run/ctbtest/trust-anchors.pem\": \n$"},
			},
		} {
			pod := podForCTBProjection(v1.VolumeProjection{
				ClusterTrustBundle: &v1.ClusterTrustBundleProjection{
					Name:     &tt.ctbName,
					Path:     "trust-anchors.pem",
					Optional: tt.optional,
				},
			})

			fileModeRegexp := getFileModeRegex("/var/run/ctbtest/trust-anchors.pem", nil)
			expectedOutput := append(tt.expectedOutput, fileModeRegexp)

			e2epodoutput.TestContainerOutputRegexp(ctx, f, "project cluster trust bundle", pod, 0, expectedOutput)
		}
	})

	ginkgo.Describe("should be capable to mount multiple trust bundles by signer+labels", func() {
		fileModeRegexp := getFileModeRegex("/var/run/ctbtest/trust-bundle.crt", nil)

		for _, tt := range []struct {
			name                string
			signerName          string
			selector            *metav1.LabelSelector
			optionalVolume      *bool
			expectedOutputRegex []string
		}{
			{
				name:       "can combine multiple CTBs with signer name and label selector",
				signerName: testSignerOneName,
				selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"signer.alive": "true",
					},
				},
				expectedOutputRegex: expectedRegexFromPEMs(pemMapping[testSignerOneName].Intersection(pemMapping[aliveSignersKey]).UnsortedList()...),
			},
			{
				name:                "should start if only signer name and nil label selector + optional=true",
				signerName:          testSignerOneName,
				selector:            nil, // == match nothing
				optionalVolume:      ptr.To(true),
				expectedOutputRegex: []string{"content of file \"/var/run/ctbtest/trust-bundle.crt\": \n$"},
			},
			{
				name:                "should start if only signer name and explicit label selector matches nothing + optional=true",
				signerName:          testSignerOneName,
				selector:            &metav1.LabelSelector{MatchLabels: map[string]string{"thismatches": "nothing"}},
				optionalVolume:      ptr.To(true),
				expectedOutputRegex: []string{"content of file \"/var/run/ctbtest/trust-bundle.crt\": \n$"},
			},
			{
				name:                "can combine all signer CTBs with an empty label selector",
				signerName:          testSignerOneName,
				selector:            &metav1.LabelSelector{},
				expectedOutputRegex: expectedRegexFromPEMs(pemMapping[testSignerOneName].UnsortedList()...),
			},
		} {
			ginkgo.It(tt.name, func(ctx context.Context) {
				signerName := tt.signerName + f.UniqueName
				pod := podForCTBProjection(v1.VolumeProjection{
					ClusterTrustBundle: &v1.ClusterTrustBundleProjection{
						Path:          "trust-bundle.crt",
						SignerName:    &signerName,
						LabelSelector: tt.selector,
						Optional:      tt.optionalVolume,
					},
				})

				expectedOutput := append(tt.expectedOutputRegex, fileModeRegexp)

				e2epodoutput.TestContainerOutputRegexp(ctx, f, "project cluster trust bundle", pod, 0, expectedOutput)
			})
		}
	})

	ginkgo.Describe("should prevent a pod from starting if: ", func() {

		for _, tt := range []struct {
			name string
			ctb  *v1.ClusterTrustBundleProjection
		}{
			{
				name: "sets optional=false and no trust bundle matches query",
				ctb: &v1.ClusterTrustBundleProjection{
					Optional:   ptr.To(false),
					Path:       "trust-bundle.crt",
					SignerName: ptr.To(testSignerOneName + f.UniqueName),
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"signer.alive": "unknown",
						},
					},
				},
			},
			{
				name: "sets optional=false and the configured CTB does not exist",
				ctb: &v1.ClusterTrustBundleProjection{
					Optional: ptr.To(false),
					Path:     "trust-bundle.crt",
					Name:     ptr.To("does-not-exist"),
				},
			},
		} {
			ginkgo.It(tt.name, func(ctx context.Context) {
				pod := podForCTBProjection(v1.VolumeProjection{ClusterTrustBundle: tt.ctb})

				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
				if err != nil {
					framework.Failf("failed to create a testing container: %v", err)
				}

				volumeNotReady := false
				var latestReadyStatus *v1.PodCondition
				err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(waitCtx context.Context) (done bool, err error) {
					waitPod, err := f.ClientSet.CoreV1().Pods(pod.Namespace).Get(waitCtx, pod.Name, metav1.GetOptions{})
					if err != nil {
						framework.Logf("failed to get pod: %v", err)
						return false, nil
					}

					if waitPod.Status.Phase == v1.PodRunning {
						return true, nil
					}

					if latestReadyStatus = podutil.GetPodReadyCondition(waitPod.Status); latestReadyStatus != nil &&
						latestReadyStatus.Status == v1.ConditionFalse &&
						latestReadyStatus.Reason == "ContainersNotReady" &&
						latestReadyStatus.Message == "containers with unready status: [projected-ctb-volume-test-0]" {
						volumeNotReady = true
						return false, nil
					}
					volumeNotReady = false

					return false, nil
				})

				if err == nil {
					framework.Fail("expected the pod not to start running, but it did")
				} else if !errors.Is(err, context.DeadlineExceeded) {
					framework.Failf("expected deadline exceeded, but got: %v", err)
				}

				if !volumeNotReady {
					framework.Failf("expected the pod to not be ready because of a missing volume, but its status is different: %v", latestReadyStatus)
				}
			})
		}
	})
	ginkgo.It("should be able to specify multiple CTB volumes", func(ctx context.Context) {
		pod := podForCTBProjection(
			v1.VolumeProjection{
				ClusterTrustBundle: &v1.ClusterTrustBundleProjection{
					Name: ptr.To("test.test.signer-one.4" + f.UniqueName),
					Path: "trust-anchors.pem",
				},
			},
			v1.VolumeProjection{
				ClusterTrustBundle: &v1.ClusterTrustBundleProjection{
					Path:       "trust-bundle.crt",
					SignerName: ptr.To(testSignerOneName + f.UniqueName),
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"signer.alive": "false",
						},
					},
				},
			},
		)
		expectedOutputs := map[int][]string{
			0: append(expectedRegexFromPEMs(pemMapping[noSignerKey].UnsortedList()...), getFileModeRegex("/var/run/ctbtest/trust-anchors.pem", nil)),
			1: append(expectedRegexFromPEMs(pemMapping[testSignerOneName].Intersection(pemMapping[deadSignersKey]).UnsortedList()...), getFileModeRegex("/var/run/ctbtest/trust-bundle.crt", nil)),
		}

		e2epodoutput.TestContainerOutputsRegexp(ctx, f, "multiple CTB volumes", pod, expectedOutputs)
	})

	ginkgo.It("should be able to mount a big number (>100) of CTBs", func(ctx context.Context) {
		const numCTBs = 150

		var initCTBs []*certificatesv1beta1.ClusterTrustBundle
		var cleanups []func(ctx context.Context)
		var projections []v1.VolumeProjection

		ginkgo.DeferCleanup(func(ctx context.Context) {
			for _, c := range cleanups {
				c(ctx)
			}
		})
		for i := range numCTBs {
			ctb := ctbForCA(fmt.Sprintf("test.test:signer-hundreds:%d", i), "test.test/signer-hundreds", mustMakeCAPEM(fmt.Sprintf("root%d", i)), nil)
			initCTBs = append(initCTBs, ctb)
			cleanups = append(cleanups, mustCreateCTB(ctx, f, ctb))
			projections = append(projections, v1.VolumeProjection{ClusterTrustBundle: &v1.ClusterTrustBundleProjection{ // TODO: maybe mount them all to a single pod?
				Name: ptr.To(fmt.Sprintf("test.test:signer-hundreds%s:%d", f.UniqueName, i)),
				Path: fmt.Sprintf("trust-anchors-%d.pem", i),
			},
			})
		}

		ginkgo.By("as a single projection with many sources", func() {
			randomIndexToTest := mathrand.Int32N(numCTBs)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "pod-projected-ctb-",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{
						{
							Name:  "projected-ctb-volume-test",
							Image: imageutils.GetE2EImage(imageutils.Agnhost),
							Args: []string{
								"mounttest",
								fmt.Sprintf("--file_content=/var/run/ctbtest/trust-anchors-%d.pem", randomIndexToTest),
								fmt.Sprintf("--file_mode=/var/run/ctbtest/trust-anchors-%d.pem", randomIndexToTest),
							},
							VolumeMounts: []v1.VolumeMount{{
								Name:      "ctb-volume",
								MountPath: "/var/run/ctbtest",
							}},
						},
					},
					Volumes: []v1.Volume{{
						Name: "ctb-volume",
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: projections,
							},
						},
					}},
				},
			}

			expectedOutputs := append(expectedRegexFromPEMs(initCTBs[randomIndexToTest].Spec.TrustBundle), getFileModeRegex(fmt.Sprintf("/var/run/ctbtest/trust-anchors-%d.pem", randomIndexToTest), nil))
			e2epodoutput.TestContainerOutputRegexp(ctx, f, "single CTB volume with many files", pod, 0, expectedOutputs)
		})

		ginkgo.By("as separate projections", func() {
			randomIndexToTest := mathrand.Int32N(numCTBs)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "pod-projected-ctb-",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{
						{
							Name:  "projected-ctb-volume-test",
							Image: imageutils.GetE2EImage(imageutils.Agnhost),
							Args: []string{
								"mounttest",
								fmt.Sprintf("--file_content=/var/run/ctbtest-%d/%s", randomIndexToTest, projections[randomIndexToTest].ClusterTrustBundle.Path),
								fmt.Sprintf("--file_mode=/var/run/ctbtest-%d/%s", randomIndexToTest, projections[randomIndexToTest].ClusterTrustBundle.Path),
							},
						},
					},
				},
			}
			for i := range projections {
				pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
					Name: fmt.Sprintf("ctb-volume-%d", i),
					VolumeSource: v1.VolumeSource{
						Projected: &v1.ProjectedVolumeSource{
							Sources: []v1.VolumeProjection{projections[i]},
						},
					},
				})
				pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
					Name:      fmt.Sprintf("ctb-volume-%d", i),
					MountPath: fmt.Sprintf("/var/run/ctbtest-%d", i),
				})
			}

			expectedOutputs := append(expectedRegexFromPEMs(initCTBs[randomIndexToTest].Spec.TrustBundle), getFileModeRegex(fmt.Sprintf("/var/run/ctbtest-%d/trust-anchors-%d.pem", randomIndexToTest, randomIndexToTest), nil))
			e2epodoutput.TestContainerOutputRegexp(ctx, f, "many CTB volumes", pod, 0, expectedOutputs)
		})

		ginkgo.By("as a single projection joined in a single file by signer name", func() {
			pod := podForCTBProjection(v1.VolumeProjection{
				ClusterTrustBundle: &v1.ClusterTrustBundleProjection{
					Path:          "trust-anchors.pem",
					SignerName:    ptr.To("test.test/signer-hundreds" + f.UniqueName),
					LabelSelector: &metav1.LabelSelector{}, // == match everything
				},
			})

			expectedOutputs := append(expectedRegexFromPEMs(ctbsToPEMs(initCTBs)...), getFileModeRegex("/var/run/ctbtest/trust-anchors.pem", nil))
			e2epodoutput.TestContainerOutputRegexp(ctx, f, "single CTB volume with a single file", pod, 0, expectedOutputs)

		})
	})
})

func expectedRegexFromPEMs(certPEMs ...string) []string {
	var ret []string
	for _, pem := range certPEMs {
		ret = append(ret, regexp.QuoteMeta(pem))
	}
	return ret
}

func podForCTBProjection(projectionSources ...v1.VolumeProjection) *v1.Pod {
	const volumeNameFmt = "ctb-volume-%d"

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-projected-ctb-" + string(uuid.NewUUID()),
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	for i := range projectionSources {
		pod.Spec.Containers = append(pod.Spec.Containers,
			v1.Container{
				Name:  fmt.Sprintf("projected-ctb-volume-test-%d", i),
				Image: imageutils.GetE2EImage(imageutils.Agnhost),
				Args: []string{
					"mounttest",
					fmt.Sprintf("--file_content=/var/run/ctbtest/%s", projectionSources[i].ClusterTrustBundle.Path),
					fmt.Sprintf("--file_mode=/var/run/ctbtest/%s", projectionSources[i].ClusterTrustBundle.Path),
				},
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      fmt.Sprintf(volumeNameFmt, i),
						MountPath: "/var/run/ctbtest",
					},
				},
			})
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			v1.Volume{
				Name: fmt.Sprintf(volumeNameFmt, i),
				VolumeSource: v1.VolumeSource{
					Projected: &v1.ProjectedVolumeSource{
						Sources: []v1.VolumeProjection{projectionSources[i]},
					},
				},
			})
	}

	return pod
}

// mustInitCTBs creates a testSet of ClusterTrustBundles and spreads them into several
// categories based on their signer name and labels.
// It returns a cleanup function for all the ClusterTrustBundle objects it created.
// It also returns a map of sets of PEMs like so:
//
//	{
//	  "test.test/signer-one": <set of all PEMs that are owned by test.test/signer-one>,
//	  "test.test/signer-two": <set of all PEMs that are owned by test.test/signer-two>,
//	  "signer.alive=true": <set of all PEMs whose CTBs contain `signer.alive: true` labels>,
//	  "signer.alive=false": <set of all PEMs whose CTBs contain `signer.alive: false` labels>,
//	  "no-signer": <set of all PEMs that appear in CTBs with no specific signers>,
//	}
func initCTBData() ([]*certificatesv1beta1.ClusterTrustBundle, map[string]sets.Set[string]) {
	var pemSets = map[string]sets.Set[string]{
		testSignerOneName: sets.New[string](),
		testSignerTwoName: sets.New[string](),
		aliveSignersKey:   sets.New[string](),
		deadSignersKey:    sets.New[string](),
		noSignerKey:       sets.New[string](),
	}

	var ctbs []*certificatesv1beta1.ClusterTrustBundle

	for i := range 10 {
		caPEM := mustMakeCAPEM(fmt.Sprintf("root%d", i))

		switch i {
		case 1, 2, 3:
			ctbs = append(ctbs, ctbForCA(fmt.Sprintf("test.test:signer-one:%d", i), testSignerOneName, caPEM, map[string]string{"signer.alive": "true"}))

			pemSets[testSignerOneName].Insert(caPEM)
			pemSets[aliveSignersKey].Insert(caPEM)
		case 4:
			ctbs = append(ctbs, ctbForCA(fmt.Sprintf("test.test.signer-one.%d", i), "", caPEM, map[string]string{"signer.alive": "true"}))

			pemSets[noSignerKey].Insert(caPEM)
		case 5:
			ctbs = append(ctbs, ctbForCA(fmt.Sprintf("test.test:signer-two:%d", i), testSignerTwoName, caPEM, map[string]string{"signer.alive": "true"}))

			pemSets[testSignerTwoName].Insert(caPEM)
			pemSets[aliveSignersKey].Insert(caPEM)
		case 6, 7:
			ctbs = append(ctbs, ctbForCA(fmt.Sprintf("test.test:signer-one:%d", i), testSignerOneName, caPEM, map[string]string{"signer.alive": "false"}))

			pemSets[testSignerOneName].Insert(caPEM)
			pemSets[deadSignersKey].Insert(caPEM)
		default: // 0, 8 ,9
			ctbs = append(ctbs, ctbForCA(fmt.Sprintf("test.test:signer-one:%d", i), testSignerOneName, caPEM, nil))

			pemSets[testSignerOneName].Insert(caPEM)
		}
	}

	return ctbs, pemSets
}

func ctbForCA(ctbName, signerName, caPEM string, labels map[string]string) *certificatesv1beta1.ClusterTrustBundle {
	return &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ctbName,
			Labels: labels,
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			SignerName:  signerName,
			TrustBundle: caPEM,
		},
	}
}

func mustInitCTBs(ctx context.Context, f *framework.Framework, ctbs []*certificatesv1beta1.ClusterTrustBundle) func(context.Context) {
	cleanups := []func(context.Context){}
	for _, ctb := range ctbs {
		ctb := ctb
		cleanups = append(cleanups, mustCreateCTB(ctx, f, ctb.DeepCopy()))
	}

	return func(ctx context.Context) {
		for _, c := range cleanups {
			c(ctx)
		}
	}
}

func mustCreateCTB(ctx context.Context, f *framework.Framework, ctb *certificatesv1beta1.ClusterTrustBundle) func(context.Context) {
	mutateCTBForTesting(ctb, f.UniqueName)

	if _, err := f.ClientSet.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, ctb, metav1.CreateOptions{}); err != nil {
		framework.Failf("Error while creating ClusterTrustBundle: %v", err)
	}

	return func(ctx context.Context) {
		if err := f.ClientSet.CertificatesV1beta1().ClusterTrustBundles().Delete(ctx, ctb.Name, metav1.DeleteOptions{}); err != nil {
			framework.Logf("failed to remove a cluster trust bundle: %v", err)
		}
	}
}

func mustMakeCAPEM(cn string) string {
	asnCert := mustMakeCertificate(&x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			CommonName: cn,
		},
		IsCA:                  true,
		BasicConstraintsValid: true,
	})

	return mustMakePEMBlock("CERTIFICATE", nil, asnCert)
}

func mustMakeCertificate(template *x509.Certificate) []byte {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		framework.Failf("Error while generating key: %v", err)
	}

	cert, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		framework.Failf("Error while making certificate: %v", err)
	}

	return cert
}

func mustMakePEMBlock(blockType string, headers map[string]string, data []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{
		Type:    blockType,
		Headers: headers,
		Bytes:   data,
	}))
}

// getFileModeRegex returns a file mode related regex which should be matched by the mounttest pods' output.
// If the given mask is nil, then the regex will contain the default OS file modes, which are 0644 for Linux and 0775 for Windows.
func getFileModeRegex(filePath string, mask *int32) string {
	var (
		linuxMask   int32
		windowsMask int32
	)
	if mask == nil {
		linuxMask = int32(0644)
		windowsMask = int32(0775)
	} else {
		linuxMask = *mask
		windowsMask = *mask
	}

	linuxOutput := fmt.Sprintf("mode of file \"%s\": %v", filePath, os.FileMode(linuxMask))
	windowsOutput := fmt.Sprintf("mode of Windows file \"%v\": %s", filePath, os.FileMode(windowsMask))

	return fmt.Sprintf("(%s|%s)", linuxOutput, windowsOutput)
}

func ctbsToPEMs(ctbs []*certificatesv1beta1.ClusterTrustBundle) []string {
	var certPEMs []string
	for _, ctb := range ctbs {
		certPEMs = append(certPEMs, ctb.Spec.TrustBundle)
	}
	return certPEMs
}

// mutateCTBForTesting mutates the .spec.signerName and .name so that the created cluster
// objects are unique and the tests can run in parallel
func mutateCTBForTesting(ctb *certificatesv1beta1.ClusterTrustBundle, uniqueName string) {
	signer := ctb.Spec.SignerName
	if len(signer) == 0 {
		ctb.Name += uniqueName
		return
	}

	newSigner := ctb.Spec.SignerName + uniqueName
	ctb.Name = strings.Replace(ctb.Name, signerNameToCTBName(signer), signerNameToCTBName(newSigner), 1)
	ctb.Spec.SignerName = newSigner
}

func signerNameToCTBName(signerName string) string {
	return strings.ReplaceAll(signerName, "/", ":")
}
