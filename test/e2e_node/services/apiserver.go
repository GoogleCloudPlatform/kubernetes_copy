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

package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	utiltesting "k8s.io/client-go/util/testing"

	"k8s.io/apiserver/pkg/storage/storagebackend"
	netutils "k8s.io/utils/net"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	apiserver "k8s.io/kubernetes/cmd/kube-apiserver/app"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	clusterIPRange = "10.0.0.1/24"
	// This key is for testing purposes only and is not considered secure.
	ecdsaPrivateKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIEZmTmUhuanLjPA2CLquXivuwBDHTt5XYwgIr/kA1LtRoAoGCCqGSM49
AwEHoUQDQgAEH6cuzP8XuD5wal6wf9M6xDljTOPLX2i8uIp/C/ASqiIGUeeKQtX0
/IR3qCXyThP/dbCiHrF3v1cuhBOHY8CLVg==
-----END EC PRIVATE KEY-----`
)

// APIServer is a server which manages apiserver.
type APIServer struct {
	storageConfig storagebackend.Config
	cancel        func(error)
}

// NewAPIServer creates an apiserver.
func NewAPIServer(storageConfig storagebackend.Config) *APIServer {
	return &APIServer{
		storageConfig: storageConfig,
	}
}

// Start starts the apiserver, returns when apiserver is ready.
// The background goroutine runs until the context is canceled
// or Stop is called, whether happens first.
func (a *APIServer) Start(ctx context.Context) error {
	const tokenFilePath = "known_tokens.csv"

	o := options.NewServerRunOptions()
	o.Etcd.StorageConfig = a.storageConfig
	_, ipnet, err := netutils.ParseCIDRSloppy(clusterIPRange)
	if err != nil {
		return err
	}
	if len(framework.TestContext.RuntimeConfig) > 0 {
		o.APIEnablement.RuntimeConfig = framework.TestContext.RuntimeConfig
	}
	o.SecureServing.BindAddress = netutils.ParseIPSloppy("127.0.0.1")
	o.ServiceClusterIPRanges = ipnet.String()
	o.AllowPrivileged = true
	if err := generateTokenFile(tokenFilePath); err != nil {
		return fmt.Errorf("failed to generate token file %s: %w", tokenFilePath, err)
	}
	o.Authentication.TokenFile.TokenFile = tokenFilePath
	o.Admission.GenericAdmission.DisablePlugins = []string{"ServiceAccount", "TaintNodesByCondition"}

	err = os.Mkdir("/tmp", 0755)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create temp dir failed: %w", err)
	}

	saSigningKeyFile, err := os.CreateTemp("/tmp", "insecure_test_key")
	if err != nil {
		return fmt.Errorf("create temp file failed: %w", err)
	}
	defer utiltesting.CloseAndRemove(&testing.T{}, saSigningKeyFile)
	if err = os.WriteFile(saSigningKeyFile.Name(), []byte(ecdsaPrivateKey), 0666); err != nil {
		return fmt.Errorf("write file %s failed: %w", saSigningKeyFile.Name(), err)
	}
	o.ServiceAccountSigningKeyFile = saSigningKeyFile.Name()
	o.Authentication.APIAudiences = []string{"https://foo.bar.example.com"}
	o.Authentication.ServiceAccounts.Issuers = []string{"https://foo.bar.example.com"}
	o.Authentication.ServiceAccounts.KeyFiles = []string{saSigningKeyFile.Name()}

	o.KubeletConfig.PreferredAddressTypes = []string{"InternalIP"}

	ctx, cancel := context.WithCancelCause(ctx)
	a.cancel = cancel
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		defer cancel(errors.New("shutting down")) // Calling Stop is optional, but cancel always should be invoked.
		completedOptions, err := o.Complete(ctx)
		if err != nil {
			errCh <- fmt.Errorf("set apiserver default options error: %w", err)
			return
		}
		if errs := completedOptions.Validate(); len(errs) != 0 {
			errCh <- fmt.Errorf("failed to validate ServerRunOptions: %v", utilerrors.NewAggregate(errs))
			return
		}

		err = apiserver.Run(ctx, completedOptions)
		if err != nil {
			errCh <- fmt.Errorf("run apiserver error: %w", err)
			return
		}
	}()

	err = readinessCheck("apiserver", []string{getAPIServerHealthCheckURL()}, errCh)
	if err != nil {
		return err
	}
	return nil
}

// Stop stops the apiserver. Does not block.
func (a *APIServer) Stop() error {
	// nil when Start has never been called.
	if a.cancel != nil {
		a.cancel(errors.New("stopping API server"))
	}
	return nil
}

const apiserverName = "apiserver"

// Name returns the name of APIServer.
func (a *APIServer) Name() string {
	return apiserverName
}

func getAPIServerClientURL() string {
	return framework.TestContext.Host
}

func getAPIServerHealthCheckURL() string {
	return framework.TestContext.Host + "/healthz"
}

func generateTokenFile(tokenFilePath string) error {
	tokenFile := fmt.Sprintf("%s,kubelet,uid,system:masters\n", framework.TestContext.BearerToken)
	return os.WriteFile(tokenFilePath, []byte(tokenFile), 0644)
}
