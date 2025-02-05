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

package podresources

import (
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"k8s.io/cri-client/pkg/util"
	"k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

// Note: Consumers of the pod resources API should not be importing this package.
// They should copy paste the function in their project.

// GetV1alpha1Client returns a client for the PodResourcesLister grpc service
// Note: This is deprecated
func GetV1alpha1Client(socket string, connectionTimeout time.Duration, maxMsgSize int) (v1alpha1.PodResourcesListerClient, *grpc.ClientConn, error) {
	addr, dialer, err := util.GetAddressAndDialer(socket)
	if err != nil {
		return nil, nil, err
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)))
	if err != nil {
		return nil, nil, fmt.Errorf("error dialing socket %s: %v", socket, err)
	}
	return v1alpha1.NewPodResourcesListerClient(conn), conn, nil
}

// GetV1Client returns a client for the PodResourcesLister grpc service
func GetV1Client(socket string, connectionTimeout time.Duration, maxMsgSize int) (v1.PodResourcesListerClient, *grpc.ClientConn, error) {
	addr, dialer, err := util.GetAddressAndDialer(socket)
	if err != nil {
		return nil, nil, err
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)))
	if err != nil {
		return nil, nil, fmt.Errorf("error dialing socket %s: %v", socket, err)
	}
	return v1.NewPodResourcesListerClient(conn), conn, nil
}
