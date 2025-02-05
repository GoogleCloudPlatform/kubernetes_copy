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

package strategicpatch

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/mergepatch"
)

// An alternate implementation of JSON Merge Patch
// (https://tools.ietf.org/html/rfc7386) which supports the ability to annotate
// certain fields with metadata that indicates whether the elements of JSON
// lists should be merged or replaced.
//
// For more information, see the PATCH section of docs/devel/api-conventions.md.
//
// Some of the content of this package was borrowed with minor adaptations from
// evanphx/json-patch and openshift/origin.

const (
	directiveMarker  = "$patch"
	deleteDirective  = "delete"
	replaceDirective = "replace"
	mergeDirective   = "merge"

	retainKeysStrategy = "retainKeys"

	deleteFromPrimitiveListDirectivePrefix = "$deleteFromPrimitiveList"
	retainKeysDirective                    = "$" + retainKeysStrategy
	setElementOrderDirectivePrefix         = "$setElementOrder"
)

var supportDuplicateMergeKeyValues = true

// JSONMap is a representations of JSON object encoded as map[string]interface{}
// where the children can be either map[string]interface{}, []interface{} or
// primitive type).
// Operating on JSONMap representation is much faster as it doesn't require any
// json marshaling and/or unmarshaling operations.
type JSONMap map[string]interface{}

type DiffOptions struct {
	// SetElementOrder determines whether we generate the $setElementOrder parallel list.
	SetElementOrder bool
	// IgnoreChangesAndAdditions indicates if we keep the changes and additions in the patch.
	IgnoreChangesAndAdditions bool
	// IgnoreDeletions indicates if we keep the deletions in the patch.
	IgnoreDeletions bool
	// We introduce a new value retainKeys for patchStrategy.
	// It indicates that all fields needing to be preserved must be
	// present in the `retainKeys` list.
	// And the fields that are present will be merged with live object.
	// All the missing fields will be cleared when patching.
	BuildRetainKeysDirective bool
}

type MergeOptions struct {
	// MergeParallelList indicates if we are merging the parallel list.
	// We don't merge parallel list when calling mergeMap() in CreateThreeWayMergePatch()
	// which is called client-side.
	// We merge parallel list iff when calling mergeMap() in StrategicMergeMapPatch()
	// which is called server-side
	MergeParallelList bool
	// IgnoreUnmatchedNulls indicates if we should process the unmatched nulls.
	IgnoreUnmatchedNulls bool
}

// The following code is adapted from github.com/openshift/origin/pkg/util/jsonmerge.
// Instead of defining a Delta that holds an original, a patch and a set of preconditions,
// the reconcile method accepts a set of preconditions as an argument.

// CreateTwoWayMergePatch creates a patch that can be passed to StrategicMergePatch from an original
// document and a modified document, which are passed to the method as json encoded content. It will
// return a patch that yields the modified document when applied to the original document, or an error
// if either of the two documents is invalid.
func CreateTwoWayMergePatch(original, modified []byte, dataStruct interface{}, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	return CreateTwoWayMergePatchUsingLookupPatchMeta(original, modified, schema, fns...)
}

