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

// This file holds the code related with the sample nodeipamcontroller
// which demonstrates how cloud providers add external controllers to cloud-controller-manager

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	cloudcontrollerconfig "k8s.io/cloud-provider/app/config"
	genericcontrollermanager "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/klog/v2"
	nodeipamcontrolleroptions "k8s.io/kubernetes/cmd/kube-controller-manager/app/options"
	nodeipamcontroller "k8s.io/kubernetes/pkg/controller/nodeipam"
	nodeipamconfig "k8s.io/kubernetes/pkg/controller/nodeipam/config"
	"k8s.io/kubernetes/pkg/controller/nodeipam/ipam"
	netutils "k8s.io/utils/net"
)

const (
	// defaultNodeMaskCIDRIPv4 is default mask size for IPv4 node cidr
	defaultNodeMaskCIDRIPv4 = 24
	// defaultNodeMaskCIDRIPv6 is default mask size for IPv6 node cidr
	defaultNodeMaskCIDRIPv6 = 64
)

type nodeIPAMController struct {
	nodeIPAMControllerConfiguration nodeipamconfig.NodeIPAMControllerConfiguration
	nodeIPAMControllerOptions       nodeipamcontrolleroptions.NodeIPAMControllerOptions
}

func (nodeIpamController *nodeIPAMController) StartNodeIpamControllerWrapper(logger klog.Logger, initContext app.ControllerInitContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) app.InitFunc {
	allErrors := nodeIpamController.nodeIPAMControllerOptions.Validate()
	if len(allErrors) > 0 {
		logger.Error(nil, "NodeIPAM controller values are not properly set.")
	}
	nodeIpamController.nodeIPAMControllerOptions.ApplyTo(&nodeIpamController.nodeIPAMControllerConfiguration)

	return func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext) (controller.Interface, bool, error) {
		return startNodeIpamController(ctx, initContext, completedConfig, nodeIpamController.nodeIPAMControllerConfiguration, controllerContext, cloud)
	}
}

func startNodeIpamController(ctx context.Context, initContext app.ControllerInitContext, ccmConfig *cloudcontrollerconfig.CompletedConfig, nodeIPAMConfig nodeipamconfig.NodeIPAMControllerConfiguration, controllerCtx genericcontrollermanager.ControllerContext, cloud cloudprovider.Interface) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	var serviceCIDR *net.IPNet
	var secondaryServiceCIDR *net.IPNet

	// should we start nodeIPAM
	if !ccmConfig.ComponentConfig.KubeCloudShared.AllocateNodeCIDRs {
		return nil, false, nil
	}

	// Cannot run cloud ipam controller if cloud provider is nil (--cloud-provider not set or set to 'external')
	if cloud == nil && ccmConfig.ComponentConfig.KubeCloudShared.CIDRAllocatorType == string(ipam.CloudAllocatorType) {
		return nil, false, errors.New("--cidr-allocator-type is set to 'CloudAllocator' but cloud provider is not configured")
	}

	// failure: bad cidrs in config
	clusterCIDRs, dualStack, err := processCIDRs(ccmConfig.ComponentConfig.KubeCloudShared.ClusterCIDR)
	if err != nil {
		return nil, false, err
	}

	// failure: more than one cidr but they are not configured as dual stack
	if len(clusterCIDRs) > 1 && !dualStack {
		return nil, false, fmt.Errorf("len of ClusterCIDRs==%v and they are not configured as dual stack (at least one from each IPFamily", len(clusterCIDRs))
	}

	// failure: more than cidrs is not allowed even with dual stack
	if len(clusterCIDRs) > 2 {
		return nil, false, fmt.Errorf("len of clusters is:%v > more than max allowed of 2", len(clusterCIDRs))
	}

	// service cidr processing
	if len(strings.TrimSpace(nodeIPAMConfig.ServiceCIDR)) != 0 {
		_, serviceCIDR, err = netutils.ParseCIDRSloppy(nodeIPAMConfig.ServiceCIDR)
		if err != nil {
			logger.Error(err, "Unsuccessful parsing of service CIDR", "CIDR", nodeIPAMConfig.ServiceCIDR)
		}
	}

	if len(strings.TrimSpace(nodeIPAMConfig.SecondaryServiceCIDR)) != 0 {
		_, secondaryServiceCIDR, err = netutils.ParseCIDRSloppy(nodeIPAMConfig.SecondaryServiceCIDR)
		if err != nil {
			logger.Error(err, "Unsuccessful parsing of service CIDR", "CIDR", nodeIPAMConfig.SecondaryServiceCIDR)
		}
	}

	// the following checks are triggered if both serviceCIDR and secondaryServiceCIDR are provided
	if serviceCIDR != nil && secondaryServiceCIDR != nil {
		// should be dual stack (from different IPFamilies)
		dualstackServiceCIDR, err := netutils.IsDualStackCIDRs([]*net.IPNet{serviceCIDR, secondaryServiceCIDR})
		if err != nil {
			return nil, false, fmt.Errorf("failed to perform dualstack check on serviceCIDR and secondaryServiceCIDR error:%v", err)
		}
		if !dualstackServiceCIDR {
			return nil, false, fmt.Errorf("serviceCIDR and secondaryServiceCIDR are not dualstack (from different IPfamiles)")
		}
	}

	nodeCIDRMaskSizes, err := setNodeCIDRMaskSizes(nodeIPAMConfig, clusterCIDRs)
	if err != nil {
		return nil, false, err
	}

	nodeIpamController, err := nodeipamcontroller.NewNodeIpamController(
		ctx,
		controllerCtx.InformerFactory.Core().V1().Nodes(),
		cloud,
		controllerCtx.ClientBuilder.ClientOrDie(initContext.ClientName),
		clusterCIDRs,
		serviceCIDR,
		secondaryServiceCIDR,
		nodeCIDRMaskSizes,
		ipam.CIDRAllocatorType(ccmConfig.ComponentConfig.KubeCloudShared.CIDRAllocatorType),
	)
	if err != nil {
		return nil, true, err
	}
	go nodeIpamController.Run(ctx)
	return nil, true, nil
}

