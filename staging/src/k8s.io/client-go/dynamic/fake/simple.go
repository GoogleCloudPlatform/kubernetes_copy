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

package fake

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/testing"
)

func NewSimpleDynamicClient(scheme *runtime.Scheme, objects ...runtime.Object) *FakeDynamicClient {
	unstructuredScheme := runtime.NewScheme()
	for gvk := range scheme.AllKnownTypes() {
		if unstructuredScheme.Recognizes(gvk) {
			continue
		}
		if strings.HasSuffix(gvk.Kind, "List") {
			unstructuredScheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
			continue
		}
		unstructuredScheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}

	objects, err := convertObjectsToUnstructured(scheme, objects)
	if err != nil {
		panic(err)
	}

	for _, obj := range objects {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if !unstructuredScheme.Recognizes(gvk) {
			unstructuredScheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		}
		gvk.Kind += "List"
		if !unstructuredScheme.Recognizes(gvk) {
			unstructuredScheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
		}
	}

	return NewSimpleDynamicClientWithCustomListKinds(unstructuredScheme, nil, objects...)
}

// NewSimpleDynamicClientWithCustomListKinds try not to use this.  In general you want to have the scheme have the List types registered
// and allow the default guessing for resources match.  Sometimes that doesn't work, so you can specify a custom mapping here.
func NewSimpleDynamicClientWithCustomListKinds(scheme *runtime.Scheme, gvrToListKind map[schema.GroupVersionResource]string, objects ...runtime.Object) *FakeDynamicClient {
	// In order to use List with this client, you have to have your lists registered so that the object tracker will find them
	// in the scheme to support the t.scheme.New(listGVK) call when it's building the return value.
	// Since the base fake client needs the listGVK passed through the action (in cases where there are no instances, it
	// cannot look up the actual hits), we need to know a mapping of GVR to listGVK here.  For GETs and other types of calls,
	// there is no return value that contains a GVK, so it doesn't have to know the mapping in advance.

	// first we attempt to invert known List types from the scheme to auto guess the resource with unsafe guesses
	// this covers common usage of registering types in scheme and passing them
	completeGVRToListKind := map[schema.GroupVersionResource]string{}
	for listGVK := range scheme.AllKnownTypes() {
		if !strings.HasSuffix(listGVK.Kind, "List") {
			continue
		}
		nonListGVK := listGVK.GroupVersion().WithKind(listGVK.Kind[:len(listGVK.Kind)-4])
		plural, _ := meta.UnsafeGuessKindToResource(nonListGVK)
		completeGVRToListKind[plural] = listGVK.Kind
	}

	for gvr, listKind := range gvrToListKind {
		if !strings.HasSuffix(listKind, "List") {
			panic("coding error, listGVK must end in List or this fake client doesn't work right")
		}
		listGVK := gvr.GroupVersion().WithKind(listKind)

		// if we already have this type registered, just skip it
		if _, err := scheme.New(listGVK); err == nil {
			completeGVRToListKind[gvr] = listKind
			continue
		}

		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
		completeGVRToListKind[gvr] = listKind
	}

	codecs := serializer.NewCodecFactory(scheme)
	o := testing.NewObjectTracker(scheme, codecs.UniversalDecoder())
	for _, obj := range objects {
		if err := o.Add(obj); err != nil {
			panic(err)
		}
	}

	cs := &FakeDynamicClient{scheme: scheme, gvrToListKind: completeGVRToListKind, tracker: o}
	cs.AddReactor("*", "*", testing.ObjectReaction(o))
	cs.AddWatchReactor("*", func(action testing.Action) (handled bool, ret watch.Interface, err error) {
		gvr := action.GetResource()
		ns := action.GetNamespace()
		watch, err := o.Watch(gvr, ns)
		if err != nil {
			return false, nil, err
		}
		return true, watch, nil
	})

	return cs
}

