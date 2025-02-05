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

package pluginwatcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"

	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher/example_plugin_apis/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher/example_plugin_apis/v1beta2"
)

type exampleHandler struct {
	SupportedVersions []string
	ExpectedNames     map[string]int

	eventChans map[string]chan examplePluginEvent // map[pluginName]eventChan

	m sync.Mutex

	permitDeprecatedDir bool
}

type examplePluginEvent int

const (
	exampleEventValidate   examplePluginEvent = 0
	exampleEventRegister   examplePluginEvent = 1
	exampleEventDeRegister examplePluginEvent = 2
)

// NewExampleHandler provide a example handler
func NewExampleHandler(supportedVersions []string, permitDeprecatedDir bool) *exampleHandler {
	return &exampleHandler{
		SupportedVersions: supportedVersions,
		ExpectedNames:     make(map[string]int),

		eventChans:          make(map[string]chan examplePluginEvent),
		permitDeprecatedDir: permitDeprecatedDir,
	}
}

func (p *exampleHandler) ValidatePlugin(pluginName string, endpoint string, versions []string) error {
	p.SendEvent(pluginName, exampleEventValidate)

	n, ok := p.DecreasePluginCount(pluginName)
	if !ok && n > 0 {
		return fmt.Errorf("pluginName('%s') wasn't expected (count is %d)", pluginName, n)
	}

	if !reflect.DeepEqual(versions, p.SupportedVersions) {
		return fmt.Errorf("versions('%v') != supported versions('%v')", versions, p.SupportedVersions)
	}

	// this handler expects non-empty endpoint as an example
	if len(endpoint) == 0 {
		return errors.New("expecting non empty endpoint")
	}

	return nil
}

func (p *exampleHandler) RegisterPlugin(pluginName, endpoint string, versions []string) error {
	p.SendEvent(pluginName, exampleEventRegister)

	// Verifies the grpcServer is ready to serve services.
	_, conn, err := dial(endpoint, time.Second)
	if err != nil {
		return fmt.Errorf("failed dialing endpoint (%s): %v", endpoint, err)
	}
	defer conn.Close()

	// The plugin handler should be able to use any listed service API version.
	v1beta1Client := v1beta1.NewExampleClient(conn)
	v1beta2Client := v1beta2.NewExampleClient(conn)

	// Tests v1beta1 GetExampleInfo
	_, err = v1beta1Client.GetExampleInfo(context.Background(), &v1beta1.ExampleRequest{})
	if err != nil {
		return fmt.Errorf("failed GetExampleInfo for v1beta2Client(%s): %v", endpoint, err)
	}

	// Tests v1beta1 GetExampleInfo
	_, err = v1beta2Client.GetExampleInfo(context.Background(), &v1beta2.ExampleRequest{})
	if err != nil {
		return fmt.Errorf("failed GetExampleInfo for v1beta2Client(%s): %v", endpoint, err)
	}

	return nil
}

func (p *exampleHandler) DeRegisterPlugin(pluginName string) {
	p.SendEvent(pluginName, exampleEventDeRegister)
}

func (p *exampleHandler) SendEvent(pluginName string, event examplePluginEvent) {
	klog.V(2).InfoS("Sending event for plugin", "pluginName", pluginName, "event", event, "channel", p.eventChans[pluginName])
	p.eventChans[pluginName] <- event
}

func (p *exampleHandler) DecreasePluginCount(pluginName string) (old int, ok bool) {
	p.m.Lock()
	defer p.m.Unlock()

	v, ok := p.ExpectedNames[pluginName]
	if !ok {
		v = -1
	}

	return v, ok
}

// Dial establishes the gRPC communication with the picked up plugin socket. https://godoc.org/google.golang.org/grpc#Dial
func dial(unixSocketPath string, timeout time.Duration) (registerapi.RegistrationClient, *grpc.ClientConn, error) {
	c, err := grpc.NewClient(unixSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}),
	)

	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial socket %s, err: %v", unixSocketPath, err)
	}

	return registerapi.NewRegistrationClient(c), c, nil
}
