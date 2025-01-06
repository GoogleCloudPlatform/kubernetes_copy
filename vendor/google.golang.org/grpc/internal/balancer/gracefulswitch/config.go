/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package gracefulswitch

import (
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/serviceconfig"
)

type lbConfig struct {
	serviceconfig.LoadBalancingConfig

	childBuilder balancer.Builder
	childConfig  serviceconfig.LoadBalancingConfig
}

// ChildName returns the name of the child balancer of the gracefulswitch
// Balancer.
func ChildName(l serviceconfig.LoadBalancingConfig) string {
	return l.(*lbConfig).childBuilder.Name()
}

// ParseConfig parses a child config list and returns a LB config for the
// gracefulswitch Balancer.
//
// cfg is expected to be a json.RawMessage containing a JSON array of LB policy
// names + configs as the format of the "loadBalancingConfig" field in
// ServiceConfig.  It returns a type that should be passed to
// UpdateClientConnState in the BalancerConfig field.
func ParseConfig(cfg json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
	var lbCfg []map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &lbCfg); err != nil {
		return nil, err
	}
	for i, e := range lbCfg {
		if len(e) != 1 {
			return nil, fmt.Errorf("expected a JSON struct with one entry; received entry %v at index %d", e, i)
		}

		var name string
		var jsonCfg json.RawMessage
		for name, jsonCfg = range e {
		}

		builder := balancer.Get(name)
		if builder == nil {
			// Skip unregistered balancer names.
			continue
		}

		parser, ok := builder.(balancer.ConfigParser)
		if !ok {
			// This is a valid child with no config.
			return &lbConfig{childBuilder: builder}, nil
		}

		cfg, err := parser.ParseConfig(jsonCfg)
		if err != nil {
			return nil, fmt.Errorf("error parsing config for policy %q: %v", name, err)
		}
		return &lbConfig{childBuilder: builder, childConfig: cfg}, nil
	}

	return nil, fmt.Errorf("no supported policies found in config: %v", string(cfg))
}
