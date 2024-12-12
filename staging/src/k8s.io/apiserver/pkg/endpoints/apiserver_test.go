/*
Copyright 2014 The Kubernetes Authors.

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

package endpoints

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emicklei/go-restful/v3"
	"github.com/google/go-cmp/cmp"

	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/admission"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/apis/example"
	examplefuzzer "k8s.io/apiserver/pkg/apis/example/fuzzer"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/audit"
	auditpolicy "k8s.io/apiserver/pkg/audit/policy"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	genericapitesting "k8s.io/apiserver/pkg/endpoints/testing"
	"k8s.io/apiserver/pkg/registry/rest"
)

type alwaysMutatingDeny struct{}

func (alwaysMutatingDeny) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	return admission.NewForbidden(a, errors.New("Mutating admission control is denying all modifications"))
}

func (alwaysMutatingDeny) Handles(operation admission.Operation) bool {
	return true
}

var _ admission.MutationInterface = &alwaysMutatingDeny{}

type alwaysValidatingDeny struct{}

func (alwaysValidatingDeny) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	return admission.NewForbidden(a, errors.New("Validating admission control is denying all modifications"))
}

func (alwaysValidatingDeny) Handles(operation admission.Operation) bool {
	return true
}

var _ admission.ValidationInterface = &alwaysValidatingDeny{}

// This creates fake API versions, similar to api/latest.go.
var testAPIGroup = "test.group"
var testAPIGroup2 = "test.group2"
var testInternalGroupVersion = schema.GroupVersion{Group: testAPIGroup, Version: runtime.APIVersionInternal}
var testGroupVersion = schema.GroupVersion{Group: testAPIGroup, Version: "version"}
var newGroupVersion = schema.GroupVersion{Group: testAPIGroup, Version: "version2"}
var testGroup2Version = schema.GroupVersion{Group: testAPIGroup2, Version: "version"}
var testInternalGroup2Version = schema.GroupVersion{Group: testAPIGroup2, Version: runtime.APIVersionInternal}
var prefix = "apis"

var grouplessGroupVersion = schema.GroupVersion{Group: "", Version: "v1"}
var grouplessInternalGroupVersion = schema.GroupVersion{Group: "", Version: runtime.APIVersionInternal}
var grouplessPrefix = "api"

var groupVersions = []schema.GroupVersion{grouplessGroupVersion, testGroupVersion, newGroupVersion}

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

var codec = codecs.LegacyCodec(groupVersions...)
var testCodec = codecs.LegacyCodec(testGroupVersion)
var newCodec = codecs.LegacyCodec(newGroupVersion)
var parameterCodec = runtime.NewParameterCodec(scheme)

var accessor = meta.NewAccessor()
var namer runtime.Namer = accessor
var admissionControl admission.Interface

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)

	// unnamed core group
	scheme.AddUnversionedTypes(grouplessGroupVersion, &metav1.Status{})
	metav1.AddToGroupVersion(scheme, grouplessGroupVersion)

	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))
}

func addGrouplessTypes() {
	scheme.AddKnownTypes(grouplessGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{}, &metav1.ListOptions{},
		&metav1.DeleteOptions{}, &genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{})
	scheme.AddKnownTypes(grouplessInternalGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{})

	utilruntime.Must(genericapitesting.RegisterConversions(scheme))
}

func addTestTypes() {
	scheme.AddKnownTypes(testGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&metav1.DeleteOptions{}, &genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{},
		&genericapitesting.SimpleXGSubresource{})
	scheme.AddKnownTypes(testGroupVersion, &examplev1.Pod{})
	scheme.AddKnownTypes(testInternalGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{},
		&genericapitesting.SimpleXGSubresource{})
	scheme.AddKnownTypes(testInternalGroupVersion, &example.Pod{})
	// Register SimpleXGSubresource in both testGroupVersion and testGroup2Version, and also their
	// their corresponding internal versions, to verify that the desired group version object is
	// served in the tests.
	scheme.AddKnownTypes(testGroup2Version, &genericapitesting.SimpleXGSubresource{})
	scheme.AddKnownTypes(testInternalGroup2Version, &genericapitesting.SimpleXGSubresource{})
	metav1.AddToGroupVersion(scheme, testGroupVersion)

	utilruntime.Must(genericapitesting.RegisterConversions(scheme))
}

func addNewTestTypes() {
	scheme.AddKnownTypes(newGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&metav1.DeleteOptions{}, &genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{},
		&examplev1.Pod{},
	)
	metav1.AddToGroupVersion(scheme, newGroupVersion)

	utilruntime.Must(genericapitesting.RegisterConversions(scheme))
}

func init() {
	// Certain API objects are returned regardless of the contents of storage:
	// api.Status is returned in errors

	addGrouplessTypes()
	addTestTypes()
	addNewTestTypes()

	scheme.AddFieldLabelConversionFunc(grouplessGroupVersion.WithKind("Simple"),
		func(label, value string) (string, string, error) {
			return label, value, nil
		},
	)
	scheme.AddFieldLabelConversionFunc(testGroupVersion.WithKind("Simple"),
		func(label, value string) (string, string, error) {
			return label, value, nil
		},
	)
	scheme.AddFieldLabelConversionFunc(newGroupVersion.WithKind("Simple"),
		func(label, value string) (string, string, error) {
			return label, value, nil
		},
	)
}

// defaultAPIServer exposes nested objects for testability.
type defaultAPIServer struct {
	http.Handler
	container *restful.Container
}

func handleWithWarnings(storage map[string]rest.Storage) http.Handler {
	return genericapifilters.WithWarningRecorder(handle(storage))
}

// uses the default settings
func handle(storage map[string]rest.Storage) http.Handler {
	return handleInternal(storage, admissionControl, nil)
}

func handleInternal(storage map[string]rest.Storage, admissionControl admission.Interface, auditSink audit.Sink) http.Handler {
	container := restful.NewContainer()
	container.Router(restful.CurlyRouter{})
	mux := container.ServeMux

	template := APIGroupVersion{
		Storage: storage,

		Creater:         scheme,
		Convertor:       scheme,
		TypeConverter:   managedfields.NewDeducedTypeConverter(),
		UnsafeConvertor: runtime.UnsafeObjectConvertor(scheme),
		Defaulter:       scheme,
		Typer:           scheme,
		Namer:           namer,

		EquivalentResourceRegistry: runtime.NewEquivalentResourceRegistry(),

		ParameterCodec: parameterCodec,

		Admit: admissionControl,
	}

	// groupless v1 version
	{
		group := template
		group.Root = "/" + grouplessPrefix
		group.GroupVersion = grouplessGroupVersion
		group.OptionsExternalVersion = &grouplessGroupVersion
		group.Serializer = codecs
		if _, _, err := (&group).InstallREST(container); err != nil {
			panic(fmt.Sprintf("unable to install container %s: %v", group.GroupVersion, err))
		}
	}

	// group version 1
	{
		group := template
		group.Root = "/" + prefix
		group.GroupVersion = testGroupVersion
		group.OptionsExternalVersion = &testGroupVersion
		group.Serializer = codecs
		if _, _, err := (&group).InstallREST(container); err != nil {
			panic(fmt.Sprintf("unable to install container %s: %v", group.GroupVersion, err))
		}
	}

	// group version 2
	{
		group := template
		group.Root = "/" + prefix
		group.GroupVersion = newGroupVersion
		group.OptionsExternalVersion = &newGroupVersion
		group.Serializer = codecs
		if _, _, err := (&group).InstallREST(container); err != nil {
			panic(fmt.Sprintf("unable to install container %s: %v", group.GroupVersion, err))
		}
	}
	longRunningCheck := func(r *http.Request, requestInfo *request.RequestInfo) bool {
		// simplified long-running check
		return requestInfo.Verb == "watch" || requestInfo.Verb == "proxy"
	}
	fakeRuleEvaluator := auditpolicy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	handler := genericapifilters.WithAudit(mux, auditSink, fakeRuleEvaluator, longRunningCheck)
	handler = genericapifilters.WithRequestDeadline(handler, auditSink, fakeRuleEvaluator, longRunningCheck, codecs, 60*time.Second)
	handler = genericapifilters.WithRequestInfo(handler, testRequestInfoResolver())
	handler = genericapifilters.WithAuditInit(handler)

	return &defaultAPIServer{handler, container}
}

func testRequestInfoResolver() *request.RequestInfoFactory {
	return &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
}

func TestSimpleSetupRight(t *testing.T) {
	s := &genericapitesting.Simple{ObjectMeta: metav1.ObjectMeta{Name: "aName"}}
	wire, err := runtime.Encode(codec, s)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := runtime.Decode(codec, wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, s2) {
		t.Fatalf("encode/decode broken:\n%#v\n%#v\n", s, s2)
	}
}

func TestSimpleOptionsSetupRight(t *testing.T) {
	s := &genericapitesting.SimpleGetOptions{}
	wire, err := runtime.Encode(codec, s)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := runtime.Decode(codec, wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, s2) {
		t.Fatalf("encode/decode broken:\n%#v\n%#v\n", s, s2)
	}
}

type SimpleRESTStorage struct {
	lock sync.Mutex

	errors map[string]error
	list   []genericapitesting.Simple
	item   genericapitesting.Simple

	updated *genericapitesting.Simple
	created *genericapitesting.Simple

	stream *SimpleStream

	deleted       string
	deleteOptions *metav1.DeleteOptions

	actualNamespace  string
	namespacePresent bool

	// These are set when Watch is called
	fakeWatch                  *watch.FakeWatcher
	requestedLabelSelector     labels.Selector
	requestedFieldSelector     fields.Selector
	requestedResourceVersion   string
	requestedResourceNamespace string

	expectedResourceNamespace string

	// If non-nil, called inside the WorkFunc when answering update, delete, create.
	// obj receives the original input to the update, delete, or create call.
	injectedFunction func(obj runtime.Object) (returnObj runtime.Object, err error)
}

func (storage *SimpleRESTStorage) NamespaceScoped() bool {
	return true
}

func (storage *SimpleRESTStorage) ConvertToTable(ctx context.Context, obj runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	return rest.NewDefaultTableConvertor(schema.GroupResource{Resource: "simple"}).ConvertToTable(ctx, obj, tableOptions)
}

func (storate *SimpleRESTStorage) GetSingularName() string {
	return "simple"
}

func (storage *SimpleRESTStorage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	storage.checkContext(ctx)
	result := &genericapitesting.SimpleList{
		ListMeta: metav1.ListMeta{
			ResourceVersion: "10",
		},
		Items: storage.list,
	}
	storage.requestedLabelSelector = labels.Everything()
	if options != nil && options.LabelSelector != nil {
		storage.requestedLabelSelector = options.LabelSelector
	}
	storage.requestedFieldSelector = fields.Everything()
	if options != nil && options.FieldSelector != nil {
		storage.requestedFieldSelector = options.FieldSelector
	}
	return result, storage.errors["list"]
}

type SimpleStream struct {
	version     string
	accept      string
	contentType string
	err         error

	io.Reader
	closed bool
}

func (s *SimpleStream) Close() error {
	s.closed = true
	return nil
}

func (s *SimpleStream) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
func (s *SimpleStream) DeepCopyObject() runtime.Object {
	panic("SimpleStream does not support DeepCopy")
}

func (s *SimpleStream) InputStream(_ context.Context, version, accept string) (io.ReadCloser, bool, string, error) {
	s.version = version
	s.accept = accept
	return s, false, s.contentType, s.err
}

type OutputConnect struct {
	response string
}

func (h *OutputConnect) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Write([]byte(h.response))
}

func (storage *SimpleRESTStorage) Get(ctx context.Context, id string, options *metav1.GetOptions) (runtime.Object, error) {
	storage.checkContext(ctx)
	if id == "binary" {
		return storage.stream, storage.errors["get"]
	}
	return storage.item.DeepCopy(), storage.errors["get"]
}

func (storage *SimpleRESTStorage) checkContext(ctx context.Context) {
	storage.actualNamespace, storage.namespacePresent = request.NamespaceFrom(ctx)
}

func (storage *SimpleRESTStorage) Delete(ctx context.Context, id string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	storage.checkContext(ctx)
	storage.deleted = id
	storage.deleteOptions = options
	if err := storage.errors["delete"]; err != nil {
		return nil, false, err
	}
	if err := deleteValidation(ctx, &storage.item); err != nil {
		return nil, false, err
	}
	var obj runtime.Object = &metav1.Status{Status: metav1.StatusSuccess}
	var err error
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(&genericapitesting.Simple{ObjectMeta: metav1.ObjectMeta{Name: id}})
	}
	return obj, true, err
}

func (storage *SimpleRESTStorage) New() runtime.Object {
	return &genericapitesting.Simple{}
}

func (storage *SimpleRESTStorage) NewList() runtime.Object {
	return &genericapitesting.SimpleList{}
}

func (storage *SimpleRESTStorage) Destroy() {
}

func (storage *SimpleRESTStorage) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	storage.checkContext(ctx)
	storage.created = obj.(*genericapitesting.Simple)
	if err := storage.errors["create"]; err != nil {
		return nil, err
	}
	var err error
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(obj)
	}
	if err := createValidation(ctx, obj); err != nil {
		return nil, err
	}
	return obj, err
}

func (storage *SimpleRESTStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	storage.checkContext(ctx)
	obj, err := objInfo.UpdatedObject(ctx, &storage.item)
	if err != nil {
		return nil, false, err
	}
	storage.updated = obj.(*genericapitesting.Simple)
	if err := storage.errors["update"]; err != nil {
		return nil, false, err
	}
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(obj)
	}
	if err := updateValidation(ctx, &storage.item, obj); err != nil {
		return nil, false, err
	}
	return obj, false, err
}

// Implement ResourceWatcher.
func (storage *SimpleRESTStorage) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	storage.lock.Lock()
	defer storage.lock.Unlock()
	storage.checkContext(ctx)
	storage.requestedLabelSelector = labels.Everything()
	if options != nil && options.LabelSelector != nil {
		storage.requestedLabelSelector = options.LabelSelector
	}
	storage.requestedFieldSelector = fields.Everything()
	if options != nil && options.FieldSelector != nil {
		storage.requestedFieldSelector = options.FieldSelector
	}
	storage.requestedResourceVersion = ""
	if options != nil {
		storage.requestedResourceVersion = options.ResourceVersion
	}
	storage.requestedResourceNamespace = request.NamespaceValue(ctx)
	if err := storage.errors["watch"]; err != nil {
		return nil, err
	}
	storage.fakeWatch = watch.NewFake()
	return storage.fakeWatch, nil
}

func (storage *SimpleRESTStorage) Watcher() *watch.FakeWatcher {
	storage.lock.Lock()
	defer storage.lock.Unlock()
	return storage.fakeWatch
}

// Implement Connecter
type ConnecterRESTStorage struct {
	connectHandler http.Handler
	handlerFunc    func() http.Handler

	emptyConnectOptions    runtime.Object
	receivedConnectOptions runtime.Object
	receivedID             string
	receivedResponder      rest.Responder
	takesPath              string
}

// Implement Connecter
var _ = rest.Connecter(&ConnecterRESTStorage{})

func (s *ConnecterRESTStorage) New() runtime.Object {
	return &genericapitesting.Simple{}
}

func (s *ConnecterRESTStorage) Destroy() {
}

func (s *ConnecterRESTStorage) Connect(ctx context.Context, id string, options runtime.Object, responder rest.Responder) (http.Handler, error) {
	s.receivedConnectOptions = options
	s.receivedID = id
	s.receivedResponder = responder
	if s.handlerFunc != nil {
		return s.handlerFunc(), nil
	}
	return s.connectHandler, nil
}

func (s *ConnecterRESTStorage) ConnectMethods() []string {
	return []string{"GET", "POST", "PUT", "DELETE"}
}

func (s *ConnecterRESTStorage) NewConnectOptions() (runtime.Object, bool, string) {
	if len(s.takesPath) > 0 {
		return s.emptyConnectOptions, true, s.takesPath
	}
	return s.emptyConnectOptions, false, ""
}

func (s *ConnecterRESTStorage) GetSingularName() string {
	return "simple"
}

type MetadataRESTStorage struct {
	*SimpleRESTStorage
	types []string
}

func (m *MetadataRESTStorage) ProducesMIMETypes(method string) []string {
	return m.types
}

func (m *MetadataRESTStorage) ProducesObject(verb string) interface{} {
	return nil
}

var _ rest.StorageMetadata = &MetadataRESTStorage{}

type GetWithOptionsRESTStorage struct {
	*SimpleRESTStorage
	optionsReceived runtime.Object
	takesPath       string
}

func (r *GetWithOptionsRESTStorage) Get(ctx context.Context, name string, options runtime.Object) (runtime.Object, error) {
	if _, ok := options.(*genericapitesting.SimpleGetOptions); !ok {
		return nil, fmt.Errorf("Unexpected options object: %#v", options)
	}
	r.optionsReceived = options
	return r.SimpleRESTStorage.Get(ctx, name, &metav1.GetOptions{})
}

func (r *GetWithOptionsRESTStorage) NewGetOptions() (runtime.Object, bool, string) {
	if len(r.takesPath) > 0 {
		return &genericapitesting.SimpleGetOptions{}, true, r.takesPath
	}
	return &genericapitesting.SimpleGetOptions{}, false, ""
}

var _ rest.GetterWithOptions = &GetWithOptionsRESTStorage{}

type GetWithOptionsRootRESTStorage struct {
	*SimpleTypedStorage
	optionsReceived runtime.Object
	takesPath       string
}

func (r *GetWithOptionsRootRESTStorage) GetSingularName() string {
	return "simple"
}

func (r *GetWithOptionsRootRESTStorage) NamespaceScoped() bool {
	return false
}

func (r *GetWithOptionsRootRESTStorage) Get(ctx context.Context, name string, options runtime.Object) (runtime.Object, error) {
	if _, ok := options.(*genericapitesting.SimpleGetOptions); !ok {
		return nil, fmt.Errorf("Unexpected options object: %#v", options)
	}
	r.optionsReceived = options
	return r.SimpleTypedStorage.Get(ctx, name, &metav1.GetOptions{})
}

func (r *GetWithOptionsRootRESTStorage) NewGetOptions() (runtime.Object, bool, string) {
	if len(r.takesPath) > 0 {
		return &genericapitesting.SimpleGetOptions{}, true, r.takesPath
	}
	return &genericapitesting.SimpleGetOptions{}, false, ""
}

var _ rest.GetterWithOptions = &GetWithOptionsRootRESTStorage{}

type NamedCreaterRESTStorage struct {
	*SimpleRESTStorage
	createdName string
}

func (storage *NamedCreaterRESTStorage) Create(ctx context.Context, name string, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	storage.checkContext(ctx)
	storage.created = obj.(*genericapitesting.Simple)
	storage.createdName = name
	if err := storage.errors["create"]; err != nil {
		return nil, err
	}
	var err error
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(obj)
	}
	if err := createValidation(ctx, obj); err != nil {
		return nil, err
	}
	return obj, err
}

type SimpleTypedStorage struct {
	errors   map[string]error
	item     runtime.Object
	baseType runtime.Object

	actualNamespace  string
	namespacePresent bool
}

func (storage *SimpleTypedStorage) New() runtime.Object {
	return storage.baseType
}

func (storage *SimpleTypedStorage) Destroy() {
}

func (storage *SimpleTypedStorage) Get(ctx context.Context, id string, options *metav1.GetOptions) (runtime.Object, error) {
	storage.checkContext(ctx)
	return storage.item.DeepCopyObject(), storage.errors["get"]
}

func (storage *SimpleTypedStorage) checkContext(ctx context.Context) {
	storage.actualNamespace, storage.namespacePresent = request.NamespaceFrom(ctx)
}

func (storage *SimpleTypedStorage) GetSingularName() string {
	return "simple"
}

func bodyOrDie(response *http.Response) string {
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func extractBody(response *http.Response, object runtime.Object) (string, error) {
	return extractBodyDecoder(response, object, codec)
}

func extractBodyDecoder(response *http.Response, object runtime.Object, decoder runtime.Decoder) (string, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return string(body), err
	}
	return string(body), runtime.DecodeInto(decoder, body, object)
}

func extractBodyObject(response *http.Response, decoder runtime.Decoder) (runtime.Object, string, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, string(body), err
	}
	obj, err := runtime.Decode(decoder, body)
	return obj, string(body), err
}

func TestNotFound(t *testing.T) {
	type T struct {
		Method string
		Path   string
		Status int
	}
	cases := map[string]T{
		// Positive checks to make sure everything is wired correctly
		"groupless GET root":       {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots", http.StatusOK},
		"groupless GET namespaced": {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples", http.StatusOK},

		"groupless GET long prefix": {"GET", "/" + grouplessPrefix + "/", http.StatusNotFound},

		"groupless root PATCH method":                 {"PATCH", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots", http.StatusMethodNotAllowed},
		"groupless root GET missing storage":          {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/blah", http.StatusNotFound},
		"groupless root GET with extra segment":       {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots/bar/baz", http.StatusNotFound},
		"groupless root DELETE without extra segment": {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots", http.StatusMethodNotAllowed},
		"groupless root DELETE with extra segment":    {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots/bar/baz", http.StatusNotFound},
		"groupless root PUT without extra segment":    {"PUT", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots", http.StatusMethodNotAllowed},
		"groupless root PUT with extra segment":       {"PUT", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simpleroots/bar/baz", http.StatusNotFound},
		"groupless root watch missing storage":        {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/watch/", http.StatusInternalServerError},

		"groupless namespaced PATCH method":                 {"PATCH", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples", http.StatusMethodNotAllowed},
		"groupless namespaced GET long prefix":              {"GET", "/" + grouplessPrefix + "/", http.StatusNotFound},
		"groupless namespaced GET missing storage":          {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/blah", http.StatusNotFound},
		"groupless namespaced GET with extra segment":       {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples/bar/baz", http.StatusNotFound},
		"groupless namespaced POST with extra segment":      {"POST", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples/bar", http.StatusMethodNotAllowed},
		"groupless namespaced DELETE without extra segment": {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples", http.StatusMethodNotAllowed},
		"groupless namespaced DELETE with extra segment":    {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples/bar/baz", http.StatusNotFound},
		"groupless namespaced PUT without extra segment":    {"PUT", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples", http.StatusMethodNotAllowed},
		"groupless namespaced PUT with extra segment":       {"PUT", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples/bar/baz", http.StatusNotFound},
		"groupless namespaced watch missing storage":        {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/watch/", http.StatusInternalServerError},
		"groupless namespaced watch with bad method":        {"POST", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/watch/namespaces/ns/simples/bar", http.StatusMethodNotAllowed},
		"groupless namespaced watch param with bad method":  {"POST", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/ns/simples/bar?watch=true", http.StatusMethodNotAllowed},

		// Positive checks to make sure everything is wired correctly
		"GET root": {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots", http.StatusOK},
		// TODO: JTL: "GET root item":       {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots/bar", http.StatusOK},
		"GET namespaced": {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples", http.StatusOK},
		// TODO: JTL: "GET namespaced item": {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples/bar", http.StatusOK},

		"GET long prefix": {"GET", "/" + prefix + "/", http.StatusNotFound},

		"root PATCH method":           {"PATCH", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots", http.StatusMethodNotAllowed},
		"root GET missing storage":    {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/blah", http.StatusNotFound},
		"root GET with extra segment": {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots/bar/baz", http.StatusNotFound},
		// TODO: JTL: "root POST with extra segment":      {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots/bar", http.StatusMethodNotAllowed},
		"root DELETE without extra segment": {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots", http.StatusMethodNotAllowed},
		"root DELETE with extra segment":    {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots/bar/baz", http.StatusNotFound},
		"root PUT without extra segment":    {"PUT", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots", http.StatusMethodNotAllowed},
		"root PUT with extra segment":       {"PUT", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simpleroots/bar/baz", http.StatusNotFound},
		"root watch missing storage":        {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/watch/", http.StatusInternalServerError},
		// TODO: JTL: "root watch with bad method":        {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/watch/simpleroot/bar", http.StatusMethodNotAllowed},

		"namespaced PATCH method":                 {"PATCH", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples", http.StatusMethodNotAllowed},
		"namespaced GET long prefix":              {"GET", "/" + prefix + "/", http.StatusNotFound},
		"namespaced GET missing storage":          {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/blah", http.StatusNotFound},
		"namespaced GET with extra segment":       {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples/bar/baz", http.StatusNotFound},
		"namespaced POST with extra segment":      {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples/bar", http.StatusMethodNotAllowed},
		"namespaced DELETE without extra segment": {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples", http.StatusMethodNotAllowed},
		"namespaced DELETE with extra segment":    {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples/bar/baz", http.StatusNotFound},
		"namespaced PUT without extra segment":    {"PUT", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples", http.StatusMethodNotAllowed},
		"namespaced PUT with extra segment":       {"PUT", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples/bar/baz", http.StatusNotFound},
		"namespaced watch missing storage":        {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/watch/", http.StatusInternalServerError},
		"namespaced watch with bad method":        {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/watch/namespaces/ns/simples/bar", http.StatusMethodNotAllowed},
		"namespaced watch param with bad method":  {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/ns/simples/bar?watch=true", http.StatusMethodNotAllowed},
	}
	handler := handle(map[string]rest.Storage{
		"simples":     &SimpleRESTStorage{},
		"simpleroots": &SimpleRESTStorage{},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}
	for k, v := range cases {
		request, err := http.NewRequest(v.Method, server.URL+v.Path, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		response, err := client.Do(request)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if response.StatusCode != v.Status {
			t.Errorf("Expected %d for %s (%s), Got %#v", v.Status, v.Method, k, response)
		}
	}
}

type UnimplementedRESTStorage struct{}

func (UnimplementedRESTStorage) NamespaceScoped() bool {
	return true
}

func (UnimplementedRESTStorage) New() runtime.Object {
	return &genericapitesting.Simple{}
}

func (UnimplementedRESTStorage) Destroy() {
}

func (UnimplementedRESTStorage) GetSingularName() string {
	return ""
}

// TestUnimplementedRESTStorage ensures that if a rest.Storage does not implement a given
// method, that it is literally not registered with the server.  In the past,
// we registered everything, and returned method not supported if it didn't support
// a verb.  Now we literally do not register a storage if it does not implement anything.
// TODO: in future, we should update proxy/redirect
func TestUnimplementedRESTStorage(t *testing.T) {
	type T struct {
		Method  string
		Path    string
		ErrCode int
	}
	cases := map[string]T{
		"groupless GET object":    {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/foo/bar", http.StatusNotFound},
		"groupless GET list":      {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/foo", http.StatusNotFound},
		"groupless POST list":     {"POST", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/foo", http.StatusNotFound},
		"groupless PUT object":    {"PUT", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/foo/bar", http.StatusNotFound},
		"groupless DELETE object": {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/foo/bar", http.StatusNotFound},
		"groupless watch list":    {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/watch/foo", http.StatusNotFound},
		"groupless watch object":  {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/watch/foo/bar", http.StatusNotFound},
		"groupless proxy object":  {"GET", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/proxy/foo/bar", http.StatusNotFound},

		"GET object":    {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/foo/bar", http.StatusNotFound},
		"GET list":      {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/foo", http.StatusNotFound},
		"POST list":     {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/foo", http.StatusNotFound},
		"PUT object":    {"PUT", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/foo/bar", http.StatusNotFound},
		"DELETE object": {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/foo/bar", http.StatusNotFound},
		"watch list":    {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/watch/foo", http.StatusNotFound},
		"watch object":  {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/watch/foo/bar", http.StatusNotFound},
		"proxy object":  {"GET", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/proxy/foo/bar", http.StatusNotFound},
	}
	handler := handle(map[string]rest.Storage{
		"foo": UnimplementedRESTStorage{},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}
	for k, v := range cases {
		request, err := http.NewRequest(v.Method, server.URL+v.Path, bytes.NewReader([]byte(`{"kind":"Simple","apiVersion":"version"}`)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if response.StatusCode != v.ErrCode {
			t.Errorf("%s: expected %d for %s, Got %s", k, v.ErrCode, v.Method, string(data))
			continue
		}
	}
}

type OnlyGetRESTStorage struct {
	UnimplementedRESTStorage
}

func (OnlyGetRESTStorage) Get(ctx context.Context, id string, options *metav1.GetOptions) (runtime.Object, error) {
	return nil, nil
}

func (OnlyGetRESTStorage) NewList() runtime.Object {
	return &genericapitesting.SimpleList{}
}

func (OnlyGetRESTStorage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	return nil, nil
}

func (OnlyGetRESTStorage) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	return nil, nil
}

// TestSomeUnimplementedRESTStorage ensures that if a rest.Storage does
// not implement a given method, that it is literally not registered
// with the server. We need to have at least one verb supported inorder
// to get a MethodNotAllowed rather than NotFound error.
func TestSomeUnimplementedRESTStorage(t *testing.T) {
	type T struct {
		Method  string
		Path    string
		ErrCode int
	}

	cases := map[string]T{
		"groupless POST list":         {"POST", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/foo", http.StatusMethodNotAllowed},
		"groupless PUT object":        {"PUT", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/foo/bar", http.StatusMethodNotAllowed},
		"groupless DELETE object":     {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/foo/bar", http.StatusMethodNotAllowed},
		"groupless DELETE collection": {"DELETE", "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/foo", http.StatusMethodNotAllowed},
		"POST list":                   {"POST", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/foo", http.StatusMethodNotAllowed},
		"PUT object":                  {"PUT", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/foo/bar", http.StatusMethodNotAllowed},
		"DELETE object":               {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/foo/bar", http.StatusMethodNotAllowed},
		"DELETE collection":           {"DELETE", "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/foo", http.StatusMethodNotAllowed},
	}
	handler := handle(map[string]rest.Storage{
		"foo": OnlyGetRESTStorage{},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}
	for k, v := range cases {
		request, err := http.NewRequest(v.Method, server.URL+v.Path, bytes.NewReader([]byte(`{"kind":"Simple","apiVersion":"version"}`)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if response.StatusCode != v.ErrCode {
			t.Errorf("%s: expected %d for %s, Got %s", k, v.ErrCode, v.Method, string(data))
			continue
		}
	}
}

func TestList(t *testing.T) {
	testCases := []struct {
		url       string
		namespace string
		legacy    bool
		label     string
		field     string
	}{
		// Groupless API

		// legacy namespace param is ignored
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simple?namespace=",
			namespace: "",
			legacy:    true,
		},
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simple?namespace=other",
			namespace: "",
			legacy:    true,
		},
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simple?namespace=other&labelSelector=a%3Db&fieldSelector=c%3Dd",
			namespace: "",
			legacy:    true,
			label:     "a=b",
			field:     "c=d",
		},
		// legacy api version is honored
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simple",
			namespace: "",
			legacy:    true,
		},
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/other/simple",
			namespace: "other",
			legacy:    true,
		},
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/other/simple?labelSelector=a%3Db&fieldSelector=c%3Dd",
			namespace: "other",
			legacy:    true,
			label:     "a=b",
			field:     "c=d",
		},
		// list items across all namespaces
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simple",
			namespace: "",
			legacy:    true,
		},
		// list items in a namespace in the path
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/simple",
			namespace: "default",
		},
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/other/simple",
			namespace: "other",
		},
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/other/simple?labelSelector=a%3Db&fieldSelector=c%3Dd",
			namespace: "other",
			label:     "a=b",
			field:     "c=d",
		},
		// list items across all namespaces
		{
			url:       "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/simple",
			namespace: "",
		},

		// Group API

		// legacy namespace param is ignored
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple?namespace=",
			namespace: "",
			legacy:    true,
		},
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple?namespace=other",
			namespace: "",
			legacy:    true,
		},
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple?namespace=other&labelSelector=a%3Db&fieldSelector=c%3Dd",
			namespace: "",
			legacy:    true,
			label:     "a=b",
			field:     "c=d",
		},
		// legacy api version is honored
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple",
			namespace: "",
			legacy:    true,
		},
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/other/simple",
			namespace: "other",
			legacy:    true,
		},
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/other/simple?labelSelector=a%3Db&fieldSelector=c%3Dd",
			namespace: "other",
			legacy:    true,
			label:     "a=b",
			field:     "c=d",
		},
		// list items across all namespaces
		{
			url:       "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple",
			namespace: "",
			legacy:    true,
		},
		// list items in a namespace in the path
		{
			url:       "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/namespaces/default/simple",
			namespace: "default",
		},
		{
			url:       "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/namespaces/other/simple",
			namespace: "other",
		},
		{
			url:       "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/namespaces/other/simple?labelSelector=a%3Db&fieldSelector=c%3Dd",
			namespace: "other",
			label:     "a=b",
			field:     "c=d",
		},
		// list items across all namespaces
		{
			url:       "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/simple",
			namespace: "",
		},
	}
	for i, testCase := range testCases {
		storage := map[string]rest.Storage{}
		simpleStorage := SimpleRESTStorage{expectedResourceNamespace: testCase.namespace}
		storage["simple"] = &simpleStorage
		var handler = handleInternal(storage, admissionControl, nil)
		server := httptest.NewServer(handler)
		defer server.Close()

		resp, err := http.Get(server.URL + testCase.url)
		if err != nil {
			t.Errorf("%d: unexpected error: %v", i, err)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%d: unexpected status: %d from url %s, Expected: %d, %#v", i, resp.StatusCode, testCase.url, http.StatusOK, resp)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("%d: unexpected error: %v", i, err)
				continue
			}
			t.Logf("%d: body: %s", i, string(body))
			continue
		}
		if !simpleStorage.namespacePresent {
			t.Errorf("%d: namespace not set", i)
		} else if simpleStorage.actualNamespace != testCase.namespace {
			t.Errorf("%d: %q unexpected resource namespace: %s", i, testCase.url, simpleStorage.actualNamespace)
		}
		if simpleStorage.requestedLabelSelector == nil || simpleStorage.requestedLabelSelector.String() != testCase.label {
			t.Errorf("%d: unexpected label selector: expected=%v got=%v", i, testCase.label, simpleStorage.requestedLabelSelector)
		}
		if simpleStorage.requestedFieldSelector == nil || simpleStorage.requestedFieldSelector.String() != testCase.field {
			t.Errorf("%d: unexpected field selector: expected=%v got=%v", i, testCase.field, simpleStorage.requestedFieldSelector)
		}
	}
}

func TestRequestsWithInvalidQuery(t *testing.T) {
	storage := map[string]rest.Storage{}

	storage["simple"] = &SimpleRESTStorage{expectedResourceNamespace: "default"}
	storage["withoptions"] = GetWithOptionsRESTStorage{}

	var handler = handleInternal(storage, admissionControl, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	for i, test := range []struct {
		postfix string
		method  string
	}{
		{"/simple?labelSelector=<invalid>", http.MethodGet},
		{"/simple/foo?gracePeriodSeconds=<invalid>", http.MethodDelete},
		// {"/simple?labelSelector=<value>", http.MethodDelete}, TODO: implement DeleteCollection in  SimpleRESTStorage
		// {"/simple/foo?export=<invalid>", http.MethodGet}, TODO: there is no invalid bool in conversion. Should we be more strict?
		// {"/simple/foo?resourceVersion=<invalid>", http.MethodGet}, TODO: there is no invalid resourceVersion. Should we be more strict?
		// {"/withoptions?labelSelector=<invalid>", http.MethodGet}, TODO: SimpleGetOptions is always valid. Add more validation that can fail.
	} {
		baseURL := server.URL + "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default"
		url := baseURL + test.postfix
		r, err := http.NewRequest(test.method, url, nil)
		if err != nil {
			t.Errorf("%d: unexpected error: %v", i, err)
			continue
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Errorf("%d: unexpected error: %v", i, err)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%d: unexpected status: %d from url %s, Expected: %d, %#v", i, resp.StatusCode, url, http.StatusBadRequest, resp)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("%d: unexpected error: %v", i, err)
				continue
			}
			t.Logf("%d: body: %s", i, string(body))
		}
	}
}

func TestListCompression(t *testing.T) {
	testCases := []struct {
		url            string
		namespace      string
		legacy         bool
		label          string
		field          string
		acceptEncoding string
	}{
		// list items in a namespace in the path
		{
			url:            "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/simple",
			namespace:      "default",
			acceptEncoding: "",
		},
		{
			url:            "/" + grouplessPrefix + "/" + grouplessGroupVersion.Version + "/namespaces/default/simple",
			namespace:      "default",
			acceptEncoding: "gzip",
		},
	}
	for i, testCase := range testCases {
		storage := map[string]rest.Storage{}
		simpleStorage := SimpleRESTStorage{
			expectedResourceNamespace: testCase.namespace,
			list: []genericapitesting.Simple{
				{Other: strings.Repeat("0123456789abcdef", (128*1024/16)+1)},
			},
		}
		storage["simple"] = &simpleStorage
		var handler = handleInternal(storage, admissionControl, nil)

		handler = genericapifilters.WithRequestInfo(handler, newTestRequestInfoResolver())

		server := httptest.NewServer(handler)

		defer server.Close()

		req, err := http.NewRequest("GET", server.URL+testCase.url, nil)
		if err != nil {
			t.Errorf("%d: unexpected error: %v", i, err)
			continue
		}
		// It's necessary to manually set Accept-Encoding here
		// to prevent http.DefaultClient from automatically
		// decoding responses
		req.Header.Set("Accept-Encoding", testCase.acceptEncoding)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("%d: unexpected error: %v", i, err)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%d: unexpected status: %d from url %s, Expected: %d, %#v", i, resp.StatusCode, testCase.url, http.StatusOK, resp)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("%d: unexpected error: %v", i, err)
				continue
			}
			t.Logf("%d: body: %s", i, string(body))
			continue
		}
		if !simpleStorage.namespacePresent {
			t.Errorf("%d: namespace not set", i)
		} else if simpleStorage.actualNamespace != testCase.namespace {
			t.Errorf("%d: %q unexpected resource namespace: %s", i, testCase.url, simpleStorage.actualNamespace)
		}
		if simpleStorage.requestedLabelSelector == nil || simpleStorage.requestedLabelSelector.String() != testCase.label {
			t.Errorf("%d: unexpected label selector: %v", i, simpleStorage.requestedLabelSelector)
		}
		if simpleStorage.requestedFieldSelector == nil || simpleStorage.requestedFieldSelector.String() != testCase.field {
			t.Errorf("%d: unexpected field selector: %v", i, simpleStorage.requestedFieldSelector)
		}

		var decoder *json.Decoder
		if testCase.acceptEncoding == "gzip" {
			gzipReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatalf("unexpected error creating gzip reader: %v", err)
			}
			decoder = json.NewDecoder(gzipReader)
		} else {
			decoder = json.NewDecoder(resp.Body)
		}
		var itemOut genericapitesting.SimpleList
		err = decoder.Decode(&itemOut)
		if err != nil {
			t.Errorf("failed to read response body as SimpleList: %v", err)
		}
	}
}

func TestLogs(t *testing.T) {
	handler := handle(map[string]rest.Storage{})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	request, err := http.NewRequest("GET", server.URL+"/logs", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("Data: %s", string(body))
}

func TestErrorList(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"list": fmt.Errorf("test Error")},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", resp.StatusCode, http.StatusInternalServerError, resp)
	}
}

func TestNonEmptyList(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		list: []genericapitesting.Simple{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "something", Namespace: "other"},
				Other:      "foo",
			},
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/simple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", resp.StatusCode, http.StatusOK, resp)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Logf("Data: %s", string(body))
	}

	var listOut genericapitesting.SimpleList
	body, err := extractBody(resp, &listOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Log(body)

	if len(listOut.Items) != 1 {
		t.Errorf("Unexpected response: %#v", listOut)
		return
	}
	if listOut.Items[0].Other != simpleStorage.list[0].Other {
		t.Errorf("Unexpected data: %#v, %s", listOut.Items[0], string(body))
	}
}

func TestMetadata(t *testing.T) {
	simpleStorage := &MetadataRESTStorage{&SimpleRESTStorage{}, []string{"text/plain"}}
	h := handle(map[string]rest.Storage{"simple": simpleStorage})
	ws := h.(*defaultAPIServer).container.RegisteredWebServices()
	if len(ws) == 0 {
		t.Fatal("no web services registered")
	}
	matches := map[string]int{}
	for _, w := range ws {
		for _, r := range w.Routes() {
			t.Logf("%v %v %#v", r.Method, r.Path, r.Produces)
			s := strings.Join(r.Produces, ",")
			i := matches[s]
			matches[s] = i + 1
		}
	}
	cs := []func() bool{
		func() bool {
			return matches["text/plain,application/json,application/yaml,application/vnd.kubernetes.protobuf"] == 0
		},
		func() bool {
			return matches["application/json,application/yaml,application/vnd.kubernetes.protobuf,application/json;stream=watch,application/vnd.kubernetes.protobuf;stream=watch"] == 0
		},
		func() bool {
			return matches["application/json,application/yaml,application/vnd.kubernetes.protobuf"] == 0
		},
		func() bool {
			return len(matches) != 4
		},
	}
	for i, c := range cs {
		if c() {
			t.Errorf("[%d]unexpected mime types: %#v", i, matches)
		}
	}
}

func TestGet(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		item: genericapitesting.Simple{
			Other: "foo",
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %#v", resp)
	}
	var itemOut genericapitesting.Simple
	body, err := extractBody(resp, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if itemOut.Name != simpleStorage.item.Name {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simpleStorage.item, string(body))
	}
}

func BenchmarkGet(b *testing.B) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		item: genericapitesting.Simple{
			Other: "foo",
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	u := server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/id"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(u)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("unexpected response: %#v", resp)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatalf("unable to read body")
		}
	}
	b.StopTimer()
}

func BenchmarkGetNoCompression(b *testing.B) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		item: genericapitesting.Simple{
			Other: "foo",
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}

	u := server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/id"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(u)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("unexpected response: %#v", resp)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatalf("unable to read body")
		}
	}
	b.StopTimer()
}

func TestGetCompression(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		item: genericapitesting.Simple{
			Other: strings.Repeat("0123456789abcdef", (128*1024/16)+1),
		},
	}

	storage["simple"] = &simpleStorage
	handler := handle(storage)
	handler = genericapifilters.WithRequestInfo(handler, newTestRequestInfoResolver())
	server := httptest.NewServer(handler)
	defer server.Close()

	tests := []struct {
		acceptEncoding string
	}{
		{acceptEncoding: ""},
		{acceptEncoding: "gzip"},
	}

	for _, test := range tests {
		req, err := http.NewRequest("GET", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/id", nil)
		if err != nil {
			t.Fatalf("unexpected error creating request: %v", err)
		}
		// It's necessary to manually set Accept-Encoding here
		// to prevent http.DefaultClient from automatically
		// decoding responses
		req.Header.Set("Accept-Encoding", test.acceptEncoding)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected response: %#v", resp)
		}
		var decoder *json.Decoder
		if test.acceptEncoding == "gzip" {
			gzipReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatalf("unexpected error creating gzip reader: %v", err)
			}
			decoder = json.NewDecoder(gzipReader)
		} else {
			decoder = json.NewDecoder(resp.Body)
		}
		var itemOut genericapitesting.Simple
		err = decoder.Decode(&itemOut)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("unexpected error reading body: %v", err)
		}

		if itemOut.Name != simpleStorage.item.Name {
			t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simpleStorage.item, string(body))
		}
	}
}

func TestGetPretty(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		item: genericapitesting.Simple{
			Other: "foo",
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	tests := []struct {
		accept    string
		userAgent string
		params    url.Values
		pretty    bool
	}{
		{accept: runtime.ContentTypeJSON},
		{accept: "application/json;pretty=0"},
		{accept: runtime.ContentTypeJSON, userAgent: "kubectl"},
		{accept: runtime.ContentTypeJSON, params: url.Values{"pretty": {"0"}}},

		{pretty: true, accept: runtime.ContentTypeJSON, userAgent: "curl"},
		{pretty: true, accept: runtime.ContentTypeJSON, userAgent: "Mozilla/5.0"},
		{pretty: true, accept: runtime.ContentTypeJSON, userAgent: "Wget"},
		{pretty: true, accept: "application/json;pretty=1"},
		{pretty: true, accept: runtime.ContentTypeJSON, params: url.Values{"pretty": {"1"}}},
		{pretty: true, accept: runtime.ContentTypeJSON, params: url.Values{"pretty": {"true"}}},
	}
	for i, test := range tests {
		u, err := url.Parse(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/id")
		if err != nil {
			t.Fatal(err)
		}
		u.RawQuery = test.params.Encode()
		req := &http.Request{Method: "GET", URL: u}
		req.Header = http.Header{}
		req.Header.Set("Accept", test.accept)
		req.Header.Set("User-Agent", test.userAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatal(err)
		}
		var itemOut genericapitesting.Simple
		body, err := extractBody(resp, &itemOut)
		if err != nil {
			t.Fatal(err)
		}
		// to get stable ordering we need to use a go type
		unstructured := genericapitesting.Simple{}
		if err := json.Unmarshal([]byte(body), &unstructured); err != nil {
			t.Fatal(err)
		}
		var expect string
		if test.pretty {
			out, err := json.MarshalIndent(unstructured, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			expect = string(out)
		} else {
			out, err := json.Marshal(unstructured)
			if err != nil {
				t.Fatal(err)
			}
			expect = string(out) + "\n"
		}
		if expect != body {
			t.Errorf("%d: body did not match expected:\n%s\n%s", i, body, expect)
		}
	}
}

func TestGetTable(t *testing.T) {
	now := metav1.Now()
	obj := genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", ResourceVersion: "10", CreationTimestamp: now, UID: types.UID("abcdef0123")},
		Other:      "foo",
	}

	m, err := meta.Accessor(&obj)
	if err != nil {
		t.Fatal(err)
	}
	var encodedV1Beta1Body []byte
	{
		partial := meta.AsPartialObjectMetadata(m)
		partial.GetObjectKind().SetGroupVersionKind(metav1beta1.SchemeGroupVersion.WithKind("PartialObjectMetadata"))
		encodedBody, err := runtime.Encode(metainternalversionscheme.Codecs.LegacyCodec(metav1beta1.SchemeGroupVersion), partial)
		if err != nil {
			t.Fatal(err)
		}
		// the codec includes a trailing newline that is not present during decode
		encodedV1Beta1Body = bytes.TrimSpace(encodedBody)
	}
	var encodedV1Body []byte
	{
		partial := meta.AsPartialObjectMetadata(m)
		partial.GetObjectKind().SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("PartialObjectMetadata"))
		encodedBody, err := runtime.Encode(metainternalversionscheme.Codecs.LegacyCodec(metav1.SchemeGroupVersion), partial)
		if err != nil {
			t.Fatal(err)
		}
		// the codec includes a trailing newline that is not present during decode
		encodedV1Body = bytes.TrimSpace(encodedBody)
	}

	metaDoc := metav1.ObjectMeta{}.SwaggerDoc()

	tests := []struct {
		accept     string
		params     url.Values
		pretty     bool
		expected   *metav1.Table
		statusCode int
		item       bool
	}{
		{
			accept:     "application/json;as=Table;v=v1alpha1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},
		{
			accept:     runtime.ContentTypeProtobuf + ";as=Table;v=v1beta1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},
		{
			accept:     runtime.ContentTypeProtobuf + ";as=Table;v=v1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},

		{
			item:   true,
			accept: "application/json;as=Table;v=v1;g=meta.k8s.io",
			expected: &metav1.Table{
				TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1"},
				ListMeta: metav1.ListMeta{ResourceVersion: "10"},
				ColumnDefinitions: []metav1.TableColumnDefinition{
					{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
					{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
				},
				Rows: []metav1.TableRow{
					{Cells: []interface{}{"foo1", now.Time.UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedV1Body}},
				},
			},
		},
		{
			item:   true,
			accept: "application/json;as=Table;v=v1beta1;g=meta.k8s.io",
			expected: &metav1.Table{
				TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
				ListMeta: metav1.ListMeta{ResourceVersion: "10"},
				ColumnDefinitions: []metav1.TableColumnDefinition{
					{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
					{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
				},
				Rows: []metav1.TableRow{
					{Cells: []interface{}{"foo1", now.Time.UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedV1Beta1Body}},
				},
			},
		},
		{
			item: true,
			accept: strings.Join([]string{
				runtime.ContentTypeProtobuf + ";as=Table;v=v1beta1;g=meta.k8s.io",
				"application/json;as=Table;v=v1beta1;g=meta.k8s.io",
			}, ","),
			expected: &metav1.Table{
				TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
				ListMeta: metav1.ListMeta{ResourceVersion: "10"},
				ColumnDefinitions: []metav1.TableColumnDefinition{
					{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
					{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
				},
				Rows: []metav1.TableRow{
					{Cells: []interface{}{"foo1", now.Time.UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedV1Beta1Body}},
				},
			},
		},
		{
			item:   true,
			accept: "application/json;as=Table;v=v1beta1;g=meta.k8s.io",
			params: url.Values{"includeObject": []string{"Metadata"}},
			expected: &metav1.Table{
				TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
				ListMeta: metav1.ListMeta{ResourceVersion: "10"},
				ColumnDefinitions: []metav1.TableColumnDefinition{
					{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
					{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
				},
				Rows: []metav1.TableRow{
					{Cells: []interface{}{"foo1", now.Time.UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedV1Beta1Body}},
				},
			},
		},
		{
			accept: "application/json;as=Table;v=v1beta1;g=meta.k8s.io",
			params: url.Values{"includeObject": []string{"Metadata"}},
			expected: &metav1.Table{
				TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
				ListMeta: metav1.ListMeta{ResourceVersion: "10"},
				ColumnDefinitions: []metav1.TableColumnDefinition{
					{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
					{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
				},
				Rows: []metav1.TableRow{
					{Cells: []interface{}{"foo1", now.Time.UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedV1Beta1Body}},
				},
			},
		},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			storage := map[string]rest.Storage{}
			simpleStorage := SimpleRESTStorage{
				item: obj,
				list: []genericapitesting.Simple{obj},
			}
			storage["simple"] = &simpleStorage
			handler := handle(storage)
			server := httptest.NewServer(handler)
			defer server.Close()

			var id string
			if test.item {
				id = "/id"
			}
			u, err := url.Parse(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple" + id)
			if err != nil {
				t.Fatal(err)
			}
			u.RawQuery = test.params.Encode()
			req := &http.Request{Method: "GET", URL: u}
			req.Header = http.Header{}
			req.Header.Set("Accept", test.accept)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if test.statusCode != 0 {
				if resp.StatusCode != test.statusCode {
					t.Errorf("%d: unexpected response: %#v", i, resp)
				}
				obj, _, err := extractBodyObject(resp, unstructured.UnstructuredJSONScheme)
				if err != nil {
					t.Fatalf("%d: unexpected body read error: %v", i, err)
				}
				gvk := schema.GroupVersionKind{Version: "v1", Kind: "Status"}
				if obj.GetObjectKind().GroupVersionKind() != gvk {
					t.Fatalf("%d: unexpected error body: %#v", i, obj)
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%d: unexpected response: %#v", i, resp)
			}
			var itemOut metav1.Table
			body, err := extractBody(resp, &itemOut)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(test.expected, &itemOut) {
				t.Log(body)
				t.Errorf("%d: did not match: %s", i, cmp.Diff(test.expected, &itemOut))
			}
		})
	}
}

func TestWatchTable(t *testing.T) {
	obj := genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", ResourceVersion: "10", CreationTimestamp: metav1.NewTime(time.Unix(1, 0)), UID: types.UID("abcdef0123")},
		Other:      "foo",
	}

	m, err := meta.Accessor(&obj)
	if err != nil {
		t.Fatal(err)
	}
	partial := meta.AsPartialObjectMetadata(m)
	partial.GetObjectKind().SetGroupVersionKind(metav1beta1.SchemeGroupVersion.WithKind("PartialObjectMetadata"))
	encodedBody, err := runtime.Encode(metainternalversionscheme.Codecs.LegacyCodec(metav1beta1.SchemeGroupVersion), partial)
	if err != nil {
		t.Fatal(err)
	}
	// the codec includes a trailing newline that is not present during decode
	encodedBody = bytes.TrimSpace(encodedBody)

	encodedBodyV1, err := runtime.Encode(metainternalversionscheme.Codecs.LegacyCodec(metav1.SchemeGroupVersion), partial)
	if err != nil {
		t.Fatal(err)
	}
	// the codec includes a trailing newline that is not present during decode
	encodedBodyV1 = bytes.TrimSpace(encodedBodyV1)

	metaDoc := metav1.ObjectMeta{}.SwaggerDoc()

	s := metainternalversionscheme.Codecs.SupportedMediaTypes()[0].Serializer

	tests := []struct {
		accept string
		params url.Values
		send   func(w *watch.FakeWatcher)

		expected    []*metav1.WatchEvent
		contentType string
		statusCode  int
		item        bool
	}{
		{
			accept:     "application/json;as=Table;v=v1alpha1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},
		{
			accept: "application/json;as=Table;v=v1beta1;g=meta.k8s.io",
			send: func(w *watch.FakeWatcher) {
				w.Add(&obj)
			},
			expected: []*metav1.WatchEvent{
				{
					Type: "ADDED",
					Object: runtime.RawExtension{
						Raw: []byte(strings.TrimSpace(runtime.EncodeOrDie(s, &metav1.Table{
							TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
							ListMeta: metav1.ListMeta{ResourceVersion: "10"},
							ColumnDefinitions: []metav1.TableColumnDefinition{
								{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
								{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
							},
							Rows: []metav1.TableRow{
								{Cells: []interface{}{"foo1", time.Unix(1, 0).UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedBody}},
							},
						}))),
					},
				},
			},
		},
		{
			accept: "application/json;as=Table;v=v1beta1;g=meta.k8s.io",
			send: func(w *watch.FakeWatcher) {
				w.Add(&obj)
				w.Modify(&obj)
			},
			expected: []*metav1.WatchEvent{
				{
					Type: "ADDED",
					Object: runtime.RawExtension{
						Raw: []byte(strings.TrimSpace(runtime.EncodeOrDie(s, &metav1.Table{
							TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
							ListMeta: metav1.ListMeta{ResourceVersion: "10"},
							ColumnDefinitions: []metav1.TableColumnDefinition{
								{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
								{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
							},
							Rows: []metav1.TableRow{
								{Cells: []interface{}{"foo1", time.Unix(1, 0).UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedBody}},
							},
						}))),
					},
				},
				{
					Type: "MODIFIED",
					Object: runtime.RawExtension{
						Raw: []byte(strings.TrimSpace(runtime.EncodeOrDie(s, &metav1.Table{
							TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1beta1"},
							ListMeta: metav1.ListMeta{ResourceVersion: "10"},
							Rows: []metav1.TableRow{
								{Cells: []interface{}{"foo1", time.Unix(1, 0).UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedBody}},
							},
						}))),
					},
				},
			},
		},
		{
			accept: "application/json;as=Table;v=v1;g=meta.k8s.io",
			send: func(w *watch.FakeWatcher) {
				w.Add(&obj)
				w.Modify(&obj)
			},
			expected: []*metav1.WatchEvent{
				{
					Type: "ADDED",
					Object: runtime.RawExtension{
						Raw: []byte(strings.TrimSpace(runtime.EncodeOrDie(s, &metav1.Table{
							TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1"},
							ListMeta: metav1.ListMeta{ResourceVersion: "10"},
							ColumnDefinitions: []metav1.TableColumnDefinition{
								{Name: "Name", Type: "string", Format: "name", Description: metaDoc["name"]},
								{Name: "Created At", Type: "date", Description: metaDoc["creationTimestamp"]},
							},
							Rows: []metav1.TableRow{
								{Cells: []interface{}{"foo1", time.Unix(1, 0).UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedBodyV1}},
							},
						}))),
					},
				},
				{
					Type: "MODIFIED",
					Object: runtime.RawExtension{
						Raw: []byte(strings.TrimSpace(runtime.EncodeOrDie(s, &metav1.Table{
							TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1"},
							ListMeta: metav1.ListMeta{ResourceVersion: "10"},
							Rows: []metav1.TableRow{
								{Cells: []interface{}{"foo1", time.Unix(1, 0).UTC().Format(time.RFC3339)}, Object: runtime.RawExtension{Raw: encodedBodyV1}},
							},
						}))),
					},
				},
			},
		},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			storage := map[string]rest.Storage{}
			simpleStorage := SimpleRESTStorage{
				item: obj,
				list: []genericapitesting.Simple{obj},
			}

			storage["simple"] = &simpleStorage
			handler := handle(storage)
			server := httptest.NewServer(handler)
			defer server.Close()

			var id string
			if test.item {
				id = "/id"
			}
			u, err := url.Parse(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple")
			if err != nil {
				t.Fatal(err)
			}
			if test.params == nil {
				test.params = url.Values{}
			}
			if test.item {
				test.params["fieldSelector"] = []string{fmt.Sprintf("metadata.name=%s", id)}
			}
			test.params["watch"] = []string{"1"}

			u.RawQuery = test.params.Encode()
			req := &http.Request{Method: "GET", URL: u}
			req.Header = http.Header{}
			req.Header.Set("Accept", test.accept)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if test.statusCode != 0 {
				if resp.StatusCode != test.statusCode {
					t.Fatalf("%d: unexpected response: %#v", i, resp)
				}
				obj, _, err := extractBodyObject(resp, unstructured.UnstructuredJSONScheme)
				if err != nil {
					t.Fatalf("%d: unexpected body read error: %v", i, err)
				}
				gvk := schema.GroupVersionKind{Version: "v1", Kind: "Status"}
				if obj.GetObjectKind().GroupVersionKind() != gvk {
					t.Fatalf("%d: unexpected error body: %#v", i, obj)
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%d: unexpected response: %#v", i, resp)
			}

			go func() {
				defer simpleStorage.fakeWatch.Stop()
				test.send(simpleStorage.fakeWatch)
			}()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Body:\n%s", string(body))
			d := watcher(resp.Header.Get("Content-Type"), io.NopCloser(bytes.NewReader(body)))
			var actual []*metav1.WatchEvent
			for {
				var event metav1.WatchEvent
				_, _, err := d.Decode(nil, &event)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				actual = append(actual, &event)
			}
			if !reflect.DeepEqual(test.expected, actual) {
				t.Fatalf("unexpected: %s", cmp.Diff(test.expected, actual))
			}
		})
	}
}

func watcher(mediaType string, r io.ReadCloser) streaming.Decoder {
	info, ok := runtime.SerializerInfoForMediaType(metainternalversionscheme.Codecs.SupportedMediaTypes(), mediaType)
	if !ok || info.StreamSerializer == nil {
		panic(info)
	}
	streamSerializer := info.StreamSerializer
	fr := streamSerializer.Framer.NewFrameReader(r)
	d := streaming.NewDecoder(fr, streamSerializer.Serializer)
	return d
}

func TestGetPartialObjectMetadata(t *testing.T) {
	now := metav1.Time{Time: metav1.Now().Rfc3339Copy().Local()}
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		item: genericapitesting.Simple{
			ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", CreationTimestamp: now, UID: types.UID("abcdef0123")},
			Other:      "foo",
		},
		list: []genericapitesting.Simple{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", CreationTimestamp: now, UID: types.UID("newer")},
				Other:      "foo",
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "foo2", Namespace: "ns2", CreationTimestamp: now, UID: types.UID("older")},
				Other:      "bar",
			},
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	tests := []struct {
		accept     string
		params     url.Values
		pretty     bool
		list       bool
		expected   runtime.Object
		expectKind schema.GroupVersionKind
		statusCode int
	}{
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1alpha1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1alpha1;g=meta.k8s.io, application/json",
			expectKind: schema.GroupVersionKind{Kind: "Simple", Group: testGroupVersion.Group, Version: testGroupVersion.Version},
		},
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io, application/json",
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1beta1"},
		},
		{
			list:       true,
			accept:     "application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},

		// verify preferred version overrides supported version
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io, application/json;as=PartialObjectMetadata;v=v1;g=meta.k8s.io, application/json",
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1beta1"},
		},
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1;g=meta.k8s.io, application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io, application/json",
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1"},
		},
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io, application/json;as=PartialObjectMetadata;v=v1;g=meta.k8s.io",
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1beta1"},
		},
		{
			accept:     "application/json;as=PartialObjectMetadata;v=v1;g=meta.k8s.io, application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io",
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1"},
		},

		{
			list:       true,
			accept:     "application/json;as=PartialObjectMetadata;v=v1alpha1;g=meta.k8s.io, application/json",
			expectKind: schema.GroupVersionKind{Kind: "SimpleList", Group: testGroupVersion.Group, Version: testGroupVersion.Version},
		},
		{
			list:       true,
			accept:     "application/json;as=PartialObjectMetadataList;v=v1beta1;g=meta.k8s.io, application/json",
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadataList", Group: "meta.k8s.io", Version: "v1beta1"},
		},
		{
			accept:     "application/json;as=PartialObjectMetadataList;v=v1beta1;g=meta.k8s.io",
			statusCode: http.StatusNotAcceptable,
		},
		{
			accept: "application/json;as=PartialObjectMetadata;v=v1beta1;g=meta.k8s.io",
			expected: &metav1beta1.PartialObjectMetadata{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", CreationTimestamp: now, UID: types.UID("abcdef0123")},
			},
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1beta1"},
		},
		{
			accept: "application/json;as=PartialObjectMetadata;v=v1;g=meta.k8s.io",
			expected: &metav1.PartialObjectMetadata{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", CreationTimestamp: now, UID: types.UID("abcdef0123")},
			},
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadata", Group: "meta.k8s.io", Version: "v1"},
		},
		{
			list:   true,
			accept: "application/json;as=PartialObjectMetadataList;v=v1beta1;g=meta.k8s.io",
			expected: &metav1beta1.PartialObjectMetadataList{
				ListMeta: metav1.ListMeta{
					ResourceVersion: "10",
				},
				Items: []metav1beta1.PartialObjectMetadata{
					{
						TypeMeta:   metav1.TypeMeta{APIVersion: "meta.k8s.io/v1beta1", Kind: "PartialObjectMetadata"},
						ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "ns1", CreationTimestamp: now, UID: types.UID("newer")},
					},
					{
						TypeMeta:   metav1.TypeMeta{APIVersion: "meta.k8s.io/v1beta1", Kind: "PartialObjectMetadata"},
						ObjectMeta: metav1.ObjectMeta{Name: "foo2", Namespace: "ns2", CreationTimestamp: now, UID: types.UID("older")},
					},
				},
			},
			expectKind: schema.GroupVersionKind{Kind: "PartialObjectMetadataList", Group: "meta.k8s.io", Version: "v1beta1"},
		},
	}
	for i, test := range tests {
		suffix := "/namespaces/default/simple/id"
		if test.list {
			suffix = "/namespaces/default/simple"
		}
		u, err := url.Parse(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + suffix)
		if err != nil {
			t.Fatal(err)
		}
		u.RawQuery = test.params.Encode()
		req := &http.Request{Method: "GET", URL: u}
		req.Header = http.Header{}
		req.Header.Set("Accept", test.accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if test.statusCode != 0 {
			if resp.StatusCode != test.statusCode {
				t.Errorf("%d: unexpected response: %#v", i, resp)
			}
			obj, _, err := extractBodyObject(resp, unstructured.UnstructuredJSONScheme)
			if err != nil {
				t.Errorf("%d: unexpected body read error: %v", i, err)
				continue
			}
			gvk := schema.GroupVersionKind{Version: "v1", Kind: "Status"}
			if obj.GetObjectKind().GroupVersionKind() != gvk {
				t.Errorf("%d: unexpected error body: %#v", i, obj)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%d: invalid status: %#v\n%s", i, resp, bodyOrDie(resp))
			continue
		}
		body := ""
		if test.expected != nil {
			itemOut, d, err := extractBodyObject(resp, metainternalversionscheme.Codecs.LegacyCodec(metav1beta1.SchemeGroupVersion))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(test.expected, itemOut) {
				t.Errorf("%d: did not match: %s", i, cmp.Diff(test.expected, itemOut))
			}
			body = d
		} else {
			d, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			body = string(d)
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal([]byte(body), obj); err != nil {
			t.Fatal(err)
		}
		if obj.GetObjectKind().GroupVersionKind() != test.expectKind {
			t.Errorf("%d: unexpected kind: %#v", i, obj.GetObjectKind().GroupVersionKind())
		}
	}
}

func TestGetBinary(t *testing.T) {
	simpleStorage := SimpleRESTStorage{
		stream: &SimpleStream{
			contentType: "text/plain",
			Reader:      bytes.NewBufferString("response data"),
		},
	}
	stream := simpleStorage.stream
	server := httptest.NewServer(handle(map[string]rest.Storage{"simple": &simpleStorage}))
	defer server.Close()

	req, err := http.NewRequest("GET", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/binary", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req.Header.Add("Accept", "text/other, */*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %#v", resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !stream.closed || stream.version != testGroupVersion.String() || stream.accept != "text/other, */*" ||
		resp.Header.Get("Content-Type") != stream.contentType || string(body) != "response data" {
		t.Errorf("unexpected stream: %#v", stream)
	}
}

