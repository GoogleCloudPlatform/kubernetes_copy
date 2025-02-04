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

package storage

import (
	"mime"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	apimachineryversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/apis/example"
	exampleinstall "k8s.io/apiserver/pkg/apis/example/install"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/apis/example2"
	example2install "k8s.io/apiserver/pkg/apis/example2/install"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	basecompatibility "k8s.io/component-base/compatibility"
)

var (
	v1GroupVersion = schema.GroupVersion{Group: "", Version: "v1"}

	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	scheme.AddUnversionedTypes(v1GroupVersion,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)

	exampleinstall.Install(scheme)
	example2install.Install(scheme)
}

type fakeNegotiater struct {
	serializer, streamSerializer runtime.Serializer
	framer                       runtime.Framer
	types, streamTypes           []string
}

func (n *fakeNegotiater) SupportedMediaTypes() []runtime.SerializerInfo {
	var out []runtime.SerializerInfo
	for _, s := range n.types {
		mediaType, _, err := mime.ParseMediaType(s)
		if err != nil {
			panic(err)
		}
		parts := strings.SplitN(mediaType, "/", 2)
		if len(parts) == 1 {
			// this is an error on the server side
			parts = append(parts, "")
		}

		info := runtime.SerializerInfo{
			Serializer:       n.serializer,
			MediaType:        s,
			MediaTypeType:    parts[0],
			MediaTypeSubType: parts[1],
			EncodesAsText:    true,
		}

		for _, t := range n.streamTypes {
			if t == s {
				info.StreamSerializer = &runtime.StreamSerializerInfo{
					EncodesAsText: true,
					Framer:        n.framer,
					Serializer:    n.streamSerializer,
				}
			}
		}
		out = append(out, info)
	}
	return out
}

func (n *fakeNegotiater) UniversalDeserializer() runtime.Decoder {
	return n.serializer
}

func (n *fakeNegotiater) EncoderForVersion(serializer runtime.Encoder, gv runtime.GroupVersioner) runtime.Encoder {
	return n.serializer
}

func (n *fakeNegotiater) DecoderToVersion(serializer runtime.Decoder, gv runtime.GroupVersioner) runtime.Decoder {
	return n.serializer
}

type newStorageCodecFn func(opts StorageCodecConfig) (codec runtime.Codec, encodeVersioner runtime.GroupVersioner, err error)
type newStorageCodecRecorder struct {
	opt StorageCodecConfig
}

func (c *newStorageCodecRecorder) Decorators(f newStorageCodecFn) newStorageCodecFn {
	return func(opts StorageCodecConfig) (codec runtime.Codec, encodeVersioner runtime.GroupVersioner, err error) {
		c.opt = opts
		return f(opts)
	}
}

