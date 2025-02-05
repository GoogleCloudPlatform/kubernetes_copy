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

package testing

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
)

func init() {
	// If instantiated more than once or together with other servers, the
	// servers would try to modify the global logging state. This must get
	// ignored during testing.
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged

	// Because the test server gets started after other goroutines are
	// running already, we also have to initialize logging here when
	// those goroutines are not running yet. This works because the
	// test server uses the default config.
	config := logsapi.NewLoggingConfiguration()
	if err := logsapi.ValidateAndApply(config, nil); err != nil {
		panic(err)
	}
}

// TearDownFunc is to be called to tear down a test server.
type TearDownFunc func()

// TestServer return values supplied by kube-test-ApiServer
type TestServer struct {
	LoopbackClientConfig *restclient.Config // Rest client config using the magic token
	Options              *options.CloudControllerManagerOptions
	Config               *config.CompletedConfig
	TearDownFn           TearDownFunc // TearDown function
	TmpDir               string       // Temp Dir used, by the apiserver
}

// StartTestServer starts a cloud-controller-manager. A rest client config and a tear-down func,
// and location of the tmpdir are returned.
//
// Note: we return a tear-down func instead of a stop channel because the later will leak temporary
// files that because Golang testing's call to os.Exit will not give a stop channel go routine
// enough time to remove temporary files.
func StartTestServer(ctx context.Context, customFlags []string) (result TestServer, err error) {
	return StartTestServerWithOptions(ctx, customFlags, app.DefaultInitFuncConstructors, names.CCMControllerAliases())
}

// StartTestServerWithOptions starts a cloud-controller-manager with extra controller loop initializer constructors
// and controller loop aliaes. Within the returned TestServer, a rest client config, a tear-down func,
// and location of the tmpdir are returned; or an error if one occurred.
func StartTestServerWithOptions(ctx context.Context,
	customFlags []string,
	constructors map[string]app.ControllerInitFuncConstructor,
	aliases map[string]string) (result TestServer, err error) {
	logger := klog.FromContext(ctx)
	stopCh := make(chan struct{})
	var errCh chan error
	configDoneCh := make(chan struct{})
	var capturedConfig config.CompletedConfig
	tearDown := func() {
		close(stopCh)

		// If cloud-controller-manager was started, let's wait for
		// it to shutdown clearly.
		if errCh != nil {
			err, ok := <-errCh
			if ok && err != nil {
				klog.Errorf("Failed to shutdown test server clearly: %v", err)
			}
		}
		if len(result.TmpDir) != 0 {
			os.RemoveAll(result.TmpDir)
		}
	}
	defer func() {
		if result.TearDownFn == nil {
			tearDown()
		}
	}()

	result.TmpDir, err = os.MkdirTemp("", "cloud-controller-manager")
	if err != nil {
		return result, fmt.Errorf("failed to create temp dir: %v", err)
	}

	// Do not run leader election.
	s, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		return TestServer{}, err
	}
	s.Generic.LeaderElection.LeaderElect = false

	cloudInitializer := func(logger klog.Logger, config *config.CompletedConfig) cloudprovider.Interface {
		capturedConfig = *config
		// send signal to indicate the capturedConfig has been properly set
		close(configDoneCh)
		cloudConfig := config.ComponentConfig.KubeCloudShared.CloudProvider
		cloud, err := cloudprovider.InitCloudProvider(cloudConfig.Name, cloudConfig.CloudConfigFile)
		if err != nil {
			panic(fmt.Errorf("Cloud provider could not be initialized: %v", err))
		}
		s.SecureServing.ServerCert.CertDirectory = result.TmpDir
		if cloud == nil {
			panic("Cloud provider is nil")
		}
		return cloud
	}
	fss := cliflag.NamedFlagSets{}
	command := app.NewCloudControllerManagerCommand(logger, s, cloudInitializer, constructors, aliases, fss, stopCh)

	commandArgs := []string{}
	listeners := []net.Listener{}
	disableSecure := false
	webhookServing := false
	for _, arg := range customFlags {
		// This block collects all custom flags other than secure serving flags,
		// which are added after creating a listener.
		if strings.HasPrefix(arg, "--secure-port=") || strings.HasPrefix(arg, "--cert-dir=") {
			if arg == "--secure-port=0" {
				commandArgs = append(commandArgs, arg)
				disableSecure = true
			}
		} else if strings.HasPrefix(arg, "--webhook-secure-port=") || strings.HasPrefix(arg, "--webhook-cert-dir=") {
			if arg == "--webhook-secure-port=0" {
				commandArgs = append(commandArgs, arg)
				webhookServing = false
			} else {
				webhookServing = true
			}
		} else {
			commandArgs = append(commandArgs, arg)
		}
	}

	if !disableSecure {
		listener, bindPort, err := createListenerOnFreePort()
		if err != nil {
			return result, fmt.Errorf("failed to create listener: %v", err)
		}
		listeners = append(listeners, listener)
		commandArgs = append(commandArgs, fmt.Sprintf("--secure-port=%d", bindPort))
		commandArgs = append(commandArgs, fmt.Sprintf("--cert-dir=%s", result.TmpDir))

		logger.Info("cloud-controller-manager will listen securely", "port", bindPort)
	}

	if webhookServing {
		listener, bindPort, err := createListenerOnFreePort()
		if err != nil {
			return result, fmt.Errorf("failed to create listener: %v", err)
		}
		listeners = append(listeners, listener)
		commandArgs = append(commandArgs, fmt.Sprintf("--webhook-secure-port=%d", bindPort))
		commandArgs = append(commandArgs, fmt.Sprintf("--webhook-cert-dir=%s", result.TmpDir))

		logger.Info("cloud-controller-manager (webhook endpoint) will listen securely", "port", bindPort)
	}

	for _, listener := range listeners {
		listener.Close()
	}

	errCh = make(chan error)
	go func() {
		defer close(errCh)

		command.SetArgs(commandArgs)
		if err := command.Execute(); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-configDoneCh:

	case err := <-errCh:
		return result, err
	}

	logger.Info("Waiting for /healthz to be ok...")
	client, err := kubernetes.NewForConfig(capturedConfig.LoopbackClientConfig)
	if err != nil {
		return result, fmt.Errorf("failed to create a client: %v", err)
	}
	err = wait.Poll(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		select {
		case err := <-errCh:
			return false, err
		default:
		}

		result := client.CoreV1().RESTClient().Get().AbsPath("/healthz").Do(context.TODO())
		status := 0
		result.StatusCode(&status)
		if status == 200 {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return result, fmt.Errorf("failed to wait for /healthz to return ok: %v", err)
	}

	// from here the caller must call tearDown
	result.LoopbackClientConfig = capturedConfig.LoopbackClientConfig
	result.Options = s
	result.Config = &capturedConfig
	result.TearDownFn = tearDown

	return result, nil
}

// StartTestServerOrDie calls StartTestServer panic if it does not succeed.
func StartTestServerOrDie(ctx context.Context, flags []string) *TestServer {
	result, err := StartTestServer(ctx, flags)
	if err == nil {
		return &result
	}

	panic(fmt.Errorf("failed to launch server: %v", err))
}

func createListenerOnFreePort() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, 0, err
	}

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}

	return ln, tcpAddr.Port, nil
}
