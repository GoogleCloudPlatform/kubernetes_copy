/*
Copyright 2018 The Kubernetes Authors.

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

package options

import (
	"github.com/spf13/pflag"
	componentbaseconfig "k8s.io/component-base/config"
)

// DeprecatedOptions contains deprecated options and their flags.
// TODO remove these fields once the deprecated flags are removed.
type DeprecatedOptions struct {
	componentbaseconfig.DebuggingConfiguration
	componentbaseconfig.ClientConnectionConfiguration
}

// AddFlags adds flags for the deprecated options.
func (o *DeprecatedOptions) AddFlags(fs *pflag.FlagSet) {
	if o == nil {
		return
	}

	fs.BoolVar(&o.EnableProfiling, "profiling", true, "DEPRECATED: enable profiling via web interface host:port/debug/pprof/. This parameter is ignored if a config file is specified in --config.")
	fs.BoolVar(&o.EnableContentionProfiling, "contention-profiling", true, "DEPRECATED: enable block profiling, if profiling is enabled. This parameter is ignored if a config file is specified in --config.")
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "DEPRECATED: path to kubeconfig file with authorization and master location information. This parameter is ignored if a config file is specified in --config.")
	fs.StringVar(&o.ContentType, "kube-api-content-type", "application/vnd.kubernetes.protobuf", "DEPRECATED: content type of requests sent to apiserver. This parameter is ignored if a config file is specified in --config.")
	fs.Float32Var(&o.QPS, "kube-api-qps", 50.0, "DEPRECATED: QPS to use while talking with kubernetes apiserver. This parameter is ignored if a config file is specified in --config.")
	fs.Int32Var(&o.Burst, "kube-api-burst", 100, "DEPRECATED: burst to use while talking with kubernetes apiserver. This parameter is ignored if a config file is specified in --config.")
}