func TestConfigurableStorageFactory(t *testing.T) {
	var emptyResource schema.GroupResource

	testCases := []struct {
		name                  string
		enabledResources      []schema.GroupVersionResource
		overrideResource      schema.GroupResource
		cohabitatingResources []schema.GroupResource
		resourceForConfig     schema.GroupResource
		exampleForConfig      runtime.Object
		wantStorageVersion    schema.GroupVersion
	}{
		{
			name: "config resource is primary cohabitating resources",
			enabledResources: []schema.GroupVersionResource{
				example.Resource("replicasets").WithVersion("v1"),
				example2.Resource("replicasets").WithVersion("v1"),
			},
			cohabitatingResources: []schema.GroupResource{example.Resource("replicasets"), example2.Resource("replicasets")},
			resourceForConfig:     example.Resource("replicasets"),
			exampleForConfig:      &example.ReplicaSet{},
			wantStorageVersion:    schema.GroupVersion{Group: example.GroupName, Version: "v1"},
		},
		{
			name: "config resource is secondary cohabitating resources",
			enabledResources: []schema.GroupVersionResource{
				example.Resource("replicasets").WithVersion("v1"),
				example2.Resource("replicasets").WithVersion("v1"),
			},
			cohabitatingResources: []schema.GroupResource{example.Resource("replicasets"), example2.Resource("replicasets")},
			resourceForConfig:     example2.Resource("replicasets"),
			exampleForConfig:      &example.ReplicaSet{},
			wantStorageVersion:    schema.GroupVersion{Group: example.GroupName, Version: "v1"},
		},
		{
			name: "config resource is primary cohabitating resources and not enabled",
			enabledResources: []schema.GroupVersionResource{
				// example.Resource("replicasets").WithVersion("v1"), // <- disabled
				example2.Resource("replicasets").WithVersion("v1"),
			},
			cohabitatingResources: []schema.GroupResource{example.Resource("replicasets"), example2.Resource("replicasets")},
			resourceForConfig:     example.Resource("replicasets"),
			exampleForConfig:      &example.ReplicaSet{},
			wantStorageVersion:    schema.GroupVersion{Group: example2.GroupName, Version: "v1"},
		},
		{
			name: "config resource is secondary cohabitating resources and not enabled",
			enabledResources: []schema.GroupVersionResource{
				example.Resource("replicasets").WithVersion("v1"),
				// example2.Resource("replicasets").WithVersion("v1"),  // <- disabled
			},
			cohabitatingResources: []schema.GroupResource{example.Resource("replicasets"), example2.Resource("replicasets")},
			resourceForConfig:     example2.Resource("replicasets"),
			exampleForConfig:      &example.ReplicaSet{},
			wantStorageVersion:    schema.GroupVersion{Group: example.GroupName, Version: "v1"},
		},
		{
			name: "override config for one resource of group",
			enabledResources: []schema.GroupVersionResource{
				example.Resource("replicasets").WithVersion("v1"),
				example2.Resource("replicasets").WithVersion("v1"),
			},
			overrideResource:   example.Resource("replicasets"),
			resourceForConfig:  example.Resource("replicasets"),
			exampleForConfig:   &example.ReplicaSet{},
			wantStorageVersion: schema.GroupVersion{Group: example.GroupName, Version: "v1"},
		},
		{
			name: "override config for all resource of group",
			enabledResources: []schema.GroupVersionResource{
				example.Resource("replicasets").WithVersion("v1"),
				example2.Resource("replicasets").WithVersion("v1"),
			},
			cohabitatingResources: []schema.GroupResource{example.Resource("replicasets"), example2.Resource("replicasets")},
			overrideResource:      example.Resource("*"),
			resourceForConfig:     example.Resource("replicasets"),
			exampleForConfig:      &example.ReplicaSet{},
			wantStorageVersion:    schema.GroupVersion{Group: example.GroupName, Version: "v1"},
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			ns := &fakeNegotiater{types: []string{"test/test"}}
			resourceConfig := NewResourceConfig()
			resourceConfig.EnableResources(test.enabledResources...)

			f := NewDefaultStorageFactory(storagebackend.Config{}, "test/test", ns, NewDefaultResourceEncodingConfig(scheme), resourceConfig, nil)

			encoderChainCalled := false
			recorder := newStorageCodecRecorder{}

			f.newStorageCodecFn = recorder.Decorators(f.newStorageCodecFn)
			f.AddCohabitatingResources(test.cohabitatingResources...)

			if test.overrideResource != emptyResource {
				testEncoderChain := func(e runtime.Encoder) runtime.Encoder {
					encoderChainCalled = true
					return e
				}

				f.SetEtcdLocation(test.overrideResource, []string{"/server2"})
				f.AddSerializationChains(testEncoderChain, nil, test.overrideResource)
				f.SetEtcdPrefix(test.overrideResource, "/prefix_for_test")
			}

			config, err := f.NewConfig(test.resourceForConfig, test.exampleForConfig)
			if err != nil {
				t.Fatal(err)
			}

			// check override resources config
			if test.overrideResource != emptyResource {
				if config.Prefix != "/prefix_for_test" || !reflect.DeepEqual(config.Transport.ServerList, []string{"/server2"}) {
					t.Errorf("unexpected config %#v", config)
				}
				if !encoderChainCalled {
					t.Errorf("expected encoder chain to be called")
				}
			}

			// check cohabitating resources config
			if recorder.opt.StorageVersion != test.wantStorageVersion {
				t.Errorf("unexpected encoding version %#v, but expected %#v", recorder.opt.StorageVersion, test.wantStorageVersion)
			}
		})
	}
}

