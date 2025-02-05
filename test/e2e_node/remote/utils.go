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

package remote

import (
	"fmt"
	"path/filepath"

	"k8s.io/klog/v2"

	"k8s.io/kubernetes/test/e2e_node/builder"
)

// utils.go contains functions used across test suites.

const (
	cniVersion       = "v1.6.2"
	cniDirectory     = "cni/bin" // The CNI tarball places binaries under directory under "cni/bin".
	cniConfDirectory = "cni/net.d"

	ecrCredentialProviderVersion = "v1.27.1"
)

const cniConfig = `{
  "name": "mynet",
  "type": "bridge",
  "bridge": "mynet0",
  "isDefaultGateway": true,
  "forceAddress": false,
  "ipMasq": true,
  "hairpinMode": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.10.0.0/16"
  }
}
`

const credentialGCPProviderConfig = `kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
 - name: gcp-credential-provider
   apiVersion: credentialprovider.kubelet.k8s.io/v1
   matchImages:
   - "gcr.io"
   - "*.gcr.io"
   - "container.cloud.google.com"
   - "*.pkg.dev"
   defaultCacheDuration: 1m
 - name: gcp-credential-provider-with-sa
   apiVersion: credentialprovider.kubelet.k8s.io/v1
   matchImages:
   - "gcr.io"
   - "*.gcr.io"
   - "container.cloud.google.com"
   - "*.pkg.dev"
   defaultCacheDuration: 1m
   tokenAttributes:
     serviceAccountTokenAudience: test-audience
     requireServiceAccount: true
     serviceAccountAnnotationKeys:
     - "domain.io/identity-id"
     - "domain.io/identity-type"
   env:
   - name: PLUGIN_MODE
     value: "serviceaccount"`

const credentialAWSProviderConfig = `kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
- name: ecr-credential-provider
  apiVersion: credentialprovider.kubelet.k8s.io/v1
  matchImages:
  - "*.dkr.ecr.*.amazonaws.com"
  - "*.dkr.ecr.*.amazonaws.com.cn"
  - "*.dkr.ecr-fips.*.amazonaws.com"
  - "*.dkr.ecr.us-iso-east-1.c2s.ic.gov"
  - "*.dkr.ecr.us-isob-east-1.sc2s.sgov.gov"
  defaultCacheDuration: 12h`

func getCNIURL() string {
	cniArch := "amd64"
	if builder.IsTargetArchArm64() {
		cniArch = "arm64"
	}
	cniURL := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-%s-%s.tgz", cniVersion, cniArch, cniVersion)
	return cniURL

}

// Install the cni plugin and add basic bridge configuration to the
// configuration directory.
func setupCNI(host, workspace string) error {
	klog.V(2).Infof("Install CNI on %q", host)
	cniPath := filepath.Join(workspace, cniDirectory)
	klog.V(2).Infof("Install CNI on path %q", cniPath)
	cmd := getSSHCommand(" ; ",
		fmt.Sprintf("mkdir -p %s", cniPath),
		fmt.Sprintf("curl -s -L %s | tar -xz -C %s", getCNIURL(), cniPath),
	)
	if output, err := SSH(host, "sh", "-c", cmd); err != nil {
		return fmt.Errorf("failed to install cni plugin on %q: %v output: %q", host, err, output)
	}

	// The added CNI network config is not needed for kubenet. It is only
	// used when testing the CNI network plugin, but is added in both cases
	// for consistency and simplicity.
	klog.V(2).Infof("Adding CNI configuration on %q", host)
	cniConfigPath := filepath.Join(workspace, cniConfDirectory)
	cmd = getSSHCommand(" ; ",
		fmt.Sprintf("mkdir -p %s", cniConfigPath),
		fmt.Sprintf("echo %s > %s", quote(cniConfig), filepath.Join(cniConfigPath, "mynet.conf")),
	)
	if output, err := SSH(host, "sh", "-c", cmd); err != nil {
		return fmt.Errorf("failed to write cni configuration on %q: %v output: %q", host, err, output)
	}
	return nil
}

func getECRCredentialProviderURL() string {
	arch := "amd64"
	if builder.IsTargetArchArm64() {
		arch = "arm64"
	}
	return fmt.Sprintf("https://artifacts.k8s.io/binaries/cloud-provider-aws/%s/linux/%s/ecr-credential-provider-linux-%s", ecrCredentialProviderVersion, arch, arch)
}

func setupECRCredentialProvider(host, workspace string) error {
	klog.V(2).Infof("Install ecr-credential-provider on %q at %q", host, workspace)
	cmd := getSSHCommand(" ; ",
		fmt.Sprintf("curl -s -Lo %s/ecr-credential-provider %s", workspace, getECRCredentialProviderURL()),
		fmt.Sprintf("chmod a+x %s/ecr-credential-provider", workspace),
	)
	if output, err := SSH(host, "sh", "-c", cmd); err != nil {
		return fmt.Errorf("failed to install ecr-credential-provider plugin on %q: %v output: %q", host, err, output)
	}
	return nil
}

func configureCredentialProvider(host, workspace string) error {
	klog.V(2).Infof("Configuring kubelet credential provider on %q", host)

	credentialProviderConfig := credentialGCPProviderConfig
	if GetSSHUser() == "ec2-user" {
		credentialProviderConfig = credentialAWSProviderConfig
		err := setupECRCredentialProvider(host, workspace)
		if err != nil {
			return err
		}
	}

	cmd := getSSHCommand(" ; ",
		fmt.Sprintf("echo %s > %s", quote(credentialProviderConfig), filepath.Join(workspace, "credential-provider.yaml")),
	)
	if output, err := SSH(host, "sh", "-c", cmd); err != nil {
		return fmt.Errorf("failed to write credential provider configuration on %q: %v output: %q", host, err, output)
	}

	return nil
}

// configureFirewall configures iptable firewall rules.
func configureFirewall(host string) error {
	klog.V(2).Infof("Configure iptables firewall rules on %q", host)

	// Since the goal is to enable connectivity without taking into account current rule,
	// we can just prepend the accept rules directly without any check
	cmd := getSSHCommand("&&",
		"iptables -I INPUT 1 -w -p tcp -j ACCEPT",
		"iptables -I INPUT 1 -w -p udp -j ACCEPT",
		"iptables -I INPUT 1 -w -p icmp -j ACCEPT",
		"iptables -I FORWARD 1 -w -p tcp -j ACCEPT",
		"iptables -I FORWARD 1 -w -p udp -j ACCEPT",
		"iptables -I FORWARD 1 -w -p icmp -j ACCEPT",
	)
	output, err := SSH(host, "sh", "-c", cmd)
	if err != nil {
		return fmt.Errorf("failed to configured firewall on %q: %v output: %v", host, err, output)
	}
	return nil
}

// cleanupNodeProcesses kills all running node processes may conflict with the test.
func cleanupNodeProcesses(host string) {
	klog.V(2).Infof("Killing any existing node processes on %q", host)
	cmd := getSSHCommand(" ; ",
		"pkill kubelet",
		"pkill kube-apiserver",
		"pkill etcd",
		"pkill e2e_node.test",
	)
	// No need to log an error if pkill fails since pkill will fail if the commands are not running.
	// If we are unable to stop existing running k8s processes, we should see messages in the kubelet/apiserver/etcd
	// logs about failing to bind the required ports.
	SSH(host, "sh", "-c", cmd)
}

// Quotes a shell literal so it can be nested within another shell scope.
func quote(s string) string {
	return fmt.Sprintf("'\"'\"'%s'\"'\"'", s)
}
