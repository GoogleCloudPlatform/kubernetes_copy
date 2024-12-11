// Copyright 2015 The etcd Authors
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

package types

import (
	"strconv"
	"strings"
)

// ID represents a generic identifier which is canonically
// stored as a uint64 but is typically represented as a
// base-16 string for input/output
type ID uint64

func (i ID) String() string {
	return strconv.FormatUint(uint64(i), 16)
}

// IDFromString attempts to create an ID from a base-16 string.
func IDFromString(s string) (ID, error) {
	i, err := strconv.ParseUint(s, 16, 64)
	return ID(i), err
}

// IDSlice implements the sort interface
type IDSlice []ID

func (p IDSlice) Len() int           { return len(p) }
func (p IDSlice) Less(i, j int) bool { return uint64(p[i]) < uint64(p[j]) }
func (p IDSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func (p IDSlice) String() string {
	var b strings.Builder
	if p.Len() > 0 {
		b.WriteString(p[0].String())
	}

	for i := 1; i < p.Len(); i++ {
		b.WriteString(",")
		b.WriteString(p[i].String())
	}

	return b.String()
}