func TestUpdateEtcdOverrides(t *testing.T) {
	exampleinstall.Install(scheme)

	testCases := []struct {
		resource schema.GroupResource
		servers  []string
	}{
		{
			resource: schema.GroupResource{Group: example.GroupName, Resource: "resource"},
			servers:  []string{"http://127.0.0.1:10000"},
		},
		{
			resource: schema.GroupResource{Group: example.GroupName, Resource: "resource"},
			servers:  []string{"http://127.0.0.1:10000", "http://127.0.0.1:20000"},
		},
		{
			resource: schema.GroupResource{Group: example.GroupName, Resource: "resource"},
			servers:  []string{"http://127.0.0.1:10000"},
		},
	}

	defaultEtcdLocation := []string{"http://127.0.0.1"}
	for i, test := range testCases {
		defaultConfig := storagebackend.Config{
			Prefix: "/registry",
			Transport: storagebackend.TransportConfig{
				ServerList: defaultEtcdLocation,
			},
		}
		storageFactory := NewDefaultStorageFactory(defaultConfig, "", codecs, NewDefaultResourceEncodingConfig(scheme), NewResourceConfig(), nil)
		storageFactory.SetEtcdLocation(test.resource, test.servers)

		var err error
		config, err := storageFactory.NewConfig(test.resource, nil)
		if err != nil {
			t.Errorf("%d: unexpected error %v", i, err)
			continue
		}
		if !reflect.DeepEqual(config.Transport.ServerList, test.servers) {
			t.Errorf("%d: expected %v, got %v", i, test.servers, config.Transport.ServerList)
			continue
		}

		config, err = storageFactory.NewConfig(schema.GroupResource{Group: examplev1.GroupName, Resource: "unlikely"}, nil)
		if err != nil {
			t.Errorf("%d: unexpected error %v", i, err)
			continue
		}
		if !reflect.DeepEqual(config.Transport.ServerList, defaultEtcdLocation) {
			t.Errorf("%d: expected %v, got %v", i, defaultEtcdLocation, config.Transport.ServerList)
			continue
		}

	}
}

func TestConfigs(t *testing.T) {
	exampleinstall.Install(scheme)
	defaultEtcdLocations := []string{"http://127.0.0.1", "http://127.0.0.2"}

	testCases := []struct {
		resource    *schema.GroupResource
		servers     []string
		wantConfigs []storagebackend.Config
	}{
		{
			wantConfigs: []storagebackend.Config{
				{Transport: storagebackend.TransportConfig{ServerList: defaultEtcdLocations}, Prefix: "/registry"},
			},
		},
		{
			resource: &schema.GroupResource{Group: example.GroupName, Resource: "resource"},
			servers:  []string{},
			wantConfigs: []storagebackend.Config{
				{Transport: storagebackend.TransportConfig{ServerList: defaultEtcdLocations}, Prefix: "/registry"},
			},
		},
		{
			resource: &schema.GroupResource{Group: example.GroupName, Resource: "resource"},
			servers:  []string{"http://127.0.0.1:10000"},
			wantConfigs: []storagebackend.Config{
				{Transport: storagebackend.TransportConfig{ServerList: defaultEtcdLocations}, Prefix: "/registry"},
				{Transport: storagebackend.TransportConfig{ServerList: []string{"http://127.0.0.1:10000"}}, Prefix: "/registry"},
			},
		},
		{
			resource: &schema.GroupResource{Group: example.GroupName, Resource: "resource"},
			servers:  []string{"http://127.0.0.1:10000", "https://127.0.0.1", "http://127.0.0.2"},
			wantConfigs: []storagebackend.Config{
				{Transport: storagebackend.TransportConfig{ServerList: defaultEtcdLocations}, Prefix: "/registry"},
				{Transport: storagebackend.TransportConfig{ServerList: []string{"http://127.0.0.1:10000", "https://127.0.0.1", "http://127.0.0.2"}}, Prefix: "/registry"},
			},
		},
	}

	for i, test := range testCases {
		defaultConfig := storagebackend.Config{
			Prefix: "/registry",
			Transport: storagebackend.TransportConfig{
				ServerList: defaultEtcdLocations,
			},
		}
		storageFactory := NewDefaultStorageFactory(defaultConfig, "", codecs, NewDefaultResourceEncodingConfig(scheme), NewResourceConfig(), nil)
		if test.resource != nil {
			storageFactory.SetEtcdLocation(*test.resource, test.servers)
		}

		got := storageFactory.Configs()
		if !reflect.DeepEqual(test.wantConfigs, got) {
			t.Errorf("%d: expected %v, got %v", i, test.wantConfigs, got)
			continue
		}
	}
}