func validateSimpleGetOptionsParams(t *testing.T, route *restful.Route) {
	// Validate name and description
	expectedParams := map[string]string{
		"param1":  "description for param1",
		"param2":  "description for param2",
		"atAPath": "",
	}
	for _, p := range route.ParameterDocs {
		data := p.Data()
		if desc, exists := expectedParams[data.Name]; exists {
			if desc != data.Description {
				t.Errorf("unexpected description for parameter %s: %s\n", data.Name, data.Description)
			}
			delete(expectedParams, data.Name)
		}
	}
	if len(expectedParams) > 0 {
		t.Errorf("did not find all expected parameters: %#v", expectedParams)
	}
}

func TestGetWithOptionsRouteParams(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := GetWithOptionsRESTStorage{
		SimpleRESTStorage: &SimpleRESTStorage{},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	ws := handler.(*defaultAPIServer).container.RegisteredWebServices()
	if len(ws) == 0 {
		t.Fatal("no web services registered")
	}
	routes := ws[0].Routes()
	for i := range routes {
		if routes[i].Method == "GET" && routes[i].Operation == "readNamespacedSimple" {
			validateSimpleGetOptionsParams(t, &routes[i])
			break
		}
	}
}

func TestGetWithOptions(t *testing.T) {

	tests := []struct {
		name         string
		rootScoped   bool
		requestURL   string
		expectedPath string
	}{
		{
			name:         "basic",
			requestURL:   "/namespaces/default/simple/id?param1=test1&param2=test2",
			expectedPath: "",
		},
		{
			name:         "with root slash",
			requestURL:   "/namespaces/default/simple/id/?param1=test1&param2=test2",
			expectedPath: "/",
		},
		{
			name:         "with path",
			requestURL:   "/namespaces/default/simple/id/a/different/path?param1=test1&param2=test2",
			expectedPath: "/a/different/path",
		},
		{
			name:         "with path with trailing slash",
			requestURL:   "/namespaces/default/simple/id/a/different/path/?param1=test1&param2=test2",
			expectedPath: "/a/different/path/",
		},
		{
			name:         "as subresource",
			requestURL:   "/namespaces/default/simple/id/subresource/another/different/path?param1=test1&param2=test2",
			expectedPath: "/another/different/path",
		},
		{
			name:         "cluster-scoped basic",
			rootScoped:   true,
			requestURL:   "/simple/id?param1=test1&param2=test2",
			expectedPath: "",
		},
		{
			name:         "cluster-scoped basic with path",
			rootScoped:   true,
			requestURL:   "/simple/id/a/cluster/path?param1=test1&param2=test2",
			expectedPath: "/a/cluster/path",
		},
		{
			name:         "cluster-scoped basic as subresource",
			rootScoped:   true,
			requestURL:   "/simple/id/subresource/another/cluster/path?param1=test1&param2=test2",
			expectedPath: "/another/cluster/path",
		},
	}

	for _, test := range tests {
		simpleStorage := GetWithOptionsRESTStorage{
			SimpleRESTStorage: &SimpleRESTStorage{
				item: genericapitesting.Simple{
					Other: "foo",
				},
			},
			takesPath: "atAPath",
		}
		simpleRootStorage := GetWithOptionsRootRESTStorage{
			SimpleTypedStorage: &SimpleTypedStorage{
				baseType: &genericapitesting.SimpleRoot{}, // a root scoped type
				item: &genericapitesting.SimpleRoot{
					Other: "foo",
				},
			},
			takesPath: "atAPath",
		}

		storage := map[string]rest.Storage{}
		if test.rootScoped {
			storage["simple"] = &simpleRootStorage
			storage["simple/subresource"] = &simpleRootStorage
		} else {
			storage["simple"] = &simpleStorage
			storage["simple/subresource"] = &simpleStorage
		}
		handler := handle(storage)
		server := httptest.NewServer(handler)
		defer server.Close()

		resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + test.requestURL)
		if err != nil {
			t.Errorf("%s: %v", test.name, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: unexpected response: %#v", test.name, resp)
			continue
		}

		var itemOut runtime.Object
		if test.rootScoped {
			itemOut = &genericapitesting.SimpleRoot{}
		} else {
			itemOut = &genericapitesting.Simple{}
		}
		body, err := extractBody(resp, itemOut)
		if err != nil {
			t.Errorf("%s: %v", test.name, err)
			continue
		}
		if metadata, err := meta.Accessor(itemOut); err == nil {
			if metadata.GetName() != simpleStorage.item.Name {
				t.Errorf("%s: Unexpected data: %#v, expected %#v (%s)", test.name, itemOut, simpleStorage.item, string(body))
				continue
			}
		} else {
			t.Errorf("%s: Couldn't get name from %#v: %v", test.name, itemOut, err)
		}

		var opts *genericapitesting.SimpleGetOptions
		var ok bool
		if test.rootScoped {
			opts, ok = simpleRootStorage.optionsReceived.(*genericapitesting.SimpleGetOptions)
		} else {
			opts, ok = simpleStorage.optionsReceived.(*genericapitesting.SimpleGetOptions)

		}
		if !ok {
			t.Errorf("%s: Unexpected options object received: %#v", test.name, simpleStorage.optionsReceived)
			continue
		}
		if opts.Param1 != "test1" || opts.Param2 != "test2" {
			t.Errorf("%s: Did not receive expected options: %#v", test.name, opts)
			continue
		}
		if opts.Path != test.expectedPath {
			t.Errorf("%s: Unexpected path value. Expected: %s. Actual: %s.", test.name, test.expectedPath, opts.Path)
			continue
		}
	}
}

func TestGetMissing(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"get": apierrors.NewNotFound(schema.GroupResource{Resource: "simples"}, "id")},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/id")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", resp)
	}
}

