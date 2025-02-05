/*
Copyright 2020 The Kubernetes Authors.

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

package internal_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/managedfields/managedfieldstest"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields/internal"
	"sigs.k8s.io/yaml"
)

func TestManagedFieldsUpdateDoesModifyTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = updateObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()

	time.Sleep(time.Second)

	err = updateObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "new-value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()

	if previousManagedFields[0].Time.Equal(newManagedFields[0].Time) {
		t.Errorf("ManagedFields time has not been updated:\n%v", newManagedFields)
	}
}

func TestManagedFieldsApplyDoesModifyTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = applyObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()

	time.Sleep(time.Second)

	err = applyObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "new-value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()

	if previousManagedFields[0].Time.Equal(newManagedFields[0].Time) {
		t.Errorf("ManagedFields time has not been updated:\n%v", newManagedFields)
	}
}

func TestManagedFieldsUpdateWithoutChangesDoesNotModifyTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = updateObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()

	time.Sleep(time.Second)

	err = updateObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()

	if !previousManagedFields[0].Time.Equal(newManagedFields[0].Time) {
		t.Errorf("ManagedFields time has changed:\nBefore:\n%v\nAfter:\n%v", previousManagedFields, newManagedFields)
	}
}

func TestManagedFieldsApplyWithoutChangesDoesNotModifyTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = applyObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()

	time.Sleep(time.Second)

	err = applyObject(f, "fieldmanager_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()

	if !previousManagedFields[0].Time.Equal(newManagedFields[0].Time) {
		t.Errorf("ManagedFields time has changed:\nBefore:\n%v\nAfter:\n%v", previousManagedFields, newManagedFields)
	}
}

func TestNonManagedFieldsUpdateDoesNotModifyTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = updateObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	err = updateObject(f, "fieldmanager_b_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_b": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()
	previousEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range previousManagedFields {
		previousEntries[entry.Manager] = entry
	}

	time.Sleep(time.Second)

	err = updateObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value",
			"key_b": "new-value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()
	newEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range newManagedFields {
		newEntries[entry.Manager] = entry
	}

	if _, ok := newEntries["fieldmanager_b_test"]; ok {
		t.Errorf("FieldManager B ManagedFields has changed:\n%v", newEntries["fieldmanager_b_test"])
	}
}

func TestNonManagedFieldsApplyDoesNotModifyTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = applyObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	err = applyObject(f, "fieldmanager_b_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_b": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()
	previousEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range previousManagedFields {
		previousEntries[entry.Manager] = entry
	}

	time.Sleep(time.Second)

	err = applyObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "new-value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()
	newEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range newManagedFields {
		newEntries[entry.Manager] = entry
	}

	if !previousEntries["fieldmanager_b_test"].Time.Equal(newEntries["fieldmanager_b_test"].Time) {
		t.Errorf("FieldManager B ManagedFields time changed:\nBefore:\n%v\nAfter:\n%v",
			previousEntries["fieldmanager_b_test"], newEntries["fieldmanager_b_test"])
	}
}

func TestTakingOverManagedFieldsDuringUpdateDoesNotModifyPreviousManagerTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = updateObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value",
			"key_b": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()
	previousEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range previousManagedFields {
		previousEntries[entry.Manager] = entry
	}

	time.Sleep(time.Second)

	err = updateObject(f, "fieldmanager_b_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_b": "new-value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()
	newEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range newManagedFields {
		newEntries[entry.Manager] = entry
	}

	if !previousEntries["fieldmanager_a_test"].Time.Equal(newEntries["fieldmanager_a_test"].Time) {
		t.Errorf("FieldManager A ManagedFields time has been updated:\nBefore:\n%v\nAfter:\n%v",
			previousEntries["fieldmanager_a_test"], newEntries["fieldmanager_a_test"])
	}
}

func TestTakingOverManagedFieldsDuringApplyDoesNotModifyPreviousManagerTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = applyObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value",
			"key_b": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()
	previousEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range previousManagedFields {
		previousEntries[entry.Manager] = entry
	}

	time.Sleep(time.Second)

	err = applyObject(f, "fieldmanager_b_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_b": "new-value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()
	newEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range newManagedFields {
		newEntries[entry.Manager] = entry
	}

	if !previousEntries["fieldmanager_a_test"].Time.Equal(newEntries["fieldmanager_a_test"].Time) {
		t.Errorf("FieldManager A ManagedFields time has been updated:\nBefore:\n%v\nAfter:\n%v",
			previousEntries["fieldmanager_a_test"], newEntries["fieldmanager_a_test"])
	}
}

func TestCoOwningManagedFieldsByApplyingSameObjResultsInNewManagerTime(t *testing.T) {
	var err error
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "ConfigMap"))

	err = applyObject(f, "fieldmanager_a_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value",
			"key_b": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	previousManagedFields := f.ManagedFields()
	previousEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range previousManagedFields {
		previousEntries[entry.Manager] = entry
	}

	time.Sleep(time.Second)

	err = applyObject(f, "fieldmanager_b_test", []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "configmap"
		},
		"data": {
			"key_a": "value",
			"key_b": "value"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	newManagedFields := f.ManagedFields()
	newEntries := map[string]v1.ManagedFieldsEntry{}
	for _, entry := range newManagedFields {
		newEntries[entry.Manager] = entry
	}

	if !reflect.DeepEqual(previousEntries["fieldmanager_a_test"].FieldsV1, newEntries["fieldmanager_a_test"].FieldsV1) {
		t.Errorf("FieldManager A's ManagedFields were changed':\nPrevious:\n%v\nNew:\n%v",
			previousEntries["fieldmanager_a_test"], newEntries["fieldmanager_a_test"])
	}

	if !reflect.DeepEqual(newEntries["fieldmanager_a_test"].FieldsV1, newEntries["fieldmanager_b_test"].FieldsV1) {
		t.Errorf("FieldManagers A and B do not co-own all the ManagedFields:\nFieldManager A:\n%v\nFieldManager B:\n%v",
			newEntries["fieldmanager_a_test"], newEntries["fieldmanager_b_test"])
	}

	if newEntries["fieldmanager_b_test"].Time == nil {
		t.Errorf("FieldManager B's ManagedFields time is missing:\nFieldManager A:\n%v\nFieldManager B:\n%v",
			newEntries["fieldmanager_a_test"], newEntries["fieldmanager_b_test"])
	}

	if newEntries["fieldmanager_a_test"].Time.Equal(newEntries["fieldmanager_b_test"].Time) {
		t.Errorf("FieldManager A and B have the same ManagedFields time:\nFieldManager A:\n%v\nFieldManager B:\n%v",
			newEntries["fieldmanager_a_test"], newEntries["fieldmanager_b_test"])
	}
}

type NoopManager struct{}

func (NoopManager) Apply(liveObj, appliedObj runtime.Object, managed internal.Managed, fieldManager string, force bool) (runtime.Object, internal.Managed, error) {
	return nil, managed, nil
}

func (NoopManager) Update(liveObj, newObj runtime.Object, managed internal.Managed, manager string) (runtime.Object, internal.Managed, error) {
	return nil, nil, nil
}

func updateObject(f managedfieldstest.TestFieldManager, fieldManagerName string, object []byte) error {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := yaml.Unmarshal(object, &obj.Object); err != nil {
		return fmt.Errorf("error decoding YAML: %v", err)
	}
	if err := f.Update(obj, fieldManagerName); err != nil {
		return fmt.Errorf("failed to update object: %v", err)
	}
	return nil
}

func applyObject(f managedfieldstest.TestFieldManager, fieldManagerName string, object []byte) error {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := yaml.Unmarshal(object, &obj.Object); err != nil {
		return fmt.Errorf("error decoding YAML: %v", err)
	}
	if err := f.Apply(obj, fieldManagerName, true); err != nil {
		return fmt.Errorf("failed to apply object: %v", err)
	}
	return nil
}

// Ensures that if ManagedFieldsUpdater gets a nil value from its nested manager
// chain (meaning the operation was a no-op), then the ManagedFieldsUpdater
// itself will return a copy of the input live object, with its managed fields
// removed
func TestNilNewObjectReplacedWithDeepCopyExcludingManagedFields(t *testing.T) {
	// Initialize our "live object" with some managed fields
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := yaml.Unmarshal([]byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "pod",
			"labels": {"app": "nginx"},
			"managedFields": [
				{
					"apiVersion": "v1",
					"fieldsType": "FieldsV1",
					"fieldsV1": {
						"f:metadata": {
							"f:labels": {
								"f:app": {}
							}
						}
					},
					"manager": "fieldmanager_test",
					"operation": "Apply",
					"time": "2021-11-11T18:41:17Z"
				}
			]
		}
	}`), &obj.Object); err != nil {
		t.Fatalf("error decoding YAML: %v", err)
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatalf("couldn't get accessor: %v", err)
	}

	// Decode the managed fields in the live object, since it isn't allowed in the patch.
	managed, err := internal.DecodeManagedFields(accessor.GetManagedFields())
	if err != nil {
		t.Fatalf("failed to decode managed fields: %v", err)
	}

	updater := internal.NewManagedFieldsUpdater(NoopManager{})

	newObject, _, err := updater.Apply(obj, obj.DeepCopyObject(), managed, "some_manager", false)
	if err != nil {
		t.Fatalf("failed to apply configuration %v", err)
	}

	if newObject == obj {
		t.Fatalf("returned newObject must not be the same instance as the passed in liveObj")
	}

	// Rip off managed fields of live, and check that it is deeply
	// equal to newObject
	liveWithoutManaged := obj.DeepCopyObject()
	internal.RemoveObjectManagedFields(liveWithoutManaged)

	if !reflect.DeepEqual(liveWithoutManaged, newObject) {
		t.Fatalf("returned newObject must be deeply equal to the input live object, without managed fields")
	}
}