var introducedLifecycles = map[reflect.Type]*apimachineryversion.Version{}
var removedLifecycles = map[reflect.Type]*apimachineryversion.Version{}

type fakeLifecycler[T, V any] struct {
	metav1.TypeMeta
	metav1.ObjectMeta
}

type removedLifecycler[T, V any] struct {
	fakeLifecycler[T, V]
}

func (f *fakeLifecycler[T, V]) GetObjectKind() schema.ObjectKind { return f }
func (f *fakeLifecycler[T, V]) DeepCopyObject() runtime.Object   { return f }
func (f *fakeLifecycler[T, V]) APILifecycleIntroduced() (major, minor int) {
	if introduced, ok := introducedLifecycles[reflect.TypeOf(f)]; ok {
		return int(introduced.Major()), int(introduced.Minor())
	}
	panic("no lifecycle version set")
}
func (f *removedLifecycler[T, V]) APILifecycleRemoved() (major, minor int) {
	if removed, ok := removedLifecycles[reflect.TypeOf(f)]; ok {
		return int(removed.Major()), int(removed.Minor())
	}
	panic("no lifecycle version set")
}

func registerFakeLifecycle[T, V any](sch *runtime.Scheme, group, introduced, removed string) {
	f := fakeLifecycler[T, V]{}

	introducedLifecycles[reflect.TypeOf(&f)] = apimachineryversion.MustParseSemantic(introduced)

	var res runtime.Object
	if removed != "" {
		removedLifecycles[reflect.TypeOf(&f)] = apimachineryversion.MustParseSemantic(removed)
		res = &removedLifecycler[T, V]{fakeLifecycler: f}
	} else {
		res = &f
	}

	var v V
	var t T
	sch.AddKnownTypeWithName(
		schema.GroupVersionKind{
			Group:   group,
			Version: strings.ToLower(reflect.TypeOf(v).Name()),
			Kind:    reflect.TypeOf(t).Name(),
		},
		res,
	)

	// Also ensure internal version is registered
	// If it is registertd multiple times, it will ignore subsequent registrations
	internalInstance := &fakeLifecycler[T, struct{}]{}
	sch.AddKnownTypeWithName(
		schema.GroupVersionKind{
			Group:   group,
			Version: runtime.APIVersionInternal,
			Kind:    reflect.TypeOf(t).Name(),
		},
		internalInstance,
	)
}

