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

package membership

const DefaultMaxLearners = 1

type ClusterOptions struct {
	maxLearners int
}

// ClusterOption are options which can be applied to the raft cluster.
type ClusterOption func(*ClusterOptions)

func newClusterOpts(opts ...ClusterOption) *ClusterOptions {
	clOpts := &ClusterOptions{}
	clOpts.applyOpts(opts)
	return clOpts
}

func (co *ClusterOptions) applyOpts(opts []ClusterOption) {
	for _, opt := range opts {
		opt(co)
	}
}

// WithMaxLearners sets the maximum number of learners that can exist in the cluster membership.
func WithMaxLearners(max int) ClusterOption {
	return func(co *ClusterOptions) {
		co.maxLearners = max
	}
}
