/*
Copyright 2021 The Kubernetes Authors.

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

package version

import (
	"fmt"
	"io"
	"math"

	"k8s.io/apimachinery/pkg/util/version"
	apimachineryversion "k8s.io/apimachinery/pkg/version"
)

// supportedMinorVersionSkew is the maximum supported difference between the client and server minor versions.
// For example: client versions 1.18, 1.19, and 1.20 would be within the supported version skew for server version 1.19,
// and server versions 1.18, 1.19, and 1.20 would be within the supported version skew for client version 1.19.
const supportedMinorVersionSkew = 1

// printVersionSkewWarning prints a warning message if the difference between the client and version is greater than
// the supported version skew , returns the warning string.
func printVersionSkewWarning(w io.Writer, clientVersion, serverVersion apimachineryversion.Info) (string, error) {
	parsedClientVersion, err := version.ParseSemantic(clientVersion.GitVersion)
	if err != nil {
		return "", err
	}

	parsedServerVersion, err := version.ParseSemantic(serverVersion.GitVersion)
	if err != nil {
		return "", err
	}

	majorVersionDifference := math.Abs(float64(parsedClientVersion.Major()) - float64(parsedServerVersion.Major()))
	minorVersionDifference := math.Abs(float64(parsedClientVersion.Minor()) - float64(parsedServerVersion.Minor()))

	if majorVersionDifference > 0 || minorVersionDifference > supportedMinorVersionSkew {
		warningMessage := fmt.Sprintf("WARNING: version difference between client (%d.%d) and server (%d.%d) exceeds the supported minor version skew of +/-%d\n",
			parsedClientVersion.Major(), parsedClientVersion.Minor(), parsedServerVersion.Major(), parsedServerVersion.Minor(), supportedMinorVersionSkew)
		fmt.Fprint(w, warningMessage)
		return warningMessage, nil
	}
	return "", nil
}
