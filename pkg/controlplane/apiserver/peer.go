/*
Copyright 2024 The Kubernetes Authors.

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

package apiserver

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/reconcilers"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilpeerproxy "k8s.io/apiserver/pkg/util/peerproxy"
	clientgoinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	api "k8s.io/kubernetes/pkg/apis/core"
)

const (
	// DefaultPeerEndpointReconcileInterval is the default amount of time for how often
	// the peer endpoint leases are reconciled.
	DefaultPeerEndpointReconcileInterval = 10 * time.Second
	// DefaultPeerEndpointReconcilerTTL is the default TTL timeout for peer endpoint
	// leases on the storage layer
	DefaultPeerEndpointReconcilerTTL = 15 * time.Second
)

func BuildPeerProxy(versionedInformer clientgoinformers.SharedInformerFactory, loopbackClientConfig *rest.Config,
	proxyClientCertFile string, proxyClientKeyFile string, peerCAFile string, peerAdvertiseAddress reconcilers.PeerAdvertiseAddress,
	apiServerID string, reconciler reconcilers.PeerEndpointLeaseReconciler, serializer runtime.NegotiatedSerializer) (utilpeerproxy.Interface, error) {
	if proxyClientCertFile == "" {
		return nil, fmt.Errorf("error building peer proxy handler, proxy-cert-file not specified")
	}
	if proxyClientKeyFile == "" {
		return nil, fmt.Errorf("error building peer proxy handler, proxy-key-file not specified")
	}

	proxyClientConfig := &transport.Config{
		TLS: transport.TLSConfig{
			Insecure:   false,
			CertFile:   proxyClientCertFile,
			KeyFile:    proxyClientKeyFile,
			CAFile:     peerCAFile,
			ServerName: "kubernetes.default.svc",
		}}

	return utilpeerproxy.NewPeerProxyHandler(
		versionedInformer,
		apiServerID,
		reconciler,
		serializer,
		loopbackClientConfig,
		proxyClientConfig,
	), nil
}

// CreatePeerEndpointLeaseReconciler creates a apiserver endpoint lease reconciliation loop
// The peer endpoint leases are used to find network locations of apiservers for peer proxy
func CreatePeerEndpointLeaseReconciler(c genericapiserver.Config, storageFactory serverstorage.StorageFactory) (reconcilers.PeerEndpointLeaseReconciler, error) {
	ttl := DefaultPeerEndpointReconcilerTTL
	config, err := storageFactory.NewConfig(api.Resource("apiServerPeerIPInfo"), &api.Endpoints{})
	if err != nil {
		return nil, fmt.Errorf("error creating storage factory config: %w", err)
	}
	reconciler, err := reconcilers.NewPeerEndpointLeaseReconciler(config, "/peerserverleases/", ttl)
	return reconciler, err
}

// utility function to get the apiserver address that is used by peer apiservers to proxy
// requests to this apiserver in case the peer is incapable of serving the request
func getPeerAddress(peerAdvertiseAddress reconcilers.PeerAdvertiseAddress, publicAddress net.IP, publicServicePort int) string {
	if peerAdvertiseAddress.PeerAdvertiseIP != "" && peerAdvertiseAddress.PeerAdvertisePort != "" {
		return net.JoinHostPort(peerAdvertiseAddress.PeerAdvertiseIP, peerAdvertiseAddress.PeerAdvertisePort)
	} else {
		return net.JoinHostPort(publicAddress.String(), strconv.Itoa(publicServicePort))
	}
}
