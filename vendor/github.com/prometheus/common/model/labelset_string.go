// Copyright 2024 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"bytes"
	"slices"
	"strconv"
)

// String will look like `{foo="bar", more="less"}`. Names are sorted alphabetically.
func (l LabelSet) String() string {
	var lna [32]string // On stack to avoid memory allocation for sorting names.
	labelNames := lna[:0]
	for name := range l {
		labelNames = append(labelNames, string(name))
	}
	slices.Sort(labelNames)
	var bytea [1024]byte // On stack to avoid memory allocation while building the output.
	b := bytes.NewBuffer(bytea[:0])
	b.WriteByte('{')
	for i, name := range labelNames {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.Write(strconv.AppendQuote(b.AvailableBuffer(), string(l[LabelName(name)])))
	}
	b.WriteByte('}')
	return b.String()
}