func TestGetRetryAfter(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"get": apierrors.NewServerTimeout(schema.GroupResource{Resource: "simples"}, "id", 2)},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/id")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Unexpected response %#v", resp)
	}
	if resp.Header.Get("Retry-After") != "2" {
		t.Errorf("Unexpected Retry-After header: %v", resp.Header)
	}
}

func TestConnect(t *testing.T) {
	responseText := "Hello World"
	itemID := "theID"
	connectStorage := &ConnecterRESTStorage{
		connectHandler: &OutputConnect{
			response: responseText,
		},
	}
	storage := map[string]rest.Storage{
		"simple":         &SimpleRESTStorage{},
		"simple/connect": connectStorage,
	}
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/" + itemID + "/connect")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %#v", resp)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if connectStorage.receivedID != itemID {
		t.Errorf("Unexpected item id. Expected: %s. Actual: %s.", itemID, connectStorage.receivedID)
	}
	if string(body) != responseText {
		t.Errorf("Unexpected response. Expected: %s. Actual: %s.", responseText, string(body))
	}
}

func TestConnectResponderObject(t *testing.T) {
	itemID := "theID"
	simple := &genericapitesting.Simple{Other: "foo"}
	connectStorage := &ConnecterRESTStorage{}
	connectStorage.handlerFunc = func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			connectStorage.receivedResponder.Object(http.StatusCreated, simple)
		})
	}
	storage := map[string]rest.Storage{
		"simple":         &SimpleRESTStorage{},
		"simple/connect": connectStorage,
	}
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/" + itemID + "/connect")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("unexpected response: %#v", resp)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if connectStorage.receivedID != itemID {
		t.Errorf("Unexpected item id. Expected: %s. Actual: %s.", itemID, connectStorage.receivedID)
	}
	obj, err := runtime.Decode(codec, body)
	if err != nil {
		t.Fatal(err)
	}
	if !apiequality.Semantic.DeepEqual(obj, simple) {
		t.Errorf("Unexpected response: %#v", obj)
	}
}

