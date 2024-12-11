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

package flags

import (
	"flag"
	"fmt"
	"net/url"
	"strings"

	"go.etcd.io/etcd/client/pkg/v3/types"
)

// URLsValue wraps "types.URLs".
type URLsValue types.URLs

// Set parses a command line set of URLs formatted like:
// http://127.0.0.1:2380,http://10.1.1.2:80
// Implements "flag.Value" interface.
func (us *URLsValue) Set(s string) error {
	ss, err := types.NewURLs(strings.Split(s, ","))
	if err != nil {
		return err
	}
	*us = URLsValue(ss)
	return nil
}

// String implements "flag.Value" interface.
func (us *URLsValue) String() string {
	all := make([]string, len(*us))
	for i, u := range *us {
		all[i] = u.String()
	}
	return strings.Join(all, ",")
}

// NewURLsValue implements "url.URL" slice as flag.Value interface.
// Given value is to be separated by comma.
func NewURLsValue(s string) *URLsValue {
	if s == "" {
		return &URLsValue{}
	}
	v := &URLsValue{}
	if err := v.Set(s); err != nil {
		panic(fmt.Sprintf("new URLsValue should never fail: %v", err))
	}
	return v
}

// URLsFromFlag returns a slices from url got from the flag.
func URLsFromFlag(fs *flag.FlagSet, urlsFlagName string) []url.URL {
	return *fs.Lookup(urlsFlagName).Value.(*URLsValue)
}
