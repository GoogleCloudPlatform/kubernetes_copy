package jsonpatch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/evanphx/json-patch/v5/internal/json"
)

func merge(cur, patch *lazyNode, mergeMerge bool, options *ApplyOptions) *lazyNode {
	curDoc, err := cur.intoDoc(options)

	if err != nil {
		pruneNulls(patch, options)
		return patch
	}

	patchDoc, err := patch.intoDoc(options)

	if err != nil {
		return patch
	}

	mergeDocs(curDoc, patchDoc, mergeMerge, options)

	return cur
}

func mergeDocs(doc, patch *partialDoc, mergeMerge bool, options *ApplyOptions) {
	for k, v := range patch.obj {
		if v == nil {
			if mergeMerge {
				idx := -1
				for i, key := range doc.keys {
					if key == k {
						idx = i
						break
					}
				}
				if idx == -1 {
					doc.keys = append(doc.keys, k)
				}
				doc.obj[k] = nil
			} else {
				_ = doc.remove(k, options)
			}
		} else {
			cur, ok := doc.obj[k]

			if !ok || cur == nil {
				if !mergeMerge {
					pruneNulls(v, options)
				}
				_ = doc.set(k, v, options)
			} else {
				_ = doc.set(k, merge(cur, v, mergeMerge, options), options)
			}
		}
	}
}

func pruneNulls(n *lazyNode, options *ApplyOptions) {
	sub, err := n.intoDoc(options)

	if err == nil {
		pruneDocNulls(sub, options)
	} else {
		ary, err := n.intoAry()

		if err == nil {
			pruneAryNulls(ary, options)
		}
	}
}

func pruneDocNulls(doc *partialDoc, options *ApplyOptions) *partialDoc {
	for k, v := range doc.obj {
		if v == nil {
			_ = doc.remove(k, &ApplyOptions{})
		} else {
			pruneNulls(v, options)
		}
	}

	return doc
}

func pruneAryNulls(ary *partialArray, options *ApplyOptions) *partialArray {
	newAry := []*lazyNode{}

	for _, v := range ary.nodes {
		if v != nil {
			pruneNulls(v, options)
		}
		newAry = append(newAry, v)
	}

	ary.nodes = newAry

	return ary
}

var ErrBadJSONDoc = fmt.Errorf("Invalid JSON Document")
var ErrBadJSONPatch = fmt.Errorf("Invalid JSON Patch")
var errBadMergeTypes = fmt.Errorf("Mismatched JSON Documents")

// MergeMergePatches merges two merge patches together, such that
// applying this resulting merged merge patch to a document yields the same
// as merging each merge patch to the document in succession.
func MergeMergePatches(patch1Data, patch2Data []byte) ([]byte, error) {
	return doMergePatch(patch1Data, patch2Data, true)
}

// MergePatch merges the patchData into the docData.
func MergePatch(docData, patchData []byte) ([]byte, error) {
	return doMergePatch(docData, patchData, false)
}

func doMergePatch(docData, patchData []byte, mergeMerge bool) ([]byte, error) {
	if !json.Valid(docData) {
		return nil, ErrBadJSONDoc
	}

	if !json.Valid(patchData) {
		return nil, ErrBadJSONPatch
	}

	options := NewApplyOptions()

	doc := &partialDoc{
		opts: options,
	}

	docErr := doc.UnmarshalJSON(docData)

	patch := &partialDoc{
		opts: options,
	}

	patchErr := patch.UnmarshalJSON(patchData)

	if isSyntaxError(docErr) {
		return nil, ErrBadJSONDoc
	}

	if isSyntaxError(patchErr) {
		return patchData, nil
	}

	if docErr == nil && doc.obj == nil {
		return nil, ErrBadJSONDoc
	}

	if patchErr == nil && patch.obj == nil {
		return patchData, nil
	}

	if docErr != nil || patchErr != nil {
		// Not an error, just not a doc, so we turn straight into the patch
		if patchErr == nil {
			if mergeMerge {
				doc = patch
			} else {
				doc = pruneDocNulls(patch, options)
			}
		} else {
			patchAry := &partialArray{}
			patchErr = unmarshal(patchData, &patchAry.nodes)

			if patchErr != nil {
				// Not an array either, a literal is the result directly.
				if json.Valid(patchData) {
					return patchData, nil
				}
				return nil, ErrBadJSONPatch
			}

			pruneAryNulls(patchAry, options)

			out, patchErr := json.Marshal(patchAry.nodes)

			if patchErr != nil {
				return nil, ErrBadJSONPatch
			}

			return out, nil
		}
	} else {
		mergeDocs(doc, patch, mergeMerge, options)
	}

	return json.Marshal(doc)
}

func isSyntaxError(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if _, ok := err.(*json.SyntaxError); ok {
		return true
	}
	if _, ok := err.(*syntaxError); ok {
		return true
	}
	return false
}

// resemblesJSONArray indicates whether the byte-slice "appears" to be
// a JSON array or not.
// False-positives are possible, as this function does not check the internal
// structure of the array. It only checks that the outer syntax is present and
// correct.
func resemblesJSONArray(input []byte) bool {
	input = bytes.TrimSpace(input)

	hasPrefix := bytes.HasPrefix(input, []byte("["))
	hasSuffix := bytes.HasSuffix(input, []byte("]"))

	return hasPrefix && hasSuffix
}