func TestConnectResponderError(t *testing.T) {
	itemID := "theID"
	connectStorage := &ConnecterRESTStorage{}
	connectStorage.handlerFunc = func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			connectStorage.receivedResponder.Error(apierrors.NewForbidden(schema.GroupResource{Resource: "simples"}, itemID, errors.New("you are terminated")))
		})
	}
	storage := map[string]rest.Storage{
		"simple":         &SimpleRESTStorage{},
		"simple/connect": connectStorage,
	}
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/" + itemID + "/connect")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unexpected response: %#v", resp)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if connectStorage.receivedID != itemID {
		t.Errorf("Unexpected item id. Expected: %s. Actual: %s.", itemID, connectStorage.receivedID)
	}
	obj, err := runtime.Decode(codec, body)
	if err != nil {
		t.Fatal(err)
	}
	if obj.(*metav1.Status).Code != http.StatusForbidden {
		t.Errorf("Unexpected response: %#v", obj)
	}
}

func TestConnectWithOptionsRouteParams(t *testing.T) {
	connectStorage := &ConnecterRESTStorage{
		connectHandler:      &OutputConnect{},
		emptyConnectOptions: &genericapitesting.SimpleGetOptions{},
	}
	storage := map[string]rest.Storage{
		"simple":         &SimpleRESTStorage{},
		"simple/connect": connectStorage,
	}
	handler := handle(storage)
	ws := handler.(*defaultAPIServer).container.RegisteredWebServices()
	if len(ws) == 0 {
		t.Fatal("no web services registered")
	}
	routes := ws[0].Routes()
	for i := range routes {
		switch routes[i].Operation {
		case "connectGetNamespacedSimpleConnect":
		case "connectPostNamespacedSimpleConnect":
		case "connectPutNamespacedSimpleConnect":
		case "connectDeleteNamespacedSimpleConnect":
			validateSimpleGetOptionsParams(t, &routes[i])

		}
	}
}

