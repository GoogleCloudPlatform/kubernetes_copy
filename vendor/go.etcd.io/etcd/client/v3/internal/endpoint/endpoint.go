// Copyright 2021 The etcd Authors
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

package endpoint

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

type CredsRequirement int

const (
	// CredsRequire - Credentials/certificate required for thi type of connection.
	CredsRequire CredsRequirement = iota
	// CredsDrop - Credentials/certificate not needed and should get ignored.
	CredsDrop
	// CredsOptional - Credentials/certificate might be used if supplied
	CredsOptional
)

func extractHostFromHostPort(ep string) string {
	host, _, err := net.SplitHostPort(ep)
	if err != nil {
		return ep
	}
	return host
}

// mustSplit2 returns the values from strings.SplitN(s, sep, 2).
// If sep is not found, it returns ("", "", false) instead.
func mustSplit2(s, sep string) (string, string) {
	spl := strings.SplitN(s, sep, 2)
	if len(spl) < 2 {
		panic(fmt.Errorf("token '%v' expected to have separator sep: `%v`", s, sep))
	}
	return spl[0], spl[1]
}

func schemeToCredsRequirement(schema string) CredsRequirement {
	switch schema {
	case "https", "unixs":
		return CredsRequire
	case "http":
		return CredsDrop
	case "unix":
		// Preserving previous behavior from:
		// https://github.com/etcd-io/etcd/blob/dae29bb719dd69dc119146fc297a0628fcc1ccf8/client/v3/client.go#L212
		// that likely was a bug due to missing 'fallthrough'.
		// At the same time it seems legit to let the users decide whether they
		// want credential control or not (and 'unixs' schema is not a standard thing).
		return CredsOptional
	case "":
		return CredsOptional
	default:
		return CredsOptional
	}
}

// This function translates endpoints names supported by etcd server into
// endpoints as supported by grpc with additional information
// (server_name for cert validation, requireCreds - whether certs are needed).
// The main differences:
//   - etcd supports unixs & https names as opposed to unix & http to
//     distinguish need to configure certificates.
//   - etcd support http(s) names as opposed to tcp supported by grpc/dial method.
//   - etcd supports unix(s)://local-file naming schema
//     (as opposed to unix:local-file canonical name used by grpc for current dir files).
//   - Within the unix(s) schemas, the last segment (filename) without 'port' (content after colon)
//     is considered serverName - to allow local testing of cert-protected communication.
//
// See more:
//   - https://github.com/grpc/grpc-go/blob/26c143bd5f59344a4b8a1e491e0f5e18aa97abc7/internal/grpcutil/target.go#L47
//   - https://golang.org/pkg/net/#Dial
//   - https://github.com/grpc/grpc/blob/master/doc/naming.md
func translateEndpoint(ep string) (addr string, serverName string, requireCreds CredsRequirement) {
	if strings.HasPrefix(ep, "unix:") || strings.HasPrefix(ep, "unixs:") {
		if strings.HasPrefix(ep, "unix:///") || strings.HasPrefix(ep, "unixs:///") {
			// absolute path case
			schema, absolutePath := mustSplit2(ep, "://")
			return "unix://" + absolutePath, path.Base(absolutePath), schemeToCredsRequirement(schema)
		}
		if strings.HasPrefix(ep, "unix://") || strings.HasPrefix(ep, "unixs://") {
			// legacy etcd local path
			schema, localPath := mustSplit2(ep, "://")
			return "unix:" + localPath, path.Base(localPath), schemeToCredsRequirement(schema)
		}
		schema, localPath := mustSplit2(ep, ":")
		return "unix:" + localPath, path.Base(localPath), schemeToCredsRequirement(schema)
	}

	if strings.Contains(ep, "://") {
		url, err := url.Parse(ep)
		if err != nil {
			return ep, ep, CredsOptional
		}
		if url.Scheme == "http" || url.Scheme == "https" {
			return url.Host, url.Host, schemeToCredsRequirement(url.Scheme)
		}
		return ep, url.Host, schemeToCredsRequirement(url.Scheme)
	}
	// Handles plain addresses like 10.0.0.44:437.
	return ep, ep, CredsOptional
}

// RequiresCredentials returns whether given endpoint requires
// credentials/certificates for connection.
func RequiresCredentials(ep string) CredsRequirement {
	_, _, requireCreds := translateEndpoint(ep)
	return requireCreds
}

// Interpret endpoint parses an endpoint of the form
// (http|https)://<host>*|(unix|unixs)://<path>)
// and returns low-level address (supported by 'net') to connect to,
// and a server name used for x509 certificate matching.
func Interpret(ep string) (address string, serverName string) {
	addr, serverName, _ := translateEndpoint(ep)
	return addr, serverName
}
