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

package storage

import (
	"github.com/prometheus/client_golang/prometheus"
)

var quotaBackendBytes = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "etcd",
	Subsystem: "server",
	Name:      "quota_backend_bytes",
	Help:      "Current backend storage quota size in bytes.",
})

func init() {
	prometheus.MustRegister(quotaBackendBytes)
}
