/*
Copyright 2023 The Kubernetes Authors.

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

package knftables

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Interface is an interface for running nftables commands against a given family and table.
type Interface interface {
	// NewTransaction returns a new (empty) Transaction
	NewTransaction() *Transaction

	// Run runs a Transaction and returns the result. The IsNotFound and
	// IsAlreadyExists methods can be used to test the result.
	Run(ctx context.Context, tx *Transaction) error

	// Check does a dry-run of a Transaction (as with `nft --check`) and returns the
	// result. The IsNotFound and IsAlreadyExists methods can be used to test the
	// result.
	Check(ctx context.Context, tx *Transaction) error

	// List returns a list of the names of the objects of objectType ("chain", "set",
	// "map" or "counter") in the table. If there are no such objects, this will return an empty
	// list and no error.
	List(ctx context.Context, objectType string) ([]string, error)

	// ListRules returns a list of the rules in a chain, in order. If no chain name is
	// specified, then all rules within the table will be returned. Note that at the
	// present time, the Rule objects will have their `Comment` and `Handle` fields
	// filled in, but *not* the actual `Rule` field. So this can only be used to find
	// the handles of rules if they have unique comments to recognize them by, or if
	// you know the order of the rules within the chain. If the chain exists but
	// contains no rules, this will return an empty list and no error.
	ListRules(ctx context.Context, chain string) ([]*Rule, error)

	// ListElements returns a list of the elements in a set or map. (objectType should
	// be "set" or "map".) If the set/map exists but contains no elements, this will
	// return an empty list and no error.
	ListElements(ctx context.Context, objectType, name string) ([]*Element, error)

	// ListCounters returns a list of the counters.
	ListCounters(ctx context.Context) ([]*Counter, error)
}

type nftContext struct {
	family Family
	table  string

	// noObjectComments is true if comments on Table/Chain/Set/Map are not supported.
	// (Comments on Rule and Element are always supported.)
	noObjectComments bool
}

// realNFTables is an implementation of Interface
type realNFTables struct {
	nftContext

	bufferMutex sync.Mutex
	buffer      *bytes.Buffer

	exec execer
	path string
}

// newInternal creates a new nftables.Interface for interacting with the given table; this
// is split out from New() so it can be used from unit tests with a fakeExec.
func newInternal(family Family, table string, execer execer) (Interface, error) {
	var err error

	nft := &realNFTables{
		nftContext: nftContext{
			family: family,
			table:  table,
		},
		buffer: &bytes.Buffer{},
		exec:   execer,
	}

	nft.path, err = nft.exec.LookPath("nft")
	if err != nil {
		return nil, fmt.Errorf("could not find nftables binary: %w", err)
	}

	cmd := exec.Command(nft.path, "--version")
	out, err := nft.exec.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("could not run nftables command: %w", err)
	}
	if strings.HasPrefix(out, "nftables v0.") || strings.HasPrefix(out, "nftables v1.0.0 ") {
		return nil, fmt.Errorf("nft version must be v1.0.1 or later (got %s)", strings.TrimSpace(out))
	}

	// Check that (a) nft works, (b) we have permission, (c) the kernel is new enough
	// to support object comments.
	tx := nft.NewTransaction()
	tx.Add(&Table{
		Comment: PtrTo("test"),
	})
	if err := nft.Check(context.TODO(), tx); err != nil {
		// Try again, checking just that (a) nft works, (b) we have permission.
		tx := nft.NewTransaction()
		tx.Add(&Table{})
		if err := nft.Check(context.TODO(), tx); err != nil {
			return nil, fmt.Errorf("could not run nftables command: %w", err)
		}

		nft.noObjectComments = true
	}

	return nft, nil
}

// New creates a new nftables.Interface for interacting with the given table. If nftables
// is not available/usable on the current host, it will return an error.
func New(family Family, table string) (Interface, error) {
	return newInternal(family, table, realExec{})
}

// NewTransaction is part of Interface
func (nft *realNFTables) NewTransaction() *Transaction {
	return &Transaction{nftContext: &nft.nftContext}
}

// Run is part of Interface
func (nft *realNFTables) Run(ctx context.Context, tx *Transaction) error {
	nft.bufferMutex.Lock()
	defer nft.bufferMutex.Unlock()

	if tx.err != nil {
		return tx.err
	}

	nft.buffer.Reset()
	err := tx.populateCommandBuf(nft.buffer)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, nft.path, "-f", "-")
	cmd.Stdin = nft.buffer
	_, err = nft.exec.Run(cmd)
	return err
}

// Check is part of Interface
func (nft *realNFTables) Check(ctx context.Context, tx *Transaction) error {
	nft.bufferMutex.Lock()
	defer nft.bufferMutex.Unlock()

	if tx.err != nil {
		return tx.err
	}

	nft.buffer.Reset()
	err := tx.populateCommandBuf(nft.buffer)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, nft.path, "--check", "-f", "-")
	cmd.Stdin = nft.buffer
	_, err = nft.exec.Run(cmd)
	return err
}

// jsonVal looks up key in json; if it exists and is of type T, it returns (json[key], true).
// Otherwise it returns (_, false).
func jsonVal[T any](json map[string]interface{}, key string) (T, bool) {
	if ifVal, exists := json[key]; exists {
		tVal, ok := ifVal.(T)
		return tVal, ok
	}
	var zero T
	return zero, false
}

// getJSONObjects takes the output of "nft -j list", validates it, and returns an array
// of just the objects of objectType.
func getJSONObjects(listOutput, objectType string) ([]map[string]interface{}, error) {
	// listOutput should contain JSON looking like:
	//
	// {
	//   "nftables": [
	//     {
	//       "metainfo": {
	//         "json_schema_version": 1,
	//         ...
	//       }
	//     },
	//     {
	//       "chain": {
	//         "family": "ip",
	//         "table": "kube-proxy",
	//         "name": "KUBE-SERVICES",
	//         "handle": 3
	//       }
	//     },
	//     {
	//       "chain": {
	//         "family": "ip",
	//         "table": "kube-proxy",
	//         "name": "KUBE-NODEPORTS",
	//         "handle": 4
	//       }
	//     },
	//     ...
	//   ]
	// }
	//
	// In this case, given objectType "chain", we would return
	//
	// [
	//   {
	//     "family": "ip",
	//     "table": "kube-proxy",
	//     "name": "KUBE-SERVICES",
	//     "handle": 3
	//   },
	//   {
	//     "family": "ip",
	//     "table": "kube-proxy",
	//     "name": "KUBE-NODEPORTS",
	//     "handle": 4
	//   },
	//   ...
	// ]

	jsonResult := map[string][]map[string]map[string]interface{}{}
	if err := json.Unmarshal([]byte(listOutput), &jsonResult); err != nil {
		return nil, fmt.Errorf("could not parse nft output: %w", err)
	}

	nftablesResult := jsonResult["nftables"]
	if len(nftablesResult) == 0 {
		return nil, fmt.Errorf("could not find result in nft output %q", listOutput)
	}
	metainfo := nftablesResult[0]["metainfo"]
	if metainfo == nil {
		return nil, fmt.Errorf("could not find metadata in nft output %q", listOutput)
	}
	// json_schema_version is an integer but `json.Unmarshal()` will have parsed it as
	// a float64 since we didn't tell it otherwise.
	if version, ok := jsonVal[float64](metainfo, "json_schema_version"); !ok || version != 1.0 {
		return nil, fmt.Errorf("could not find supported json_schema_version in nft output %q", listOutput)
	}

	var objects []map[string]interface{}
	for _, objContainer := range nftablesResult {
		obj := objContainer[objectType]
		if obj != nil {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

// List is part of Interface.
func (nft *realNFTables) List(ctx context.Context, objectType string) ([]string, error) {
	// All currently-existing nftables object types have plural forms that are just
	// the singular form plus 's'.
	var typeSingular, typePlural string
	if objectType[len(objectType)-1] == 's' {
		typeSingular = objectType[:len(objectType)-1]
		typePlural = objectType
	} else {
		typeSingular = objectType
		typePlural = objectType + "s"
	}

	cmd := exec.CommandContext(ctx, nft.path, "--json", "list", typePlural, string(nft.family))
	out, err := nft.exec.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to run nft: %w", err)
	}

	objects, err := getJSONObjects(out, typeSingular)
	if err != nil {
		return nil, err
	}

	var result []string
	for _, obj := range objects {
		objTable, _ := jsonVal[string](obj, "table")
		if objTable != nft.table {
			continue
		}

		if name, ok := jsonVal[string](obj, "name"); ok {
			result = append(result, name)
		}
	}
	return result, nil
}

// ListRules is part of Interface
func (nft *realNFTables) ListRules(ctx context.Context, chain string) ([]*Rule, error) {
	// If no chain is given, return all rules from within the table.
	var cmd *exec.Cmd
	if chain == "" {
		cmd = exec.CommandContext(ctx, nft.path, "--json", "list", "table", string(nft.family), nft.table)
	} else {
		cmd = exec.CommandContext(ctx, nft.path, "--json", "list", "chain", string(nft.family), nft.table, chain)
	}
	out, err := nft.exec.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to run nft: %w", err)
	}

	jsonRules, err := getJSONObjects(out, "rule")
	if err != nil {
		return nil, fmt.Errorf("unable to parse JSON output: %w", err)
	}

	rules := make([]*Rule, 0, len(jsonRules))
	for _, jsonRule := range jsonRules {
		parentChain, ok := jsonVal[string](jsonRule, "chain")
		if !ok {
			return nil, fmt.Errorf("unexpected JSON output from nft (rule with no chain)")
		}
		rule := &Rule{
			Chain: parentChain,
		}

		// handle is written as an integer in nft's output, but json.Unmarshal
		// will have parsed it as a float64. (Handles are uint64s, but they are
		// assigned consecutively starting from 1, so as long as fewer than 2**53
		// nftables objects have been created since boot time, we won't run into
		// float64-vs-uint64 precision issues.)
		if handle, ok := jsonVal[float64](jsonRule, "handle"); ok {
			rule.Handle = PtrTo(int(handle))
		}
		if comment, ok := jsonVal[string](jsonRule, "comment"); ok {
			rule.Comment = &comment
		}

		rules = append(rules, rule)
	}
	return rules, nil
}

// ListElements is part of Interface
func (nft *realNFTables) ListElements(ctx context.Context, objectType, name string) ([]*Element, error) {
	cmd := exec.CommandContext(ctx, nft.path, "--json", "list", objectType, string(nft.family), nft.table, name)
	out, err := nft.exec.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to run nft: %w", err)
	}

	jsonSetsOrMaps, err := getJSONObjects(out, objectType)
	if err != nil {
		return nil, fmt.Errorf("unable to parse JSON output: %w", err)
	}
	if len(jsonSetsOrMaps) != 1 {
		return nil, fmt.Errorf("unexpected JSON output from nft (multiple results)")
	}

	jsonElements, _ := jsonVal[[]interface{}](jsonSetsOrMaps[0], "elem")
	elements := make([]*Element, 0, len(jsonElements))
	for _, jsonElement := range jsonElements {
		var key, value interface{}

		elem := &Element{}
		if objectType == "set" {
			elem.Set = name
			key = jsonElement
		} else {
			elem.Map = name
			tuple, ok := jsonElement.([]interface{})
			if !ok || len(tuple) != 2 {
				return nil, fmt.Errorf("unexpected JSON output from nft (elem is not [key,val]: %q)", jsonElement)
			}
			key, value = tuple[0], tuple[1]
		}

		// If the element has a comment, then key will be a compound object like:
		//
		//   {
		//     "elem": {
		//       "val": "192.168.0.1",
		//       "comment": "this is a comment"
		//     }
		//   }
		//
		// (Where "val" contains the value that key would have held if there was no
		// comment.)
		if obj, ok := key.(map[string]interface{}); ok {
			if compoundElem, ok := jsonVal[map[string]interface{}](obj, "elem"); ok {
				if key, ok = jsonVal[interface{}](compoundElem, "val"); !ok {
					return nil, fmt.Errorf("unexpected JSON output from nft (elem with no val: %q)", jsonElement)
				}
				if comment, ok := jsonVal[string](compoundElem, "comment"); ok {
					elem.Comment = &comment
				}
			}
		}

		elem.Key, err = parseElementValue(key)
		if err != nil {
			return nil, err
		}
		if value != nil {
			elem.Value, err = parseElementValue(value)
			if err != nil {
				return nil, err
			}
		}

		elements = append(elements, elem)
	}
	return elements, nil
}

// parseElementValue parses a JSON element key/value, handling concatenations, prefixes, and
// converting numeric or "verdict" values to strings.
func parseElementValue(json interface{}) ([]string, error) {
	// json can be:
	//
	//   - a single string, e.g. "192.168.1.3"
	//
	//   - a single number, e.g. 80
	//
	//   - a prefix, expressed as an object:
	//     {
	//       "prefix": {
	//         "addr": "192.168.0.0",
	//         "len": 16,
	//       }
	//     }
	//
	//   - a concatenation, expressed as an object containing an array of simple
	//     values:
	//        {
	//          "concat": [
	//            "192.168.1.3",
	//            "tcp",
	//            80
	//          ]
	//        }
	//
	//   - a verdict (for a vmap value), expressed as an object:
	//        {
	//          "drop": null
	//        }
	//
	//        {
	//          "goto": {
	//            "target": "destchain"
	//          }
	//        }

	switch val := json.(type) {
	case string:
		return []string{val}, nil
	case float64:
		return []string{fmt.Sprintf("%d", int(val))}, nil
	case map[string]interface{}:
		if concat, _ := jsonVal[[]interface{}](val, "concat"); concat != nil {
			vals := make([]string, len(concat))
			for i := range concat {
				if str, ok := concat[i].(string); ok {
					vals[i] = str
				} else if num, ok := concat[i].(float64); ok {
					vals[i] = fmt.Sprintf("%d", int(num))
				} else {
					return nil, fmt.Errorf("could not parse element value %q", concat[i])
				}
			}
			return vals, nil
		} else if prefix, _ := jsonVal[map[string]interface{}](val, "prefix"); prefix != nil {
			// For prefix-type elements, return the element in CIDR representation.
			addr, ok := jsonVal[string](prefix, "addr")
			if !ok {
				return nil, fmt.Errorf("could not parse 'addr' value as string: %q", prefix)
			}
			length, ok := jsonVal[float64](prefix, "len")
			if !ok {
				return nil, fmt.Errorf("could not parse 'len' value as number: %q", prefix)
			}
			return []string{fmt.Sprintf("%s/%d", addr, int(length))}, nil
		} else if len(val) == 1 {
			var verdict string
			// We just checked that len(val) == 1, so this loop body will only
			// run once
			for k, v := range val {
				if v == nil {
					verdict = k
				} else if target, ok := v.(map[string]interface{}); ok {
					verdict = fmt.Sprintf("%s %s", k, target["target"])
				}
			}
			return []string{verdict}, nil
		}
	}

	return nil, fmt.Errorf("could not parse element value %q", json)
}

// ListCounters is part of Interface
func (nft *realNFTables) ListCounters(ctx context.Context) ([]*Counter, error) {
	cmd := exec.CommandContext(ctx, nft.path, "--json", "list", "counters", "table", string(nft.family), nft.table)
	out, err := nft.exec.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to run nft: %w", err)
	}

	objects, err := getJSONObjects(out, "counter")
	if err != nil {
		return nil, err
	}

	objectToCounter := func(object map[string]interface{}) *Counter {
		counter := &Counter{
			Name:    object["name"].(string),
			Packets: PtrTo(uint64(object["packets"].(float64))),
			Bytes:   PtrTo(uint64(object["bytes"].(float64))),
		}
		if handle, ok := jsonVal[string](object, "comment"); ok {
			counter.Comment = PtrTo(handle)
		}
		if handle, ok := jsonVal[float64](object, "handle"); ok {
			counter.Handle = PtrTo(int(handle))
		}

		return counter
	}

	counters := make([]*Counter, 0, len(objects))
	for _, object := range objects {
		counters = append(counters, objectToCounter(object))
	}
	return counters, nil
}
