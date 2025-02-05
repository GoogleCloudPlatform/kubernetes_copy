/*
Copyright 2025 The Kubernetes Authors.

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
	"context"
	"strings"
	"testing"

	"k8s.io/kubernetes/test/utils/ktesting"
)

// TestHealthHandler tests /health?verbose with etcd overrides servers.
func TestHealthHandler(t *testing.T) {
	tCtx := ktesting.Init(t)
	// Setup multi etcd servers which is using `--etcd-servers-overrides`
	c, closeFn := multiEtcdSetup(tCtx, t)
	defer closeFn()

	// Test /healthz
	var statusCode int
	req := c.CoreV1().RESTClient().Get().AbsPath("/healthz")
	req.Param("verbose", "true")
	result := req.Do(context.TODO())
	result.StatusCode(&statusCode)
	if statusCode == 200 {
		t.Logf("Health check passed")
	} else {
		t.Errorf("Health check failed with status code: %d", statusCode)
	}
	raw, err := result.Raw()
	if err != nil {
		t.Errorf("Failed to get health check result: %v", err)
	}
	t.Logf("Health check result: %v", string(raw))
	// assert the raw contains `[+]etcd-override-0 ok`
	if !strings.Contains(string(raw), "[+]etcd-override-0 ok") {
		t.Errorf("Health check result does not contain etcd-override-0 ok")
	}
}
