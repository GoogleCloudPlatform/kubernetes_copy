/*
Copyright 2017 The Kubernetes Authors.

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

package upgrade

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/features"
	kubeletphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/kubelet"
	kubeadmutil "k8s.io/kubernetes/cmd/kubeadm/app/util"
	dryrunutil "k8s.io/kubernetes/cmd/kubeadm/app/util/dryrun"
)

// UnupgradedControlPlaneInstances returns a list of control plane instances that have not yet been upgraded.
//
// NB. This function can only be called after the current control plane instance has been upgraded already.
// Because it determines whether the other control plane instances have been upgraded by checking whether
// the kube-apiserver image of other control plane instance is the same as that of this instance.
func UnupgradedControlPlaneInstances(client clientset.Interface, nodeName string) ([]string, error) {
	selector := labels.SelectorFromSet(labels.Set(map[string]string{
		"component": kubeadmconstants.KubeAPIServer,
	}))
	pods, err := client.CoreV1().Pods(metav1.NamespaceSystem).List(context.TODO(), metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list kube-apiserver Pod from cluster")
	}
	if len(pods.Items) == 0 {
		return nil, errors.Errorf("cannot find kube-apiserver Pod by label selector: %v", selector.String())
	}

	nodeImageMap := map[string]string{}

	for _, pod := range pods.Items {
		found := false
		for _, c := range pod.Spec.Containers {
			if c.Name == kubeadmconstants.KubeAPIServer {
				nodeImageMap[pod.Spec.NodeName] = c.Image
				found = true
				break
			}
		}
		if !found {
			return nil, errors.Errorf("cannot find container by name %q for Pod %v", kubeadmconstants.KubeAPIServer, klog.KObj(&pod))
		}
	}

	upgradedImage, ok := nodeImageMap[nodeName]
	if !ok {
		return nil, errors.Errorf("cannot find kube-apiserver image for current control plane instance %v", nodeName)
	}

	unupgradedNodes := sets.New[string]()
	for node, image := range nodeImageMap {
		if image != upgradedImage {
			unupgradedNodes.Insert(node)
		}
	}

	if len(unupgradedNodes) > 0 {
		return sets.List(unupgradedNodes), nil
	}

	return nil, nil
}

// WriteKubeletConfigFiles writes the kubelet config file to disk, but first creates a backup of any existing one.
func WriteKubeletConfigFiles(cfg *kubeadmapi.InitConfiguration, kubeletConfigDir string, patchesDir string, dryRun bool, out io.Writer) error {
	var (
		err        error
		kubeletDir = kubeadmconstants.KubeletRunDirectory
	)
	// If dry-running, this will return a directory under /etc/kubernetes/tmp or kubeletConfigDir.
	if dryRun {
		kubeletDir, err = kubeadmconstants.CreateTempDir(kubeletConfigDir, "kubeadm-upgrade-dryrun")
	}
	if err != nil {
		// The error here should never occur in reality, would only be thrown if /tmp doesn't exist on the machine.
		return err
	}
	// Create a copy of the kubelet config file in the /etc/kubernetes/tmp or kubeletConfigDir.
	backupDir, err := kubeadmconstants.CreateTempDir(kubeletConfigDir, "kubeadm-kubelet-config")
	if err != nil {
		return err
	}
	klog.Warningf("Using temporary directory %s for kubelet config. To override it set the environment variable %s",
		backupDir, kubeadmconstants.EnvVarUpgradeDryRunDir)

	src := filepath.Join(kubeletDir, kubeadmconstants.KubeletConfigurationFileName)
	dest := filepath.Join(backupDir, kubeadmconstants.KubeletConfigurationFileName)

	if !dryRun {
		fmt.Printf("[upgrade] Backing up kubelet config file to %s\n", dest)
		err := kubeadmutil.CopyFile(src, dest)
		if err != nil {
			return errors.Wrap(err, "error backing up the kubelet config file")
		}
	} else {
		fmt.Printf("[dryrun] Would back up kubelet config file to %s\n", dest)
	}

	if features.Enabled(cfg.FeatureGates, features.NodeLocalCRISocket) {
		// If instance-config.yaml exist on disk, we don't need to create it.
		_, err := os.Stat(filepath.Join(kubeletDir, kubeadmconstants.KubeletInstanceConfigurationFileName))
		// After the NodeLocalCRISocket feature gate is removed, os.IsNotExist(err) should also be removed.
		// If there is no instance configuration, it indicates that the configuration on the node has been corrupted,
		// and an error needs to be reported.
		if os.IsNotExist(err) {
			var containerRuntimeEndpoint string
			var kubeletFlags []kubeadmapi.Arg
			dynamicFlags, err := kubeletphase.ReadKubeletDynamicEnvFile(filepath.Join(kubeletDir, kubeadmconstants.KubeletEnvFileName))
			if err == nil {
				args := kubeadmutil.ArgumentsFromCommand(dynamicFlags)
				for _, arg := range args {
					if arg.Name == "container-runtime-endpoint" {
						containerRuntimeEndpoint = arg.Value
						continue
					}
					kubeletFlags = append(kubeletFlags, arg)
				}
				if len(containerRuntimeEndpoint) != 0 {
					if err := kubeletphase.WriteKubeletArgsToFile(kubeletFlags, nil, kubeletDir); err != nil {
						return err
					}
				}
			} else if dryRun {
				fmt.Fprintf(os.Stdout, "[dryrun] would read the flag --container-runtime-endpoint value from %q, which is missing. "+
					"Using default socket %q instead", kubeadmconstants.KubeletEnvFileName, kubeadmconstants.DefaultCRISocket)
				containerRuntimeEndpoint = kubeadmconstants.DefaultCRISocket
			} else {
				return errors.Wrap(err, "error reading kubeadm flags file")
			}

			kubeletConfig := &kubeletconfig.KubeletConfiguration{
				ContainerRuntimeEndpoint: containerRuntimeEndpoint,
			}

			if err := kubeletphase.WriteInstanceConfigToDisk(kubeletConfig, kubeletDir); err != nil {
				return errors.Wrap(err, "error writing kubelet instance configuration")
			}

			if dryRun { // Print what contents would be written
				err = dryrunutil.PrintDryRunFile(kubeadmconstants.KubeletInstanceConfigurationFileName, kubeletDir, kubeadmconstants.KubeletRunDirectory, os.Stdout)
				if err != nil {
					return errors.Wrap(err, "error printing kubelet instance configuration file on dryrun")
				}
			}
		}
	}

	// Write the configuration for the kubelet down to disk so the upgraded kubelet can start with fresh config
	if err := kubeletphase.WriteConfigToDisk(&cfg.ClusterConfiguration, kubeletDir, patchesDir, out); err != nil {
		return errors.Wrap(err, "error writing kubelet configuration to file")
	}

	if dryRun { // Print what contents would be written
		err := dryrunutil.PrintDryRunFile(kubeadmconstants.KubeletConfigurationFileName, kubeletDir, kubeadmconstants.KubeletRunDirectory, os.Stdout)
		if err != nil {
			return errors.Wrap(err, "error printing kubelet configuration file on dryrun")
		}
	}
	return nil
}

// UpdateKubeletKubeconfigServer changes the Server URL in the kubelets kubeconfig if necessary depending on the
// ControlPlaneKubeletLocalMode feature gate.
// TODO: remove this function once ControlPlaneKubeletLocalMode goes GA and is hardcoded to be enabled by default:
// https://github.com/kubernetes/kubeadm/issues/2271
func UpdateKubeletKubeconfigServer(cfg *kubeadmapi.InitConfiguration, dryRun bool) error {
	kubeletKubeConfigFilePath := filepath.Join(kubeadmconstants.KubernetesDir, kubeadmconstants.KubeletKubeConfigFileName)

	if _, err := os.Stat(kubeletKubeConfigFilePath); err != nil {
		if os.IsNotExist(err) {
			klog.V(2).Infof("Could not mutate the Server URL in %s: %v", kubeletKubeConfigFilePath, err)
			return nil
		}
		return err
	}

	config, err := clientcmd.LoadFromFile(kubeletKubeConfigFilePath)
	if err != nil {
		return err
	}

	configContext, ok := config.Contexts[config.CurrentContext]
	if !ok {
		return errors.Errorf("cannot find cluster for active context in kubeconfig %q", kubeletKubeConfigFilePath)
	}

	localAPIEndpoint, err := kubeadmutil.GetLocalAPIEndpoint(&cfg.LocalAPIEndpoint)
	if err != nil {
		return err
	}

	controlPlaneAPIEndpoint, err := kubeadmutil.GetControlPlaneEndpoint(cfg.ControlPlaneEndpoint, &cfg.LocalAPIEndpoint)
	if err != nil {
		return err
	}

	var targetServer string
	if features.Enabled(cfg.FeatureGates, features.ControlPlaneKubeletLocalMode) {
		// Skip changing kubeconfig file if Server does not match the ControlPlaneEndpoint.
		if config.Clusters[configContext.Cluster].Server != controlPlaneAPIEndpoint || controlPlaneAPIEndpoint == localAPIEndpoint {
			klog.V(2).Infof("Skipping update of the Server URL in %s, because it's already not equal to %q or already matches the localAPIEndpoint", kubeletKubeConfigFilePath, controlPlaneAPIEndpoint)
			return nil
		}
		targetServer = localAPIEndpoint
	} else {
		// Skip changing kubeconfig file if Server does not match the localAPIEndpoint.
		if config.Clusters[configContext.Cluster].Server != localAPIEndpoint {
			klog.V(2).Infof("Skipping update of the Server URL in %s, because it already matches the controlPlaneAPIEndpoint", kubeletKubeConfigFilePath)
			return nil
		}
		targetServer = controlPlaneAPIEndpoint
	}

	if dryRun {
		fmt.Printf("[dryrun] Would change the Server URL from %q to %q in %s and try to restart kubelet\n", config.Clusters[configContext.Cluster].Server, targetServer, kubeletKubeConfigFilePath)
		return nil
	}

	klog.V(1).Infof("Changing the Server URL from %q to %q in %s", config.Clusters[configContext.Cluster].Server, targetServer, kubeletKubeConfigFilePath)
	config.Clusters[configContext.Cluster].Server = targetServer

	if err := clientcmd.WriteToFile(*config, kubeletKubeConfigFilePath); err != nil {
		return err
	}

	kubeletphase.TryRestartKubelet()

	return nil
}