func TestConnectWithOptions(t *testing.T) {
	responseText := "Hello World"
	itemID := "theID"
	connectStorage := &ConnecterRESTStorage{
		connectHandler: &OutputConnect{
			response: responseText,
		},
		emptyConnectOptions: &genericapitesting.SimpleGetOptions{},
	}
	storage := map[string]rest.Storage{
		"simple":         &SimpleRESTStorage{},
		"simple/connect": connectStorage,
	}
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/" + itemID + "/connect?param1=value1&param2=value2")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %#v", resp)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if connectStorage.receivedID != itemID {
		t.Errorf("Unexpected item id. Expected: %s. Actual: %s.", itemID, connectStorage.receivedID)
	}
	if string(body) != responseText {
		t.Errorf("Unexpected response. Expected: %s. Actual: %s.", responseText, string(body))
	}
	if connectStorage.receivedResponder == nil {
		t.Errorf("Unexpected responder")
	}
	opts, ok := connectStorage.receivedConnectOptions.(*genericapitesting.SimpleGetOptions)
	if !ok {
		t.Fatalf("Unexpected options type: %#v", connectStorage.receivedConnectOptions)
	}
	if opts.Param1 != "value1" && opts.Param2 != "value2" {
		t.Errorf("Unexpected options value: %#v", opts)
	}
}

