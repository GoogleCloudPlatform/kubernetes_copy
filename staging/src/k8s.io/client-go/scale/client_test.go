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

package scale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakedisco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	fakerest "k8s.io/client-go/rest/fake"

	"github.com/stretchr/testify/assert"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/restmapper"
	coretesting "k8s.io/client-go/testing"
)

func bytesBody(bodyBytes []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(bodyBytes))
}

func defaultHeaders() http.Header {
	header := http.Header{}
	header.Set("Content-Type", runtime.ContentTypeJSON)
	return header
}

func fakeScaleClient(t *testing.T) (ScalesGetter, []schema.GroupResource) {
	fakeDiscoveryClient := &fakedisco.FakeDiscovery{Fake: &coretesting.Fake{}}
	fakeDiscoveryClient.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: corev1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod"},
				{Name: "replicationcontrollers", Namespaced: true, Kind: "ReplicationController"},
				{Name: "replicationcontrollers/scale", Namespaced: true, Kind: "Scale", Group: "autoscaling", Version: "v1"},
			},
		},
		{
			GroupVersion: extv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
				{Name: "replicasets/scale", Namespaced: true, Kind: "Scale"},
			},
		},
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
		{
			GroupVersion: appsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "statefulsets", Namespaced: true, Kind: "StatefulSet"},
				{Name: "statefulsets/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta1"},
			},
		},
		// test a resource that doesn't exist anywere to make sure we're not accidentally depending
		// on a static RESTMapper anywhere.
		{
			GroupVersion: "cheese.testing.k8s.io/v27alpha15",
			APIResources: []metav1.APIResource{
				{Name: "cheddars", Namespaced: true, Kind: "Cheddar"},
				{Name: "cheddars/scale", Namespaced: true, Kind: "Scale", Group: "extensions", Version: "v1beta1"},
			},
		},
	}

	restMapperRes, err := restmapper.GetAPIGroupResources(fakeDiscoveryClient)
	if err != nil {
		t.Fatalf("unexpected error while constructing resource list from fake discovery client: %v", err)
	}
	restMapper := restmapper.NewDiscoveryRESTMapper(restMapperRes)

	autoscalingScale := &autoscalingv1.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: autoscalingv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: autoscalingv1.ScaleSpec{Replicas: 10},
		Status: autoscalingv1.ScaleStatus{
			Replicas: 10,
			Selector: "foo=bar",
		},
	}
	extScale := &extv1beta1.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: extv1beta1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: extv1beta1.ScaleSpec{Replicas: 10},
		Status: extv1beta1.ScaleStatus{
			Replicas:       10,
			TargetSelector: "foo=bar",
		},
	}
	appsV1beta2Scale := &appsv1beta2.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: appsv1beta2.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: appsv1beta2.ScaleSpec{Replicas: 10},
		Status: appsv1beta2.ScaleStatus{
			Replicas:       10,
			TargetSelector: "foo=bar",
		},
	}
	appsV1beta1Scale := &appsv1beta1.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: appsv1beta1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: appsv1beta1.ScaleSpec{Replicas: 10},
		Status: appsv1beta1.ScaleStatus{
			Replicas:       10,
			TargetSelector: "foo=bar",
		},
	}

	resourcePaths := map[string]runtime.Object{
		"/api/v1/namespaces/default/replicationcontrollers/foo/scale":                  autoscalingScale,
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale":            extScale,
		"/apis/apps/v1beta1/namespaces/default/statefulsets/foo/scale":                 appsV1beta1Scale,
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale":                  appsV1beta2Scale,
		"/apis/cheese.testing.k8s.io/v27alpha15/namespaces/default/cheddars/foo/scale": extScale,
	}

	fakeReqHandler := func(req *http.Request) (*http.Response, error) {
		scale, isScalePath := resourcePaths[req.URL.Path]
		if !isScalePath {
			return nil, fmt.Errorf("unexpected request for URL %q with method %q", req.URL.String(), req.Method)
		}

		switch req.Method {
		case "GET":
			res, err := json.Marshal(scale)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: defaultHeaders(), Body: bytesBody(res)}, nil
		case "PUT":
			decoder := codecs.UniversalDeserializer()
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			newScale, newScaleGVK, err := decoder.Decode(body, nil, nil)
			if err != nil {
				return nil, fmt.Errorf("unexpected request body: %v", err)
			}
			if *newScaleGVK != scale.GetObjectKind().GroupVersionKind() {
				return nil, fmt.Errorf("unexpected scale API version %s (expected %s)", newScaleGVK.String(), scale.GetObjectKind().GroupVersionKind().String())
			}
			res, err := json.Marshal(newScale)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: defaultHeaders(), Body: bytesBody(res)}, nil
		case "PATCH":
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			originScale, err := json.Marshal(scale)
			if err != nil {
				return nil, err
			}
			var res []byte
			contentType := req.Header.Get("Content-Type")
			pt := types.PatchType(contentType)
			switch pt {
			case types.MergePatchType:
				res, err = jsonpatch.MergePatch(originScale, body)
				if err != nil {
					return nil, err
				}
			case types.JSONPatchType:
				patch, err := jsonpatch.DecodePatch(body)
				if err != nil {
					return nil, err
				}
				res, err = patch.Apply(originScale)
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("invalid patch type")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: defaultHeaders(), Body: bytesBody(res)}, nil
		default:
			return nil, fmt.Errorf("unexpected request for URL %q with method %q", req.URL.String(), req.Method)
		}
	}

	fakeClient := &fakerest.RESTClient{
		Client:               fakerest.CreateHTTPClient(fakeReqHandler),
		NegotiatedSerializer: codecs.WithoutConversion(),
		GroupVersion:         schema.GroupVersion{},
		VersionedAPIPath:     "/not/a/real/path",
	}

	resolver := NewDiscoveryScaleKindResolver(fakeDiscoveryClient)
	client := New(fakeClient, restMapper, dynamic.LegacyAPIPathResolverFunc, resolver)

	groupResources := []schema.GroupResource{
		{Group: corev1.GroupName, Resource: "replicationcontrollers"},
		{Group: extv1beta1.GroupName, Resource: "replicasets"},
		{Group: appsv1beta2.GroupName, Resource: "deployments"},
		{Group: "cheese.testing.k8s.io", Resource: "cheddars"},
	}

	return client, groupResources
}

