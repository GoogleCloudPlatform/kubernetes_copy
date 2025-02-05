/*
Copyright 2017 The Kubernetes Authors.

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
	"fmt"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/server"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

type FeatureOptions struct {
	EnableProfiling           bool
	DebugSocketPath           string
	EnableContentionProfiling bool
	EnablePriorityAndFairness bool
}

func NewFeatureOptions() *FeatureOptions {
	defaults := server.NewConfig(serializer.CodecFactory{})

	return &FeatureOptions{
		EnableProfiling:           defaults.EnableProfiling,
		DebugSocketPath:           defaults.DebugSocketPath,
		EnableContentionProfiling: defaults.EnableContentionProfiling,
		// Disable PriorityAndFairness by default since it only makes sense to have
		// on by default for the kube-apiserver and not for the aggregated
		// apiservers that are reusing this library.
		EnablePriorityAndFairness: false,
	}
}

func (o *FeatureOptions) AddFlags(fs *pflag.FlagSet) {
	if o == nil {
		return
	}

	fs.BoolVar(&o.EnableProfiling, "profiling", o.EnableProfiling,
		"Enable profiling via web interface host:port/debug/pprof/")
	fs.BoolVar(&o.EnableContentionProfiling, "contention-profiling", o.EnableContentionProfiling,
		"Enable block profiling, if profiling is enabled")
	fs.StringVar(&o.DebugSocketPath, "debug-socket-path", o.DebugSocketPath,
		"Use an unprotected (no authn/authz) unix-domain socket for profiling with the given path")
	fs.BoolVar(&o.EnablePriorityAndFairness, "enable-priority-and-fairness", o.EnablePriorityAndFairness, ""+
		"If true, replace the max-in-flight handler with an enhanced one that queues and dispatches with priority and fairness")
}

func (o *FeatureOptions) ApplyTo(c *server.Config, clientset kubernetes.Interface, informers informers.SharedInformerFactory) error {
	if o == nil {
		return nil
	}

	c.EnableProfiling = o.EnableProfiling
	c.DebugSocketPath = o.DebugSocketPath
	c.EnableContentionProfiling = o.EnableContentionProfiling

	if o.EnablePriorityAndFairness {
		if clientset == nil {
			return fmt.Errorf("invalid configuration: priority and fairness requires a core Kubernetes client")
		}
		if c.MaxRequestsInFlight+c.MaxMutatingRequestsInFlight <= 0 {
			return fmt.Errorf("invalid configuration: MaxRequestsInFlight=%d and MaxMutatingRequestsInFlight=%d; they must add up to something positive", c.MaxRequestsInFlight, c.MaxMutatingRequestsInFlight)

		}
		c.FlowControl = utilflowcontrol.New(
			informers,
			clientset.FlowcontrolV1(),
			c.MaxRequestsInFlight+c.MaxMutatingRequestsInFlight,
		)
	}

	return nil
}

func (o *FeatureOptions) Validate() []error {
	if o == nil {
		return nil
	}

	errs := []error{}
	return errs
}