func TestConnectWithOptionsAndPath(t *testing.T) {
	responseText := "Hello World"
	itemID := "theID"
	testPath := "/a/b/c/def"
	connectStorage := &ConnecterRESTStorage{
		connectHandler: &OutputConnect{
			response: responseText,
		},
		emptyConnectOptions: &genericapitesting.SimpleGetOptions{},
		takesPath:           "atAPath",
	}
	storage := map[string]rest.Storage{
		"simple":         &SimpleRESTStorage{},
		"simple/connect": connectStorage,
	}
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/" + itemID + "/connect" + testPath + "?param1=value1&param2=value2")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %#v", resp)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if connectStorage.receivedID != itemID {
		t.Errorf("Unexpected item id. Expected: %s. Actual: %s.", itemID, connectStorage.receivedID)
	}
	if string(body) != responseText {
		t.Errorf("Unexpected response. Expected: %s. Actual: %s.", responseText, string(body))
	}
	opts, ok := connectStorage.receivedConnectOptions.(*genericapitesting.SimpleGetOptions)
	if !ok {
		t.Fatalf("Unexpected options type: %#v", connectStorage.receivedConnectOptions)
	}
	if opts.Param1 != "value1" && opts.Param2 != "value2" {
		t.Errorf("Unexpected options value: %#v", opts)
	}
	if opts.Path != testPath {
		t.Errorf("Unexpected path value. Expected: %s. Actual: %s.", testPath, opts.Path)
	}
}

func TestDelete(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	res, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %#v", res)
	}
	if simpleStorage.deleted != ID {
		t.Errorf("Unexpected delete: %s, expected %s", simpleStorage.deleted, ID)
	}
}

func TestDeleteWithOptions(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	grace := int64(300)
	item := &metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}
	body, err := runtime.Encode(codec, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	res, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %s %#v", request.URL, res)
		s, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Log(string(s))
	}
	if simpleStorage.deleted != ID {
		t.Errorf("Unexpected delete: %s, expected %s", simpleStorage.deleted, ID)
	}
	simpleStorage.deleteOptions.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	if !apiequality.Semantic.DeepEqual(simpleStorage.deleteOptions, item) {
		t.Errorf("unexpected delete options: %s", cmp.Diff(simpleStorage.deleteOptions, item))
	}
}

func TestDeleteWithOptionsQuery(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	grace := int64(300)
	item := &metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID+"?gracePeriodSeconds="+strconv.FormatInt(grace, 10), nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	res, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %s %#v", request.URL, res)
		s, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Log(string(s))
	}
	if simpleStorage.deleted != ID {
		t.Fatalf("Unexpected delete: %s, expected %s", simpleStorage.deleted, ID)
	}
	simpleStorage.deleteOptions.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	if !apiequality.Semantic.DeepEqual(simpleStorage.deleteOptions, item) {
		t.Errorf("unexpected delete options: %s", cmp.Diff(simpleStorage.deleteOptions, item))
	}
}

func TestDeleteWithOptionsQueryAndBody(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	grace := int64(300)
	item := &metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}
	body, err := runtime.Encode(codec, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID+"?gracePeriodSeconds="+strconv.FormatInt(grace+10, 10), bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	res, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %s %#v", request.URL, res)
		s, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Log(string(s))
	}
	if simpleStorage.deleted != ID {
		t.Errorf("Unexpected delete: %s, expected %s", simpleStorage.deleted, ID)
	}
	simpleStorage.deleteOptions.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	if !apiequality.Semantic.DeepEqual(simpleStorage.deleteOptions, item) {
		t.Errorf("unexpected delete options: %s", cmp.Diff(simpleStorage.deleteOptions, item))
	}
}

func TestDeleteInvokesAdmissionControl(t *testing.T) {
	// TODO: remove mutating deny when we removed it from the endpoint implementation and ported all plugins
	for _, admit := range []admission.Interface{alwaysMutatingDeny{}, alwaysValidatingDeny{}} {
		t.Logf("Testing %T", admit)

		storage := map[string]rest.Storage{}
		simpleStorage := SimpleRESTStorage{}
		ID := "id"
		storage["simple"] = &simpleStorage
		handler := handleInternal(storage, admit, nil)
		server := httptest.NewServer(handler)
		defer server.Close()

		client := http.Client{}
		request, err := http.NewRequest("DELETE", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("Unexpected response %#v", response)
		}
	}
}

func TestDeleteMissing(t *testing.T) {
	storage := map[string]rest.Storage{}
	ID := "id"
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"delete": apierrors.NewNotFound(schema.GroupResource{Resource: "simples"}, ID)},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdate(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ID,
			Namespace: "", // update should allow the client to send an empty namespace
		},
		Other: "bar",
	}
	body, err := runtime.Encode(testCodec, item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	dump, _ := httputil.DumpResponse(response, true)
	t.Log(string(dump))

	if simpleStorage.updated == nil || simpleStorage.updated.Name != item.Name {
		t.Errorf("Unexpected update value %#v, expected %#v.", simpleStorage.updated, item)
	}
}

func TestUpdateInvokesAdmissionControl(t *testing.T) {
	for _, admit := range []admission.Interface{alwaysMutatingDeny{}, alwaysValidatingDeny{}} {
		t.Logf("Testing %T", admit)

		storage := map[string]rest.Storage{}
		simpleStorage := SimpleRESTStorage{}
		ID := "id"
		storage["simple"] = &simpleStorage
		handler := handleInternal(storage, admit, nil)
		server := httptest.NewServer(handler)
		defer server.Close()

		item := &genericapitesting.Simple{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ID,
				Namespace: metav1.NamespaceDefault,
			},
			Other: "bar",
		}
		body, err := runtime.Encode(testCodec, item)
		if err != nil {
			// The following cases will fail, so die now
			t.Fatalf("unexpected error: %v", err)
		}

		client := http.Client{}
		request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		dump, _ := httputil.DumpResponse(response, true)
		t.Log(string(dump))

		if response.StatusCode != http.StatusForbidden {
			t.Errorf("Unexpected response %#v", response)
		}
	}
}

func TestUpdateRequiresMatchingName(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &genericapitesting.Simple{
		Other: "bar",
	}
	body, err := runtime.Encode(testCodec, item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		dump, _ := httputil.DumpResponse(response, true)
		t.Log(string(dump))
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdateAllowsMissingNamespace(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{
			Name: ID,
		},
		Other: "bar",
	}
	body, err := runtime.Encode(testCodec, item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	dump, _ := httputil.DumpResponse(response, true)
	t.Log(string(dump))

	if response.StatusCode != http.StatusOK {
		t.Errorf("Unexpected response %#v", response)
	}
}

// when the object name and namespace can't be retrieved, don't update.  It isn't safe.
func TestUpdateDisallowsMismatchedNamespaceOnError(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ID,
			Namespace: "other", // does not match request
		},
		Other: "bar",
	}
	body, err := runtime.Encode(testCodec, item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	dump, _ := httputil.DumpResponse(response, true)
	t.Log(string(dump))

	if simpleStorage.updated != nil {
		t.Errorf("Unexpected update value %#v.", simpleStorage.updated)
	}
}

func TestUpdatePreventsMismatchedNamespace(t *testing.T) {
	storage := map[string]rest.Storage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ID,
			Namespace: "other",
		},
		Other: "bar",
	}
	body, err := runtime.Encode(testCodec, item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdateMissing(t *testing.T) {
	storage := map[string]rest.Storage{}
	ID := "id"
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"update": apierrors.NewNotFound(schema.GroupResource{Resource: "simples"}, ID)},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &genericapitesting.Simple{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ID,
			Namespace: metav1.NamespaceDefault,
		},
		Other: "bar",
	}
	body, err := runtime.Encode(testCodec, item)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+ID, bytes.NewReader(body))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestCreateNotFound(t *testing.T) {
	handler := handle(map[string]rest.Storage{
		"simple": &SimpleRESTStorage{
			// storage.Create can fail with not found error in theory.
			// See https://pr.k8s.io/486#discussion_r15037092.
			errors: map[string]error{"create": apierrors.NewNotFound(schema.GroupResource{Resource: "simples"}, "id")},
		},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{Other: "foo"}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestCreateChecksDecode(t *testing.T) {
	handler := handle(map[string]rest.Storage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &example.Pod{}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	} else if !strings.Contains(string(b), "cannot be handled as a Simple") {
		t.Errorf("unexpected response: %s", string(b))
	}
}

func TestParentResourceIsRequired(t *testing.T) {
	storage := &SimpleTypedStorage{
		baseType: &genericapitesting.SimpleRoot{}, // a root scoped type
		item:     &genericapitesting.SimpleRoot{},
	}
	group := &APIGroupVersion{
		Storage: map[string]rest.Storage{
			"simple/sub": storage,
		},
		Root:            "/" + prefix,
		Creater:         scheme,
		Convertor:       scheme,
		UnsafeConvertor: runtime.UnsafeObjectConvertor(scheme),
		Defaulter:       scheme,
		Typer:           scheme,
		Namer:           namer,

		EquivalentResourceRegistry: runtime.NewEquivalentResourceRegistry(),

		Admit: admissionControl,

		GroupVersion:           newGroupVersion,
		OptionsExternalVersion: &newGroupVersion,

		Serializer:     codecs,
		ParameterCodec: parameterCodec,
	}
	container := restful.NewContainer()
	if _, _, err := group.InstallREST(container); err == nil {
		t.Fatal("expected error")
	}

	storage = &SimpleTypedStorage{
		baseType: &genericapitesting.SimpleRoot{}, // a root scoped type
		item:     &genericapitesting.SimpleRoot{},
	}
	group = &APIGroupVersion{
		Storage: map[string]rest.Storage{
			"simple":     &SimpleRESTStorage{},
			"simple/sub": storage,
		},
		Root:            "/" + prefix,
		Creater:         scheme,
		Convertor:       scheme,
		UnsafeConvertor: runtime.UnsafeObjectConvertor(scheme),
		TypeConverter:   managedfields.NewDeducedTypeConverter(),
		Defaulter:       scheme,
		Typer:           scheme,
		Namer:           namer,

		EquivalentResourceRegistry: runtime.NewEquivalentResourceRegistry(),

		Admit: admissionControl,

		GroupVersion:           newGroupVersion,
		OptionsExternalVersion: &newGroupVersion,

		Serializer:     codecs,
		ParameterCodec: parameterCodec,
	}
	container = restful.NewContainer()
	if _, _, err := group.InstallREST(container); err != nil {
		t.Fatal(err)
	}

	handler := genericapifilters.WithRequestInfo(container, newTestRequestInfoResolver())

	// resource is NOT registered in the root scope
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, &http.Request{Method: "GET", URL: &url.URL{Path: "/" + prefix + "/simple/test/sub"}})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected not found: %#v", w)
	}

	// resource is registered in the namespace scope
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, &http.Request{Method: "GET", URL: &url.URL{Path: "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/namespaces/test/simple/test/sub"}})
	if w.Code != http.StatusOK {
		t.Fatalf("expected OK: %#v", w)
	}
	if storage.actualNamespace != "test" {
		t.Errorf("namespace should be set %#v", storage)
	}
}

func TestNamedCreaterWithName(t *testing.T) {
	pathName := "helloworld"
	storage := &NamedCreaterRESTStorage{SimpleRESTStorage: &SimpleRESTStorage{}}
	handler := handle(map[string]rest.Storage{
		"simple":     &SimpleRESTStorage{},
		"simple/sub": storage,
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{Other: "foo"}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/"+pathName+"/sub", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected response %#v", response)
	}
	if storage.createdName != pathName {
		t.Errorf("Did not get expected name in create context. Got: %s, Expected: %s", storage.createdName, pathName)
	}
}

func TestNamedCreaterWithoutName(t *testing.T) {
	storage := &NamedCreaterRESTStorage{
		SimpleRESTStorage: &SimpleRESTStorage{
			injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
				time.Sleep(5 * time.Millisecond)
				return obj, nil
			},
		},
	}

	handler := handle(map[string]rest.Storage{"foo": storage})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{
		Other: "bar",
	}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/foo", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// empty name is not allowed for NamedCreater
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
}

type namePopulatorAdmissionControl struct {
	t            *testing.T
	populateName string
}

func (npac *namePopulatorAdmissionControl) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	if a.GetName() != npac.populateName {
		npac.t.Errorf("Unexpected name: got %q, expected %q", a.GetName(), npac.populateName)
	}
	return nil
}

func (npac *namePopulatorAdmissionControl) Handles(operation admission.Operation) bool {
	return true
}

var _ admission.ValidationInterface = &namePopulatorAdmissionControl{}

func TestNamedCreaterWithGenerateName(t *testing.T) {
	populateName := "bar"
	storage := &SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			if metadata, err := meta.Accessor(obj); err == nil {
				if len(metadata.GetName()) != 0 {
					t.Errorf("Unexpected name %q", metadata.GetName())
				}
				metadata.SetName(populateName)
			} else {
				return nil, err
			}
			return obj, nil
		},
	}

	ac := &namePopulatorAdmissionControl{
		t:            t,
		populateName: populateName,
	}

	handler := handleInternal(map[string]rest.Storage{"foo": storage}, ac, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{
		Other: "bar",
	}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/foo", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusOK, response)
	}

	var itemOut genericapitesting.Simple
	body, err := extractBody(response, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v %#v", err, response)
	}

	// Avoid comparing managed fields in expected result
	itemOut.ManagedFields = nil
	itemOut.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	simple.Name = populateName
	simple.Namespace = "default" // populated by create handler to match request URL
	if !reflect.DeepEqual(&itemOut, simple) {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simple, string(body))
	}
}

