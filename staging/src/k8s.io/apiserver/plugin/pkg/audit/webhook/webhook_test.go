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

package webhook

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/wait"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/audit"
	v1 "k8s.io/client-go/tools/clientcmd/api/v1"
)

// newWebhookHandler returns a handler which receives webhook events and decodes the
// request body. The caller passes a callback which is called on each webhook POST.
// The object passed to cb is of the same type as list.
func newWebhookHandler(t *testing.T, list runtime.Object, cb func(events runtime.Object)) http.Handler {
	s := json.NewSerializerWithOptions(json.DefaultMetaFactory, audit.Scheme, audit.Scheme, json.SerializerOptions{})
	return &testWebhookHandler{
		t:          t,
		list:       list,
		onEvents:   cb,
		serializer: s,
	}
}

type testWebhookHandler struct {
	t *testing.T

	list     runtime.Object
	onEvents func(events runtime.Object)

	serializer runtime.Serializer
}

func (t *testWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := func() error {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("read webhook request body: %v", err)
		}

		obj, _, err := t.serializer.Decode(body, nil, t.list.DeepCopyObject())
		if err != nil {
			return fmt.Errorf("decode request body: %v", err)
		}
		if reflect.TypeOf(obj).Elem() != reflect.TypeOf(t.list).Elem() {
			return fmt.Errorf("expected %T, got %T", t.list, obj)
		}
		t.onEvents(obj)
		return nil
	}()

	if err == nil {
		io.WriteString(w, "{}")
		return
	}
	// In a goroutine, can't call Fatal.
	assert.NoError(t.t, err, "failed to read request body")
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func newWebhook(t *testing.T, endpoint string, groupVersion schema.GroupVersion) *backend {
	config := v1.Config{
		Clusters: []v1.NamedCluster{
			{Cluster: v1.Cluster{Server: endpoint, InsecureSkipTLSVerify: true}},
		},
	}
	f, err := os.CreateTemp("", "k8s_audit_webhook_test_")
	require.NoError(t, err, "creating temp file")

	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	// NOTE(ericchiang): Do we need to use a proper serializer?
	require.NoError(t, stdjson.NewEncoder(f).Encode(config), "writing kubeconfig")

	retryBackoff := wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   1.5,
		Jitter:   0.2,
		Steps:    5,
	}
	b, err := NewBackend(f.Name(), groupVersion, retryBackoff, nil)
	require.NoError(t, err, "initializing backend")

	return b.(*backend)
}

func TestWebhook(t *testing.T) {
	versions := []schema.GroupVersion{auditv1.SchemeGroupVersion}
	for _, version := range versions {
		gotEvents := false

		s := httptest.NewServer(newWebhookHandler(t, &auditv1.EventList{}, func(events runtime.Object) {
			gotEvents = true
		}))
		defer s.Close()

		backend := newWebhook(t, s.URL, auditv1.SchemeGroupVersion)

		// Ensure this doesn't return a serialization error.
		event := &auditinternal.Event{}
		require.NoErrorf(t, backend.processEvents(event), "failed to send events, apiVersion: %s", version)
		require.Truef(t, gotEvents, "no events received, apiVersion: %s", version)
	}
}
