/*
Copyright 2024 The Kubernetes Authors.

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

package nfacct

// Counter represents a nfacct accounting object.
type Counter struct {
	Name    string
	Packets uint64
	Bytes   uint64
}

// Interface is an injectable interface for running nfacct commands.
type Interface interface {
	// Ensure checks the existence of a nfacct counter with the provided name and creates it if absent.
	Ensure(name string) error
	// Add creates a nfacct counter with the given name, returning an error if it already exists.
	Add(name string) error
	// Get retrieves the nfacct counter with the specified name, returning an error if it doesn't exist.
	Get(name string) (*Counter, error)
	// List retrieves nfacct counters, it could receive all counters or a subset of them with an unix.EINTR error.
	List() ([]*Counter, error)
}