func TestStorageFactoryCompatibilityVersion(t *testing.T) {
	// Creates a scheme with stub types for unit test
	sch := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(sch)

	type Internal = struct{}
	type V1beta1 struct{}
	type V1beta2 struct{}
	type V1beta3 struct{}
	type V1 struct{}

	type Pod struct{}
	type FlowSchema struct{}
	type ValidatingAdmisisonPolicy struct{}
	type CronJob struct{}

	// Order dictates priority order
	registerFakeLifecycle[FlowSchema, V1](sch, "flowcontrol.apiserver.k8s.io", "1.29.0", "")
	registerFakeLifecycle[FlowSchema, V1beta3](sch, "flowcontrol.apiserver.k8s.io", "1.26.0", "1.32.0")
	registerFakeLifecycle[FlowSchema, V1beta2](sch, "flowcontrol.apiserver.k8s.io", "1.23.0", "1.29.0")
	registerFakeLifecycle[FlowSchema, V1beta1](sch, "flowcontrol.apiserver.k8s.io", "1.20.0", "1.26.0")
	registerFakeLifecycle[CronJob, V1](sch, "batch", "1.21.0", "")
	registerFakeLifecycle[CronJob, V1beta1](sch, "batch", "1.8.0", "1.21.0")
	registerFakeLifecycle[ValidatingAdmisisonPolicy, V1](sch, "admissionregistration.k8s.io", "1.30.0", "")
	registerFakeLifecycle[ValidatingAdmisisonPolicy, V1beta1](sch, "admissionregistration.k8s.io", "1.28.0", "1.34.0")
	registerFakeLifecycle[Pod, V1](sch, "", "1.31.0", "")

	// FlowSchema
	//   - v1beta1: 1.20.0 - 1.23.0
	//   - v1beta2: 1.23.0 - 1.26.0
	//   - v1beta3: 1.26.0 - 1.30.0
	//   - v1: 1.29.0+
	// CronJob
	//	 - v1beta1: 1.8.0 - 1.21.0
	//	 - v1: 1.21.0+
	// ValidatingAdmissionPolicy
	//	 - v1beta1: 1.28.0 - 1.31.0
	//	 - v1: 1.30.0+

	testcases := []struct {
		effectiveVersion string
		example          runtime.Object
		expectedVersion  schema.GroupVersion
	}{
		{
			// Basic case. Beta version for long time
			effectiveVersion: "1.14.0",
			example:          &fakeLifecycler[CronJob, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "batch", Version: "v1beta1"},
		},
		{
			// Basic case. Beta version for long time
			effectiveVersion: "1.20.0",
			example:          &fakeLifecycler[CronJob, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "batch", Version: "v1beta1"},
		},
		{
			// Basic case. GA version for long time
			effectiveVersion: "1.28.0",
			example:          &fakeLifecycler[CronJob, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "batch", Version: "v1"},
		},
		{
			// Basic core/v1
			effectiveVersion: "1.31.0",
			example:          &fakeLifecycler[Pod, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "", Version: "v1"},
		},
		{
			// Corner case: 1.1.0 has no flowcontrol. Options are to error
			// out or to use the latest version. This test assumes the latter.
			effectiveVersion: "1.1.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1"},
		},
		{
			effectiveVersion: "1.21.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta1"},
		},
		{
			// v2Beta1 introduced this version, but minCompatibility should
			// force v1beta1
			effectiveVersion: "1.23.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta1"},
		},
		{
			effectiveVersion: "1.24.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2"},
		},
		{
			effectiveVersion: "1.26.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2"},
		},
		{
			effectiveVersion: "1.27.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3"},
		},
		{
			// GA API introduced 1.29 but must keep storing in v1beta3 for downgrades
			effectiveVersion: "1.29.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3"},
		},
		{
			// Version after GA api is introduced
			effectiveVersion: "1.30.0",
			example:          &fakeLifecycler[FlowSchema, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "flowcontrol.apiserver.k8s.io", Version: "v1"},
		},
		{
			effectiveVersion: "1.30.0",
			example:          &fakeLifecycler[ValidatingAdmisisonPolicy, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "admissionregistration.k8s.io", Version: "v1beta1"},
		},
		{
			effectiveVersion: "1.31.0",
			example:          &fakeLifecycler[ValidatingAdmisisonPolicy, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "admissionregistration.k8s.io", Version: "v1"},
		},
		{
			effectiveVersion: "1.29.0",
			example:          &fakeLifecycler[ValidatingAdmisisonPolicy, Internal]{},
			expectedVersion:  schema.GroupVersion{Group: "admissionregistration.k8s.io", Version: "v1beta1"},
		},
	}

	for _, tc := range testcases {
		gvks, _, err := sch.ObjectKinds(tc.example)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		gvk := gvks[0]
		t.Run(gvk.GroupKind().String()+"@"+tc.effectiveVersion, func(t *testing.T) {
			config := NewDefaultResourceEncodingConfigForEffectiveVersion(sch, basecompatibility.NewEffectiveVersionFromString(tc.effectiveVersion, "", ""))
			f := NewDefaultStorageFactory(
				storagebackend.Config{},
				"",
				codecs,
				config,
				NewResourceConfig(),
				nil)

			cfg, err := f.NewConfig(schema.GroupResource{
				Group:    gvk.Group,
				Resource: gvk.Kind, // doesnt really matter here
			}, tc.example)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gvks, _, err := sch.ObjectKinds(tc.example)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			expectEncodeVersioner := runtime.NewMultiGroupVersioner(tc.expectedVersion,
				schema.GroupKind{
					Group: gvks[0].Group,
				}, schema.GroupKind{
					Group: gvks[0].Group,
				})
			if cfg.EncodeVersioner.Identifier() != expectEncodeVersioner.Identifier() {
				t.Errorf("expected %v, got %v", expectEncodeVersioner, cfg.EncodeVersioner)
			}
		})
	}
}