func CreateTwoWayMergePatchUsingLookupPatchMeta(
	original, modified []byte, schema LookupPatchMeta, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	originalMap := map[string]interface{}{}
	if len(original) > 0 {
		if err := json.Unmarshal(original, &originalMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	modifiedMap := map[string]interface{}{}
	if len(modified) > 0 {
		if err := json.Unmarshal(modified, &modifiedMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	patchMap, err := CreateTwoWayMergeMapPatchUsingLookupPatchMeta(originalMap, modifiedMap, schema, fns...)
	if err != nil {
		return nil, err
	}

	return json.Marshal(patchMap)
}

// CreateTwoWayMergeMapPatch creates a patch from an original and modified JSON objects,
// encoded JSONMap.
// The serialized version of the map can then be passed to StrategicMergeMapPatch.
func CreateTwoWayMergeMapPatch(original, modified JSONMap, dataStruct interface{}, fns ...mergepatch.PreconditionFunc) (JSONMap, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	return CreateTwoWayMergeMapPatchUsingLookupPatchMeta(original, modified, schema, fns...)
}

func CreateTwoWayMergeMapPatchUsingLookupPatchMeta(original, modified JSONMap, schema LookupPatchMeta, fns ...mergepatch.PreconditionFunc) (JSONMap, error) {
	diffOptions := DiffOptions{
		SetElementOrder: true,
	}
	patchMap, err := diffMaps(original, modified, schema, diffOptions)
	if err != nil {
		return nil, err
	}

	// Apply the preconditions to the patch, and return an error if any of them fail.
	for _, fn := range fns {
		if !fn(patchMap) {
			return nil, mergepatch.NewErrPreconditionFailed(patchMap)
		}
	}

	return patchMap, nil
}

// Returns a (recursive) strategic merge patch that yields modified when applied to original.
// Including:
// - Adding fields to the patch present in modified, missing from original
// - Setting fields to the patch present in modified and original with different values
// - Delete fields present in original, missing from modified through
// - IFF map field - set to nil in patch
// - IFF list of maps && merge strategy - use deleteDirective for the elements
// - IFF list of primitives && merge strategy - use parallel deletion list
// - IFF list of maps or primitives with replace strategy (default) - set patch value to the value in modified
// - Build $retainKeys directive for fields with retainKeys patch strategy
func diffMaps(original, modified map[string]interface{}, schema LookupPatchMeta, diffOptions DiffOptions) (map[string]interface{}, error) {
	patch := map[string]interface{}{}

	// This will be used to build the $retainKeys directive sent in the patch
	retainKeysList := make([]interface{}, 0, len(modified))

	// Compare each value in the modified map against the value in the original map
	for key, modifiedValue := range modified {
		// Get the underlying type for pointers
		if diffOptions.BuildRetainKeysDirective && modifiedValue != nil {
			retainKeysList = append(retainKeysList, key)
		}

		originalValue, ok := original[key]
		if !ok {
			// Key was added, so add to patch
			if !diffOptions.IgnoreChangesAndAdditions {
				patch[key] = modifiedValue
			}
			continue
		}

		// The patch may have a patch directive
		// TODO: figure out if we need this. This shouldn't be needed by apply. When would the original map have patch directives in it?
		foundDirectiveMarker, err := handleDirectiveMarker(key, originalValue, modifiedValue, patch)
		if err != nil {
			return nil, err
		}
		if foundDirectiveMarker {
			continue
		}

		if reflect.TypeOf(originalValue) != reflect.TypeOf(modifiedValue) {
			// Types have changed, so add to patch
			if !diffOptions.IgnoreChangesAndAdditions {
				patch[key] = modifiedValue
			}
			continue
		}

		// Types are the same, so compare values
		switch originalValueTyped := originalValue.(type) {
		case map[string]interface{}:
			modifiedValueTyped := modifiedValue.(map[string]interface{})
			err = handleMapDiff(key, originalValueTyped, modifiedValueTyped, patch, schema, diffOptions)
		case []interface{}:
			modifiedValueTyped := modifiedValue.([]interface{})
			err = handleSliceDiff(key, originalValueTyped, modifiedValueTyped, patch, schema, diffOptions)
		default:
			replacePatchFieldIfNotEqual(key, originalValue, modifiedValue, patch, diffOptions)
		}
		if err != nil {
			return nil, err
		}
	}

	updatePatchIfMissing(original, modified, patch, diffOptions)
	// Insert the retainKeysList if there are values present in the retainKeysList and
	// either of the following is true:
	// - the patch is not empty
	// - there are additional field in original that need to be cleared
	if len(retainKeysList) > 0 &&
		(len(patch) > 0 || hasAdditionalNewField(original, modified)) {
		patch[retainKeysDirective] = sortScalars(retainKeysList)
	}
	return patch, nil
}

// handleDirectiveMarker handles how to diff directive marker between 2 objects
func handleDirectiveMarker(key string, originalValue, modifiedValue interface{}, patch map[string]interface{}) (bool, error) {
	if key == directiveMarker {
		originalString, ok := originalValue.(string)
		if !ok {
			return false, fmt.Errorf("invalid value for special key: %s", directiveMarker)
		}
		modifiedString, ok := modifiedValue.(string)
		if !ok {
			return false, fmt.Errorf("invalid value for special key: %s", directiveMarker)
		}
		if modifiedString != originalString {
			patch[directiveMarker] = modifiedValue
		}
		return true, nil
	}
	return false, nil
}

// handleMapDiff diff between 2 maps `originalValueTyped` and `modifiedValue`,
// puts the diff in the `patch` associated with `key`
// key is the key associated with originalValue and modifiedValue.
// originalValue, modifiedValue are the old and new value respectively.They are both maps
// patch is the patch map that contains key and the updated value, and it is the parent of originalValue, modifiedValue
// diffOptions contains multiple options to control how we do the diff.
func handleMapDiff(key string, originalValue, modifiedValue, patch map[string]interface{},
	schema LookupPatchMeta, diffOptions DiffOptions) error {
	subschema, patchMeta, err := schema.LookupPatchMetadataForStruct(key)

	if err != nil {
		// We couldn't look up metadata for the field
		// If the values are identical, this doesn't matter, no patch is needed
		if reflect.DeepEqual(originalValue, modifiedValue) {
			return nil
		}
		// Otherwise, return the error
		return err
	}
	retainKeys, patchStrategy, err := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
	if err != nil {
		return err
	}
	diffOptions.BuildRetainKeysDirective = retainKeys
	switch patchStrategy {
	// The patch strategic from metadata tells us to replace the entire object instead of diffing it
	case replaceDirective:
		if !diffOptions.IgnoreChangesAndAdditions {
			patch[key] = modifiedValue
		}
	default:
		patchValue, err := diffMaps(originalValue, modifiedValue, subschema, diffOptions)
		if err != nil {
			return err
		}
		// Maps were not identical, use provided patch value
		if len(patchValue) > 0 {
			patch[key] = patchValue
		}
	}
	return nil
}

// handleSliceDiff diff between 2 slices `originalValueTyped` and `modifiedValue`,
// puts the diff in the `patch` associated with `key`
// key is the key associated with originalValue and modifiedValue.
// originalValue, modifiedValue are the old and new value respectively.They are both slices
// patch is the patch map that contains key and the updated value, and it is the parent of originalValue, modifiedValue
// diffOptions contains multiple options to control how we do the diff.
func handleSliceDiff(key string, originalValue, modifiedValue []interface{}, patch map[string]interface{},
	schema LookupPatchMeta, diffOptions DiffOptions) error {
	subschema, patchMeta, err := schema.LookupPatchMetadataForSlice(key)
	if err != nil {
		// We couldn't look up metadata for the field
		// If the values are identical, this doesn't matter, no patch is needed
		if reflect.DeepEqual(originalValue, modifiedValue) {
			return nil
		}
		// Otherwise, return the error
		return err
	}
	retainKeys, patchStrategy, err := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
	if err != nil {
		return err
	}
	switch patchStrategy {
	// Merge the 2 slices using mergePatchKey
	case mergeDirective:
		diffOptions.BuildRetainKeysDirective = retainKeys
		addList, deletionList, setOrderList, err := diffLists(originalValue, modifiedValue, subschema, patchMeta.GetPatchMergeKey(), diffOptions)
		if err != nil {
			return err
		}
		if len(addList) > 0 {
			patch[key] = addList
		}
		// generate a parallel list for deletion
		if len(deletionList) > 0 {
			parallelDeletionListKey := fmt.Sprintf("%s/%s", deleteFromPrimitiveListDirectivePrefix, key)
			patch[parallelDeletionListKey] = deletionList
		}
		if len(setOrderList) > 0 {
			parallelSetOrderListKey := fmt.Sprintf("%s/%s", setElementOrderDirectivePrefix, key)
			patch[parallelSetOrderListKey] = setOrderList
		}
	default:
		replacePatchFieldIfNotEqual(key, originalValue, modifiedValue, patch, diffOptions)
	}
	return nil
}

// replacePatchFieldIfNotEqual updates the patch if original and modified are not deep equal
// if diffOptions.IgnoreChangesAndAdditions is false.
// original is the old value, maybe either the live cluster object or the last applied configuration
// modified is the new value, is always the users new config
func replacePatchFieldIfNotEqual(key string, original, modified interface{},
	patch map[string]interface{}, diffOptions DiffOptions) {
	if diffOptions.IgnoreChangesAndAdditions {
		// Ignoring changes - do nothing
		return
	}
	if reflect.DeepEqual(original, modified) {
		// Contents are identical - do nothing
		return
	}
	// Create a patch to replace the old value with the new one
	patch[key] = modified
}

// updatePatchIfMissing iterates over `original` when ignoreDeletions is false.
// Clear the field whose key is not present in `modified`.
// original is the old value, maybe either the live cluster object or the last applied configuration
// modified is the new value, is always the users new config
func updatePatchIfMissing(original, modified, patch map[string]interface{}, diffOptions DiffOptions) {
	if diffOptions.IgnoreDeletions {
		// Ignoring deletion - do nothing
		return
	}
	// Add nils for deleted values
	for key := range original {
		if _, found := modified[key]; !found {
			patch[key] = nil
		}
	}
}

// validateMergeKeyInLists checks if each map in the list has the mentryerge key.
func validateMergeKeyInLists(mergeKey string, lists ...[]interface{}) error {
	for _, list := range lists {
		for _, item := range list {
			m, ok := item.(map[string]interface{})
			if !ok {
				return mergepatch.ErrBadArgType(m, item)
			}
			if _, ok = m[mergeKey]; !ok {
				return mergepatch.ErrNoMergeKey(m, mergeKey)
			}
		}
	}
	return nil
}

// normalizeElementOrder sort `patch` list by `patchOrder` and sort `serverOnly` list by `serverOrder`.
// Then it merges the 2 sorted lists.
// It guarantee the relative order in the patch list and in the serverOnly list is kept.
// `patch` is a list of items in the patch, and `serverOnly` is a list of items in the live object.
// `patchOrder` is the order we want `patch` list to have and
// `serverOrder` is the order we want `serverOnly` list to have.
// kind is the kind of each item in the lists `patch` and `serverOnly`.
func normalizeElementOrder(patch, serverOnly, patchOrder, serverOrder []interface{}, mergeKey string, kind reflect.Kind) ([]interface{}, error) {
	patch, err := normalizeSliceOrder(patch, patchOrder, mergeKey, kind)
	if err != nil {
		return nil, err
	}
	serverOnly, err = normalizeSliceOrder(serverOnly, serverOrder, mergeKey, kind)
	if err != nil {
		return nil, err
	}
	all := mergeSortedSlice(serverOnly, patch, serverOrder, mergeKey, kind)

	return all, nil
}

// mergeSortedSlice merges the 2 sorted lists by serverOrder with best effort.
// It will insert each item in `left` list to `right` list. In most cases, the 2 lists will be interleaved.
// The relative order of left and right are guaranteed to be kept.
// They have higher precedence than the order in the live list.
// The place for a item in `left` is found by:
// scan from the place of last insertion in `right` to the end of `right`,
// the place is before the first item that is greater than the item we want to insert.
// example usage: using server-only items as left and patch items as right. We insert server-only items
// to patch list. We use the order of live object as record for comparison.
func mergeSortedSlice(left, right, serverOrder []interface{}, mergeKey string, kind reflect.Kind) []interface{} {
	// Returns if l is less than r, and if both have been found.
	// If l and r both present and l is in front of r, l is less than r.
	less := func(l, r interface{}) (bool, bool) {
		li := index(serverOrder, l, mergeKey, kind)
		ri := index(serverOrder, r, mergeKey, kind)
		if li >= 0 && ri >= 0 {
			return li < ri, true
		} else {
			return false, false
		}
	}

	// left and right should be non-overlapping.
	size := len(left) + len(right)
	i, j := 0, 0
	s := make([]interface{}, size, size)

	for k := 0; k < size; k++ {
		if i >= len(left) && j < len(right) {
			// have items left in `right` list
			s[k] = right[j]
			j++
		} else if j >= len(right) && i < len(left) {
			// have items left in `left` list
			s[k] = left[i]
			i++
		} else {
			// compare them if i and j are both in bound
			less, foundBoth := less(left[i], right[j])
			if foundBoth && less {
				s[k] = left[i]
				i++
			} else {
				s[k] = right[j]
				j++
			}
		}
	}
	return s
}

// index returns the index of the item in the given items, or -1 if it doesn't exist
// l must NOT be a slice of slices, this should be checked before calling.
func index(l []interface{}, valToLookUp interface{}, mergeKey string, kind reflect.Kind) int {
	var getValFn func(interface{}) interface{}
	// Get the correct `getValFn` based on item `kind`.
	// It should return the value of merge key for maps and
	// return the item for other kinds.
	switch kind {
	case reflect.Map:
		getValFn = func(item interface{}) interface{} {
			typedItem, ok := item.(map[string]interface{})
			if !ok {
				return nil
			}
			val := typedItem[mergeKey]
			return val
		}
	default:
		getValFn = func(item interface{}) interface{} {
			return item
		}
	}

	for i, v := range l {
		if getValFn(valToLookUp) == getValFn(v) {
			return i
		}
	}
	return -1
}

// extractToDeleteItems takes a list and
// returns 2 lists: one contains items that should be kept and the other contains items to be deleted.
func extractToDeleteItems(l []interface{}) ([]interface{}, []interface{}, error) {
	var nonDelete, toDelete []interface{}
	for _, v := range l {
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil, nil, mergepatch.ErrBadArgType(m, v)
		}

		directive, foundDirective := m[directiveMarker]
		if foundDirective && directive == deleteDirective {
			toDelete = append(toDelete, v)
		} else {
			nonDelete = append(nonDelete, v)
		}
	}
	return nonDelete, toDelete, nil
}

// normalizeSliceOrder sort `toSort` list by `order`
func normalizeSliceOrder(toSort, order []interface{}, mergeKey string, kind reflect.Kind) ([]interface{}, error) {
	var toDelete []interface{}
	if kind == reflect.Map {
		// make sure each item in toSort, order has merge key
		err := validateMergeKeyInLists(mergeKey, toSort, order)
		if err != nil {
			return nil, err
		}
		toSort, toDelete, err = extractToDeleteItems(toSort)
		if err != nil {
			return nil, err
		}
	}

	sort.SliceStable(toSort, func(i, j int) bool {
		if ii := index(order, toSort[i], mergeKey, kind); ii >= 0 {
			if ij := index(order, toSort[j], mergeKey, kind); ij >= 0 {
				return ii < ij
			}
		}
		return true
	})
	toSort = append(toSort, toDelete...)
	return toSort, nil
}

// Returns a (recursive) strategic merge patch, a parallel deletion list if necessary and
// another list to set the order of the list
// Only list of primitives with merge strategy will generate a parallel deletion list.
// These two lists should yield modified when applied to original, for lists with merge semantics.
func diffLists(original, modified []interface{}, schema LookupPatchMeta, mergeKey string, diffOptions DiffOptions) ([]interface{}, []interface{}, []interface{}, error) {
	if len(original) == 0 {
		// Both slices are empty - do nothing
		if len(modified) == 0 || diffOptions.IgnoreChangesAndAdditions {
			return nil, nil, nil, nil
		}

		// Old slice was empty - add all elements from the new slice
		return modified, nil, nil, nil
	}

	elementType, err := sliceElementType(original, modified)
	if err != nil {
		return nil, nil, nil, err
	}

	var patchList, deleteList, setOrderList []interface{}
	kind := elementType.Kind()
	switch kind {
	case reflect.Map:
		patchList, deleteList, err = diffListsOfMaps(original, modified, schema, mergeKey, diffOptions)
		if err != nil {
			return nil, nil, nil, err
		}
		patchList, err = normalizeSliceOrder(patchList, modified, mergeKey, kind)
		if err != nil {
			return nil, nil, nil, err
		}
		orderSame, err := isOrderSame(original, modified, mergeKey)
		if err != nil {
			return nil, nil, nil, err
		}
		// append the deletions to the end of the patch list.
		patchList = append(patchList, deleteList...)
		deleteList = nil
		// generate the setElementOrder list when there are content changes or order changes
		if diffOptions.SetElementOrder &&
			((!diffOptions.IgnoreChangesAndAdditions && (len(patchList) > 0 || !orderSame)) ||
				(!diffOptions.IgnoreDeletions && len(patchList) > 0)) {
			// Generate a list of maps that each item contains only the merge key.
			setOrderList = make([]interface{}, len(modified))
			for i, v := range modified {
				typedV := v.(map[string]interface{})
				setOrderList[i] = map[string]interface{}{
					mergeKey: typedV[mergeKey],
				}
			}
		}
	case reflect.Slice:
		// Lists of Lists are not permitted by the api
		return nil, nil, nil, mergepatch.ErrNoListOfLists
	default:
		patchList, deleteList, err = diffListsOfScalars(original, modified, diffOptions)
		if err != nil {
			return nil, nil, nil, err
		}
		patchList, err = normalizeSliceOrder(patchList, modified, mergeKey, kind)
		// generate the setElementOrder list when there are content changes or order changes
		if diffOptions.SetElementOrder && ((!diffOptions.IgnoreDeletions && len(deleteList) > 0) ||
			(!diffOptions.IgnoreChangesAndAdditions && !reflect.DeepEqual(original, modified))) {
			setOrderList = modified
		}
	}
	return patchList, deleteList, setOrderList, err
}

// isOrderSame checks if the order in a list has changed
func isOrderSame(original, modified []interface{}, mergeKey string) (bool, error) {
	if len(original) != len(modified) {
		return false, nil
	}
	for i, modifiedItem := range modified {
		equal, err := mergeKeyValueEqual(original[i], modifiedItem, mergeKey)
		if err != nil || !equal {
			return equal, err
		}
	}
	return true, nil
}

// diffListsOfScalars returns 2 lists, the first one is addList and the second one is deletionList.
// Argument diffOptions.IgnoreChangesAndAdditions controls if calculate addList. true means not calculate.
// Argument diffOptions.IgnoreDeletions controls if calculate deletionList. true means not calculate.
// original may be changed, but modified is guaranteed to not be changed
func diffListsOfScalars(original, modified []interface{}, diffOptions DiffOptions) ([]interface{}, []interface{}, error) {
	modifiedCopy := make([]interface{}, len(modified))
	copy(modifiedCopy, modified)
	// Sort the scalars for easier calculating the diff
	originalScalars := sortScalars(original)
	modifiedScalars := sortScalars(modifiedCopy)

	originalIndex, modifiedIndex := 0, 0
	addList := []interface{}{}
	deletionList := []interface{}{}

	for {
		originalInBounds := originalIndex < len(originalScalars)
		modifiedInBounds := modifiedIndex < len(modifiedScalars)
		if !originalInBounds && !modifiedInBounds {
			break
		}
		// we need to compare the string representation of the scalar,
		// because the scalar is an interface which doesn't support either < or >
		// And that's how func sortScalars compare scalars.
		var originalString, modifiedString string
		var originalValue, modifiedValue interface{}
		if originalInBounds {
			originalValue = originalScalars[originalIndex]
			originalString = fmt.Sprintf("%v", originalValue)
		}
		if modifiedInBounds {
			modifiedValue = modifiedScalars[modifiedIndex]
			modifiedString = fmt.Sprintf("%v", modifiedValue)
		}

		originalV, modifiedV := compareListValuesAtIndex(originalInBounds, modifiedInBounds, originalString, modifiedString)
		switch {
		case originalV == nil && modifiedV == nil:
			originalIndex++
			modifiedIndex++
		case originalV != nil && modifiedV == nil:
			if !diffOptions.IgnoreDeletions {
				deletionList = append(deletionList, originalValue)
			}
			originalIndex++
		case originalV == nil && modifiedV != nil:
			if !diffOptions.IgnoreChangesAndAdditions {
				addList = append(addList, modifiedValue)
			}
			modifiedIndex++
		default:
			return nil, nil, fmt.Errorf("Unexpected returned value from compareListValuesAtIndex: %v and %v", originalV, modifiedV)
		}
	}

	return addList, deduplicateScalars(deletionList), nil
}

// If first return value is non-nil, list1 contains an element not present in list2
// If second return value is non-nil, list2 contains an element not present in list1
func compareListValuesAtIndex(list1Inbounds, list2Inbounds bool, list1Value, list2Value string) (interface{}, interface{}) {
	bothInBounds := list1Inbounds && list2Inbounds
	switch {
	// scalars are identical
	case bothInBounds && list1Value == list2Value:
		return nil, nil
	// only list2 is in bound
	case !list1Inbounds:
		fallthrough
	// list2 has additional scalar
	case bothInBounds && list1Value > list2Value:
		return nil, list2Value
	// only original is in bound
	case !list2Inbounds:
		fallthrough
	// original has additional scalar
	case bothInBounds && list1Value < list2Value:
		return list1Value, nil
	default:
		return nil, nil
	}
}

// diffListsOfMaps takes a pair of lists and
// returns a (recursive) strategic merge patch list contains additions and changes and
// a deletion list contains deletions
func diffListsOfMaps(original, modified []interface{}, schema LookupPatchMeta, mergeKey string, diffOptions DiffOptions) ([]interface{}, []interface{}, error) {
	patch := make([]interface{}, 0, len(modified))
	deletionList := make([]interface{}, 0, len(original))

	originalSorted, err := sortMergeListsByNameArray(original, schema, mergeKey, false)
	if err != nil {
		return nil, nil, err
	}
	modifiedSorted, err := sortMergeListsByNameArray(modified, schema, mergeKey, false)
	if err != nil {
		return nil, nil, err
	}

	var previousOriginalElementMergeKeyValueString, previousModifiedElementMergeKeyValueString string
	bothPreviousInBounds := false

	originalIndex, modifiedIndex := 0, 0
	for {
		originalInBounds := originalIndex < len(originalSorted)
		modifiedInBounds := modifiedIndex < len(modifiedSorted)
		bothInBounds := originalInBounds && modifiedInBounds
		if !originalInBounds && !modifiedInBounds {
			break
		}

		var originalElementMergeKeyValueString, modifiedElementMergeKeyValueString string
		var originalElementMergeKeyValue, modifiedElementMergeKeyValue interface{}
		var originalElement, modifiedElement map[string]interface{}
		if originalInBounds {
			originalElement, originalElementMergeKeyValue, err = getMapAndMergeKeyValueByIndex(originalIndex, mergeKey, originalSorted)
			if err != nil {
				return nil, nil, err
			}
			originalElementMergeKeyValueString = fmt.Sprintf("%v", originalElementMergeKeyValue)
		}
		if modifiedInBounds {
			modifiedElement, modifiedElementMergeKeyValue, err = getMapAndMergeKeyValueByIndex(modifiedIndex, mergeKey, modifiedSorted)
			if err != nil {
				return nil, nil, err
			}
			modifiedElementMergeKeyValueString = fmt.Sprintf("%v", modifiedElementMergeKeyValue)
		}

		switch {
		case bothInBounds && ItemMatchesOriginalAndModifiedSlice(originalElementMergeKeyValueString, modifiedElementMergeKeyValueString):
			// Merge key values are equal, so recurse
			patchValue, err := diffMaps(originalElement, modifiedElement, schema, diffOptions)
			if err != nil {
				return nil, nil, err
			}
			if len(patchValue) > 0 {
				patchValue[mergeKey] = modifiedElementMergeKeyValue
				patch = append(patch, patchValue)
			}
			originalIndex++
			modifiedIndex++
		// only modified is in bound
		case !originalInBounds:
			fallthrough
		// modified has additional map
		case bothInBounds && ItemAddedToModifiedSlice(originalElementMergeKeyValueString, modifiedElementMergeKeyValueString):
			if !diffOptions.IgnoreChangesAndAdditions {
				patch = append(patch, modifiedElement)
			}
			modifiedIndex++
		// modified missing one of duplicated by MergeKey value elements
		case supportDuplicateMergeKeyValues && bothPreviousInBounds &&
			originalElementMergeKeyValueString == previousOriginalElementMergeKeyValueString &&
			previousOriginalElementMergeKeyValueString == previousModifiedElementMergeKeyValueString:
			// if deleted one of duplicates by mergeKey, will send "delete" and "insert" commands
			if !diffOptions.IgnoreDeletions {
				var modifiedToAdd []interface{}
				for i := modifiedIndex - 1; i >= 0; i-- {
					element, mergeKeyValue, err := getMapAndMergeKeyValueByIndex(i, mergeKey, modifiedSorted)
					if err != nil {
						return nil, nil, err
					}
					if fmt.Sprintf("%v", mergeKeyValue) != originalElementMergeKeyValueString {
						break
					}
					// reverse to keep ordering
					modifiedToAdd = append([]interface{}{element}, modifiedToAdd...)
				}
				deletionList = append(deletionList, CreateDeleteDirective(mergeKey, originalElementMergeKeyValue))
				// remove existing occurencies added if previous elements pair has diffs
				patch, err = removePatchElementsByMergeKeyValue(patch, mergeKey, originalElementMergeKeyValueString)
				if err != nil {
					return nil, nil, err
				}
				patch = append(patch, modifiedToAdd...)
			}
			originalIndex++
		// only original is in bound
		case !modifiedInBounds:
			fallthrough
		// original has additional map
		case bothInBounds && ItemRemovedFromModifiedSlice(originalElementMergeKeyValueString, modifiedElementMergeKeyValueString):
			if !diffOptions.IgnoreDeletions {
				// Item was deleted, so add delete directive
				deletionList = append(deletionList, CreateDeleteDirective(mergeKey, originalElementMergeKeyValue))
			}
			originalIndex++
		}

		previousOriginalElementMergeKeyValueString = originalElementMergeKeyValueString
		previousModifiedElementMergeKeyValueString = modifiedElementMergeKeyValueString
		bothPreviousInBounds = bothInBounds
	}

	return patch, deletionList, nil
}

// removePatchElementsByMergeKeyValue removes all elements from patch list with matching MergeKey value
func removePatchElementsByMergeKeyValue(patch []interface{}, mergeKey, mergeKeyValue string) ([]interface{}, error) {
	result := make([]interface{}, 0, len(patch))
	for i, val := range patch {
		_, key, err := getMapAndMergeKeyValueByIndex(i, mergeKey, patch)
		if err != nil {
			return nil, err
		}
		if fmt.Sprintf("%v", key) != mergeKeyValue {
			result = append(result, val)
		}
	}
	return result, nil
}

// getMapAndMergeKeyValueByIndex return a map in the list and its merge key value given the index of the map.
func getMapAndMergeKeyValueByIndex(index int, mergeKey string, listOfMaps []interface{}) (map[string]interface{}, interface{}, error) {
	m, ok := listOfMaps[index].(map[string]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(m, listOfMaps[index])
	}

	val, ok := m[mergeKey]
	if !ok {
		return nil, nil, mergepatch.ErrNoMergeKey(m, mergeKey)
	}
	return m, val, nil
}

// StrategicMergePatch applies a strategic merge patch. The patch and the original document
// must be json encoded content. A patch can be created from an original and a modified document
// by calling CreateStrategicMergePatch.
func StrategicMergePatch(original, patch []byte, dataStruct interface{}) ([]byte, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	return StrategicMergePatchUsingLookupPatchMeta(original, patch, schema)
}

func StrategicMergePatchUsingLookupPatchMeta(original, patch []byte, schema LookupPatchMeta) ([]byte, error) {
	originalMap, err := handleUnmarshal(original)
	if err != nil {
		return nil, err
	}
	patchMap, err := handleUnmarshal(patch)
	if err != nil {
		return nil, err
	}

	result, err := StrategicMergeMapPatchUsingLookupPatchMeta(originalMap, patchMap, schema)
	if err != nil {
		return nil, err
	}

	return json.Marshal(result)
}

func handleUnmarshal(j []byte) (map[string]interface{}, error) {
	if j == nil {
		j = []byte("{}")
	}

	m := map[string]interface{}{}
	err := json.Unmarshal(j, &m)
	if err != nil {
		return nil, mergepatch.ErrBadJSONDoc
	}
	return m, nil
}

// StrategicMergeMapPatch applies a strategic merge patch. The original and patch documents
// must be JSONMap. A patch can be created from an original and modified document by
// calling CreateTwoWayMergeMapPatch.
// Warning: the original and patch JSONMap objects are mutated by this function and should not be reused.
func StrategicMergeMapPatch(original, patch JSONMap, dataStruct interface{}) (JSONMap, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	// We need the go struct tags `patchMergeKey` and `patchStrategy` for fields that support a strategic merge patch.
	// For native resources, we can easily figure out these tags since we know the fields.

	// Because custom resources are decoded as Unstructured and because we're missing the metadata about how to handle
	// each field in a strategic merge patch, we can't find the go struct tags. Hence, we can't easily  do a strategic merge
	// for custom resources. So we should fail fast and return an error.
	if _, ok := dataStruct.(*unstructured.Unstructured); ok {
		return nil, mergepatch.ErrUnsupportedStrategicMergePatchFormat
	}

	return StrategicMergeMapPatchUsingLookupPatchMeta(original, patch, schema)
}

func StrategicMergeMapPatchUsingLookupPatchMeta(original, patch JSONMap, schema LookupPatchMeta) (JSONMap, error) {
	mergeOptions := MergeOptions{
		MergeParallelList:    true,
		IgnoreUnmatchedNulls: true,
	}
	return mergeMap(original, patch, schema, mergeOptions)
}

// MergeStrategicMergeMapPatchUsingLookupPatchMeta merges strategic merge
// patches retaining `null` fields and parallel lists. If 2 patches change the
// same fields and the latter one will override the former one. If you don't
// want that happen, you need to run func MergingMapsHaveConflicts before
// merging these patches. Applying the resulting merged merge patch to a JSONMap
// yields the same as merging each strategic merge patch to the JSONMap in
// succession.
func MergeStrategicMergeMapPatchUsingLookupPatchMeta(schema LookupPatchMeta, patches ...JSONMap) (JSONMap, error) {
	mergeOptions := MergeOptions{
		MergeParallelList:    false,
		IgnoreUnmatchedNulls: false,
	}
	merged := JSONMap{}
	var err error
	for _, patch := range patches {
		merged, err = mergeMap(merged, patch, schema, mergeOptions)
		if err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// handleDirectiveInMergeMap handles the patch directive when merging 2 maps.
func handleDirectiveInMergeMap(directive interface{}, patch map[string]interface{}) (map[string]interface{}, error) {
	if directive == replaceDirective {
		// If the patch contains "$patch: replace", don't merge it, just use the
		// patch directly. Later on, we can add a single level replace that only
		// affects the map that the $patch is in.
		delete(patch, directiveMarker)
		return patch, nil
	}

	if directive == deleteDirective {
		// If the patch contains "$patch: delete", don't merge it, just return
		//  an empty map.
		return map[string]interface{}{}, nil
	}

	return nil, mergepatch.ErrBadPatchType(directive, patch)
}

func containsDirectiveMarker(item interface{}) bool {
	m, ok := item.(map[string]interface{})
	if ok {
		if _, foundDirectiveMarker := m[directiveMarker]; foundDirectiveMarker {
			return true
		}
	}
	return false
}

func mergeKeyValueEqual(left, right interface{}, mergeKey string) (bool, error) {
	if len(mergeKey) == 0 {
		return left == right, nil
	}
	typedLeft, ok := left.(map[string]interface{})
	if !ok {
		return false, mergepatch.ErrBadArgType(typedLeft, left)
	}
	typedRight, ok := right.(map[string]interface{})
	if !ok {
		return false, mergepatch.ErrBadArgType(typedRight, right)
	}
	mergeKeyLeft, ok := typedLeft[mergeKey]
	if !ok {
		return false, mergepatch.ErrNoMergeKey(typedLeft, mergeKey)
	}
	mergeKeyRight, ok := typedRight[mergeKey]
	if !ok {
		return false, mergepatch.ErrNoMergeKey(typedRight, mergeKey)
	}
	return mergeKeyLeft == mergeKeyRight, nil
}

// extractKey trims the prefix and return the original key
func extractKey(s, prefix string) (string, error) {
	substrings := strings.SplitN(s, "/", 2)
	if len(substrings) <= 1 || substrings[0] != prefix {
		switch prefix {
		case deleteFromPrimitiveListDirectivePrefix:
			return "", mergepatch.ErrBadPatchFormatForPrimitiveList
		case setElementOrderDirectivePrefix:
			return "", mergepatch.ErrBadPatchFormatForSetElementOrderList
		default:
			return "", fmt.Errorf("fail to find unknown prefix %q in %s\n", prefix, s)
		}
	}
	return substrings[1], nil
}

// validatePatchUsingSetOrderList verifies:
// the relative order of any two items in the setOrderList list matches that in the patch list.
// the items in the patch list must be a subset or the same as the $setElementOrder list (deletions are ignored).
func validatePatchWithSetOrderList(patchList, setOrderList interface{}, mergeKey string) error {
	typedSetOrderList, ok := setOrderList.([]interface{})
	if !ok {
		return mergepatch.ErrBadPatchFormatForSetElementOrderList
	}
	typedPatchList, ok := patchList.([]interface{})
	if !ok {
		return mergepatch.ErrBadPatchFormatForSetElementOrderList
	}
	if len(typedSetOrderList) == 0 || len(typedPatchList) == 0 {
		return nil
	}

	var nonDeleteList []interface{}
	var err error
	if len(mergeKey) > 0 {
		nonDeleteList, _, err = extractToDeleteItems(typedPatchList)
		if err != nil {
			return err
		}
	} else {
		nonDeleteList = typedPatchList
	}

	patchIndex, setOrderIndex := 0, 0
	for patchIndex < len(nonDeleteList) && setOrderIndex < len(typedSetOrderList) {
		if containsDirectiveMarker(nonDeleteList[patchIndex]) {
			patchIndex++
			continue
		}
		mergeKeyEqual, err := mergeKeyValueEqual(nonDeleteList[patchIndex], typedSetOrderList[setOrderIndex], mergeKey)
		if err != nil {
			return err
		}
		if mergeKeyEqual {
			patchIndex++
		}
		setOrderIndex++
	}
	// If patchIndex is inbound but setOrderIndex if out of bound mean there are items mismatching between the patch list and setElementOrder list.
	// the second check is a sanity check, and should always be true if the first is true.
	if patchIndex < len(nonDeleteList) && setOrderIndex >= len(typedSetOrderList) {
		return fmt.Errorf("The order in patch list:\n%v\n doesn't match %s list:\n%v\n", typedPatchList, setElementOrderDirectivePrefix, setOrderList)
	}
	return nil
}

// preprocessDeletionListForMerging preprocesses the deletion list.
// it returns shouldContinue, isDeletionList, noPrefixKey
func preprocessDeletionListForMerging(key string, original map[string]interface{},
	patchVal interface{}, mergeDeletionList bool) (bool, bool, string, error) {
	// If found a parallel list for deletion and we are going to merge the list,
	// overwrite the key to the original key and set flag isDeleteList
	foundParallelListPrefix := strings.HasPrefix(key, deleteFromPrimitiveListDirectivePrefix)
	if foundParallelListPrefix {
		if !mergeDeletionList {
			original[key] = patchVal
			return true, false, "", nil
		}
		originalKey, err := extractKey(key, deleteFromPrimitiveListDirectivePrefix)
		return false, true, originalKey, err
	}
	return false, false, "", nil
}

// applyRetainKeysDirective looks for a retainKeys directive and applies to original
// - if no directive exists do nothing
// - if directive is found, clear keys in original missing from the directive list
// - validate that all keys present in the patch are present in the retainKeys directive
// note: original may be another patch request, e.g. applying the add+modified patch to the deletions patch. In this case it may have directives
func applyRetainKeysDirective(original, patch map[string]interface{}, options MergeOptions) error {
	retainKeysInPatch, foundInPatch := patch[retainKeysDirective]
	if !foundInPatch {
		return nil
	}
	// cleanup the directive
	delete(patch, retainKeysDirective)

	if !options.MergeParallelList {
		// If original is actually a patch, make sure the retainKeys directives are the same in both patches if present in both.
		// If not present in the original patch, copy from the modified patch.
		retainKeysInOriginal, foundInOriginal := original[retainKeysDirective]
		if foundInOriginal {
			if !reflect.DeepEqual(retainKeysInOriginal, retainKeysInPatch) {
				// This error actually should never happen.
				return fmt.Errorf("%v and %v are not deep equal: this may happen when calculating the 3-way diff patch", retainKeysInOriginal, retainKeysInPatch)
			}
		} else {
			original[retainKeysDirective] = retainKeysInPatch
		}
		return nil
	}

	retainKeysList, ok := retainKeysInPatch.([]interface{})
	if !ok {
		return mergepatch.ErrBadPatchFormatForRetainKeys
	}

	// validate patch to make sure all fields in the patch are present in the retainKeysList.
	// The map is used only as a set, the value is never referenced
	m := map[interface{}]struct{}{}
	for _, v := range retainKeysList {
		m[v] = struct{}{}
	}
	for k, v := range patch {
		if v == nil || strings.HasPrefix(k, deleteFromPrimitiveListDirectivePrefix) ||
			strings.HasPrefix(k, setElementOrderDirectivePrefix) {
			continue
		}
		// If there is an item present in the patch but not in the retainKeys list,
		// the patch is invalid.
		if _, found := m[k]; !found {
			return mergepatch.ErrBadPatchFormatForRetainKeys
		}
	}

	// clear not present fields
	for k := range original {
		if _, found := m[k]; !found {
			delete(original, k)
		}
	}
	return nil
}

// mergePatchIntoOriginal processes $setElementOrder list.
// When not merging the directive, it will make sure $setElementOrder list exist only in original.
// When merging the directive, it will try to find the $setElementOrder list and
// its corresponding patch list, validate it and merge it.
// Then, sort them by the relative order in setElementOrder, patch list and live list.
// The precedence is $setElementOrder > order in patch list > order in live list.
// This function will delete the item after merging it to prevent process it again in the future.
// Ref: https://git.k8s.io/design-proposals-archive/cli/preserve-order-in-strategic-merge-patch.md
func mergePatchIntoOriginal(original, patch map[string]interface{}, schema LookupPatchMeta, mergeOptions MergeOptions) error {
	for key, patchV := range patch {
		// Do nothing if there is no ordering directive
		if !strings.HasPrefix(key, setElementOrderDirectivePrefix) {
			continue
		}

		setElementOrderInPatch := patchV
		// Copies directive from the second patch (`patch`) to the first patch (`original`)
		// and checks they are equal and delete the directive in the second patch
		if !mergeOptions.MergeParallelList {
			setElementOrderListInOriginal, ok := original[key]
			if ok {
				// check if the setElementOrder list in original and the one in patch matches
				if !reflect.DeepEqual(setElementOrderListInOriginal, setElementOrderInPatch) {
					return mergepatch.ErrBadPatchFormatForSetElementOrderList
				}
			} else {
				// move the setElementOrder list from patch to original
				original[key] = setElementOrderInPatch
			}
		}
		delete(patch, key)

		var (
			ok                                          bool
			originalFieldValue, patchFieldValue, merged []interface{}
			patchStrategy                               string
			patchMeta                                   PatchMeta
			subschema                                   LookupPatchMeta
		)
		typedSetElementOrderList, ok := setElementOrderInPatch.([]interface{})
		if !ok {
			return mergepatch.ErrBadArgType(typedSetElementOrderList, setElementOrderInPatch)
		}
		// Trim the setElementOrderDirectivePrefix to get the key of the list field in original.
		originalKey, err := extractKey(key, setElementOrderDirectivePrefix)
		if err != nil {
			return err
		}
		// try to find the list with `originalKey` in `original` and `modified` and merge them.
		originalList, foundOriginal := original[originalKey]
		patchList, foundPatch := patch[originalKey]
		if foundOriginal {
			originalFieldValue, ok = originalList.([]interface{})
			if !ok {
				return mergepatch.ErrBadArgType(originalFieldValue, originalList)
			}
		}
		if foundPatch {
			patchFieldValue, ok = patchList.([]interface{})
			if !ok {
				return mergepatch.ErrBadArgType(patchFieldValue, patchList)
			}
		}
		subschema, patchMeta, err = schema.LookupPatchMetadataForSlice(originalKey)
		if err != nil {
			return err
		}
		_, patchStrategy, err = extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
		if err != nil {
			return err
		}
		// Check for consistency between the element order list and the field it applies to
		err = validatePatchWithSetOrderList(patchFieldValue, typedSetElementOrderList, patchMeta.GetPatchMergeKey())
		if err != nil {
			return err
		}

		switch {
		case foundOriginal && !foundPatch:
			// no change to list contents
			merged = originalFieldValue
		case !foundOriginal && foundPatch:
			// list was added
			v, keep := removeDirectives(patchFieldValue)
			if !keep {
				// Shouldn't be possible since patchFieldValue is a slice
				continue
			}

			merged = v.([]interface{})
		case foundOriginal && foundPatch:
			merged, err = mergeSliceHandler(originalList, patchList, subschema,
				patchStrategy, patchMeta.GetPatchMergeKey(), false, mergeOptions)
			if err != nil {
				return err
			}
		case !foundOriginal && !foundPatch:
			continue
		}

		// Split all items into patch items and server-only items and then enforce the order.
		var patchItems, serverOnlyItems []interface{}
		if len(patchMeta.GetPatchMergeKey()) == 0 {
			// Primitives doesn't need merge key to do partitioning.
			patchItems, serverOnlyItems = partitionPrimitivesByPresentInList(merged, typedSetElementOrderList)

		} else {
			// Maps need merge key to do partitioning.
			patchItems, serverOnlyItems, err = partitionMapsByPresentInList(merged, typedSetElementOrderList, patchMeta.GetPatchMergeKey())
			if err != nil {
				return err
			}
		}

		elementType, err := sliceElementType(originalFieldValue, patchFieldValue)
		if err != nil {
			return err
		}
		kind := elementType.Kind()
		// normalize merged list
		// typedSetElementOrderList contains all the relative order in typedPatchList,
		// so don't need to use typedPatchList
		both, err := normalizeElementOrder(patchItems, serverOnlyItems, typedSetElementOrderList, originalFieldValue, patchMeta.GetPatchMergeKey(), kind)
		if err != nil {
			return err
		}
		original[originalKey] = both
		// delete patch list from patch to prevent process again in the future
		delete(patch, originalKey)
	}
	return nil
}

// partitionPrimitivesByPresentInList partitions elements into 2 slices, the first containing items present in partitionBy, the other not.
func partitionPrimitivesByPresentInList(original, partitionBy []interface{}) ([]interface{}, []interface{}) {
	patch := make([]interface{}, 0, len(original))
	serverOnly := make([]interface{}, 0, len(original))
	inPatch := map[interface{}]bool{}
	for _, v := range partitionBy {
		inPatch[v] = true
	}
	for _, v := range original {
		if !inPatch[v] {
			serverOnly = append(serverOnly, v)
		} else {
			patch = append(patch, v)
		}
	}
	return patch, serverOnly
}

// partitionMapsByPresentInList partitions elements into 2 slices, the first containing items present in partitionBy, the other not.
func partitionMapsByPresentInList(original, partitionBy []interface{}, mergeKey string) ([]interface{}, []interface{}, error) {
	patch := make([]interface{}, 0, len(original))
	serverOnly := make([]interface{}, 0, len(original))
	for _, v := range original {
		typedV, ok := v.(map[string]interface{})
		if !ok {
			return nil, nil, mergepatch.ErrBadArgType(typedV, v)
		}
		mergeKeyValue, foundMergeKey := typedV[mergeKey]
		if !foundMergeKey {
			return nil, nil, mergepatch.ErrNoMergeKey(typedV, mergeKey)
		}
		_, _, found, err := findMapInSliceBasedOnKeyValue(partitionBy, mergeKey, mergeKeyValue)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			serverOnly = append(serverOnly, v)
		} else {
			patch = append(patch, v)
		}
	}
	return patch, serverOnly, nil
}

// Removes directives from an object and returns value to use instead and whether
// or not the field/index should even be kept
// May modify input
func removeDirectives(obj interface{}) (interface{}, bool) {
	if obj == nil {
		return obj, true
	} else if typedV, ok := obj.(map[string]interface{}); ok {
		if _, hasDirective := typedV[directiveMarker]; hasDirective {
			return nil, false
		}

		for k, v := range typedV {
			var keep bool
			typedV[k], keep = removeDirectives(v)
			if !keep {
				delete(typedV, k)
			}
		}
		return typedV, true
	} else if typedV, ok := obj.([]interface{}); ok {
		var res []interface{}
		if typedV != nil {
			// Make sure res is non-nil if patch is non-nil
			res = []interface{}{}
		}
		for _, v := range typedV {
			if newV, keep := removeDirectives(v); keep {
				res = append(res, newV)
			}
		}
		return res, true
	} else {
		return obj, true
	}
}

// Merge fields from a patch map into the original map. Note: This may modify
// both the original map and the patch because getting a deep copy of a map in
// golang is highly non-trivial.
// flag mergeOptions.MergeParallelList controls if using the parallel list to delete or keeping the list.
// If patch contains any null field (e.g. field_1: null) that is not
// present in original, then to propagate it to the end result use
// mergeOptions.IgnoreUnmatchedNulls == false.
func mergeMap(original, patch map[string]interface{}, schema LookupPatchMeta, mergeOptions MergeOptions) (map[string]interface{}, error) {
	if v, ok := patch[directiveMarker]; ok {
		return handleDirectiveInMergeMap(v, patch)
	}

	// nil is an accepted value for original to simplify logic in other places.
	// If original is nil, replace it with an empty map and then apply the patch.
	if original == nil {
		original = map[string]interface{}{}
	}

	err := applyRetainKeysDirective(original, patch, mergeOptions)
	if err != nil {
		return nil, err
	}

	// Process $setElementOrder list and other lists sharing the same key.
	// When not merging the directive, it will make sure $setElementOrder list exist only in original.
	// When merging the directive, it will process $setElementOrder and its patch list together.
	// This function will delete the merged elements from patch so they will not be reprocessed
	err = mergePatchIntoOriginal(original, patch, schema, mergeOptions)
	if err != nil {
		return nil, err
	}

	// Start merging the patch into the original.
	for k, patchV := range patch {
		skipProcessing, isDeleteList, noPrefixKey, err := preprocessDeletionListForMerging(k, original, patchV, mergeOptions.MergeParallelList)
		if err != nil {
			return nil, err
		}
		if skipProcessing {
			continue
		}
		if len(noPrefixKey) > 0 {
			k = noPrefixKey
		}

		// If the value of this key is null, delete the key if it exists in the
		// original. Otherwise, check if we want to preserve it or skip it.
		// Preserving the null value is useful when we want to send an explicit
		// delete to the API server.
		// In some cases, this may lead to inconsistent behavior with create.
		// ref: https://github.com/kubernetes/kubernetes/issues/123304
		// To avoid breaking compatibility,
		// we made corresponding changes on the client side to ensure that the create and patch behaviors are idempotent.
		if patchV == nil {
			delete(original, k)
			if mergeOptions.IgnoreUnmatchedNulls {
				continue
			}
		}

		_, ok := original[k]
		if !ok {
			if !isDeleteList {
				// If it's not in the original document, just take the patch value.
				if mergeOptions.IgnoreUnmatchedNulls {
					discardNullValuesFromPatch(patchV)
				}
				original[k], ok = removeDirectives(patchV)
				if !ok {
					delete(original, k)
				}
			}
			continue
		}

		originalType := reflect.TypeOf(original[k])
		patchType := reflect.TypeOf(patchV)
		if originalType != patchType {
			if !isDeleteList {
				if mergeOptions.IgnoreUnmatchedNulls {
					discardNullValuesFromPatch(patchV)
				}
				original[k], ok = removeDirectives(patchV)
				if !ok {
					delete(original, k)
				}
			}
			continue
		}
		// If they're both maps or lists, recurse into the value.
		switch originalType.Kind() {
		case reflect.Map:
			subschema, patchMeta, err2 := schema.LookupPatchMetadataForStruct(k)
			if err2 != nil {
				return nil, err2
			}
			_, patchStrategy, err2 := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
			if err2 != nil {
				return nil, err2
			}
			original[k], err = mergeMapHandler(original[k], patchV, subschema, patchStrategy, mergeOptions)
		case reflect.Slice:
			subschema, patchMeta, err2 := schema.LookupPatchMetadataForSlice(k)
			if err2 != nil {
				return nil, err2
			}
			_, patchStrategy, err2 := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
			if err2 != nil {
				return nil, err2
			}
			original[k], err = mergeSliceHandler(original[k], patchV, subschema, patchStrategy, patchMeta.GetPatchMergeKey(), isDeleteList, mergeOptions)
		default:
			original[k], ok = removeDirectives(patchV)
			if !ok {
				// if patchV itself is a directive, then don't keep it
				delete(original, k)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return original, nil
}

// discardNullValuesFromPatch discards all null property values from patch.
// It traverses all slices and map types.
func discardNullValuesFromPatch(patchV interface{}) {
	switch patchV := patchV.(type) {
	case map[string]interface{}:
		for k, v := range patchV {
			if v == nil {
				delete(patchV, k)
			} else {
				discardNullValuesFromPatch(v)
			}
		}
	case []interface{}:
		for _, v := range patchV {
			discardNullValuesFromPatch(v)
		}
	}
}

// mergeMapHandler handles how to merge `patchV` whose key is `key` with `original` respecting
// fieldPatchStrategy and mergeOptions.
func mergeMapHandler(original, patch interface{}, schema LookupPatchMeta,
	fieldPatchStrategy string, mergeOptions MergeOptions) (map[string]interface{}, error) {
	typedOriginal, typedPatch, err := mapTypeAssertion(original, patch)
	if err != nil {
		return nil, err
	}

	if fieldPatchStrategy != replaceDirective {
		return mergeMap(typedOriginal, typedPatch, schema, mergeOptions)
	} else {
		return typedPatch, nil
	}
}

// mergeSliceHandler handles how to merge `patchV` whose key is `key` with `original` respecting
// fieldPatchStrategy, fieldPatchMergeKey, isDeleteList and mergeOptions.
func mergeSliceHandler(original, patch interface{}, schema LookupPatchMeta,
	fieldPatchStrategy, fieldPatchMergeKey string, isDeleteList bool, mergeOptions MergeOptions) ([]interface{}, error) {
	typedOriginal, typedPatch, err := sliceTypeAssertion(original, patch)
	if err != nil {
		return nil, err
	}

	// Delete lists are handled the same way regardless of what the field's patch strategy is
	if fieldPatchStrategy == mergeDirective || isDeleteList {
		return mergeSlice(typedOriginal, typedPatch, schema, fieldPatchMergeKey, mergeOptions, isDeleteList)
	} else {
		return typedPatch, nil
	}
}

// Merge two slices together. Note: This may modify both the original slice and
// the patch because getting a deep copy of a slice in golang is highly
// non-trivial.
func mergeSlice(original, patch []interface{}, schema LookupPatchMeta, mergeKey string, mergeOptions MergeOptions, isDeleteList bool) ([]interface{}, error) {
	if len(original) == 0 && len(patch) == 0 {
		return original, nil
	}

	// All the values must be of the same type, but not a list.
	t, err := sliceElementType(original, patch)
	if err != nil {
		return nil, err
	}

	var merged []interface{}
	kind := t.Kind()
	// If the elements are not maps, merge the slices of scalars.
	if kind != reflect.Map {
		if mergeOptions.MergeParallelList && isDeleteList {
			return deleteFromSlice(original, patch), nil
		}
		// Maybe in the future add a "concat" mode that doesn't
		// deduplicate.
		both := append(original, patch...)
		merged = deduplicateScalars(both)

	} else {
		if mergeKey == "" {
			return nil, fmt.Errorf("cannot merge lists without merge key for %s", schema.Name())
		}

		original, patch, err = mergeSliceWithSpecialElements(original, patch, mergeKey)
		if err != nil {
			return nil, err
		}

		merged, err = mergeSliceWithoutSpecialElements(original, patch, mergeKey, schema, mergeOptions)
		if err != nil {
			return nil, err
		}
	}

	// enforce the order
	var patchItems, serverOnlyItems []interface{}
	if len(mergeKey) == 0 {
		patchItems, serverOnlyItems = partitionPrimitivesByPresentInList(merged, patch)
	} else {
		patchItems, serverOnlyItems, err = partitionMapsByPresentInList(merged, patch, mergeKey)
		if err != nil {
			return nil, err
		}
	}
	return normalizeElementOrder(patchItems, serverOnlyItems, patch, original, mergeKey, kind)
}

// mergeSliceWithSpecialElements handles special elements with directiveMarker
// before merging the slices. It returns a updated `original` and a patch without special elements.
// original and patch must be slices of maps, they should be checked before calling this function.
func mergeSliceWithSpecialElements(original, patch []interface{}, mergeKey string) ([]interface{}, []interface{}, error) {
	patchWithoutSpecialElements := []interface{}{}
	replace := false
	for _, v := range patch {
		typedV := v.(map[string]interface{})
		patchType, ok := typedV[directiveMarker]
		if !ok {
			patchWithoutSpecialElements = append(patchWithoutSpecialElements, v)
		} else {
			switch patchType {
			case deleteDirective:
				mergeValue, ok := typedV[mergeKey]
				if ok {
					var err error
					// WARNING: side effect here. using 'original' in deleteMatchingEntries() if we have deletions
					// breaks content of underlying array like '1, 2, 3, 4' -> delete 3 -> '1, 2, 4, 4',
					// which causes incorrect ordering in 'mergeSlice(...)', because it uses its copy of
					// 'original' slice (on the same array, but with original length 4)
					// as source of truth about elements order.
					// This is the reason of broken ordering in test case
					// "behavior of set element order for a merging int list with duplicate"
					original, err = deleteMatchingEntries(original, mergeKey, mergeValue)
					if err != nil {
						return nil, nil, err
					}
				} else {
					return nil, nil, mergepatch.ErrNoMergeKey(typedV, mergeKey)
				}
			case replaceDirective:
				replace = true
				// Continue iterating through the array to prune any other $patch elements.
			case mergeDirective:
				return nil, nil, fmt.Errorf("merging lists cannot yet be specified in the patch")
			default:
				return nil, nil, mergepatch.ErrBadPatchType(patchType, typedV)
			}
		}
	}
	if replace {
		return patchWithoutSpecialElements, nil, nil
	}
	return original, patchWithoutSpecialElements, nil
}

// delete all matching entries (based on merge key) from a merging list
func deleteMatchingEntries(original []interface{}, mergeKey string, mergeValue interface{}) ([]interface{}, error) {
	for {
		_, originalKey, found, err := findMapInSliceBasedOnKeyValue(original, mergeKey, mergeValue)
		if err != nil {
			return nil, err
		}

		if !found {
			break
		}
		// Delete the element at originalKey.
		original = append(original[:originalKey], original[originalKey+1:]...)
	}
	return original, nil
}

// mergeSliceWithoutSpecialElements merges slices with non-special elements.
// original and patch must be slices of maps, they should be checked before calling this function.
func mergeSliceWithoutSpecialElements(original, patch []interface{}, mergeKey string, schema LookupPatchMeta, mergeOptions MergeOptions) ([]interface{}, error) {
	for _, v := range patch {
		typedV := v.(map[string]interface{})
		mergeValue, ok := typedV[mergeKey]
		if !ok {
			return nil, mergepatch.ErrNoMergeKey(typedV, mergeKey)
		}

		// If we find a value with this merge key value in original, merge the
		// maps. Otherwise append onto original.
		originalMap, originalKey, found, err := findMapInSliceBasedOnKeyValue(original, mergeKey, mergeValue)
		if err != nil {
			return nil, err
		}

		if found {
			var mergedMaps interface{}
			var err error
			// Merge into original.
			mergedMaps, err = mergeMap(originalMap, typedV, schema, mergeOptions)
			if err != nil {
				return nil, err
			}

			original[originalKey] = mergedMaps
		} else {
			original = append(original, v)
		}
	}
	return original, nil
}

// deleteFromSlice uses the parallel list to delete the items in a list of scalars
func deleteFromSlice(current, toDelete []interface{}) []interface{} {
	toDeleteMap := map[interface{}]interface{}{}
	processed := make([]interface{}, 0, len(current))
	for _, v := range toDelete {
		toDeleteMap[v] = true
	}
	for _, v := range current {
		if _, found := toDeleteMap[v]; !found {
			processed = append(processed, v)
		}
	}
	return processed
}

// This method no longer panics if any element of the slice is not a map.
func findMapInSliceBasedOnKeyValue(m []interface{}, key string, value interface{}) (map[string]interface{}, int, bool, error) {
	for k, v := range m {
		typedV, ok := v.(map[string]interface{})
		if !ok {
			return nil, 0, false, fmt.Errorf("value for key %v is not a map", k)
		}

		valueToMatch, ok := typedV[key]
		if ok && valueToMatch == value {
			return typedV, k, true, nil
		}
	}

	return nil, 0, false, nil
}

// This function takes a JSON map and sorts all the lists that should be merged
// by key. This is needed by tests because in JSON, list order is significant,
// but in Strategic Merge Patch, merge lists do not have significant order.
// Sorting the lists allows for order-insensitive comparison of patched maps.
func sortMergeListsByName(mapJSON []byte, schema LookupPatchMeta) ([]byte, error) {
	var m map[string]interface{}
	err := json.Unmarshal(mapJSON, &m)
	if err != nil {
		return nil, mergepatch.ErrBadJSONDoc
	}

	newM, err := sortMergeListsByNameMap(m, schema)
	if err != nil {
		return nil, err
	}

	return json.Marshal(newM)
}

// Function sortMergeListsByNameMap recursively sorts the merge lists by its mergeKey in a map.
func sortMergeListsByNameMap(s map[string]interface{}, schema LookupPatchMeta) (map[string]interface{}, error) {
	newS := map[string]interface{}{}
	for k, v := range s {
		if k == retainKeysDirective {
			typedV, ok := v.([]interface{})
			if !ok {
				return nil, mergepatch.ErrBadPatchFormatForRetainKeys
			}
			v = sortScalars(typedV)
		} else if strings.HasPrefix(k, deleteFromPrimitiveListDirectivePrefix) {
			typedV, ok := v.([]interface{})
			if !ok {
				return nil, mergepatch.ErrBadPatchFormatForPrimitiveList
			}
			v = sortScalars(typedV)
		} else if strings.HasPrefix(k, setElementOrderDirectivePrefix) {
			_, ok := v.([]interface{})
			if !ok {
				return nil, mergepatch.ErrBadPatchFormatForSetElementOrderList
			}
		} else if k != directiveMarker {
			// recurse for map and slice.
			switch typedV := v.(type) {
			case map[string]interface{}:
				subschema, _, err := schema.LookupPatchMetadataForStruct(k)
				if err != nil {
					return nil, err
				}
				v, err = sortMergeListsByNameMap(typedV, subschema)
				if err != nil {
					return nil, err
				}
			case []interface{}:
				subschema, patchMeta, err := schema.LookupPatchMetadataForSlice(k)
				if err != nil {
					return nil, err
				}
				_, patchStrategy, err := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
				if err != nil {
					return nil, err
				}
				if patchStrategy == mergeDirective {
					var err error
					v, err = sortMergeListsByNameArray(typedV, subschema, patchMeta.GetPatchMergeKey(), true)
					if err != nil {
						return nil, err
					}
				}
			}
		}

		newS[k] = v
	}

	return newS, nil
}

// Function sortMergeListsByNameMap recursively sorts the merge lists by its mergeKey in an array.
func sortMergeListsByNameArray(s []interface{}, schema LookupPatchMeta, mergeKey string, recurse bool) ([]interface{}, error) {
	if len(s) == 0 {
		return s, nil
	}

	// We don't support lists of lists yet.
	t, err := sliceElementType(s)
	if err != nil {
		return nil, err
	}

	// If the elements are not maps...
	if t.Kind() != reflect.Map {
		// Sort the elements, because they may have been merged out of order.
		return deduplicateAndSortScalars(s), nil
	}

	// Elements are maps - if one of the keys of the map is a map or a
	// list, we may need to recurse into it.
	newS := []interface{}{}
	for _, elem := range s {
		if recurse {
			typedElem := elem.(map[string]interface{})
			newElem, err := sortMergeListsByNameMap(typedElem, schema)
			if err != nil {
				return nil, err
			}

			newS = append(newS, newElem)
		} else {
			newS = append(newS, elem)
		}
	}

	// Sort the maps.
	newS = sortMapsBasedOnField(newS, mergeKey)
	return newS, nil
}

func sortMapsBasedOnField(m []interface{}, fieldName string) []interface{} {
	mapM := mapSliceFromSlice(m)
	ss := SortableSliceOfMaps{mapM, fieldName}
	sort.Sort(ss)
	newS := sliceFromMapSlice(ss.s)
	return newS
}

func mapSliceFromSlice(m []interface{}) []map[string]interface{} {
	newM := []map[string]interface{}{}
	for _, v := range m {
		vt := v.(map[string]interface{})
		newM = append(newM, vt)
	}

	return newM
}

func sliceFromMapSlice(s []map[string]interface{}) []interface{} {
	newS := []interface{}{}
	for _, v := range s {
		newS = append(newS, v)
	}

	return newS
}

type SortableSliceOfMaps struct {
	s []map[string]interface{}
	k string // key to sort on
}

func (ss SortableSliceOfMaps) Len() int {
	return len(ss.s)
}

func (ss SortableSliceOfMaps) Less(i, j int) bool {
	iStr := fmt.Sprintf("%v", ss.s[i][ss.k])
	jStr := fmt.Sprintf("%v", ss.s[j][ss.k])
	return sort.StringsAreSorted([]string{iStr, jStr})
}

func (ss SortableSliceOfMaps) Swap(i, j int) {
	tmp := ss.s[i]
	ss.s[i] = ss.s[j]
	ss.s[j] = tmp
}

func deduplicateAndSortScalars(s []interface{}) []interface{} {
	s = deduplicateScalars(s)
	return sortScalars(s)
}

func sortScalars(s []interface{}) []interface{} {
	ss := SortableSliceOfScalars{s}
	sort.Sort(ss)
	return ss.s
}

func deduplicateScalars(s []interface{}) []interface{} {
	// Clever algorithm to deduplicate.
	length := len(s) - 1
	for i := 0; i < length; i++ {
		for j := i + 1; j <= length; j++ {
			if s[i] == s[j] {
				s[j] = s[length]
				s = s[0:length]
				length--
				j--
			}
		}
	}

	return s
}

type SortableSliceOfScalars struct {
	s []interface{}
}

func (ss SortableSliceOfScalars) Len() int {
	return len(ss.s)
}

func (ss SortableSliceOfScalars) Less(i, j int) bool {
	iStr := fmt.Sprintf("%v", ss.s[i])
	jStr := fmt.Sprintf("%v", ss.s[j])
	return sort.StringsAreSorted([]string{iStr, jStr})
}

func (ss SortableSliceOfScalars) Swap(i, j int) {
	tmp := ss.s[i]
	ss.s[i] = ss.s[j]
	ss.s[j] = tmp
}

// Returns the type of the elements of N slice(s). If the type is different,
// another slice or undefined, returns an error.
func sliceElementType(slices ...[]interface{}) (reflect.Type, error) {
	var prevType reflect.Type
	for _, s := range slices {
		// Go through elements of all given slices and make sure they are all the same type.
		for _, v := range s {
			currentType := reflect.TypeOf(v)
			if prevType == nil {
				prevType = currentType
				// We don't support lists of lists yet.
				if prevType.Kind() == reflect.Slice {
					return nil, mergepatch.ErrNoListOfLists
				}
			} else {
				if prevType != currentType {
					return nil, fmt.Errorf("list element types are not identical: %v", fmt.Sprint(slices))
				}
				prevType = currentType
			}
		}
	}

	if prevType == nil {
		return nil, fmt.Errorf("no elements in any of the given slices")
	}

	return prevType, nil
}

// MergingMapsHaveConflicts returns true if the left and right JSON interface
// objects overlap with different values in any key. All keys are required to be
// strings. Since patches of the same Type have congruent keys, this is valid
// for multiple patch types. This method supports strategic merge patch semantics.
func MergingMapsHaveConflicts(left, right map[string]interface{}, schema LookupPatchMeta) (bool, error) {
	return mergingMapFieldsHaveConflicts(left, right, schema, "", "")
}

func mergingMapFieldsHaveConflicts(
	left, right interface{},
	schema LookupPatchMeta,
	fieldPatchStrategy, fieldPatchMergeKey string,
) (bool, error) {
	switch leftType := left.(type) {
	case map[string]interface{}:
		rightType, ok := right.(map[string]interface{})
		if !ok {
			return true, nil
		}
		leftMarker, okLeft := leftType[directiveMarker]
		rightMarker, okRight := rightType[directiveMarker]
		// if one or the other has a directive marker,
		// then we need to consider that before looking at the individual keys,
		// since a directive operates on the whole map.
		if okLeft || okRight {
			// if one has a directive marker and the other doesn't,
			// then we have a conflict, since one is deleting or replacing the whole map,
			// and the other is doing things to individual keys.
			if okLeft != okRight {
				return true, nil
			}
			// if they both have markers, but they are not the same directive,
			// then we have a conflict because they're doing different things to the map.
			if leftMarker != rightMarker {
				return true, nil
			}
		}
		if fieldPatchStrategy == replaceDirective {
			return false, nil
		}
		// Check the individual keys.
		return mapsHaveConflicts(leftType, rightType, schema)

	case []interface{}:
		rightType, ok := right.([]interface{})
		if !ok {
			return true, nil
		}
		return slicesHaveConflicts(leftType, rightType, schema, fieldPatchStrategy, fieldPatchMergeKey)
	case string, float64, bool, int64, nil:
		return !reflect.DeepEqual(left, right), nil
	default:
		return true, fmt.Errorf("unknown type: %v", reflect.TypeOf(left))
	}
}

func mapsHaveConflicts(typedLeft, typedRight map[string]interface{}, schema LookupPatchMeta) (bool, error) {
	for key, leftValue := range typedLeft {
		if key != directiveMarker && key != retainKeysDirective {
			if rightValue, ok := typedRight[key]; ok {
				var subschema LookupPatchMeta
				var patchMeta PatchMeta
				var patchStrategy string
				var err error
				switch leftValue.(type) {
				case []interface{}:
					subschema, patchMeta, err = schema.LookupPatchMetadataForSlice(key)
					if err != nil {
						return true, err
					}
					_, patchStrategy, err = extractRetainKeysPatchStrategy(patchMeta.patchStrategies)
					if err != nil {
						return true, err
					}
				case map[string]interface{}:
					subschema, patchMeta, err = schema.LookupPatchMetadataForStruct(key)
					if err != nil {
						return true, err
					}
					_, patchStrategy, err = extractRetainKeysPatchStrategy(patchMeta.patchStrategies)
					if err != nil {
						return true, err
					}
				}

				if hasConflicts, err := mergingMapFieldsHaveConflicts(leftValue, rightValue,
					subschema, patchStrategy, patchMeta.GetPatchMergeKey()); hasConflicts {
					return true, err
				}
			}
		}
	}

	return false, nil
}

func slicesHaveConflicts(
	typedLeft, typedRight []interface{},
	schema LookupPatchMeta,
	fieldPatchStrategy, fieldPatchMergeKey string,
) (bool, error) {
	elementType, err := sliceElementType(typedLeft, typedRight)
	if err != nil {
		return true, err
	}

	if fieldPatchStrategy == mergeDirective {
		// Merging lists of scalars have no conflicts by definition
		// So we only need to check further if the elements are maps
		if elementType.Kind() != reflect.Map {
			return false, nil
		}

		// Build a map for each slice and then compare the two maps
		leftMap, err := sliceOfMapsToMapOfMaps(typedLeft, fieldPatchMergeKey)
		if err != nil {
			return true, err
		}

		rightMap, err := sliceOfMapsToMapOfMaps(typedRight, fieldPatchMergeKey)
		if err != nil {
			return true, err
		}

		return mapsOfMapsHaveConflicts(leftMap, rightMap, schema)
	}

	// Either we don't have type information, or these are non-merging lists
	if len(typedLeft) != len(typedRight) {
		return true, nil
	}

	// Sort scalar slices to prevent ordering issues
	// We have no way to sort non-merging lists of maps
	if elementType.Kind() != reflect.Map {
		typedLeft = deduplicateAndSortScalars(typedLeft)
		typedRight = deduplicateAndSortScalars(typedRight)
	}

	// Compare the slices element by element in order
	// This test will fail if the slices are not sorted
	for i := range typedLeft {
		if hasConflicts, err := mergingMapFieldsHaveConflicts(typedLeft[i], typedRight[i], schema, "", ""); hasConflicts {
			return true, err
		}
	}

	return false, nil
}

func sliceOfMapsToMapOfMaps(slice []interface{}, mergeKey string) (map[string]interface{}, error) {
	result := make(map[string]interface{}, len(slice))
	for _, value := range slice {
		typedValue, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid element type in merging list:%v", slice)
		}

		mergeValue, ok := typedValue[mergeKey]
		if !ok {
			return nil, fmt.Errorf("cannot find merge key `%s` in merging list element:%v", mergeKey, typedValue)
		}

		result[fmt.Sprintf("%s", mergeValue)] = typedValue
	}

	return result, nil
}

func mapsOfMapsHaveConflicts(typedLeft, typedRight map[string]interface{}, schema LookupPatchMeta) (bool, error) {
	for key, leftValue := range typedLeft {
		if rightValue, ok := typedRight[key]; ok {
			if hasConflicts, err := mergingMapFieldsHaveConflicts(leftValue, rightValue, schema, "", ""); hasConflicts {
				return true, err
			}
		}
	}

	return false, nil
}

// CreateThreeWayMergePatch reconciles a modified configuration with an original configuration,
// while preserving any changes or deletions made to the original configuration in the interim,
// and not overridden by the current configuration. All three documents must be passed to the
// method as json encoded content. It will return a strategic merge patch, or an error if any
// of the documents is invalid, or if there are any preconditions that fail against the modified
// configuration, or, if overwrite is false and there are conflicts between the modified and current
// configurations. Conflicts are defined as keys changed differently from original to modified
// than from original to current. In other words, a conflict occurs if modified changes any key
// in a way that is different from how it is changed in current (e.g., deleting it, changing its
// value). We also propagate values fields that do not exist in original but are explicitly
// defined in modified.
func CreateThreeWayMergePatch(original, modified, current []byte, schema LookupPatchMeta, overwrite bool, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	originalMap := map[string]interface{}{}
	if len(original) > 0 {
		if err := json.Unmarshal(original, &originalMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	modifiedMap := map[string]interface{}{}
	if len(modified) > 0 {
		if err := json.Unmarshal(modified, &modifiedMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	currentMap := map[string]interface{}{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &currentMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	// The patch is the difference from current to modified without deletions, plus deletions
	// from original to modified. To find it, we compute deletions, which are the deletions from
	// original to modified, and delta, which is the difference from current to modified without
	// deletions, and then apply delta to deletions as a patch, which should be strictly additive.
	deltaMapDiffOptions := DiffOptions{
		IgnoreDeletions: true,
		SetElementOrder: true,
	}
	deltaMap, err := diffMaps(currentMap, modifiedMap, schema, deltaMapDiffOptions)
	if err != nil {
		return nil, err
	}
	deletionsMapDiffOptions := DiffOptions{
		SetElementOrder:           true,
		IgnoreChangesAndAdditions: true,
	}
	deletionsMap, err := diffMaps(originalMap, modifiedMap, schema, deletionsMapDiffOptions)
	if err != nil {
		return nil, err
	}

	mergeOptions := MergeOptions{}
	patchMap, err := mergeMap(deletionsMap, deltaMap, schema, mergeOptions)
	if err != nil {
		return nil, err
	}

	// Apply the preconditions to the patch, and return an error if any of them fail.
	for _, fn := range fns {
		if !fn(patchMap) {
			return nil, mergepatch.NewErrPreconditionFailed(patchMap)
		}
	}

	// If overwrite is false, and the patch contains any keys that were changed differently,
	// then return a conflict error.
	if !overwrite {
		changeMapDiffOptions := DiffOptions{}
		changedMap, err := diffMaps(originalMap, currentMap, schema, changeMapDiffOptions)
		if err != nil {
			return nil, err
		}

		hasConflicts, err := MergingMapsHaveConflicts(patchMap, changedMap, schema)
		if err != nil {
			return nil, err
		}

		if hasConflicts {
			return nil, mergepatch.NewErrConflict(mergepatch.ToYAMLOrError(patchMap), mergepatch.ToYAMLOrError(changedMap))
		}
	}

	return json.Marshal(patchMap)
}

func ItemAddedToModifiedSlice(original, modified string) bool { return original > modified }

func ItemRemovedFromModifiedSlice(original, modified string) bool { return original < modified }

func ItemMatchesOriginalAndModifiedSlice(original, modified string) bool { return original == modified }

func CreateDeleteDirective(mergeKey string, mergeKeyValue interface{}) map[string]interface{} {
	return map[string]interface{}{mergeKey: mergeKeyValue, directiveMarker: deleteDirective}
}

func mapTypeAssertion(original, patch interface{}) (map[string]interface{}, map[string]interface{}, error) {
	typedOriginal, ok := original.(map[string]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedOriginal, original)
	}
	typedPatch, ok := patch.(map[string]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedPatch, patch)
	}
	return typedOriginal, typedPatch, nil
}

func sliceTypeAssertion(original, patch interface{}) ([]interface{}, []interface{}, error) {
	typedOriginal, ok := original.([]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedOriginal, original)
	}
	typedPatch, ok := patch.([]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedPatch, patch)
	}
	return typedOriginal, typedPatch, nil
}

// extractRetainKeysPatchStrategy process patch strategy, which is a string may contains multiple
// patch strategies separated by ",". It returns a boolean var indicating if it has
// retainKeys strategies and a string for the other strategy.
func extractRetainKeysPatchStrategy(strategies []string) (bool, string, error) {
	switch len(strategies) {
	case 0:
		return false, "", nil
	case 1:
		singleStrategy := strategies[0]
		switch singleStrategy {
		case retainKeysStrategy:
			return true, "", nil
		default:
			return false, singleStrategy, nil
		}
	case 2:
		switch {
		case strategies[0] == retainKeysStrategy:
			return true, strategies[1], nil
		case strategies[1] == retainKeysStrategy:
			return true, strategies[0], nil
		default:
			return false, "", fmt.Errorf("unexpected patch strategy: %v", strategies)
		}
	default:
		return false, "", fmt.Errorf("unexpected patch strategy: %v", strategies)
	}
}

// hasAdditionalNewField returns if original map has additional key with non-nil value than modified.
func hasAdditionalNewField(original, modified map[string]interface{}) bool {
	for k, v := range original {
		if v == nil {
			continue
		}
		if _, found := modified[k]; !found {
			return true
		}
	}
	return false
}
