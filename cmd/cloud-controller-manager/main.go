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

// The external controller manager is responsible for running controller loops that
// are cloud provider dependent. It uses the API to listen to new events on resources.

// This file should be written by each cloud provider.
// For a minimal working example, please refer to k8s.io/cloud-provider/sample/basic_main.go
// For more details, please refer to k8s.io/kubernetes/cmd/cloud-controller-manager/main.go
// The current file demonstrate how other cloud provider should leverage CCM and it uses fake parameters. Please modify for your own use.

package main

import (
	"os"

	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	cloudcontrollerconfig "k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	"k8s.io/component-base/cli"
	cliflag "k8s.io/component-base/cli/flag"
	_ "k8s.io/component-base/metrics/prometheus/clientgo" // load all the prometheus client-go plugins
	_ "k8s.io/component-base/metrics/prometheus/version"  // for version metric registration
	"k8s.io/klog/v2"
	kcmnames "k8s.io/kubernetes/cmd/kube-controller-manager/names"
	// For existing cloud providers, the option to import legacy providers is still available.
	// e.g. _"k8s.io/legacy-cloud-providers/<provider>"
)

func main() {
	logger := klog.Background()
	ccmOptions, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		logger.Error(err, "Unable to initialize command options")
	}

	controllerInitializers := app.DefaultInitFuncConstructors
	controllerAliases := names.CCMControllerAliases()
	// Here is an example to remove the controller which is not needed.
	// e.g. remove the cloud-node-lifecycle controller which current cloud provider does not need.
	//delete(controllerInitializers, "cloud-node-lifecycle")

	// Here is an example to add an controller(NodeIpamController) which will be used by cloud provider
	// generate nodeIPAMConfig. Here is an sample code.
	// If you do not need additional controller, please ignore.

	nodeIpamController := nodeIPAMController{}
	nodeIpamController.nodeIPAMControllerOptions.NodeIPAMControllerConfiguration = &nodeIpamController.nodeIPAMControllerConfiguration
	fss := cliflag.NamedFlagSets{}
	nodeIpamController.nodeIPAMControllerOptions.AddFlags(fss.FlagSet(kcmnames.NodeIpamController))

	controllerInitializers[kcmnames.NodeIpamController] = app.ControllerInitFuncConstructor{
		// "node-controller" is the shared identity of all node controllers, including node, node lifecycle, and node ipam.
		// See https://github.com/kubernetes/kubernetes/pull/72764#issuecomment-453300990 for more context.
		InitContext: app.ControllerInitContext{
			ClientName: "node-controller",
		},
		Constructor: nodeIpamController.StartNodeIpamControllerWrapper,
	}
	controllerAliases["nodeipam"] = kcmnames.NodeIpamController

	command := app.NewCloudControllerManagerCommand(logger, ccmOptions, cloudInitializer, controllerInitializers, controllerAliases, fss, wait.NeverStop)
	code := cli.Run(command)
	os.Exit(code)
}

func cloudInitializer(logger klog.Logger, config *cloudcontrollerconfig.CompletedConfig) cloudprovider.Interface {
	cloudConfig := config.ComponentConfig.KubeCloudShared.CloudProvider
	// initialize cloud provider with the cloud provider name and config file provided
	cloud, err := cloudprovider.InitCloudProvider(cloudConfig.Name, cloudConfig.CloudConfigFile)
	if err != nil {
		logger.Error(err, "Cloud provider could not be initialized")
	}
	if cloud == nil {
		logger.Error(nil, "Cloud provider is nil")
	}

	if !cloud.HasClusterID() {
		if config.ComponentConfig.KubeCloudShared.AllowUntaggedCloud {
			logger.Info("Detected a cluster without a ClusterID.  A ClusterID will be required in the future.  Please tag your cluster to avoid any future issues")
		} else {
			logger.Error(nil, "No ClusterID found.  A ClusterID is required for the cloud provider to function properly.  This check can be bypassed by setting the allow-untagged-cloud option")
		}
	}

	return cloud
}