// Clientset implements clientset.Interface. Meant to be embedded into a
// struct to get a default implementation. This makes faking out just the method
// you want to test easier.
type FakeDynamicClient struct {
	testing.Fake
	scheme        *runtime.Scheme
	gvrToListKind map[schema.GroupVersionResource]string
	tracker       testing.ObjectTracker
}

type dynamicResourceClient struct {
	client    *FakeDynamicClient
	namespace string
	resource  schema.GroupVersionResource
	listKind  string
}

var (
	_ dynamic.Interface  = &FakeDynamicClient{}
	_ testing.FakeClient = &FakeDynamicClient{}
)

func (c *FakeDynamicClient) Tracker() testing.ObjectTracker {
	return c.tracker
}

func (c *FakeDynamicClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &dynamicResourceClient{client: c, resource: resource, listKind: c.gvrToListKind[resource]}
}

func (c *dynamicResourceClient) Namespace(ns string) dynamic.ResourceInterface {
	ret := *c
	ret.namespace = ns
	return &ret
}

func (c *dynamicResourceClient) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	var uncastRet runtime.Object
	var err error
	switch {
	case len(c.namespace) == 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootCreateActionWithOptions(c.resource, obj, opts), obj)

	case len(c.namespace) == 0 && len(subresources) > 0:
		var accessor metav1.Object // avoid shadowing err
		accessor, err = meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		name := accessor.GetName()
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootCreateSubresourceActionWithOptions(c.resource, name, strings.Join(subresources, "/"), obj, opts), obj)

	case len(c.namespace) > 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewCreateActionWithOptions(c.resource, c.namespace, obj, opts), obj)

	case len(c.namespace) > 0 && len(subresources) > 0:
		var accessor metav1.Object // avoid shadowing err
		accessor, err = meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		name := accessor.GetName()
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewCreateSubresourceActionWithOptions(c.resource, name, strings.Join(subresources, "/"), c.namespace, obj, opts), obj)

	}

	if err != nil {
		return nil, err
	}
	if uncastRet == nil {
		return nil, err
	}

	ret := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(uncastRet, ret, nil); err != nil {
		return nil, err
	}
	return ret, err
}

func (c *dynamicResourceClient) Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	var uncastRet runtime.Object
	var err error
	switch {
	case len(c.namespace) == 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootUpdateActionWithOptions(c.resource, obj, opts), obj)

	case len(c.namespace) == 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootUpdateSubresourceActionWithOptions(c.resource, strings.Join(subresources, "/"), obj, opts), obj)

	case len(c.namespace) > 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewUpdateActionWithOptions(c.resource, c.namespace, obj, opts), obj)

	case len(c.namespace) > 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewUpdateSubresourceActionWithOptions(c.resource, strings.Join(subresources, "/"), c.namespace, obj, opts), obj)

	}

	if err != nil {
		return nil, err
	}
	if uncastRet == nil {
		return nil, err
	}

	ret := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(uncastRet, ret, nil); err != nil {
		return nil, err
	}
	return ret, err
}

func (c *dynamicResourceClient) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	var uncastRet runtime.Object
	var err error
	switch {
	case len(c.namespace) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootUpdateSubresourceAction(c.resource, "status", obj), obj)

	case len(c.namespace) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewUpdateSubresourceAction(c.resource, "status", c.namespace, obj), obj)

	}

	if err != nil {
		return nil, err
	}
	if uncastRet == nil {
		return nil, err
	}

	ret := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(uncastRet, ret, nil); err != nil {
		return nil, err
	}
	return ret, err
}

