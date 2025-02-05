/*
Copyright 2016 The Kubernetes Authors.

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

package remotecommand

import (
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// errorStreamDecoder interprets the data on the error channel and creates a go error object from it.
type errorStreamDecoder interface {
	decode(message []byte) error
}

// watchErrorStream watches the errorStream for remote command error data,
// decodes it with the given errorStreamDecoder, sends the decoded error (or nil if the remote
// command exited successfully) to the returned error channel, and closes it.
// This function returns immediately.
func watchErrorStream(logger klog.Logger, errorStream io.Reader, d errorStreamDecoder) chan error {
	errorChan := make(chan error)

	go func() {
		defer runtime.HandleCrashWithLogger(logger)

		message, err := io.ReadAll(errorStream)
		switch {
		case err != nil && err != io.EOF:
			errorChan <- fmt.Errorf("error reading from error stream: %w", err)
		case len(message) > 0:
			errorChan <- d.decode(message)
		default:
			errorChan <- nil
		}
		close(errorChan)
	}()

	return errorChan
}