// CreateMergePatch will return a merge patch document capable of converting
// the original document(s) to the modified document(s).
// The parameters can be bytes of either two JSON Documents, or two arrays of
// JSON documents.
// The merge patch returned follows the specification defined at http://tools.ietf.org/html/draft-ietf-appsawg-json-merge-patch-07
func CreateMergePatch(originalJSON, modifiedJSON []byte) ([]byte, error) {
	originalResemblesArray := resemblesJSONArray(originalJSON)
	modifiedResemblesArray := resemblesJSONArray(modifiedJSON)

	// Do both byte-slices seem like JSON arrays?
	if originalResemblesArray && modifiedResemblesArray {
		return createArrayMergePatch(originalJSON, modifiedJSON)
	}

	// Are both byte-slices are not arrays? Then they are likely JSON objects...
	if !originalResemblesArray && !modifiedResemblesArray {
		return createObjectMergePatch(originalJSON, modifiedJSON)
	}

	// None of the above? Then return an error because of mismatched types.
	return nil, errBadMergeTypes
}

// createObjectMergePatch will return a merge-patch document capable of
// converting the original document to the modified document.
func createObjectMergePatch(originalJSON, modifiedJSON []byte) ([]byte, error) {
	originalDoc := map[string]interface{}{}
	modifiedDoc := map[string]interface{}{}

	err := unmarshal(originalJSON, &originalDoc)
	if err != nil {
		return nil, ErrBadJSONDoc
	}

	err = unmarshal(modifiedJSON, &modifiedDoc)
	if err != nil {
		return nil, ErrBadJSONDoc
	}

	dest, err := getDiff(originalDoc, modifiedDoc)
	if err != nil {
		return nil, err
	}

	return json.Marshal(dest)
}

func unmarshal(data []byte, into interface{}) error {
	return json.UnmarshalValid(data, into)
}

// createArrayMergePatch will return an array of merge-patch documents capable
// of converting the original document to the modified document for each
// pair of JSON documents provided in the arrays.
// Arrays of mismatched sizes will result in an error.
func createArrayMergePatch(originalJSON, modifiedJSON []byte) ([]byte, error) {
	originalDocs := []json.RawMessage{}
	modifiedDocs := []json.RawMessage{}

	err := unmarshal(originalJSON, &originalDocs)
	if err != nil {
		return nil, ErrBadJSONDoc
	}

	err = unmarshal(modifiedJSON, &modifiedDocs)
	if err != nil {
		return nil, ErrBadJSONDoc
	}

	total := len(originalDocs)
	if len(modifiedDocs) != total {
		return nil, ErrBadJSONDoc
	}

	result := []json.RawMessage{}
	for i := 0; i < len(originalDocs); i++ {
		original := originalDocs[i]
		modified := modifiedDocs[i]

		patch, err := createObjectMergePatch(original, modified)
		if err != nil {
			return nil, err
		}

		result = append(result, json.RawMessage(patch))
	}

	return json.Marshal(result)
}

// Returns true if the array matches (must be json types).
// As is idiomatic for go, an empty array is not the same as a nil array.
func matchesArray(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	if (a == nil && b != nil) || (a != nil && b == nil) {
		return false
	}
	for i := range a {
		if !matchesValue(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Returns true if the values matches (must be json types)
// The types of the values must match, otherwise it will always return false
// If two map[string]interface{} are given, all elements must match.
func matchesValue(av, bv interface{}) bool {
	if reflect.TypeOf(av) != reflect.TypeOf(bv) {
		return false
	}
	switch at := av.(type) {
	case string:
		bt := bv.(string)
		if bt == at {
			return true
		}
	case json.Number:
		bt := bv.(json.Number)
		if bt == at {
			return true
		}
	case float64:
		bt := bv.(float64)
		if bt == at {
			return true
		}
	case bool:
		bt := bv.(bool)
		if bt == at {
			return true
		}
	case nil:
		// Both nil, fine.
		return true
	case map[string]interface{}:
		bt := bv.(map[string]interface{})
		if len(bt) != len(at) {
			return false
		}
		for key := range bt {
			av, aOK := at[key]
			bv, bOK := bt[key]
			if aOK != bOK {
				return false
			}
			if !matchesValue(av, bv) {
				return false
			}
		}
		return true
	case []interface{}:
		bt := bv.([]interface{})
		return matchesArray(at, bt)
	}
	return false
}

// getDiff returns the (recursive) difference between a and b as a map[string]interface{}.
func getDiff(a, b map[string]interface{}) (map[string]interface{}, error) {
	into := map[string]interface{}{}
	for key, bv := range b {
		av, ok := a[key]
		// value was added
		if !ok {
			into[key] = bv
			continue
		}
		// If types have changed, replace completely
		if reflect.TypeOf(av) != reflect.TypeOf(bv) {
			into[key] = bv
			continue
		}
		// Types are the same, compare values
		switch at := av.(type) {
		case map[string]interface{}:
			bt := bv.(map[string]interface{})
			dst := make(map[string]interface{}, len(bt))
			dst, err := getDiff(at, bt)
			if err != nil {
				return nil, err
			}
			if len(dst) > 0 {
				into[key] = dst
			}
		case string, float64, bool, json.Number:
			if !matchesValue(av, bv) {
				into[key] = bv
			}
		case []interface{}:
			bt := bv.([]interface{})
			if !matchesArray(at, bt) {
				into[key] = bv
			}
		case nil:
			switch bv.(type) {
			case nil:
				// Both nil, fine.
			default:
				into[key] = bv
			}
		default:
			panic(fmt.Sprintf("Unknown type:%T in key %s", av, key))
		}
	}
	// Now add all deleted values as nil
	for key := range a {
		_, found := b[key]
		if !found {
			into[key] = nil
		}
	}
	return into, nil
}