func (c *dynamicResourceClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	var err error
	switch {
	case len(c.namespace) == 0 && len(subresources) == 0:
		_, err = c.client.Fake.
			Invokes(testing.NewRootDeleteActionWithOptions(c.resource, name, opts), &metav1.Status{Status: "dynamic delete fail"})

	case len(c.namespace) == 0 && len(subresources) > 0:
		_, err = c.client.Fake.
			Invokes(testing.NewRootDeleteSubresourceActionWithOptions(c.resource, strings.Join(subresources, "/"), name, opts), &metav1.Status{Status: "dynamic delete fail"})

	case len(c.namespace) > 0 && len(subresources) == 0:
		_, err = c.client.Fake.
			Invokes(testing.NewDeleteActionWithOptions(c.resource, c.namespace, name, opts), &metav1.Status{Status: "dynamic delete fail"})

	case len(c.namespace) > 0 && len(subresources) > 0:
		_, err = c.client.Fake.
			Invokes(testing.NewDeleteSubresourceActionWithOptions(c.resource, strings.Join(subresources, "/"), c.namespace, name, opts), &metav1.Status{Status: "dynamic delete fail"})
	}

	return err
}

func (c *dynamicResourceClient) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	var err error
	switch {
	case len(c.namespace) == 0:
		action := testing.NewRootDeleteCollectionAction(c.resource, listOptions)
		_, err = c.client.Fake.Invokes(action, &metav1.Status{Status: "dynamic deletecollection fail"})

	case len(c.namespace) > 0:
		action := testing.NewDeleteCollectionAction(c.resource, c.namespace, listOptions)
		_, err = c.client.Fake.Invokes(action, &metav1.Status{Status: "dynamic deletecollection fail"})

	}

	return err
}

func (c *dynamicResourceClient) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	var uncastRet runtime.Object
	var err error
	switch {
	case len(c.namespace) == 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootGetAction(c.resource, name), &metav1.Status{Status: "dynamic get fail"})

	case len(c.namespace) == 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootGetSubresourceAction(c.resource, strings.Join(subresources, "/"), name), &metav1.Status{Status: "dynamic get fail"})

	case len(c.namespace) > 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewGetAction(c.resource, c.namespace, name), &metav1.Status{Status: "dynamic get fail"})

	case len(c.namespace) > 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewGetSubresourceAction(c.resource, c.namespace, strings.Join(subresources, "/"), name), &metav1.Status{Status: "dynamic get fail"})
	}

	if err != nil {
		return nil, err
	}
	if uncastRet == nil {
		return nil, err
	}

	ret := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(uncastRet, ret, nil); err != nil {
		return nil, err
	}
	return ret, err
}

func (c *dynamicResourceClient) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if len(c.listKind) == 0 {
		panic(fmt.Sprintf("coding error: you must register resource to list kind for every resource you're going to LIST when creating the client.  See NewSimpleDynamicClientWithCustomListKinds or register the list into the scheme: %v out of %v", c.resource, c.client.gvrToListKind))
	}
	listGVK := c.resource.GroupVersion().WithKind(c.listKind)
	listForFakeClientGVK := c.resource.GroupVersion().WithKind(c.listKind[:len(c.listKind)-4]) /*base library appends List*/

	var obj runtime.Object
	var err error
	switch {
	case len(c.namespace) == 0:
		obj, err = c.client.Fake.
			Invokes(testing.NewRootListAction(c.resource, listForFakeClientGVK, opts), &metav1.Status{Status: "dynamic list fail"})

	case len(c.namespace) > 0:
		obj, err = c.client.Fake.
			Invokes(testing.NewListAction(c.resource, listForFakeClientGVK, c.namespace, opts), &metav1.Status{Status: "dynamic list fail"})

	}

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}

	retUnstructured := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(obj, retUnstructured, nil); err != nil {
		return nil, err
	}
	entireList, err := retUnstructured.ToList()
	if err != nil {
		return nil, err
	}

	list := &unstructured.UnstructuredList{}
	list.SetRemainingItemCount(entireList.GetRemainingItemCount())
	list.SetResourceVersion(entireList.GetResourceVersion())
	list.SetContinue(entireList.GetContinue())
	list.GetObjectKind().SetGroupVersionKind(listGVK)
	for i := range entireList.Items {
		item := &entireList.Items[i]
		metadata, err := meta.Accessor(item)
		if err != nil {
			return nil, err
		}
		if label.Matches(labels.Set(metadata.GetLabels())) {
			list.Items = append(list.Items, *item)
		}
	}
	return list, nil
}

