/*
Copyright 2020 The Kubernetes Authors.

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

// This file should be written by each cloud provider.
// For an minimal working example, please refer to k8s.io/cloud-provider/sample/basic_main.go
// For more details, please refer to k8s.io/kubernetes/cmd/cloud-controller-manager/main.go

package main

import (
	"os"

	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	"k8s.io/component-base/cli"
	cliflag "k8s.io/component-base/cli/flag"
	_ "k8s.io/component-base/logs/json/register"          // register optional JSON log format
	_ "k8s.io/component-base/metrics/prometheus/clientgo" // load all the prometheus client-go plugins
	_ "k8s.io/component-base/metrics/prometheus/version"  // for version metric registration
	"k8s.io/klog/v2"
	// For existing cloud providers, the option to import legacy providers is still available.
	// e.g. _"k8s.io/legacy-cloud-providers/<provider>"
)

func main() {
	logger := klog.Background()
	ccmOptions, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		klog.Fatalf("unable to initialize command options: %v", err)
	}

	fss := cliflag.NamedFlagSets{}
	command := app.NewCloudControllerManagerCommand(logger, ccmOptions, cloudInitializer, controllerInitializers(), names.CCMControllerAliases(), fss, wait.NeverStop)
	code := cli.Run(command)
	os.Exit(code)
}

// If custom ClientNames are used, as below, then the controller will not use
// the API server bootstrapped RBAC, and instead will require it to be installed
// separately.
func controllerInitializers() map[string]app.ControllerInitFuncConstructor {
	controllerInitializers := app.DefaultInitFuncConstructors
	if constructor, ok := controllerInitializers[names.CloudNodeController]; ok {
		constructor.InitContext.ClientName = "mycloud-external-cloud-node-controller"
		controllerInitializers[names.CloudNodeController] = constructor
	}
	if constructor, ok := controllerInitializers[names.CloudNodeLifecycleController]; ok {
		constructor.InitContext.ClientName = "mycloud-external-cloud-node-lifecycle-controller"
		controllerInitializers[names.CloudNodeLifecycleController] = constructor
	}
	if constructor, ok := controllerInitializers[names.ServiceLBController]; ok {
		constructor.InitContext.ClientName = "mycloud-external-service-controller"
		controllerInitializers[names.ServiceLBController] = constructor
	}
	if constructor, ok := controllerInitializers[names.NodeRouteController]; ok {
		constructor.InitContext.ClientName = "mycloud-external-route-controller"
		controllerInitializers[names.NodeRouteController] = constructor
	}
	return controllerInitializers
}

func cloudInitializer(logger klog.Logger, config *config.CompletedConfig) cloudprovider.Interface {
	cloudConfig := config.ComponentConfig.KubeCloudShared.CloudProvider

	// initialize cloud provider with the cloud provider name and config file provided
	cloud, err := cloudprovider.InitCloudProvider(cloudConfig.Name, cloudConfig.CloudConfigFile)
	if err != nil {
		klog.Fatalf("Cloud provider could not be initialized: %v", err)
	}
	if cloud == nil {
		klog.Fatalf("Cloud provider is nil")
	}

	if !cloud.HasClusterID() {
		if config.ComponentConfig.KubeCloudShared.AllowUntaggedCloud {
			klog.Warning("detected a cluster without a ClusterID.  A ClusterID will be required in the future.  Please tag your cluster to avoid any future issues")
		} else {
			klog.Fatalf("no ClusterID found.  A ClusterID is required for the cloud provider to function properly.  This check can be bypassed by setting the allow-untagged-cloud option")
		}
	}
	return cloud
}
