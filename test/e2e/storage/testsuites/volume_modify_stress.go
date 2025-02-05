/*
Copyright 2025 The Kubernetes Authors.

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

package testsuites

import (
	"context"
	"sync"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/features"
	e2efeature "k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type volumeModifyStressTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

type volumeModifyStressTest struct {
	config *storageframework.PerTestConfig

	migrationCheck *migrationOpCheck

	vac *storagev1beta1.VolumeAttributesClass

	volumes []*storageframework.VolumeResource
	pods    []*v1.Pod
	// stop and wait for any async routines
	wg sync.WaitGroup

	testOptions storageframework.StressTestOptions
}

var _ storageframework.TestSuite = &volumeModifyStressTestSuite{}

// InitCustomVolumeModifyStressTestSuite returns volumeModifyStressTestSuite that implements TestSuite interface
// using custom test patterns
func InitCustomVolumeModifyStressTestSuite(patterns []storageframework.TestPattern) storageframework.TestSuite {
	return &volumeModifyStressTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name:         "volume-modify-stress",
			TestPatterns: patterns,
			SupportedSizeRange: e2evolume.SizeRange{
				Min: "1Gi",
			},
			TestTags: []interface{}{e2efeature.VolumeAttributesClass, framework.WithFeatureGate(features.VolumeAttributesClass)},
		},
	}
}

// InitVolumeModifyStressTestSuite returns volumeModifyStressTestSuite that implements TestSuite interface
// using testsuite default patterns
func InitVolumeModifyStressTestSuite() storageframework.TestSuite {
	patterns := []storageframework.TestPattern{
		storageframework.DefaultFsDynamicPV,
		storageframework.BlockVolModeDynamicPV,
		storageframework.NtfsDynamicPV,
	}
	return InitCustomVolumeModifyStressTestSuite(patterns)
}

func (v *volumeModifyStressTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return v.tsInfo
}

func (v *volumeModifyStressTestSuite) SkipUnsupportedTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	driverInfo := driver.GetDriverInfo()
	if driverInfo.VolumeModifyStressTestOptions == nil {
		e2eskipper.Skipf("Driver %s doesn't specify volume modify stress test options -- skipping", driverInfo.Name)
	}
	if driverInfo.VolumeModifyStressTestOptions.NumPods <= 0 {
		framework.Failf("NumPods in modify volume stress test options must be a positive integer, received: %d", driverInfo.VolumeModifyStressTestOptions.NumPods)
	}
	_, ok := driver.(storageframework.VolumeAttributesClassTestDriver)
	if !ok {
		e2eskipper.Skipf("Driver %q does not support VolumeAttributesClass tests - skipping", driver.GetDriverInfo().Name)
	}
	// Skip block storage tests if the driver we are testing against does not support block volumes
	// TODO: This should be made generic so that it doesn't have to be re-written for every test that uses the 	BlockVolModeDynamicPV testcase
	if !driverInfo.Capabilities[storageframework.CapBlock] && pattern.VolMode == v1.PersistentVolumeBlock {
		e2eskipper.Skipf("Driver %q does not support block volume mode - skipping", driver.GetDriverInfo().Name)
	}
}

func (v *volumeModifyStressTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {

	var (
		dInfo = driver.GetDriverInfo()
		cs    clientset.Interface
		l     *volumeModifyStressTest
	)

	// Beware that it also registers an AfterEach which renders f unusable. Any code using
	// f must run inside an It or Context callback.
	f := framework.NewFrameworkWithCustomTimeouts("volume-modify-stress", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	init := func(ctx context.Context) {
		cs = f.ClientSet
		l = &volumeModifyStressTest{}

		// Now do the more expensive test initialization.
		l.config = driver.PrepareTest(ctx, f)
		vacDriver, _ := driver.(storageframework.VolumeAttributesClassTestDriver)
		l.volumes = []*storageframework.VolumeResource{}
		l.pods = []*v1.Pod{}
		l.vac = vacDriver.GetVolumeAttributesClass(ctx, l.config)

		if l.vac == nil {
			e2eskipper.Skipf("Driver %q returned nil VolumeAttributesClass - skipping", dInfo.Name)
		}

		ginkgo.By("Creating VolumeAttributesClass")
		_, err := f.ClientSet.StorageV1beta1().VolumeAttributesClasses().Create(ctx, l.vac, metav1.CreateOptions{})
		framework.ExpectNoError(err, "While creating VolumeAttributesClass")
	}

	createPodsAndVolumes := func(ctx context.Context) {
		for i := 0; i < l.testOptions.NumPods; i++ {
			framework.Logf("Creating resources for pod %v/%v", i, l.testOptions.NumPods-1)
			testVolumeSizeRange := v.GetTestSuiteInfo().SupportedSizeRange
			r := storageframework.CreateVolumeResourceWithVAC(ctx, driver, l.config, pattern, testVolumeSizeRange, &l.vac.Name)
			l.volumes = append(l.volumes, r)
			podConfig := e2epod.Config{
				NS:           f.Namespace.Name,
				PVCs:         []*v1.PersistentVolumeClaim{r.Pvc},
				SeLinuxLabel: e2epv.SELinuxLabel,
			}
			pod, err := e2epod.MakeSecPod(&podConfig)
			framework.ExpectNoError(err)

			l.pods = append(l.pods, pod)
		}
	}

	cleanup := func(ctx context.Context) {
		framework.Logf("Stopping and waiting for all test routines to finish")
		l.wg.Wait()

		var (
			errs []error
			mu   sync.Mutex
			wg   sync.WaitGroup
		)

		if l.vac != nil {
			ginkgo.By("Deleting VAC")
			CleanupVAC(ctx, l.vac, f.ClientSet, vacCleanupWaitPeriod)
			l.vac = nil
		}

		wg.Add(len(l.pods))
		for _, pod := range l.pods {
			go func(pod *v1.Pod) {
				defer ginkgo.GinkgoRecover()
				defer wg.Done()

				framework.Logf("Deleting pod %v", pod.Name)
				err := e2epod.DeletePodWithWait(ctx, cs, pod)
				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, err)
			}(pod)
		}
		wg.Wait()

		wg.Add(len(l.volumes))
		for _, volume := range l.volumes {
			go func(volume *storageframework.VolumeResource) {
				defer ginkgo.GinkgoRecover()
				defer wg.Done()

				framework.Logf("Deleting volume %s", volume.Pvc.GetName())
				err := volume.CleanupResource(ctx)
				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, err)
			}(volume)
		}
		wg.Wait()

		framework.ExpectNoError(errors.NewAggregate(errs), "while cleaning up resource")
		l.migrationCheck.validateMigrationVolumeOpCounts(ctx)
	}

	f.It("multiple pods should provision volumes with volumeAttributesClass", f.WithSlow(), f.WithSerial(), func(ctx context.Context) {
		init(ctx)
		ginkgo.DeferCleanup(cleanup)
		createPodsAndVolumes(ctx)
		// Restart pod repeatedly
		for i := 0; i < l.testOptions.NumPods; i++ {
			podIndex := i
			l.wg.Add(1)
			go func() {
				defer ginkgo.GinkgoRecover()
				defer l.wg.Done()
				for j := 0; j < l.testOptions.NumRestarts; j++ {
					select {
					case <-ctx.Done():
						// This looks like a in the
						// original test
						// (https://github.com/kubernetes/kubernetes/blob/21049c2a1234ae3eea57357ed4329ed567a2dab3/test/e2e/storage/testsuites/volume_stress.go#L212):
						// This early return will never
						// get reached even if some
						// other goroutine fails
						// because the context doesn't
						// get cancelled.
						return
					default:
						pod := l.pods[podIndex]
						framework.Logf("Pod-%v [%v], Iteration %v/%v", podIndex, pod.Name, j, l.testOptions.NumRestarts-1)
						_, err := cs.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
						if err != nil {
							framework.Failf("Failed to create pod-%v [%+v]. Error: %v", podIndex, pod, err)
						}

						err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, cs, pod.Name, pod.Namespace, f.Timeouts.PodStart)
						if err != nil {
							framework.Failf("Failed to wait for pod-%v [%+v] turn into running status. Error: %v", podIndex, pod, err)
						}

						err = e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
						if err != nil {
							framework.Failf("Failed to delete pod-%v [%+v]. Error: %v", podIndex, pod, err)
						}
					}
				}
			}()
		}
		l.wg.Wait()
	})
}