func TestUpdateChecksDecode(t *testing.T) {
	handler := handle(map[string]rest.Storage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &example.Pod{}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/bar", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v\n%s", response, readBodyOrDie(response.Body))
	}
	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	} else if !strings.Contains(string(b), "cannot be handled as a Simple") {
		t.Errorf("unexpected response: %s", string(b))
	}
}

func TestCreate(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			return obj, nil
		},
	}
	handler := handle(map[string]rest.Storage{"foo": &storage})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{
		Other: "bar",
	}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/foo", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	var itemOut genericapitesting.Simple
	body, err := extractBody(response, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v %#v", err, response)
	}

	// Avoid comparing managed fields in expected result
	itemOut.ManagedFields = nil
	itemOut.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	simple.Namespace = "default" // populated by create handler to match request URL
	if !reflect.DeepEqual(&itemOut, simple) {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simple, string(body))
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusOK, response)
	}
}

func TestCreateYAML(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			return obj, nil
		},
	}
	handler := handle(map[string]rest.Storage{"foo": &storage})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	// yaml encoder
	simple := &genericapitesting.Simple{
		Other: "bar",
	}
	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/yaml")
	if !ok {
		t.Fatal("No yaml serializer")
	}
	encoder := codecs.EncoderForVersion(info.Serializer, testGroupVersion)
	decoder := codecs.DecoderToVersion(info.Serializer, testInternalGroupVersion)

	data, err := runtime.Encode(encoder, simple)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/foo", bytes.NewBuffer(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	request.Header.Set("Accept", "application/yaml, application/json")
	request.Header.Set("Content-Type", "application/yaml")

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var itemOut genericapitesting.Simple
	body, err := extractBodyDecoder(response, &itemOut, decoder)
	if err != nil {
		t.Fatalf("unexpected error: %v %#v", err, response)
	}

	// Avoid comparing managed fields in expected result
	itemOut.ManagedFields = nil
	itemOut.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	simple.Namespace = "default" // populated by create handler to match request URL
	if !reflect.DeepEqual(&itemOut, simple) {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simple, string(body))
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusOK, response)
	}
}

func TestCreateInNamespace(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			return obj, nil
		},
	}
	handler := handle(map[string]rest.Storage{"foo": &storage})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{
		Other: "bar",
	}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/other/foo", bytes.NewBuffer(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var itemOut genericapitesting.Simple
	body, err := extractBody(response, &itemOut)
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, data)
	}

	// Avoid comparing managed fields in expected result
	itemOut.ManagedFields = nil
	itemOut.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	simple.Namespace = "other" // populated by create handler to match request URL
	if !reflect.DeepEqual(&itemOut, simple) {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simple, string(body))
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusOK, response)
	}
}

func TestCreateInvokeAdmissionControl(t *testing.T) {
	for _, admit := range []admission.Interface{alwaysMutatingDeny{}, alwaysValidatingDeny{}} {
		t.Logf("Testing %T", admit)

		storage := SimpleRESTStorage{
			injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
				time.Sleep(5 * time.Millisecond)
				return obj, nil
			},
		}
		handler := handleInternal(map[string]rest.Storage{"foo": &storage}, admit, nil)
		server := httptest.NewServer(handler)
		defer server.Close()
		client := http.Client{}

		simple := &genericapitesting.Simple{
			Other: "bar",
		}
		data, err := runtime.Encode(testCodec, simple)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/other/foo", bytes.NewBuffer(data))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		var response *http.Response
		response, err = client.Do(request)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusForbidden, response)
		}
	}
}

func expectAPIStatus(t *testing.T, method, url string, data []byte, code int) *metav1.Status {
	t.Helper()
	client := http.Client{}
	request, err := http.NewRequest(method, url, bytes.NewBuffer(data))
	if err != nil {
		t.Fatalf("unexpected error %#v", err)
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error on %s %s: %v", method, url, err)
		return nil
	}
	var status metav1.Status
	body, err := extractBody(response, &status)
	if err != nil {
		t.Fatalf("unexpected error on %s %s: %v\nbody:\n%s", method, url, err, body)
		return nil
	}
	if code != response.StatusCode {
		t.Fatalf("Expected %s %s to return %d, Got %d: %v", method, url, code, response.StatusCode, body)
	}
	return &status
}

func TestDelayReturnsError(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "foos"}, "bar")
		},
	}
	handler := handle(map[string]rest.Storage{"foo": &storage})
	server := httptest.NewServer(handler)
	defer server.Close()

	status := expectAPIStatus(t, "DELETE", fmt.Sprintf("%s/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/foo/bar", server.URL), nil, http.StatusConflict)
	if status.Status != metav1.StatusFailure || status.Message == "" || status.Details == nil || status.Reason != metav1.StatusReasonAlreadyExists {
		t.Errorf("Unexpected status %#v", status)
	}
}

type UnregisteredAPIObject struct {
	Value string
}

func (obj *UnregisteredAPIObject) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}
func (obj *UnregisteredAPIObject) DeepCopyObject() runtime.Object {
	if obj == nil {
		return nil
	}
	clone := *obj
	return &clone
}

func TestWriteJSONDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		responsewriters.WriteObjectNegotiated(codecs, negotiation.DefaultEndpointRestrictions, newGroupVersion, w, req, http.StatusOK, &UnregisteredAPIObject{"Undecodable"}, false)
	}))
	defer server.Close()
	// Decode error response behavior is dictated by
	// apiserver/pkg/endpoints/handlers/responsewriters/status.go::ErrorToAPIStatus().
	// Unless specific metav1.Status() parameters are implemented for the particular error in question, such that
	// the status code is defined, metav1 errors where error.status == metav1.StatusFailure
	// will throw a '500 Internal Server Error'. Non-metav1 type errors will always throw a '500 Internal Server Error'.
	status := expectAPIStatus(t, "GET", server.URL, nil, http.StatusInternalServerError)
	if status.Reason != metav1.StatusReasonUnknown {
		t.Errorf("unexpected reason %#v", status)
	}
	if !strings.Contains(status.Message, "no kind is registered for the type endpoints.UnregisteredAPIObject") {
		t.Errorf("unexpected message %#v", status)
	}
}

type marshalError struct {
	err error
}

func (m *marshalError) MarshalJSON() ([]byte, error) {
	return []byte{}, m.err
}

func TestWriteRAWJSONMarshalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		responsewriters.WriteRawJSON(http.StatusOK, &marshalError{errors.New("Undecodable")}, w)
	}))
	defer server.Close()
	client := http.Client{}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("unexpected status code %d", resp.StatusCode)
	}
}