func (c *dynamicResourceClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	switch {
	case len(c.namespace) == 0:
		return c.client.Fake.
			InvokesWatch(testing.NewRootWatchAction(c.resource, opts))

	case len(c.namespace) > 0:
		return c.client.Fake.
			InvokesWatch(testing.NewWatchAction(c.resource, c.namespace, opts))

	}

	panic("math broke")
}

// TODO: opts are currently ignored.
func (c *dynamicResourceClient) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	var uncastRet runtime.Object
	var err error
	switch {
	case len(c.namespace) == 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootPatchAction(c.resource, name, pt, data), &metav1.Status{Status: "dynamic patch fail"})

	case len(c.namespace) == 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootPatchSubresourceAction(c.resource, name, pt, data, subresources...), &metav1.Status{Status: "dynamic patch fail"})

	case len(c.namespace) > 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewPatchAction(c.resource, c.namespace, name, pt, data), &metav1.Status{Status: "dynamic patch fail"})

	case len(c.namespace) > 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewPatchSubresourceAction(c.resource, c.namespace, name, pt, data, subresources...), &metav1.Status{Status: "dynamic patch fail"})

	}

	if err != nil {
		return nil, err
	}
	if uncastRet == nil {
		return nil, err
	}

	ret := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(uncastRet, ret, nil); err != nil {
		return nil, err
	}
	return ret, err
}

// TODO: opts are currently ignored.
func (c *dynamicResourceClient) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	outBytes, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return nil, err
	}
	var uncastRet runtime.Object
	switch {
	case len(c.namespace) == 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootPatchAction(c.resource, name, types.ApplyPatchType, outBytes), &metav1.Status{Status: "dynamic patch fail"})

	case len(c.namespace) == 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewRootPatchSubresourceAction(c.resource, name, types.ApplyPatchType, outBytes, subresources...), &metav1.Status{Status: "dynamic patch fail"})

	case len(c.namespace) > 0 && len(subresources) == 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewPatchAction(c.resource, c.namespace, name, types.ApplyPatchType, outBytes), &metav1.Status{Status: "dynamic patch fail"})

	case len(c.namespace) > 0 && len(subresources) > 0:
		uncastRet, err = c.client.Fake.
			Invokes(testing.NewPatchSubresourceAction(c.resource, c.namespace, name, types.ApplyPatchType, outBytes, subresources...), &metav1.Status{Status: "dynamic patch fail"})

	}

	if err != nil {
		return nil, err
	}
	if uncastRet == nil {
		return nil, err
	}

	ret := &unstructured.Unstructured{}
	if err := c.client.scheme.Convert(uncastRet, ret, nil); err != nil {
		return nil, err
	}
	return ret, nil
}

func (c *dynamicResourceClient) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return c.Apply(ctx, name, obj, options, "status")
}

func convertObjectsToUnstructured(s *runtime.Scheme, objs []runtime.Object) ([]runtime.Object, error) {
	ul := make([]runtime.Object, 0, len(objs))

	for _, obj := range objs {
		u, err := convertToUnstructured(s, obj)
		if err != nil {
			return nil, err
		}

		ul = append(ul, u)
	}
	return ul, nil
}

func convertToUnstructured(s *runtime.Scheme, obj runtime.Object) (runtime.Object, error) {
	var (
		err error
		u   unstructured.Unstructured
	)

	u.Object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	gvk := u.GroupVersionKind()
	if gvk.Group == "" || gvk.Kind == "" {
		gvks, _, err := s.ObjectKinds(obj)
		if err != nil {
			return nil, fmt.Errorf("failed to convert to unstructured - unable to get GVK %w", err)
		}
		apiv, k := gvks[0].ToAPIVersionAndKind()
		u.SetAPIVersion(apiv)
		u.SetKind(k)
	}
	return &u, nil
}
