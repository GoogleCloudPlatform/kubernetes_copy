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

package etcdhttp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/server/v3/etcdserver"
)

const (
	versionPath = "/version"
)

func HandleVersion(mux *http.ServeMux, server etcdserver.Server) {
	mux.HandleFunc(versionPath, versionHandler(server, serveVersion))
}

func versionHandler(server etcdserver.Server, fn func(http.ResponseWriter, *http.Request, string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterVersion := server.ClusterVersion()
		storageVersion := server.StorageVersion()
		clusterVersionStr, storageVersionStr := "not_decided", "unknown"
		if clusterVersion != nil {
			clusterVersionStr = clusterVersion.String()
		}
		if storageVersion != nil {
			storageVersionStr = storageVersion.String()
		}
		fn(w, r, clusterVersionStr, storageVersionStr)
	}
}

func serveVersion(w http.ResponseWriter, r *http.Request, clusterV, storageV string) {
	if !allowMethod(w, r, "GET") {
		return
	}
	vs := version.Versions{
		Server:  version.Version,
		Cluster: clusterV,
		Storage: storageV,
	}

	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(&vs)
	if err != nil {
		panic(fmt.Sprintf("cannot marshal versions to json (%v)", err))
	}
	w.Write(b)
}