func TestCreateTimeout(t *testing.T) {
	testOver := make(chan struct{})
	defer close(testOver)
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			// Eliminate flakes by ensuring the create operation takes longer than this test.
			<-testOver
			return obj, nil
		},
	}
	handler := handle(map[string]rest.Storage{
		"foo": &storage,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	simple := &genericapitesting.Simple{Other: "foo"}
	data, err := runtime.Encode(testCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	itemOut := expectAPIStatus(t, "POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/foo?timeout=4ms", data, http.StatusGatewayTimeout)
	if itemOut.Status != metav1.StatusFailure || itemOut.Reason != metav1.StatusReasonTimeout {
		t.Errorf("Unexpected status %#v", itemOut)
	}
}

func TestCreateChecksAPIVersion(t *testing.T) {
	handler := handle(map[string]rest.Storage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{}
	//using newCodec and send the request to testVersion URL shall cause a discrepancy in apiVersion
	data, err := runtime.Encode(newCodec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	} else if !strings.Contains(string(b), "does not match the expected API version") {
		t.Errorf("unexpected response: %s", string(b))
	}
}

func TestCreateDefaultsAPIVersion(t *testing.T) {
	handler := handle(map[string]rest.Storage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{}
	data, err := runtime.Encode(codec, simple)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	m := make(map[string]interface{})
	if err := json.Unmarshal(data, &m); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	delete(m, "apiVersion")
	data, err = json.Marshal(m)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	request, err := http.NewRequest("POST", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusCreated, response)
	}
}

func TestUpdateChecksAPIVersion(t *testing.T) {
	handler := handle(map[string]rest.Storage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &genericapitesting.Simple{ObjectMeta: metav1.ObjectMeta{Name: "bar"}}
	data, err := runtime.Encode(newCodec, simple)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	request, err := http.NewRequest("PUT", server.URL+"/"+prefix+"/"+testGroupVersion.Group+"/"+testGroupVersion.Version+"/namespaces/default/simple/bar", bytes.NewBuffer(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	} else if !strings.Contains(string(b), "does not match the expected API version") {
		t.Errorf("unexpected response: %s", string(b))
	}
}

// runRequest is used by TestDryRun since it runs the test twice in a
// row with a slightly different URL (one has ?dryRun, one doesn't).
func runRequest(t testing.TB, path, verb string, data []byte, contentType string) *http.Response {
	request, err := http.NewRequest(verb, path, bytes.NewBuffer(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return response
}

type SimpleRESTStorageWithDeleteCollection struct {
	SimpleRESTStorage
}

// Delete collection doesn't do much, but let us test this path.
func (storage *SimpleRESTStorageWithDeleteCollection) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	storage.checkContext(ctx)
	return nil, nil
}

// shared vars used by both TestFieldValidation and BenchmarkFieldValidation
var (
	strictFieldValidation = "?fieldValidation=Strict"
	warnFieldValidation   = "?fieldValidation=Warn"
	ignoreFieldValidation = "?fieldValidation=Ignore"
)

// TestFieldValidation tests the create, update, and patch handlers for correctness when faced with field validation errors.
func TestFieldValidation(t *testing.T) {
	var (
		strictDecodingErr          = `strict decoding error: duplicate field \"other\", unknown field \"unknown\"`
		strictDecodingWarns        = []string{`duplicate field "other"`, `unknown field "unknown"`}
		strictDecodingErrYAML      = `strict decoding error: yaml: unmarshal errors:\n  line 6: key \"other\" already set in map, unknown field \"unknown\"`
		strictDecodingWarnsYAML    = []string{`line 6: key "other" already set in map`, `unknown field "unknown"`}
		strictDecodingErrYAMLPut   = `strict decoding error: yaml: unmarshal errors:\n  line 7: key \"other\" already set in map, unknown field \"unknown\"`
		strictDecodingWarnsYAMLPut = []string{`line 7: key "other" already set in map`, `unknown field "unknown"`}

		invalidJSONDataPost = []byte(`{"kind":"Simple", "apiVersion":"test.group/version", "metadata":{"creationTimestamp":null}, "other":"foo","other":"bar","unknown":"baz"}`)
		invalidYAMLDataPost = []byte(`apiVersion: test.group/version
kind: Simple
metadata:
  creationTimestamp: null
other: foo
other: bar
unknown: baz`)

		invalidJSONDataPut = []byte(`{"kind":"Simple", "apiVersion":"test.group/version", "metadata":{"name":"id", "creationTimestamp":null}, "other":"foo","other":"bar","unknown":"baz"}`)
		invalidYAMLDataPut = []byte(`apiVersion: test.group/version
kind: Simple
metadata:
  name: id
  creationTimestamp: null
other: foo
other: bar
unknown: baz`)

		invalidMergePatch = []byte(`{"labels":{"foo":"bar"}, "unknown": "foo", "other": "foo", "other": "bar"}`)
		invalidJSONPatch  = []byte(`
[
	{"op": "add", "path": "/unknown", "value": "foo"},
	{"op": "add", "path": "/other", "value": "foo"},
	{"op": "add", "path": "/other", "value": "bar"}
	]
	`)
		// note: duplicate fields in the patch itself
		// are dropped by the
		// evanphx/json-patch library and is expected.
		jsonPatchStrictDecodingErr   = `strict decoding error: unknown field \"unknown\"`
		jsonPatchStrictDecodingWarns = []string{`unknown field "unknown"`}

		invalidSMP = []byte(`{"unknown": "foo", "other":"foo", "other": "bar"}`)

		fieldValidationTests = []struct {
			name               string
			path               string
			verb               string
			data               []byte
			queryParams        string
			contentType        string
			expectedErr        string
			expectedWarns      []string
			expectedStatusCode int
		}{
			// Create
			{name: "post-strict-validation", path: "/namespaces/default/simples", verb: "POST", data: invalidJSONDataPost, queryParams: strictFieldValidation, expectedStatusCode: http.StatusBadRequest, expectedErr: strictDecodingErr},
			{name: "post-warn-validation", path: "/namespaces/default/simples", verb: "POST", data: invalidJSONDataPost, queryParams: warnFieldValidation, expectedStatusCode: http.StatusCreated, expectedWarns: strictDecodingWarns},
			{name: "post-ignore-validation", path: "/namespaces/default/simples", verb: "POST", data: invalidJSONDataPost, queryParams: ignoreFieldValidation, expectedStatusCode: http.StatusCreated},

			{name: "post-strict-validation-yaml", path: "/namespaces/default/simples", verb: "POST", data: invalidYAMLDataPost, queryParams: strictFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusBadRequest, expectedErr: strictDecodingErrYAML},
			{name: "post-warn-validation-yaml", path: "/namespaces/default/simples", verb: "POST", data: invalidYAMLDataPost, queryParams: warnFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusCreated, expectedWarns: strictDecodingWarnsYAML},
			{name: "post-ignore-validation-yaml", path: "/namespaces/default/simples", verb: "POST", data: invalidYAMLDataPost, queryParams: ignoreFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusCreated},

			// Update
			{name: "put-strict-validation", path: "/namespaces/default/simples/id", verb: "PUT", data: invalidJSONDataPut, queryParams: strictFieldValidation, expectedStatusCode: http.StatusBadRequest, expectedErr: strictDecodingErr},
			{name: "put-warn-validation", path: "/namespaces/default/simples/id", verb: "PUT", data: invalidJSONDataPut, queryParams: warnFieldValidation, expectedStatusCode: http.StatusOK, expectedWarns: strictDecodingWarns},
			{name: "put-ignore-validation", path: "/namespaces/default/simples/id", verb: "PUT", data: invalidJSONDataPut, queryParams: ignoreFieldValidation, expectedStatusCode: http.StatusOK},

			{name: "put-strict-validation-yaml", path: "/namespaces/default/simples/id", verb: "PUT", data: invalidYAMLDataPut, queryParams: strictFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusBadRequest, expectedErr: strictDecodingErrYAMLPut},
			{name: "put-warn-validation-yaml", path: "/namespaces/default/simples/id", verb: "PUT", data: invalidYAMLDataPut, queryParams: warnFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusOK, expectedWarns: strictDecodingWarnsYAMLPut},
			{name: "put-ignore-validation-yaml", path: "/namespaces/default/simples/id", verb: "PUT", data: invalidYAMLDataPut, queryParams: ignoreFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusOK},

			// MergePatch
			{name: "merge-patch-strict-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidMergePatch, queryParams: strictFieldValidation, contentType: "application/merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusUnprocessableEntity, expectedErr: strictDecodingErr},
			{name: "merge-patch-warn-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidMergePatch, queryParams: warnFieldValidation, contentType: "application/merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK, expectedWarns: strictDecodingWarns},
			{name: "merge-patch-ignore-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidMergePatch, queryParams: ignoreFieldValidation, contentType: "application/merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},

			// JSON Patch
			{name: "json-patch-strict-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidJSONPatch, queryParams: strictFieldValidation, contentType: "application/json-patch+json; charset=UTF-8", expectedStatusCode: http.StatusUnprocessableEntity, expectedErr: jsonPatchStrictDecodingErr},
			{name: "json-patch-warn-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidJSONPatch, queryParams: warnFieldValidation, contentType: "application/json-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK, expectedWarns: jsonPatchStrictDecodingWarns},
			{name: "json-patch-ignore-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidJSONPatch, queryParams: ignoreFieldValidation, contentType: "application/json-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},

			// SMP
			{name: "strategic-merge-patch-strict-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidSMP, queryParams: strictFieldValidation, contentType: "application/strategic-merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusUnprocessableEntity, expectedErr: strictDecodingErr},
			{name: "strategic-merge-patch-warn-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidSMP, queryParams: warnFieldValidation, contentType: "application/strategic-merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK, expectedWarns: strictDecodingWarns},
			{name: "strategic-merge-patch-ignore-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: invalidSMP, queryParams: ignoreFieldValidation, contentType: "application/strategic-merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
		}
	)

	server := httptest.NewServer(handleWithWarnings(map[string]rest.Storage{
		"simples": &SimpleRESTStorageWithDeleteCollection{
			SimpleRESTStorage{
				item: genericapitesting.Simple{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "id",
						Namespace: "",
						UID:       "uid",
					},
					Other: "baz",
				},
			},
		},
		"simples/subsimple": &SimpleXGSubresourceRESTStorage{
			item: genericapitesting.SimpleXGSubresource{
				SubresourceInfo: "foo",
			},
			itemGVK: testGroup2Version.WithKind("SimpleXGSubresource"),
		},
	}))
	defer server.Close()
	for _, test := range fieldValidationTests {
		t.Run(test.name, func(t *testing.T) {
			baseURL := server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version
			response := runRequest(t, baseURL+test.path+test.queryParams, test.verb, test.data, test.contentType)
			buf := new(bytes.Buffer)
			buf.ReadFrom(response.Body)

			if response.StatusCode != test.expectedStatusCode || !strings.Contains(buf.String(), test.expectedErr) {
				t.Fatalf("unexpected response: %#v, expected err: %#v", response, test.expectedErr)
			}

			warnings, _ := net.ParseWarningHeaders(response.Header["Warning"])
			if len(warnings) != len(test.expectedWarns) {
				t.Fatalf("unexpected number of warnings. Got count %d, expected %d. Got warnings %#v, expected %#v", len(warnings), len(test.expectedWarns), warnings, test.expectedWarns)

			}
			for i, warn := range warnings {
				if warn.Text != test.expectedWarns[i] {
					t.Fatalf("unexpected warning: %#v, expected warning: %#v", warn.Text, test.expectedWarns[i])
				}
			}
		})
	}
}

// BenchmarkFieldValidation benchmarks the create, update, and patch handlers for performance distinctions between
// strict, warn, and ignore field validation handling.
func BenchmarkFieldValidation(b *testing.B) {
	var (
		validJSONDataPost = []byte(`{"kind":"Simple", "apiVersion":"test.group/version", "metadata":{"creationTimestamp":null}, "other":"foo"}`)
		validYAMLDataPost = []byte(`apiVersion: test.group/version
kind: Simple
metadata:
  creationTimestamp: null
other: foo`)

		validJSONDataPut = []byte(`{"kind":"Simple", "apiVersion":"test.group/version", "metadata":{"name":"id", "creationTimestamp":null}, "other":"bar"}`)
		validYAMLDataPut = []byte(`apiVersion: test.group/version
kind: Simple
metadata:
  name: id
  creationTimestamp: null
other: bar`)

		validMergePatch = []byte(`{"labels":{"foo":"bar"}, "other": "bar"}`)
		validJSONPatch  = []byte(`
[
	{"op": "add", "path": "/other", "value": "bar"}
	]
	`)
		validSMP = []byte(`{"other": "bar"}`)

		fieldValidationBenchmarks = []struct {
			name               string
			path               string
			verb               string
			data               []byte
			queryParams        string
			contentType        string
			expectedStatusCode int
		}{
			// Create
			{name: "post-strict-validation", path: "/namespaces/default/simples", verb: "POST", data: validJSONDataPost, queryParams: strictFieldValidation, expectedStatusCode: http.StatusCreated},
			{name: "post-warn-validation", path: "/namespaces/default/simples", verb: "POST", data: validJSONDataPost, queryParams: warnFieldValidation, expectedStatusCode: http.StatusCreated},
			{name: "post-ignore-validation", path: "/namespaces/default/simples", verb: "POST", data: validJSONDataPost, queryParams: ignoreFieldValidation, expectedStatusCode: http.StatusCreated},

			{name: "post-strict-validation-yaml", path: "/namespaces/default/simples", verb: "POST", data: validYAMLDataPost, queryParams: strictFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusCreated},
			{name: "post-warn-validation-yaml", path: "/namespaces/default/simples", verb: "POST", data: validYAMLDataPost, queryParams: warnFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusCreated},
			{name: "post-ignore-validation-yaml", path: "/namespaces/default/simples", verb: "POST", data: validYAMLDataPost, queryParams: ignoreFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusCreated},

			// Update
			{name: "put-strict-validation", path: "/namespaces/default/simples/id", verb: "PUT", data: validJSONDataPut, queryParams: strictFieldValidation, expectedStatusCode: http.StatusOK},
			{name: "put-warn-validation", path: "/namespaces/default/simples/id", verb: "PUT", data: validJSONDataPut, queryParams: warnFieldValidation, expectedStatusCode: http.StatusOK},
			{name: "put-ignore-validation", path: "/namespaces/default/simples/id", verb: "PUT", data: validJSONDataPut, queryParams: ignoreFieldValidation, expectedStatusCode: http.StatusOK},

			{name: "put-strict-validation-yaml", path: "/namespaces/default/simples/id", verb: "PUT", data: validYAMLDataPut, queryParams: strictFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusOK},
			{name: "put-warn-validation-yaml", path: "/namespaces/default/simples/id", verb: "PUT", data: validYAMLDataPut, queryParams: warnFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusOK},
			{name: "put-ignore-validation-yaml", path: "/namespaces/default/simples/id", verb: "PUT", data: validYAMLDataPut, queryParams: ignoreFieldValidation, contentType: "application/yaml", expectedStatusCode: http.StatusOK},

			// MergePatch
			{name: "merge-patch-strict-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validMergePatch, queryParams: strictFieldValidation, contentType: "application/merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
			{name: "merge-patch-warn-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validMergePatch, queryParams: warnFieldValidation, contentType: "application/merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
			{name: "merge-patch-ignore-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validMergePatch, queryParams: ignoreFieldValidation, contentType: "application/merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},

			// JSON Patch
			{name: "json-patch-strict-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validJSONPatch, queryParams: strictFieldValidation, contentType: "application/json-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
			{name: "json-patch-warn-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validJSONPatch, queryParams: warnFieldValidation, contentType: "application/json-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
			{name: "json-patch-ignore-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validJSONPatch, queryParams: ignoreFieldValidation, contentType: "application/json-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},

			// SMP
			{name: "strategic-merge-patch-strict-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validSMP, queryParams: strictFieldValidation, contentType: "application/strategic-merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
			{name: "strategic-merge-patch-warn-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validSMP, queryParams: warnFieldValidation, contentType: "application/strategic-merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
			{name: "strategic-merge-patch-ignore-validation", path: "/namespaces/default/simples/id", verb: "PATCH", data: validSMP, queryParams: ignoreFieldValidation, contentType: "application/strategic-merge-patch+json; charset=UTF-8", expectedStatusCode: http.StatusOK},
		}
	)

	server := httptest.NewServer(handleWithWarnings(map[string]rest.Storage{
		"simples": &SimpleRESTStorageWithDeleteCollection{
			SimpleRESTStorage{
				item: genericapitesting.Simple{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "id",
						Namespace: "",
						UID:       "uid",
					},
					Other: "bar",
				},
			},
		},
		"simples/subsimple": &SimpleXGSubresourceRESTStorage{
			item: genericapitesting.SimpleXGSubresource{
				SubresourceInfo: "foo",
			},
			itemGVK: testGroup2Version.WithKind("SimpleXGSubresource"),
		},
	}))
	defer server.Close()
	for _, test := range fieldValidationBenchmarks {
		b.Run(test.name, func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				baseURL := server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version
				response := runRequest(b, baseURL+test.path+test.queryParams, test.verb, test.data, test.contentType)
				if response.StatusCode != test.expectedStatusCode {
					b.Fatalf("unexpected status code: %d, expected: %d", response.StatusCode, test.expectedStatusCode)
				}
			}
		})
	}
}

type SimpleXGSubresourceRESTStorage struct {
	item    genericapitesting.SimpleXGSubresource
	itemGVK schema.GroupVersionKind
}

var _ = rest.GroupVersionKindProvider(&SimpleXGSubresourceRESTStorage{})

func (storage *SimpleXGSubresourceRESTStorage) New() runtime.Object {
	return &genericapitesting.SimpleXGSubresource{}
}

func (storage *SimpleXGSubresourceRESTStorage) Destroy() {
}

func (storage *SimpleXGSubresourceRESTStorage) Get(ctx context.Context, id string, options *metav1.GetOptions) (runtime.Object, error) {
	return storage.item.DeepCopyObject(), nil
}

func (storage *SimpleXGSubresourceRESTStorage) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	return nil, true, nil
}

func (storage *SimpleXGSubresourceRESTStorage) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	return storage.itemGVK
}

func (storage *SimpleXGSubresourceRESTStorage) GetSingularName() string {
	return "simple"
}

func TestXGSubresource(t *testing.T) {
	container := restful.NewContainer()
	container.Router(restful.CurlyRouter{})
	mux := container.ServeMux

	itemID := "theID"
	subresourceStorage := &SimpleXGSubresourceRESTStorage{
		item: genericapitesting.SimpleXGSubresource{
			SubresourceInfo: "foo",
		},
		itemGVK: testGroup2Version.WithKind("SimpleXGSubresource"),
	}
	storage := map[string]rest.Storage{
		"simple":           &SimpleRESTStorage{},
		"simple/subsimple": subresourceStorage,
	}

	group := APIGroupVersion{
		Storage: storage,

		Creater:         scheme,
		Convertor:       scheme,
		TypeConverter:   managedfields.NewDeducedTypeConverter(),
		UnsafeConvertor: runtime.UnsafeObjectConvertor(scheme),
		Defaulter:       scheme,
		Typer:           scheme,
		Namer:           namer,

		EquivalentResourceRegistry: runtime.NewEquivalentResourceRegistry(),

		ParameterCodec: parameterCodec,

		Admit: admissionControl,

		Root:                   "/" + prefix,
		GroupVersion:           testGroupVersion,
		OptionsExternalVersion: &testGroupVersion,
		Serializer:             codecs,
	}

	if _, _, err := (&group).InstallREST(container); err != nil {
		panic(fmt.Sprintf("unable to install container %s: %v", group.GroupVersion, err))
	}

	server := newTestServer(defaultAPIServer{mux, container})
	defer server.Close()

	resp, err := http.Get(server.URL + "/" + prefix + "/" + testGroupVersion.Group + "/" + testGroupVersion.Version + "/namespaces/default/simple/" + itemID + "/subsimple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %#v", resp)
	}
	var itemOut genericapitesting.SimpleXGSubresource
	body, err := extractBody(resp, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Test if the returned object has the expected group, version and kind
	// We are directly unmarshaling JSON here because TypeMeta cannot be decoded through the
	// installed decoders. TypeMeta cannot be decoded because it is added to the ignored
	// conversion type list in API scheme and hence cannot be converted from input type object
	// to output type object. So it's values don't appear in the decoded output object.
	decoder := json.NewDecoder(strings.NewReader(body))
	var itemFromBody genericapitesting.SimpleXGSubresource
	err = decoder.Decode(&itemFromBody)
	if err != nil {
		t.Errorf("unexpected JSON decoding error: %v", err)
	}
	if want := fmt.Sprintf("%s/%s", testGroup2Version.Group, testGroup2Version.Version); itemFromBody.APIVersion != want {
		t.Errorf("unexpected APIVersion got: %+v want: %+v", itemFromBody.APIVersion, want)
	}
	if itemFromBody.Kind != "SimpleXGSubresource" {
		t.Errorf("unexpected Kind got: %+v want: SimpleXGSubresource", itemFromBody.Kind)
	}

	if itemOut.Name != subresourceStorage.item.Name {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, subresourceStorage.item, string(body))
	}
}

func readBodyOrDie(r io.Reader) []byte {
	body, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return body
}

// BenchmarkUpdateProtobuf measures the cost of processing an update on the server in proto
func BenchmarkUpdateProtobuf(b *testing.B) {
	items := benchmarkItems(b)

	simpleStorage := &SimpleRESTStorage{}
	handler := handle(map[string]rest.Storage{"simples": simpleStorage})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	dest, _ := url.Parse(server.URL)
	dest.Path = "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/namespaces/foo/simples/bar"
	dest.RawQuery = ""

	info, _ := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/vnd.kubernetes.protobuf")
	e := codecs.EncoderForVersion(info.Serializer, newGroupVersion)
	data, err := runtime.Encode(e, &items[0])
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		request, err := http.NewRequest("PUT", dest.String(), bytes.NewReader(data))
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		request.Header.Set("Accept", "application/vnd.kubernetes.protobuf")
		request.Header.Set("Content-Type", "application/vnd.kubernetes.protobuf")
		response, err := client.Do(request)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		if response.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(response.Body)
			b.Fatalf("Unexpected response %#v\n%s", response, body)
		}
		_, _ = io.ReadAll(response.Body)
		response.Body.Close()
	}
	b.StopTimer()
}

func newTestServer(handler http.Handler) *httptest.Server {
	handler = genericapifilters.WithRequestInfo(handler, newTestRequestInfoResolver())
	return httptest.NewServer(handler)
}

func newTestRequestInfoResolver() *request.RequestInfoFactory {
	return &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
}

const benchmarkSeed = 100

func benchmarkItems(b *testing.B) []example.Pod {
	clientapiObjectFuzzer := fuzzer.FuzzerFor(examplefuzzer.Funcs, rand.NewSource(benchmarkSeed), codecs)
	items := make([]example.Pod, 3)
	for i := range items {
		clientapiObjectFuzzer.Fuzz(&items[i])
	}
	return items
}
