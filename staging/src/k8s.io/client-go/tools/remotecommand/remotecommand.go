/*
Copyright 2015 The Kubernetes Authors.

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
	"context"
	"io"
	"net/http"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"
)

// StreamOptions holds information pertaining to the current streaming session:
// input/output streams, if the client is requesting a TTY, and a terminal size queue to
// support terminal resizing.
type StreamOptions struct {
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	Tty               bool
	TerminalSizeQueue TerminalSizeQueue
}

// Executor is an interface for transporting shell-style streams.
type Executor interface {
	// Deprecated: use StreamWithContext instead to avoid possible resource leaks.
	// See https://github.com/kubernetes/kubernetes/pull/103177 for details.
	Stream(options StreamOptions) error

	// StreamWithContext initiates the transport of the standard shell streams. It will
	// transport any non-nil stream to a remote system, and return an error if a problem
	// occurs. If tty is set, the stderr stream is not used (raw TTY manages stdout and
	// stderr over the stdout stream).
	// The context controls the entire lifetime of stream execution.
	StreamWithContext(ctx context.Context, options StreamOptions) error
}

type streamCreator interface {
	CreateStream(headers http.Header) (httpstream.Stream, error)
}

type streamProtocolHandler interface {
	stream(logger klog.Logger, conn streamCreator) error
}
