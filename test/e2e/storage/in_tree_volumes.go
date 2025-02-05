/*
Copyright 2018 The Kubernetes Authors.

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

package storage

import (
	"os"

	"github.com/onsi/ginkgo/v2"

	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/drivers"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
)

// List of testDrivers to be executed in below loop
var testDrivers = []func() storageframework.TestDriver{
	drivers.InitNFSDriver,
	drivers.InitISCSIDriver,
	drivers.InitHostPathDriver,
	drivers.InitHostPathSymlinkDriver,
	drivers.InitEmptydirDriver,
	drivers.InitCinderDriver,
	drivers.InitVSphereDriver,
	drivers.InitAzureDiskDriver,
	drivers.InitAzureFileDriver,
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeDirectory),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeDirectoryLink),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeDirectoryBindMounted),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeDirectoryLinkBindMounted),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeTmpfs),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeBlock),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeBlockFS),
	drivers.InitLocalDriverWithVolumeType(utils.LocalVolumeGCELocalSSD),
}

// This executes testSuites for in-tree volumes.
var _ = utils.SIGDescribe("In-tree Volumes", func() {
	defer ginkgo.GinkgoRecover()

	gceEnabled := false
	for _, driver := range framework.TestContext.EnabledVolumeDrivers {
		switch driver {
		case "gcepd":
			testDrivers = append(testDrivers, drivers.InitGcePdDriver)
			testDrivers = append(testDrivers, drivers.InitWindowsGcePdDriver)
			gceEnabled = true
		case "aws":
			testDrivers = append(testDrivers, drivers.InitAwsDriver)
			framework.Logf("Enabled aws in-tree volume drivers")
		default:
			framework.Failf("Invalid volume type %s in %v", driver, framework.TestContext.EnabledVolumeDrivers)
		}
	}

	// Support the legacy env var for gcepd.
	if enableGcePD := os.Getenv("ENABLE_STORAGE_GCE_PD_DRIVER"); enableGcePD == "yes" && !gceEnabled {
		framework.Logf("Warning: deprecated ENABLE_STORAGE_GCE_PD_DRIVER used. This will be removed in a future release. Use --enabled-volume-drivers=gcepd instead")
		testDrivers = append(testDrivers, drivers.InitGcePdDriver)
		testDrivers = append(testDrivers, drivers.InitWindowsGcePdDriver)
		gceEnabled = true
	}

	if gceEnabled {
		framework.Logf("Enabled gcepd and windows-gcepd in-tree volume drivers")
	}

	for _, initDriver := range testDrivers {
		curDriver := initDriver()

		framework.Context(append(storageframework.GetDriverNameWithFeatureTags(curDriver), func() {
			storageframework.DefineTestSuites(curDriver, testsuites.BaseSuites)
		})...)
	}
})
