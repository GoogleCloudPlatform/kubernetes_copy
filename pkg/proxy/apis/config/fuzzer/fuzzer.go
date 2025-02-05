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

package fuzzer

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/google/gofuzz"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/utils/ptr"
)

// generateRandomIP is copied from pkg/apis/networking/fuzzer/fuzzer.go
func generateRandomIP(is6 bool, c fuzz.Continue) string {
	n := 4
	if is6 {
		n = 16
	}
	bytes := make([]byte, n)
	for i := 0; i < n; i++ {
		bytes[i] = uint8(c.Rand.Intn(255))
	}

	ip, ok := netip.AddrFromSlice(bytes)
	if ok {
		return ip.String()
	}
	// this should not happen
	panic(fmt.Sprintf("invalid IP %v", bytes))
}

// generateRandomCIDR is copied from pkg/apis/networking/fuzzer/fuzzer.go
func generateRandomCIDR(is6 bool, c fuzz.Continue) string {
	ip, err := netip.ParseAddr(generateRandomIP(is6, c))
	if err != nil {
		// generateRandomIP already panics if returns a not valid ip
		panic(err)
	}

	n := 32
	if is6 {
		n = 128
	}

	bits := c.Rand.Intn(n)
	prefix := netip.PrefixFrom(ip, bits)
	return prefix.Masked().String()
}

// getRandomDualStackCIDR returns a random dual-stack CIDR.
func getRandomDualStackCIDR(c fuzz.Continue) []string {
	cidrIPv4 := generateRandomCIDR(false, c)
	cidrIPv6 := generateRandomCIDR(true, c)

	cidrs := []string{cidrIPv4, cidrIPv6}
	if c.RandBool() {
		cidrs = []string{cidrIPv6, cidrIPv4}
	}
	return cidrs[:1+c.Intn(2)]
}

// Funcs returns the fuzzer functions for the kube-proxy apis.
func Funcs(codecs runtimeserializer.CodecFactory) []interface{} {
	return []interface{}{
		func(obj *kubeproxyconfig.KubeProxyConfiguration, c fuzz.Continue) {
			c.FuzzNoCustom(obj)
			obj.BindAddress = fmt.Sprintf("%d.%d.%d.%d", c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(256))
			obj.ClientConnection.ContentType = c.RandString()
			obj.DetectLocal.ClusterCIDRs = getRandomDualStackCIDR(c)
			obj.Linux.Conntrack.MaxPerCore = ptr.To(c.Int31())
			obj.Linux.Conntrack.Min = ptr.To(c.Int31())
			obj.Linux.Conntrack.TCPCloseWaitTimeout = &metav1.Duration{Duration: time.Duration(c.Int63()) * time.Hour}
			obj.Linux.Conntrack.TCPEstablishedTimeout = &metav1.Duration{Duration: time.Duration(c.Int63()) * time.Hour}
			obj.FeatureGates = map[string]bool{c.RandString(): true}
			obj.HealthzBindAddress = fmt.Sprintf("%d.%d.%d.%d:%d", c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(65536))
			obj.IPTables.MasqueradeBit = ptr.To(c.Int31())
			obj.IPTables.LocalhostNodePorts = ptr.To(c.RandBool())
			obj.IPVS.IPSet.HashSize = ptr.To(c.Int31())
			obj.IPVS.IPSet.MaxElements = ptr.To(c.Int31())
			obj.NFTables.MasqueradeBit = ptr.To(c.Int31())
			obj.MetricsBindAddress = fmt.Sprintf("%d.%d.%d.%d:%d", c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(65536))
			obj.Linux.OOMScoreAdj = ptr.To(c.Int31())
			obj.ClientConnection.ContentType = "bar"
			obj.NodePortAddresses = []string{"1.2.3.0/24"}
			if obj.Logging.Format == "" {
				obj.Logging.Format = "text"
			}
		},
	}
}
