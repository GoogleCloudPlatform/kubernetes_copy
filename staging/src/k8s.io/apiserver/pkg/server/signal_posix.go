//go:build !windows
// +build !windows

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

package server

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
var signals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGUSR1}
var handlers chan os.Signal

func SetupSignalContextV2(name string) context.Context {
	close(onlyOneSignalHandler) // panics when called twice
	handlers = make(chan os.Signal, 3)
	ctx, cancel := context.WithCancel(context.Background())
	signal.Notify(handlers, signals...)
	go func() {
		for s := range handlers {
			klog.Infof("received signal %v", s)
			if s == syscall.SIGUSR1 {
				dumpStacks(name)
			} else {
				cancel()
				<-handlers
				os.Exit(1) // second signal. Exit directly.
			}
		}
	}()
	return ctx
}
