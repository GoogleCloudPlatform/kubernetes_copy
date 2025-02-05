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

package runtime

import (
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/naming"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Scheme defines methods for serializing and deserializing API objects, a type
// registry for converting group, version, and kind information to and from Go
// schemas, and mappings between Go schemas of different versions. A scheme is the
// foundation for a versioned API and versioned configuration over time.
//
// In a Scheme, a Type is a particular Go struct, a Version is a point-in-time
// identifier for a particular representation of that Type (typically backwards
// compatible), a Kind is the unique name for that Type within the Version, and a
// Group identifies a set of Versions, Kinds, and Types that evolve over time. An
// Unversioned Type is one that is not yet formally bound to a type and is promised
// to be backwards compatible (effectively a "v1" of a Type that does not expect
// to break in the future).
//
// Schemes are not expected to change at runtime and are only threadsafe after
// registration is complete.
type Scheme struct {
	// gvkToType allows one to figure out the go type of an object with
	// the given version and name.
	gvkToType map[schema.GroupVersionKind]reflect.Type

	// typeToGVK allows one to find metadata for a given go object.
	// The reflect.Type we index by should *not* be a pointer.
	typeToGVK map[reflect.Type][]schema.GroupVersionKind

	// unversionedTypes are transformed without conversion in ConvertToVersion.
	unversionedTypes map[reflect.Type]schema.GroupVersionKind

	// unversionedKinds are the names of kinds that can be created in the context of any group
	// or version
	// TODO: resolve the status of unversioned types.
	unversionedKinds map[string]reflect.Type

	// Map from version and resource to the corresponding func to convert
	// resource field labels in that version to internal version.
	fieldLabelConversionFuncs map[schema.GroupVersionKind]FieldLabelConversionFunc

	// defaulterFuncs is a map to funcs to be called with an object to provide defaulting
	// the provided object must be a pointer.
	defaulterFuncs map[reflect.Type]func(interface{})

	// converter stores all registered conversion functions. It also has
	// default converting behavior.
	converter *conversion.Converter

	// versionPriority is a map of groups to ordered lists of versions for those groups indicating the
	// default priorities of these versions as registered in the scheme
	versionPriority map[string][]string

	// observedVersions keeps track of the order we've seen versions during type registration
	observedVersions []schema.GroupVersion

	// schemeName is the name of this scheme.  If you don't specify a name, the stack of the NewScheme caller will be used.
	// This is useful for error reporting to indicate the origin of the scheme.
	schemeName string
}

// FieldLabelConversionFunc converts a field selector to internal representation.
type FieldLabelConversionFunc func(label, value string) (internalLabel, internalValue string, err error)

// NewScheme creates a new Scheme. This scheme is pluggable by default.
func NewScheme() *Scheme {
	s := &Scheme{
		gvkToType:                 map[schema.GroupVersionKind]reflect.Type{},
		typeToGVK:                 map[reflect.Type][]schema.GroupVersionKind{},
		unversionedTypes:          map[reflect.Type]schema.GroupVersionKind{},
		unversionedKinds:          map[string]reflect.Type{},
		fieldLabelConversionFuncs: map[schema.GroupVersionKind]FieldLabelConversionFunc{},
		defaulterFuncs:            map[reflect.Type]func(interface{}){},
		versionPriority:           map[string][]string{},
		schemeName:                naming.GetNameFromCallsite(internalPackages...),
	}
	s.converter = conversion.NewConverter(nil)

	// Enable couple default conversions by default.
	utilruntime.Must(RegisterEmbeddedConversions(s))
	utilruntime.Must(RegisterStringConversions(s))
	return s
}

// Converter allows access to the converter for the scheme
func (s *Scheme) Converter() *conversion.Converter {
	return s.converter
}

// AddUnversionedTypes registers the provided types as "unversioned", which means that they follow special rules.
// Whenever an object of this type is serialized, it is serialized with the provided group version and is not
// converted. Thus unversioned objects are expected to remain backwards compatible forever, as if they were in an
// API group and version that would never be updated.
//
// TODO: there is discussion about removing unversioned and replacing it with objects that are manifest into
// every version with particular schemas. Resolve this method at that point.
func (s *Scheme) AddUnversionedTypes(version schema.GroupVersion, types ...Object) {
	s.AddKnownTypes(version, types...)
	for _, obj := range types {
		t := reflect.TypeOf(obj).Elem()
		gvk := version.WithKind(t.Name())
		s.unversionedTypes[t] = gvk
		if old, ok := s.unversionedKinds[gvk.Kind]; ok && t != old {
			panic(fmt.Sprintf("%v.%v has already been registered as unversioned kind %q - kind name must be unique in scheme %q", old.PkgPath(), old.Name(), gvk, s.schemeName))
		}
		s.unversionedKinds[gvk.Kind] = t
	}
}

// AddKnownTypes registers all types passed in 'types' as being members of version 'version'.
// All objects passed to types should be pointers to structs. The name that go reports for
// the struct becomes the "kind" field when encoding. Version may not be empty - use the
// APIVersionInternal constant if you have a type that does not have a formal version.
func (s *Scheme) AddKnownTypes(gv schema.GroupVersion, types ...Object) {
	for _, obj := range types {
		t := reflect.TypeOf(obj)
		if t.Kind() != reflect.Pointer {
			panic("All types must be pointers to structs.")
		}
		t = t.Elem()
		s.AddKnownTypeWithName(gv.WithKind(t.Name()), obj)
	}
}

// AddKnownTypeWithName is like AddKnownTypes, but it lets you specify what this type should
// be encoded as. Useful for testing when you don't want to make multiple packages to define
// your structs. Version may not be empty - use the APIVersionInternal constant if you have a
// type that does not have a formal version.
func (s *Scheme) AddKnownTypeWithName(gvk schema.GroupVersionKind, obj Object) {
	s.addObservedVersion(gvk.GroupVersion())
	t := reflect.TypeOf(obj)
	if len(gvk.Version) == 0 {
		panic(fmt.Sprintf("version is required on all types: %s %v", gvk, t))
	}
	if t.Kind() != reflect.Pointer {
		panic("All types must be pointers to structs.")
	}
	t = t.Elem()
	if t.Kind() != reflect.Struct {
		panic("All types must be pointers to structs.")
	}

	if oldT, found := s.gvkToType[gvk]; found && oldT != t {
		panic(fmt.Sprintf("Double registration of different types for %v: old=%v.%v, new=%v.%v in scheme %q", gvk, oldT.PkgPath(), oldT.Name(), t.PkgPath(), t.Name(), s.schemeName))
	}

	s.gvkToType[gvk] = t

	for _, existingGvk := range s.typeToGVK[t] {
		if existingGvk == gvk {
			return
		}
	}
	s.typeToGVK[t] = append(s.typeToGVK[t], gvk)

	// if the type implements DeepCopyInto(<obj>), register a self-conversion
	if m := reflect.ValueOf(obj).MethodByName("DeepCopyInto"); m.IsValid() && m.Type().NumIn() == 1 && m.Type().NumOut() == 0 && m.Type().In(0) == reflect.TypeOf(obj) {
		if err := s.AddGeneratedConversionFunc(obj, obj, func(a, b interface{}, scope conversion.Scope) error {
			// copy a to b
			reflect.ValueOf(a).MethodByName("DeepCopyInto").Call([]reflect.Value{reflect.ValueOf(b)})
			// clear TypeMeta to match legacy reflective conversion
			b.(Object).GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
			return nil
		}); err != nil {
			panic(err)
		}
	}
}

// KnownTypes returns the types known for the given version.
func (s *Scheme) KnownTypes(gv schema.GroupVersion) map[string]reflect.Type {
	types := make(map[string]reflect.Type)
	for gvk, t := range s.gvkToType {
		if gv != gvk.GroupVersion() {
			continue
		}

		types[gvk.Kind] = t
	}
	return types
}

// VersionsForGroupKind returns the versions that a particular GroupKind can be converted to within the given group.
// A GroupKind might be converted to a different group. That information is available in EquivalentResourceMapper.
func (s *Scheme) VersionsForGroupKind(gk schema.GroupKind) []schema.GroupVersion {
	availableVersions := []schema.GroupVersion{}
	for gvk := range s.gvkToType {
		if gk != gvk.GroupKind() {
			continue
		}

		availableVersions = append(availableVersions, gvk.GroupVersion())
	}

	// order the return for stability
	ret := []schema.GroupVersion{}
	for _, version := range s.PrioritizedVersionsForGroup(gk.Group) {
		for _, availableVersion := range availableVersions {
			if version != availableVersion {
				continue
			}
			ret = append(ret, availableVersion)
		}
	}

	return ret
}

// AllKnownTypes returns the all known types.
func (s *Scheme) AllKnownTypes() map[schema.GroupVersionKind]reflect.Type {
	return s.gvkToType
}

// ObjectKinds returns all possible group,version,kind of the go object, true if the
// object is considered unversioned, or an error if it's not a pointer or is unregistered.
func (s *Scheme) ObjectKinds(obj Object) ([]schema.GroupVersionKind, bool, error) {
	// Unstructured objects are always considered to have their declared GVK
	if _, ok := obj.(Unstructured); ok {
		// we require that the GVK be populated in order to recognize the object
		gvk := obj.GetObjectKind().GroupVersionKind()
		if len(gvk.Kind) == 0 {
			return nil, false, NewMissingKindErr("unstructured object has no kind")
		}
		if len(gvk.Version) == 0 {
			return nil, false, NewMissingVersionErr("unstructured object has no version")
		}
		return []schema.GroupVersionKind{gvk}, false, nil
	}

	v, err := conversion.EnforcePtr(obj)
	if err != nil {
		return nil, false, err
	}
	t := v.Type()

	gvks, ok := s.typeToGVK[t]
	if !ok {
		return nil, false, NewNotRegisteredErrForType(s.schemeName, t)
	}
	_, unversionedType := s.unversionedTypes[t]

	return gvks, unversionedType, nil
}

// Recognizes returns true if the scheme is able to handle the provided group,version,kind
// of an object.
func (s *Scheme) Recognizes(gvk schema.GroupVersionKind) bool {
	_, exists := s.gvkToType[gvk]
	return exists
}

func (s *Scheme) IsUnversioned(obj Object) (bool, bool) {
	v, err := conversion.EnforcePtr(obj)
	if err != nil {
		return false, false
	}
	t := v.Type()

	if _, ok := s.typeToGVK[t]; !ok {
		return false, false
	}
	_, ok := s.unversionedTypes[t]
	return ok, true
}

// New returns a new API object of the given version and name, or an error if it hasn't
// been registered. The version and kind fields must be specified.
func (s *Scheme) New(kind schema.GroupVersionKind) (Object, error) {
	if t, exists := s.gvkToType[kind]; exists {
		return reflect.New(t).Interface().(Object), nil
	}

	if t, exists := s.unversionedKinds[kind.Kind]; exists {
		return reflect.New(t).Interface().(Object), nil
	}
	return nil, NewNotRegisteredErrForKind(s.schemeName, kind)
}

// AddIgnoredConversionType identifies a pair of types that should be skipped by
// conversion (because the data inside them is explicitly dropped during
// conversion).
func (s *Scheme) AddIgnoredConversionType(from, to interface{}) error {
	return s.converter.RegisterIgnoredConversion(from, to)
}

// AddConversionFunc registers a function that converts between a and b by passing objects of those
// types to the provided function. The function *must* accept objects of a and b - this machinery will not enforce
// any other guarantee.
func (s *Scheme) AddConversionFunc(a, b interface{}, fn conversion.ConversionFunc) error {
	return s.converter.RegisterUntypedConversionFunc(a, b, fn)
}

// AddGeneratedConversionFunc registers a function that converts between a and b by passing objects of those
// types to the provided function. The function *must* accept objects of a and b - this machinery will not enforce
// any other guarantee.
func (s *Scheme) AddGeneratedConversionFunc(a, b interface{}, fn conversion.ConversionFunc) error {
	return s.converter.RegisterGeneratedUntypedConversionFunc(a, b, fn)
}

// AddFieldLabelConversionFunc adds a conversion function to convert field selectors
// of the given kind from the given version to internal version representation.
func (s *Scheme) AddFieldLabelConversionFunc(gvk schema.GroupVersionKind, conversionFunc FieldLabelConversionFunc) error {
	s.fieldLabelConversionFuncs[gvk] = conversionFunc
	return nil
}

// AddTypeDefaultingFunc registers a function that is passed a pointer to an
// object and can default fields on the object. These functions will be invoked
// when Default() is called. The function will never be called unless the
// defaulted object matches srcType. If this function is invoked twice with the
// same srcType, the fn passed to the later call will be used instead.
func (s *Scheme) AddTypeDefaultingFunc(srcType Object, fn func(interface{})) {
	s.defaulterFuncs[reflect.TypeOf(srcType)] = fn
}

// Default sets defaults on the provided Object.
func (s *Scheme) Default(src Object) {
	if fn, ok := s.defaulterFuncs[reflect.TypeOf(src)]; ok {
		fn(src)
	}
}

// Convert will attempt to convert in into out. Both must be pointers. For easy
// testing of conversion functions. Returns an error if the conversion isn't
// possible. You can call this with types that haven't been registered (for example,
// a to test conversion of types that are nested within registered types). The
// context interface is passed to the convertor. Convert also supports Unstructured
// types and will convert them intelligently.
func (s *Scheme) Convert(in, out interface{}, context interface{}) error {
	unstructuredIn, okIn := in.(Unstructured)
	unstructuredOut, okOut := out.(Unstructured)
	switch {
	case okIn && okOut:
		// converting unstructured input to an unstructured output is a straight copy - unstructured
		// is a "smart holder" and the contents are passed by reference between the two objects
		unstructuredOut.SetUnstructuredContent(unstructuredIn.UnstructuredContent())
		return nil

	case okOut:
		// if the output is an unstructured object, use the standard Go type to unstructured
		// conversion. The object must not be internal.
		obj, ok := in.(Object)
		if !ok {
			return fmt.Errorf("unable to convert object type %T to Unstructured, must be a runtime.Object", in)
		}
		gvks, unversioned, err := s.ObjectKinds(obj)
		if err != nil {
			return err
		}
		gvk := gvks[0]

		// if no conversion is necessary, convert immediately
		if unversioned || gvk.Version != APIVersionInternal {
			content, err := DefaultUnstructuredConverter.ToUnstructured(in)
			if err != nil {
				return err
			}
			unstructuredOut.SetUnstructuredContent(content)
			unstructuredOut.GetObjectKind().SetGroupVersionKind(gvk)
			return nil
		}

		// attempt to convert the object to an external version first.
		target, ok := context.(GroupVersioner)
		if !ok {
			return fmt.Errorf("unable to convert the internal object type %T to Unstructured without providing a preferred version to convert to", in)
		}
		// Convert is implicitly unsafe, so we don't need to perform a safe conversion
		versioned, err := s.UnsafeConvertToVersion(obj, target)
		if err != nil {
			return err
		}
		content, err := DefaultUnstructuredConverter.ToUnstructured(versioned)
		if err != nil {
			return err
		}
		unstructuredOut.SetUnstructuredContent(content)
		return nil

	case okIn:
		// converting an unstructured object to any type is modeled by first converting
		// the input to a versioned type, then running standard conversions
		typed, err := s.unstructuredToTyped(unstructuredIn)
		if err != nil {
			return err
		}
		in = typed
	}

	meta := s.generateConvertMeta(in)
	meta.Context = context
	return s.converter.Convert(in, out, meta)
}

// ConvertFieldLabel alters the given field label and value for an kind field selector from
// versioned representation to an unversioned one or returns an error.
func (s *Scheme) ConvertFieldLabel(gvk schema.GroupVersionKind, label, value string) (string, string, error) {
	conversionFunc, ok := s.fieldLabelConversionFuncs[gvk]
	if !ok {
		return DefaultMetaV1FieldSelectorConversion(label, value)
	}
	return conversionFunc(label, value)
}

// ConvertToVersion attempts to convert an input object to its matching Kind in another
// version within this scheme. Will return an error if the provided version does not
// contain the inKind (or a mapping by name defined with AddKnownTypeWithName). Will also
// return an error if the conversion does not result in a valid Object being
// returned. Passes target down to the conversion methods as the Context on the scope.
func (s *Scheme) ConvertToVersion(in Object, target GroupVersioner) (Object, error) {
	return s.convertToVersion(true, in, target)
}

// UnsafeConvertToVersion will convert in to the provided target if such a conversion is possible,
// but does not guarantee the output object does not share fields with the input object. It attempts to be as
// efficient as possible when doing conversion.
func (s *Scheme) UnsafeConvertToVersion(in Object, target GroupVersioner) (Object, error) {
	return s.convertToVersion(false, in, target)
}

// convertToVersion handles conversion with an optional copy.
func (s *Scheme) convertToVersion(copy bool, in Object, target GroupVersioner) (Object, error) {
	var t reflect.Type

	if u, ok := in.(Unstructured); ok {
		typed, err := s.unstructuredToTyped(u)
		if err != nil {
			return nil, err
		}

		in = typed
		// unstructuredToTyped returns an Object, which must be a pointer to a struct.
		t = reflect.TypeOf(in).Elem()

	} else {
		// determine the incoming kinds with as few allocations as possible.
		t = reflect.TypeOf(in)
		if t.Kind() != reflect.Pointer {
			return nil, fmt.Errorf("only pointer types may be converted: %v", t)
		}
		t = t.Elem()
		if t.Kind() != reflect.Struct {
			return nil, fmt.Errorf("only pointers to struct types may be converted: %v", t)
		}
	}

	kinds, ok := s.typeToGVK[t]
	if !ok || len(kinds) == 0 {
		return nil, NewNotRegisteredErrForType(s.schemeName, t)
	}

	gvk, ok := target.KindForGroupVersionKinds(kinds)
	if !ok {
		// try to see if this type is listed as unversioned (for legacy support)
		// TODO: when we move to server API versions, we should completely remove the unversioned concept
		if unversionedKind, ok := s.unversionedTypes[t]; ok {
			if gvk, ok := target.KindForGroupVersionKinds([]schema.GroupVersionKind{unversionedKind}); ok {
				return copyAndSetTargetKind(copy, in, gvk)
			}
			return copyAndSetTargetKind(copy, in, unversionedKind)
		}
		return nil, NewNotRegisteredErrForTarget(s.schemeName, t, target)
	}

	// target wants to use the existing type, set kind and return (no conversion necessary)
	for _, kind := range kinds {
		if gvk == kind {
			return copyAndSetTargetKind(copy, in, gvk)
		}
	}

	// type is unversioned, no conversion necessary
	if unversionedKind, ok := s.unversionedTypes[t]; ok {
		if gvk, ok := target.KindForGroupVersionKinds([]schema.GroupVersionKind{unversionedKind}); ok {
			return copyAndSetTargetKind(copy, in, gvk)
		}
		return copyAndSetTargetKind(copy, in, unversionedKind)
	}

	out, err := s.New(gvk)
	if err != nil {
		return nil, err
	}

	if copy {
		in = in.DeepCopyObject()
	}

	meta := s.generateConvertMeta(in)
	meta.Context = target
	if err := s.converter.Convert(in, out, meta); err != nil {
		return nil, err
	}

	setTargetKind(out, gvk)
	return out, nil
}

// unstructuredToTyped attempts to transform an unstructured object to a typed
// object if possible. It will return an error if conversion is not possible, or the versioned
// Go form of the object. Note that this conversion will lose fields.
func (s *Scheme) unstructuredToTyped(in Unstructured) (Object, error) {
	// the type must be something we recognize
	gvks, _, err := s.ObjectKinds(in)
	if err != nil {
		return nil, err
	}
	typed, err := s.New(gvks[0])
	if err != nil {
		return nil, err
	}
	if err := DefaultUnstructuredConverter.FromUnstructured(in.UnstructuredContent(), typed); err != nil {
		return nil, fmt.Errorf("unable to convert unstructured object to %v: %v", gvks[0], err)
	}
	return typed, nil
}

// generateConvertMeta constructs the meta value we pass to Convert.
func (s *Scheme) generateConvertMeta(in interface{}) *conversion.Meta {
	return s.converter.DefaultMeta(reflect.TypeOf(in))
}

// copyAndSetTargetKind performs a conditional copy before returning the object, or an error if copy was not successful.
func copyAndSetTargetKind(copy bool, obj Object, kind schema.GroupVersionKind) (Object, error) {
	if copy {
		obj = obj.DeepCopyObject()
	}
	setTargetKind(obj, kind)
	return obj, nil
}

// setTargetKind sets the kind on an object, taking into account whether the target kind is the internal version.
func setTargetKind(obj Object, kind schema.GroupVersionKind) {
	if kind.Version == APIVersionInternal {
		// internal is a special case
		// TODO: look at removing the need to special case this
		obj.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
		return
	}
	obj.GetObjectKind().SetGroupVersionKind(kind)
}

// SetVersionPriority allows specifying a precise order of priority. All specified versions must be in the same group,
// and the specified order overwrites any previously specified order for this group
func (s *Scheme) SetVersionPriority(versions ...schema.GroupVersion) error {
	groups := sets.String{}
	order := []string{}
	for _, version := range versions {
		if len(version.Version) == 0 || version.Version == APIVersionInternal {
			return fmt.Errorf("internal versions cannot be prioritized: %v", version)
		}

		groups.Insert(version.Group)
		order = append(order, version.Version)
	}
	if len(groups) != 1 {
		return fmt.Errorf("must register versions for exactly one group: %v", strings.Join(groups.List(), ", "))
	}

	s.versionPriority[groups.List()[0]] = order
	return nil
}

// PrioritizedVersionsForGroup returns versions for a single group in priority order
func (s *Scheme) PrioritizedVersionsForGroup(group string) []schema.GroupVersion {
	ret := []schema.GroupVersion{}
	for _, version := range s.versionPriority[group] {
		ret = append(ret, schema.GroupVersion{Group: group, Version: version})
	}
	for _, observedVersion := range s.observedVersions {
		if observedVersion.Group != group {
			continue
		}
		found := false
		for _, existing := range ret {
			if existing == observedVersion {
				found = true
				break
			}
		}
		if !found {
			ret = append(ret, observedVersion)
		}
	}

	return ret
}

// PrioritizedVersionsAllGroups returns all known versions in their priority order.  Groups are random, but
// versions for a single group are prioritized
func (s *Scheme) PrioritizedVersionsAllGroups() []schema.GroupVersion {
	ret := []schema.GroupVersion{}
	for group, versions := range s.versionPriority {
		for _, version := range versions {
			ret = append(ret, schema.GroupVersion{Group: group, Version: version})
		}
	}
	for _, observedVersion := range s.observedVersions {
		found := false
		for _, existing := range ret {
			if existing == observedVersion {
				found = true
				break
			}
		}
		if !found {
			ret = append(ret, observedVersion)
		}
	}
	return ret
}

// PreferredVersionAllGroups returns the most preferred version for every group.
// group ordering is random.
func (s *Scheme) PreferredVersionAllGroups() []schema.GroupVersion {
	ret := []schema.GroupVersion{}
	for group, versions := range s.versionPriority {
		for _, version := range versions {
			ret = append(ret, schema.GroupVersion{Group: group, Version: version})
			break
		}
	}
	for _, observedVersion := range s.observedVersions {
		found := false
		for _, existing := range ret {
			if existing.Group == observedVersion.Group {
				found = true
				break
			}
		}
		if !found {
			ret = append(ret, observedVersion)
		}
	}

	return ret
}

// IsGroupRegistered returns true if types for the group have been registered with the scheme
func (s *Scheme) IsGroupRegistered(group string) bool {
	for _, observedVersion := range s.observedVersions {
		if observedVersion.Group == group {
			return true
		}
	}
	return false
}

// IsVersionRegistered returns true if types for the version have been registered with the scheme
func (s *Scheme) IsVersionRegistered(version schema.GroupVersion) bool {
	for _, observedVersion := range s.observedVersions {
		if observedVersion == version {
			return true
		}
	}

	return false
}

func (s *Scheme) addObservedVersion(version schema.GroupVersion) {
	if len(version.Version) == 0 || version.Version == APIVersionInternal {
		return
	}
	for _, observedVersion := range s.observedVersions {
		if observedVersion == version {
			return
		}
	}

	s.observedVersions = append(s.observedVersions, version)
}

func (s *Scheme) Name() string {
	return s.schemeName
}

// internalPackages are packages that ignored when creating a default reflector name. These packages are in the common
// call chains to NewReflector, so they'd be low entropy names for reflectors
var internalPackages = []string{"k8s.io/apimachinery/pkg/runtime/scheme.go"}