func TestGetScale(t *testing.T) {
	scaleClient, groupResources := fakeScaleClient(t)
	expectedScale := &autoscalingv1.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: autoscalingv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: autoscalingv1.ScaleSpec{Replicas: 10},
		Status: autoscalingv1.ScaleStatus{
			Replicas: 10,
			Selector: "foo=bar",
		},
	}

	for _, groupResource := range groupResources {
		scale, err := scaleClient.Scales("default").Get(context.TODO(), groupResource, "foo", metav1.GetOptions{})
		if !assert.NoError(t, err, "should have been able to fetch a scale for %s", groupResource.String()) {
			continue
		}
		assert.NotNil(t, scale, "should have returned a non-nil scale for %s", groupResource.String())

		assert.Equal(t, expectedScale, scale, "should have returned the expected scale for %s", groupResource.String())
	}
}

func TestUpdateScale(t *testing.T) {
	scaleClient, groupResources := fakeScaleClient(t)
	expectedScale := &autoscalingv1.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: autoscalingv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: autoscalingv1.ScaleSpec{Replicas: 10},
		Status: autoscalingv1.ScaleStatus{
			Replicas: 10,
			Selector: "foo=bar",
		},
	}

	for _, groupResource := range groupResources {
		scale, err := scaleClient.Scales("default").Update(context.TODO(), groupResource, expectedScale, metav1.UpdateOptions{})
		if !assert.NoError(t, err, "should have been able to fetch a scale for %s", groupResource.String()) {
			continue
		}
		assert.NotNil(t, scale, "should have returned a non-nil scale for %s", groupResource.String())

		assert.Equal(t, expectedScale, scale, "should have returned the expected scale for %s", groupResource.String())
	}
}

func TestPatchScale(t *testing.T) {
	scaleClient, groupResources := fakeScaleClient(t)
	expectedScale := &autoscalingv1.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: autoscalingv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: autoscalingv1.ScaleSpec{Replicas: 5},
		Status: autoscalingv1.ScaleStatus{
			Replicas: 10,
			Selector: "foo=bar",
		},
	}
	gvrs := make([]schema.GroupVersionResource, 0, len(groupResources))
	for _, gr := range groupResources {
		switch gr.Group {
		case corev1.GroupName:
			gvrs = append(gvrs, gr.WithVersion(corev1.SchemeGroupVersion.Version))
		case extv1beta1.GroupName:
			gvrs = append(gvrs, gr.WithVersion(extv1beta1.SchemeGroupVersion.Version))
		case appsv1beta2.GroupName:
			gvrs = append(gvrs, gr.WithVersion(appsv1beta2.SchemeGroupVersion.Version))
		default:
			// Group cheese.testing.k8s.io
			gvrs = append(gvrs, gr.WithVersion("v27alpha15"))
		}
	}

	patch := []byte(`{"spec":{"replicas":5}}`)
	for _, gvr := range gvrs {
		scale, err := scaleClient.Scales("default").Patch(context.TODO(), gvr, "foo", types.MergePatchType, patch, metav1.PatchOptions{})
		if !assert.NoError(t, err, "should have been able to fetch a scale for %s", gvr.String()) {
			continue
		}
		assert.NotNil(t, scale, "should have returned a non-nil scale for %s", gvr.String())
		assert.Equal(t, expectedScale, scale, "should have returned the expected scale for %s", gvr.String())
	}

	patch = []byte(`[{"op":"replace","path":"/spec/replicas","value":5}]`)
	for _, gvr := range gvrs {
		scale, err := scaleClient.Scales("default").Patch(context.TODO(), gvr, "foo", types.JSONPatchType, patch, metav1.PatchOptions{})
		if !assert.NoError(t, err, "should have been able to fetch a scale for %s", gvr.String()) {
			continue
		}
		assert.NotNil(t, scale, "should have returned a non-nil scale for %s", gvr.String())
		assert.Equal(t, expectedScale, scale, "should have returned the expected scale for %s", gvr.String())
	}
}
