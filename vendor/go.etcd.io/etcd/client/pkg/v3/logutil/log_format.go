// Copyright 2019 The etcd Authors
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

package logutil

import "fmt"

const (
	JSONLogFormat    = "json"
	ConsoleLogFormat = "console"
	//revive:disable:var-naming
	// Deprecated: Please use JSONLogFormat.
	JsonLogFormat = JSONLogFormat
	//revive:enable:var-naming
)

var DefaultLogFormat = JSONLogFormat

// ConvertToZapFormat converts and validated log format string.
func ConvertToZapFormat(format string) (string, error) {
	switch format {
	case ConsoleLogFormat:
		return ConsoleLogFormat, nil
	case JSONLogFormat:
		return JSONLogFormat, nil
	case "":
		return DefaultLogFormat, nil
	default:
		return "", fmt.Errorf("unknown log format: %s, supported values json, console", format)
	}
}
