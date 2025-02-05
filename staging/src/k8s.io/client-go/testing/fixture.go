/*
Copyright 2015 The Kubernetes Authors.

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
	"fmt"
	"reflect"
	"sigs.k8s.io/structured-merge-diff/v4/typed"
	"sigs.k8s.io/yaml"
	"sort"
	"strings"
	"sync"

	jsonpatch "github.com/evanphx/json-patch/v5"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	restclient "k8s.io/client-go/rest"
)

// ObjectTracker keeps track of objects. It is intended to be used to
// fake calls to a server by returning objects based on their kind,
// namespace and name.
type ObjectTracker interface {
	// Add adds an object to the tracker. If object being added
	// is a list, its items are added separately.
	Add(obj runtime.Object) error

	// Get retrieves the object by its kind, namespace and name.
	Get(gvr schema.GroupVersionResource, ns, name string, opts ...metav1.GetOptions) (runtime.Object, error)

	// Create adds an object to the tracker in the specified namespace.
	Create(gvr schema.GroupVersionResource, obj runtime.Object, ns string, opts ...metav1.CreateOptions) error

	// Update updates an existing object in the tracker in the specified namespace.
	Update(gvr schema.GroupVersionResource, obj runtime.Object, ns string, opts ...metav1.UpdateOptions) error

	// Patch patches an existing object in the tracker in the specified namespace.
	Patch(gvr schema.GroupVersionResource, obj runtime.Object, ns string, opts ...metav1.PatchOptions) error

	// Apply applies an object in the tracker in the specified namespace.
	Apply(gvr schema.GroupVersionResource, applyConfiguration runtime.Object, ns string, opts ...metav1.PatchOptions) error

	// List retrieves all objects of a given kind in the given
	// namespace. Only non-List kinds are accepted.
	List(gvr schema.GroupVersionResource, gvk schema.GroupVersionKind, ns string, opts ...metav1.ListOptions) (runtime.Object, error)

	// Delete deletes an existing object from the tracker. If object
	// didn't exist in the tracker prior to deletion, Delete returns
	// no error.
	Delete(gvr schema.GroupVersionResource, ns, name string, opts ...metav1.DeleteOptions) error

	// Watch watches objects from the tracker. Watch returns a channel
	// which will push added / modified / deleted object.
	Watch(gvr schema.GroupVersionResource, ns string, opts ...metav1.ListOptions) (watch.Interface, error)
}

// ObjectScheme abstracts the implementation of common operations on objects.
type ObjectScheme interface {
	runtime.ObjectCreater
	runtime.ObjectTyper
}

// ObjectReaction returns a ReactionFunc that applies core.Action to
// the given tracker.
//
// If tracker also implements ManagedFieldObjectTracker, then managed fields
// will be handled by the tracker and apply patch actions will be evaluated
// using the field manager and will take field ownership into consideration.
// Without a ManagedFieldObjectTracker, apply patch actions do not consider
// field ownership.
//
// WARNING: There is no server side defaulting, validation, or conversion handled
// by the fake client and subresources are not handled accurately (fields in the
// root resource are not automatically updated when a scale resource is updated, for example).
func ObjectReaction(tracker ObjectTracker) ReactionFunc {
	reactor := objectTrackerReact{tracker: tracker}
	return func(action Action) (bool, runtime.Object, error) {
		// Here and below we need to switch on implementation types,
		// not on interfaces, as some interfaces are identical
		// (e.g. UpdateAction and CreateAction), so if we use them,
		// updates and creates end up matching the same case branch.
		switch action := action.(type) {
		case ListActionImpl:
			obj, err := reactor.List(action)
			return true, obj, err
		case GetActionImpl:
			obj, err := reactor.Get(action)
			return true, obj, err
		case CreateActionImpl:
			obj, err := reactor.Create(action)
			return true, obj, err
		case UpdateActionImpl:
			obj, err := reactor.Update(action)
			return true, obj, err
		case DeleteActionImpl:
			obj, err := reactor.Delete(action)
			return true, obj, err
		case PatchActionImpl:
			if action.GetPatchType() == types.ApplyPatchType {
				obj, err := reactor.Apply(action)
				return true, obj, err
			}
			obj, err := reactor.Patch(action)
			return true, obj, err
		default:
			return false, nil, fmt.Errorf("no reaction implemented for %s", action)
		}
	}
}

type objectTrackerReact struct {
	tracker ObjectTracker
}

func (o objectTrackerReact) List(action ListActionImpl) (runtime.Object, error) {
	return o.tracker.List(action.GetResource(), action.GetKind(), action.GetNamespace(), action.ListOptions)
}

func (o objectTrackerReact) Get(action GetActionImpl) (runtime.Object, error) {
	return o.tracker.Get(action.GetResource(), action.GetNamespace(), action.GetName(), action.GetOptions)
}

func (o objectTrackerReact) Create(action CreateActionImpl) (runtime.Object, error) {
	ns := action.GetNamespace()
	gvr := action.GetResource()
	objMeta, err := meta.Accessor(action.GetObject())
	if err != nil {
		return nil, err
	}
	if action.GetSubresource() == "" {
		err = o.tracker.Create(gvr, action.GetObject(), ns, action.CreateOptions)
		if err != nil {
			return nil, err
		}
	} else {
		oldObj, getOldObjErr := o.tracker.Get(gvr, ns, objMeta.GetName(), metav1.GetOptions{})
		if getOldObjErr != nil {
			return nil, getOldObjErr
		}
		// Check whether the existing historical object type is the same as the current operation object type that needs to be updated, and if it is the same, perform the update operation.
		if reflect.TypeOf(oldObj) == reflect.TypeOf(action.GetObject()) {
			// TODO: Currently we're handling subresource creation as an update
			// on the enclosing resource. This works for some subresources but
			// might not be generic enough.
			err = o.tracker.Update(gvr, action.GetObject(), ns, metav1.UpdateOptions{
				DryRun:          action.CreateOptions.DryRun,
				FieldManager:    action.CreateOptions.FieldManager,
				FieldValidation: action.CreateOptions.FieldValidation,
			})
		} else {
			// If the historical object type is different from the current object type, need to make sure we return the object submitted,don't persist the submitted object in the tracker.
			return action.GetObject(), nil
		}
	}
	if err != nil {
		return nil, err
	}
	obj, err := o.tracker.Get(gvr, ns, objMeta.GetName(), metav1.GetOptions{})
	return obj, err
}

func (o objectTrackerReact) Update(action UpdateActionImpl) (runtime.Object, error) {
	ns := action.GetNamespace()
	gvr := action.GetResource()
	objMeta, err := meta.Accessor(action.GetObject())
	if err != nil {
		return nil, err
	}

	err = o.tracker.Update(gvr, action.GetObject(), ns, action.UpdateOptions)
	if err != nil {
		return nil, err
	}

	obj, err := o.tracker.Get(gvr, ns, objMeta.GetName(), metav1.GetOptions{})
	return obj, err
}

func (o objectTrackerReact) Delete(action DeleteActionImpl) (runtime.Object, error) {
	err := o.tracker.Delete(action.GetResource(), action.GetNamespace(), action.GetName(), action.DeleteOptions)
	return nil, err
}

func (o objectTrackerReact) Apply(action PatchActionImpl) (runtime.Object, error) {
	ns := action.GetNamespace()
	gvr := action.GetResource()

	patchObj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := yaml.Unmarshal(action.GetPatch(), &patchObj.Object); err != nil {
		return nil, err
	}
	patchObj.SetName(action.GetName())
	err := o.tracker.Apply(gvr, patchObj, ns, action.PatchOptions)
	if err != nil {
		return nil, err
	}
	obj, err := o.tracker.Get(gvr, ns, action.GetName(), metav1.GetOptions{})
	return obj, err
}

func (o objectTrackerReact) Patch(action PatchActionImpl) (runtime.Object, error) {
	ns := action.GetNamespace()
	gvr := action.GetResource()

	obj, err := o.tracker.Get(gvr, ns, action.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	old, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}

	// reset the object in preparation to unmarshal, since unmarshal does not guarantee that fields
	// in obj that are removed by patch are cleared
	value := reflect.ValueOf(obj)
	value.Elem().Set(reflect.New(value.Type().Elem()).Elem())

	switch action.GetPatchType() {
	case types.JSONPatchType:
		patch, err := jsonpatch.DecodePatch(action.GetPatch())
		if err != nil {
			return nil, err
		}
		modified, err := patch.Apply(old)
		if err != nil {
			return nil, err
		}

		if err = json.Unmarshal(modified, obj); err != nil {
			return nil, err
		}
	case types.MergePatchType:
		modified, err := jsonpatch.MergePatch(old, action.GetPatch())
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(modified, obj); err != nil {
			return nil, err
		}
	case types.StrategicMergePatchType:
		mergedByte, err := strategicpatch.StrategicMergePatch(old, action.GetPatch(), obj)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(mergedByte, obj); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("PatchType %s is not supported", action.GetPatchType())
	}

	if err = o.tracker.Patch(gvr, obj, ns, action.PatchOptions); err != nil {
		return nil, err
	}

	return obj, nil
}

type tracker struct {
	scheme  ObjectScheme
	decoder runtime.Decoder
	lock    sync.RWMutex
	objects map[schema.GroupVersionResource]map[types.NamespacedName]runtime.Object
	// The value type of watchers is a map of which the key is either a namespace or
	// all/non namespace aka "" and its value is list of fake watchers.
	// Manipulations on resources will broadcast the notification events into the
	// watchers' channel. Note that too many unhandled events (currently 100,
	// see apimachinery/pkg/watch.DefaultChanSize) will cause a panic.
	watchers map[schema.GroupVersionResource]map[string][]*watch.RaceFreeFakeWatcher
}

var _ ObjectTracker = &tracker{}

// NewObjectTracker returns an ObjectTracker that can be used to keep track
// of objects for the fake clientset. Mostly useful for unit tests.
func NewObjectTracker(scheme ObjectScheme, decoder runtime.Decoder) ObjectTracker {
	return &tracker{
		scheme:   scheme,
		decoder:  decoder,
		objects:  make(map[schema.GroupVersionResource]map[types.NamespacedName]runtime.Object),
		watchers: make(map[schema.GroupVersionResource]map[string][]*watch.RaceFreeFakeWatcher),
	}
}

func (t *tracker) List(gvr schema.GroupVersionResource, gvk schema.GroupVersionKind, ns string, opts ...metav1.ListOptions) (runtime.Object, error) {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return nil, err
	}
	// Heuristic for list kind: original kind + List suffix. Might
	// not always be true but this tracker has a pretty limited
	// understanding of the actual API model.
	listGVK := gvk
	listGVK.Kind = listGVK.Kind + "List"
	// GVK does have the concept of "internal version". The scheme recognizes
	// the runtime.APIVersionInternal, but not the empty string.
	if listGVK.Version == "" {
		listGVK.Version = runtime.APIVersionInternal
	}

	list, err := t.scheme.New(listGVK)
	if err != nil {
		return nil, err
	}

	if !meta.IsListType(list) {
		return nil, fmt.Errorf("%q is not a list type", listGVK.Kind)
	}

	t.lock.RLock()
	defer t.lock.RUnlock()

	objs, ok := t.objects[gvr]
	if !ok {
		return list, nil
	}

	matchingObjs, err := filterByNamespace(objs, ns)
	if err != nil {
		return nil, err
	}
	if err := meta.SetList(list, matchingObjs); err != nil {
		return nil, err
	}
	return list.DeepCopyObject(), nil
}

func (t *tracker) Watch(gvr schema.GroupVersionResource, ns string, opts ...metav1.ListOptions) (watch.Interface, error) {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return nil, err
	}

	t.lock.Lock()
	defer t.lock.Unlock()

	fakewatcher := watch.NewRaceFreeFake()

	if _, exists := t.watchers[gvr]; !exists {
		t.watchers[gvr] = make(map[string][]*watch.RaceFreeFakeWatcher)
	}
	t.watchers[gvr][ns] = append(t.watchers[gvr][ns], fakewatcher)
	return fakewatcher, nil
}

func (t *tracker) Get(gvr schema.GroupVersionResource, ns, name string, opts ...metav1.GetOptions) (runtime.Object, error) {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return nil, err
	}
	errNotFound := apierrors.NewNotFound(gvr.GroupResource(), name)

	t.lock.RLock()
	defer t.lock.RUnlock()

	objs, ok := t.objects[gvr]
	if !ok {
		return nil, errNotFound
	}

	matchingObj, ok := objs[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		return nil, errNotFound
	}

	// Only one object should match in the tracker if it works
	// correctly, as Add/Update methods enforce kind/namespace/name
	// uniqueness.
	obj := matchingObj.DeepCopyObject()
	if status, ok := obj.(*metav1.Status); ok {
		if status.Status != metav1.StatusSuccess {
			return nil, &apierrors.StatusError{ErrStatus: *status}
		}
	}

	return obj, nil
}

func (t *tracker) Add(obj runtime.Object) error {
	if meta.IsListType(obj) {
		return t.addList(obj, false)
	}
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	gvks, _, err := t.scheme.ObjectKinds(obj)
	if err != nil {
		return err
	}

	if partial, ok := obj.(*metav1.PartialObjectMetadata); ok && len(partial.TypeMeta.APIVersion) > 0 {
		gvks = []schema.GroupVersionKind{partial.TypeMeta.GroupVersionKind()}
	}

	if len(gvks) == 0 {
		return fmt.Errorf("no registered kinds for %v", obj)
	}
	for _, gvk := range gvks {
		// NOTE: UnsafeGuessKindToResource is a heuristic and default match. The
		// actual registration in apiserver can specify arbitrary route for a
		// gvk. If a test uses such objects, it cannot preset the tracker with
		// objects via Add(). Instead, it should trigger the Create() function
		// of the tracker, where an arbitrary gvr can be specified.
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		// Resource doesn't have the concept of "__internal" version, just set it to "".
		if gvr.Version == runtime.APIVersionInternal {
			gvr.Version = ""
		}

		err := t.add(gvr, obj, objMeta.GetNamespace(), false)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *tracker) Create(gvr schema.GroupVersionResource, obj runtime.Object, ns string, opts ...metav1.CreateOptions) error {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return err
	}
	return t.add(gvr, obj, ns, false)
}

func (t *tracker) Update(gvr schema.GroupVersionResource, obj runtime.Object, ns string, opts ...metav1.UpdateOptions) error {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return err
	}
	return t.add(gvr, obj, ns, true)
}

func (t *tracker) Patch(gvr schema.GroupVersionResource, patchedObject runtime.Object, ns string, opts ...metav1.PatchOptions) error {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return err
	}
	return t.add(gvr, patchedObject, ns, true)
}

func (t *tracker) Apply(gvr schema.GroupVersionResource, applyConfiguration runtime.Object, ns string, opts ...metav1.PatchOptions) error {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return err
	}
	applyConfigurationMeta, err := meta.Accessor(applyConfiguration)
	if err != nil {
		return err
	}

	obj, err := t.Get(gvr, ns, applyConfigurationMeta.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}

	old, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	// reset the object in preparation to unmarshal, since unmarshal does not guarantee that fields
	// in obj that are removed by patch are cleared
	value := reflect.ValueOf(obj)
	value.Elem().Set(reflect.New(value.Type().Elem()).Elem())

	// For backward compatibility with behavior 1.30 and earlier, continue to handle apply
	// via strategic merge patch (clients may use fake.NewClientset and ManagedFieldObjectTracker
	// for full field manager support).
	patch, err := json.Marshal(applyConfiguration)
	if err != nil {
		return err
	}
	mergedByte, err := strategicpatch.StrategicMergePatch(old, patch, obj)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(mergedByte, obj); err != nil {
		return err
	}

	return t.add(gvr, obj, ns, true)
}

func (t *tracker) getWatches(gvr schema.GroupVersionResource, ns string) []*watch.RaceFreeFakeWatcher {
	watches := []*watch.RaceFreeFakeWatcher{}
	if t.watchers[gvr] != nil {
		if w := t.watchers[gvr][ns]; w != nil {
			watches = append(watches, w...)
		}
		if ns != metav1.NamespaceAll {
			if w := t.watchers[gvr][metav1.NamespaceAll]; w != nil {
				watches = append(watches, w...)
			}
		}
	}
	return watches
}

func (t *tracker) add(gvr schema.GroupVersionResource, obj runtime.Object, ns string, replaceExisting bool) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	gr := gvr.GroupResource()

	// To avoid the object from being accidentally modified by caller
	// after it's been added to the tracker, we always store the deep
	// copy.
	obj = obj.DeepCopyObject()

	newMeta, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	// Propagate namespace to the new object if hasn't already been set.
	if len(newMeta.GetNamespace()) == 0 {
		newMeta.SetNamespace(ns)
	}

	if ns != newMeta.GetNamespace() {
		msg := fmt.Sprintf("request namespace does not match object namespace, request: %q object: %q", ns, newMeta.GetNamespace())
		return apierrors.NewBadRequest(msg)
	}

	_, ok := t.objects[gvr]
	if !ok {
		t.objects[gvr] = make(map[types.NamespacedName]runtime.Object)
	}

	namespacedName := types.NamespacedName{Namespace: newMeta.GetNamespace(), Name: newMeta.GetName()}
	if _, ok = t.objects[gvr][namespacedName]; ok {
		if replaceExisting {
			for _, w := range t.getWatches(gvr, ns) {
				// To avoid the object from being accidentally modified by watcher
				w.Modify(obj.DeepCopyObject())
			}
			t.objects[gvr][namespacedName] = obj
			return nil
		}
		return apierrors.NewAlreadyExists(gr, newMeta.GetName())
	}

	if replaceExisting {
		// Tried to update but no matching object was found.
		return apierrors.NewNotFound(gr, newMeta.GetName())
	}

	t.objects[gvr][namespacedName] = obj

	for _, w := range t.getWatches(gvr, ns) {
		// To avoid the object from being accidentally modified by watcher
		w.Add(obj.DeepCopyObject())
	}

	return nil
}

func (t *tracker) addList(obj runtime.Object, replaceExisting bool) error {
	list, err := meta.ExtractList(obj)
	if err != nil {
		return err
	}
	errs := runtime.DecodeList(list, t.decoder)
	if len(errs) > 0 {
		return errs[0]
	}
	for _, obj := range list {
		if err := t.Add(obj); err != nil {
			return err
		}
	}
	return nil
}

func (t *tracker) Delete(gvr schema.GroupVersionResource, ns, name string, opts ...metav1.DeleteOptions) error {
	_, err := assertOptionalSingleArgument(opts)
	if err != nil {
		return err
	}
	t.lock.Lock()
	defer t.lock.Unlock()

	objs, ok := t.objects[gvr]
	if !ok {
		return apierrors.NewNotFound(gvr.GroupResource(), name)
	}

	namespacedName := types.NamespacedName{Namespace: ns, Name: name}
	obj, ok := objs[namespacedName]
	if !ok {
		return apierrors.NewNotFound(gvr.GroupResource(), name)
	}

	delete(objs, namespacedName)
	for _, w := range t.getWatches(gvr, ns) {
		w.Delete(obj.DeepCopyObject())
	}
	return nil
}

type managedFieldObjectTracker struct {
	ObjectTracker
	scheme          ObjectScheme
	objectConverter runtime.ObjectConvertor
	mapper          meta.RESTMapper
	typeConverter   managedfields.TypeConverter
}

var _ ObjectTracker = &managedFieldObjectTracker{}

// NewFieldManagedObjectTracker returns an ObjectTracker that can be used to keep track
// of objects and managed fields for the fake clientset. Mostly useful for unit tests.
func NewFieldManagedObjectTracker(scheme *runtime.Scheme, decoder runtime.Decoder, typeConverter managedfields.TypeConverter) ObjectTracker {
	return &managedFieldObjectTracker{
		ObjectTracker:   NewObjectTracker(scheme, decoder),
		scheme:          scheme,
		objectConverter: scheme,
		mapper:          testrestmapper.TestOnlyStaticRESTMapper(scheme),
		typeConverter:   typeConverter,
	}
}

func (t *managedFieldObjectTracker) Create(gvr schema.GroupVersionResource, obj runtime.Object, ns string, vopts ...metav1.CreateOptions) error {
	opts, err := assertOptionalSingleArgument(vopts)
	if err != nil {
		return err
	}
	gvk, err := t.mapper.KindFor(gvr)
	if err != nil {
		return err
	}
	mgr, err := t.fieldManagerFor(gvk)
	if err != nil {
		return err
	}

	objType, err := meta.TypeAccessor(obj)
	if err != nil {
		return err
	}
	// Stamp GVK
	apiVersion, kind := gvk.ToAPIVersionAndKind()
	objType.SetAPIVersion(apiVersion)
	objType.SetKind(kind)

	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	liveObject, err := t.ObjectTracker.Get(gvr, ns, objMeta.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		liveObject, err = t.scheme.New(gvk)
		if err != nil {
			return err
		}
		liveObject.GetObjectKind().SetGroupVersionKind(gvk)
	} else if err != nil {
		return err
	}
	objWithManagedFields, err := mgr.Update(liveObject, obj, opts.FieldManager)
	if err != nil {
		return err
	}
	return t.ObjectTracker.Create(gvr, objWithManagedFields, ns, opts)
}

func (t *managedFieldObjectTracker) Update(gvr schema.GroupVersionResource, obj runtime.Object, ns string, vopts ...metav1.UpdateOptions) error {
	opts, err := assertOptionalSingleArgument(vopts)
	if err != nil {
		return err
	}
	gvk, err := t.mapper.KindFor(gvr)
	if err != nil {
		return err
	}
	mgr, err := t.fieldManagerFor(gvk)
	if err != nil {
		return err
	}

	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	oldObj, err := t.ObjectTracker.Get(gvr, ns, objMeta.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	objWithManagedFields, err := mgr.Update(oldObj, obj, opts.FieldManager)
	if err != nil {
		return err
	}

	return t.ObjectTracker.Update(gvr, objWithManagedFields, ns, opts)
}

func (t *managedFieldObjectTracker) Patch(gvr schema.GroupVersionResource, patchedObject runtime.Object, ns string, vopts ...metav1.PatchOptions) error {
	opts, err := assertOptionalSingleArgument(vopts)
	if err != nil {
		return err
	}
	gvk, err := t.mapper.KindFor(gvr)
	if err != nil {
		return err
	}
	mgr, err := t.fieldManagerFor(gvk)
	if err != nil {
		return err
	}

	objMeta, err := meta.Accessor(patchedObject)
	if err != nil {
		return err
	}
	oldObj, err := t.ObjectTracker.Get(gvr, ns, objMeta.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	objWithManagedFields, err := mgr.Update(oldObj, patchedObject, opts.FieldManager)
	if err != nil {
		return err
	}
	return t.ObjectTracker.Patch(gvr, objWithManagedFields, ns, vopts...)
}

func (t *managedFieldObjectTracker) Apply(gvr schema.GroupVersionResource, applyConfiguration runtime.Object, ns string, vopts ...metav1.PatchOptions) error {
	opts, err := assertOptionalSingleArgument(vopts)
	if err != nil {
		return err
	}
	gvk, err := t.mapper.KindFor(gvr)
	if err != nil {
		return err
	}
	applyConfigurationMeta, err := meta.Accessor(applyConfiguration)
	if err != nil {
		return err
	}

	exists := true
	liveObject, err := t.ObjectTracker.Get(gvr, ns, applyConfigurationMeta.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		exists = false
		liveObject, err = t.scheme.New(gvk)
		if err != nil {
			return err
		}
		liveObject.GetObjectKind().SetGroupVersionKind(gvk)
	} else if err != nil {
		return err
	}
	mgr, err := t.fieldManagerFor(gvk)
	if err != nil {
		return err
	}
	force := false
	if opts.Force != nil {
		force = *opts.Force
	}
	objWithManagedFields, err := mgr.Apply(liveObject, applyConfiguration, opts.FieldManager, force)
	if err != nil {
		return err
	}

	if !exists {
		return t.ObjectTracker.Create(gvr, objWithManagedFields, ns, metav1.CreateOptions{
			DryRun:          opts.DryRun,
			FieldManager:    opts.FieldManager,
			FieldValidation: opts.FieldValidation,
		})
	} else {
		return t.ObjectTracker.Update(gvr, objWithManagedFields, ns, metav1.UpdateOptions{
			DryRun:          opts.DryRun,
			FieldManager:    opts.FieldManager,
			FieldValidation: opts.FieldValidation,
		})
	}
}

func (t *managedFieldObjectTracker) fieldManagerFor(gvk schema.GroupVersionKind) (*managedfields.FieldManager, error) {
	return managedfields.NewDefaultFieldManager(
		t.typeConverter,
		t.objectConverter,
		&objectDefaulter{},
		t.scheme,
		gvk,
		gvk.GroupVersion(),
		"",
		nil)
}

// objectDefaulter implements runtime.Defaulter, but it actually
// does nothing.
type objectDefaulter struct{}

func (d *objectDefaulter) Default(_ runtime.Object) {}

// filterByNamespace returns all objects in the collection that
// match provided namespace. Empty namespace matches
// non-namespaced objects.
func filterByNamespace(objs map[types.NamespacedName]runtime.Object, ns string) ([]runtime.Object, error) {
	var res []runtime.Object

	for _, obj := range objs {
		acc, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		if ns != "" && acc.GetNamespace() != ns {
			continue
		}
		res = append(res, obj)
	}

	// Sort res to get deterministic order.
	sort.Slice(res, func(i, j int) bool {
		acc1, _ := meta.Accessor(res[i])
		acc2, _ := meta.Accessor(res[j])
		if acc1.GetNamespace() != acc2.GetNamespace() {
			return acc1.GetNamespace() < acc2.GetNamespace()
		}
		return acc1.GetName() < acc2.GetName()
	})
	return res, nil
}

func DefaultWatchReactor(watchInterface watch.Interface, err error) WatchReactionFunc {
	return func(action Action) (bool, watch.Interface, error) {
		return true, watchInterface, err
	}
}

// SimpleReactor is a Reactor.  Each reaction function is attached to a given verb,resource tuple.  "*" in either field matches everything for that value.
// For instance, *,pods matches all verbs on pods.  This allows for easier composition of reaction functions
type SimpleReactor struct {
	Verb     string
	Resource string

	Reaction ReactionFunc
}

func (r *SimpleReactor) Handles(action Action) bool {
	verbCovers := r.Verb == "*" || r.Verb == action.GetVerb()
	if !verbCovers {
		return false
	}

	return resourceCovers(r.Resource, action)
}

func (r *SimpleReactor) React(action Action) (bool, runtime.Object, error) {
	return r.Reaction(action)
}

// SimpleWatchReactor is a WatchReactor.  Each reaction function is attached to a given resource.  "*" matches everything for that value.
// For instance, *,pods matches all verbs on pods.  This allows for easier composition of reaction functions
type SimpleWatchReactor struct {
	Resource string

	Reaction WatchReactionFunc
}

func (r *SimpleWatchReactor) Handles(action Action) bool {
	return resourceCovers(r.Resource, action)
}

func (r *SimpleWatchReactor) React(action Action) (bool, watch.Interface, error) {
	return r.Reaction(action)
}

// SimpleProxyReactor is a ProxyReactor.  Each reaction function is attached to a given resource.  "*" matches everything for that value.
// For instance, *,pods matches all verbs on pods.  This allows for easier composition of reaction functions.
type SimpleProxyReactor struct {
	Resource string

	Reaction ProxyReactionFunc
}

func (r *SimpleProxyReactor) Handles(action Action) bool {
	return resourceCovers(r.Resource, action)
}

func (r *SimpleProxyReactor) React(action Action) (bool, restclient.ResponseWrapper, error) {
	return r.Reaction(action)
}

func resourceCovers(resource string, action Action) bool {
	if resource == "*" {
		return true
	}

	if resource == action.GetResource().Resource {
		return true
	}

	if index := strings.Index(resource, "/"); index != -1 &&
		resource[:index] == action.GetResource().Resource &&
		resource[index+1:] == action.GetSubresource() {
		return true
	}

	return false
}

// assertOptionalSingleArgument returns an error if there is more than one variadic argument.
// Otherwise, it returns the first variadic argument, or zero value if there are no arguments.
func assertOptionalSingleArgument[T any](arguments []T) (T, error) {
	var a T
	switch len(arguments) {
	case 0:
		return a, nil
	case 1:
		return arguments[0], nil
	default:
		return a, fmt.Errorf("expected only one option argument but got %d", len(arguments))
	}
}

type TypeResolver interface {
	Type(openAPIName string) typed.ParseableType
}

type TypeConverter struct {
	Scheme       *runtime.Scheme
	TypeResolver TypeResolver
}

func (tc TypeConverter) ObjectToTyped(obj runtime.Object, opts ...typed.ValidationOptions) (*typed.TypedValue, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	name, err := tc.openAPIName(gvk)
	if err != nil {
		return nil, err
	}
	t := tc.TypeResolver.Type(name)
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		return t.FromUnstructured(o.UnstructuredContent(), opts...)
	default:
		return t.FromStructured(obj, opts...)
	}
}

func (tc TypeConverter) TypedToObject(value *typed.TypedValue) (runtime.Object, error) {
	vu := value.AsValue().Unstructured()
	switch o := vu.(type) {
	case map[string]interface{}:
		return &unstructured.Unstructured{Object: o}, nil
	default:
		return nil, fmt.Errorf("failed to convert value to unstructured for type %T", vu)
	}
}

func (tc TypeConverter) openAPIName(kind schema.GroupVersionKind) (string, error) {
	example, err := tc.Scheme.New(kind)
	if err != nil {
		return "", err
	}
	rtype := reflect.TypeOf(example).Elem()
	name := friendlyName(rtype.PkgPath() + "." + rtype.Name())
	return name, nil
}

// This is a copy of openapi.friendlyName.
// TODO: consider introducing a shared version of this function in apimachinery.
func friendlyName(name string) string {
	nameParts := strings.Split(name, "/")
	// Reverse first part. e.g., io.k8s... instead of k8s.io...
	if len(nameParts) > 0 && strings.Contains(nameParts[0], ".") {
		parts := strings.Split(nameParts[0], ".")
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		nameParts[0] = strings.Join(parts, ".")
	}
	return strings.Join(nameParts, ".")
}
