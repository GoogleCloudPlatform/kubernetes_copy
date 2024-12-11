// Copyright 2018 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package flags

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

// StringsValue wraps "sort.StringSlice".
type StringsValue sort.StringSlice

// Set parses a command line set of strings, separated by comma.
// Implements "flag.Value" interface.
func (ss *StringsValue) Set(s string) error {
	*ss = strings.Split(s, ",")
	return nil
}

// String implements "flag.Value" interface.
func (ss *StringsValue) String() string { return strings.Join(*ss, ",") }

// NewStringsValue implements string slice as "flag.Value" interface.
// Given value is to be separated by comma.
func NewStringsValue(s string) (ss *StringsValue) {
	if s == "" {
		return &StringsValue{}
	}
	ss = new(StringsValue)
	if err := ss.Set(s); err != nil {
		panic(fmt.Sprintf("new StringsValue should never fail: %v", err))
	}
	return ss
}

// StringsFromFlag returns a string slice from the flag.
func StringsFromFlag(fs *flag.FlagSet, flagName string) []string {
	return *fs.Lookup(flagName).Value.(*StringsValue)
}
