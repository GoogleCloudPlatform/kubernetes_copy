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

package cacher

import (
	"math"
	"strings"
	"time"
)

// hasPathPrefix returns true if the string matches pathPrefix exactly, or if is prefixed with pathPrefix at a path segment boundary
func hasPathPrefix(s, pathPrefix string) bool {
	// Short circuit if s doesn't contain the prefix at all
	if !strings.HasPrefix(s, pathPrefix) {
		return false
	}

	pathPrefixLength := len(pathPrefix)

	if len(s) == pathPrefixLength {
		// Exact match
		return true
	}
	if strings.HasSuffix(pathPrefix, "/") {
		// pathPrefix already ensured a path segment boundary
		return true
	}
	if s[pathPrefixLength:pathPrefixLength+1] == "/" {
		// The next character in s is a path segment boundary
		// Check this instead of normalizing pathPrefix to avoid allocating on every call
		return true
	}
	return false
}

// calculateRetryAfterForUnreadyCache calculates the retry duration based on the cache downtime.
func calculateRetryAfterForUnreadyCache(downtime time.Duration) int {
	scale := 0.1
	result := scale * math.Pow(downtime.Seconds(), 2)
	result = math.Min(30, math.Max(1, result))
	return int(result)
}
