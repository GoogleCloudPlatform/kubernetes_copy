/*
Copyright 2014 The Kubernetes Authors.

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

package providers

import (
	"context"
	"fmt"
	"os"
	"path"

	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

const etcdImage = "3.5.17-0"

// EtcdUpgrade upgrades etcd on GCE.
func EtcdUpgrade(targetStorage, targetVersion string) error {
	switch framework.TestContext.Provider {
	case "gce":
		return etcdUpgradeGCE(targetStorage, targetVersion)
	default:
		return fmt.Errorf("EtcdUpgrade() is not implemented for provider %s", framework.TestContext.Provider)
	}
}

func etcdUpgradeGCE(targetStorage, targetVersion string) error {
	env := append(
		os.Environ(),
		"TEST_ETCD_VERSION="+targetVersion,
		"STORAGE_BACKEND="+targetStorage,
		"TEST_ETCD_IMAGE="+etcdImage)

	_, _, err := framework.RunCmdEnv(env, GCEUpgradeScript(), "-l", "-M")
	return err
}

// LocationParamGKE returns parameter related to location for gcloud command.
func LocationParamGKE() string {
	if framework.TestContext.CloudConfig.MultiMaster {
		// GKE Regional Clusters are being tested.
		return fmt.Sprintf("--region=%s", framework.TestContext.CloudConfig.Region)
	}
	return fmt.Sprintf("--zone=%s", framework.TestContext.CloudConfig.Zone)
}

// MasterUpgradeGKE upgrades master node to the specified version on GKE.
func MasterUpgradeGKE(ctx context.Context, namespace string, v string) error {
	framework.Logf("Upgrading master to %q", v)
	args := []string{
		"container",
		"clusters",
		fmt.Sprintf("--project=%s", framework.TestContext.CloudConfig.ProjectID),
		LocationParamGKE(),
		"upgrade",
		framework.TestContext.CloudConfig.Cluster,
		"--master",
		fmt.Sprintf("--cluster-version=%s", v),
		"--quiet",
	}
	_, _, err := framework.RunCmd("gcloud", framework.AppendContainerCommandGroupIfNeeded(args)...)
	if err != nil {
		return err
	}

	e2enode.WaitForSSHTunnels(ctx, namespace)

	return nil
}

// GCEUpgradeScript returns path of script for upgrading on GCE.
func GCEUpgradeScript() string {
	if len(framework.TestContext.GCEUpgradeScript) == 0 {
		return path.Join(framework.TestContext.RepoRoot, "cluster/gce/upgrade.sh")
	}
	return framework.TestContext.GCEUpgradeScript
}
