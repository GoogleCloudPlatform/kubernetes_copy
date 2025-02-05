/*
Copyright 2022 The Kubernetes Authors.

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

package gcp

import (
	"errors"

	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

func init() {
	if err := rest.RegisterAuthProviderPlugin("gcp", newGCPAuthProvider); err != nil {
		//nolint:logcheck // Should not happen.
		klog.Fatalf("Failed to register gcp auth plugin: %v", err)
	}
}

func newGCPAuthProvider(_ string, _ map[string]string, _ rest.AuthProviderConfigPersister) (rest.AuthProvider, error) {
	return nil, errors.New(`The gcp auth plugin has been removed.
Please use the "gke-gcloud-auth-plugin" kubectl/client-go credential plugin instead.
See https://cloud.google.com/blog/products/containers-kubernetes/kubectl-auth-changes-in-gke for further details`)
}