// processCIDRs is a helper function that works on a comma separated cidrs and returns
// a list of typed cidrs
// a flag if cidrs represents a dual stack
// error if failed to parse any of the cidrs
func processCIDRs(cidrsList string) ([]*net.IPNet, bool, error) {
	cidrsSplit := strings.Split(strings.TrimSpace(cidrsList), ",")

	cidrs, err := netutils.ParseCIDRs(cidrsSplit)
	if err != nil {
		return nil, false, err
	}

	// if cidrs has an error then the previous call will fail
	// safe to ignore error checking on next call
	dualstack, _ := netutils.IsDualStackCIDRs(cidrs)

	return cidrs, dualstack, nil
}

// setNodeCIDRMaskSizes returns the IPv4 and IPv6 node cidr mask sizes to the value provided
// for --node-cidr-mask-size-ipv4 and --node-cidr-mask-size-ipv6 respectively. If value not provided,
// then it will return default IPv4 and IPv6 cidr mask sizes.
func setNodeCIDRMaskSizes(cfg nodeipamconfig.NodeIPAMControllerConfiguration, clusterCIDRs []*net.IPNet) ([]int, error) {

	sortedSizes := func(maskSizeIPv4, maskSizeIPv6 int) []int {
		nodeMaskCIDRs := make([]int, len(clusterCIDRs))

		for idx, clusterCIDR := range clusterCIDRs {
			if netutils.IsIPv6CIDR(clusterCIDR) {
				nodeMaskCIDRs[idx] = maskSizeIPv6
			} else {
				nodeMaskCIDRs[idx] = maskSizeIPv4
			}
		}
		return nodeMaskCIDRs
	}

	// --node-cidr-mask-size flag is incompatible with dual stack clusters.
	ipv4Mask, ipv6Mask := defaultNodeMaskCIDRIPv4, defaultNodeMaskCIDRIPv6
	isDualstack := len(clusterCIDRs) > 1

	// case one: cluster is dualstack (i.e, more than one cidr)
	if isDualstack {
		// if --node-cidr-mask-size then fail, user must configure the correct dual-stack mask sizes (or use default)
		if cfg.NodeCIDRMaskSize != 0 {
			return nil, errors.New("usage of --node-cidr-mask-size is not allowed with dual-stack clusters")
		}

		if cfg.NodeCIDRMaskSizeIPv4 != 0 {
			ipv4Mask = int(cfg.NodeCIDRMaskSizeIPv4)
		}
		if cfg.NodeCIDRMaskSizeIPv6 != 0 {
			ipv6Mask = int(cfg.NodeCIDRMaskSizeIPv6)
		}
		return sortedSizes(ipv4Mask, ipv6Mask), nil
	}

	maskConfigured := cfg.NodeCIDRMaskSize != 0
	maskV4Configured := cfg.NodeCIDRMaskSizeIPv4 != 0
	maskV6Configured := cfg.NodeCIDRMaskSizeIPv6 != 0
	isSingleStackIPv6 := netutils.IsIPv6CIDR(clusterCIDRs[0])

	// original flag is set
	if maskConfigured {
		// original mask flag is still the main reference.
		if maskV4Configured || maskV6Configured {
			return nil, errors.New("usage of --node-cidr-mask-size-ipv4 and --node-cidr-mask-size-ipv6 is not allowed if --node-cidr-mask-size is set. For dual-stack clusters please unset it and use IPFamily specific flags")
		}

		mask := int(cfg.NodeCIDRMaskSize)
		return sortedSizes(mask, mask), nil
	}

	if maskV4Configured {
		if isSingleStackIPv6 {
			return nil, errors.New("usage of --node-cidr-mask-size-ipv4 is not allowed for a single-stack IPv6 cluster")
		}

		ipv4Mask = int(cfg.NodeCIDRMaskSizeIPv4)
	}

	// !maskV4Configured && !maskConfigured && maskV6Configured
	if maskV6Configured {
		if !isSingleStackIPv6 {
			return nil, errors.New("usage of --node-cidr-mask-size-ipv6 is not allowed for a single-stack IPv4 cluster")
		}

		ipv6Mask = int(cfg.NodeCIDRMaskSizeIPv6)
	}
	return sortedSizes(ipv4Mask, ipv6Mask), nil
}
