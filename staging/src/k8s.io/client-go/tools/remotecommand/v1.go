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
	"fmt"
	"io"
	"net/http"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"
)

// streamProtocolV1 implements the first version of the streaming exec & attach
// protocol. This version has some bugs, such as not being able to detect when
// non-interactive stdin data has ended. See https://issues.k8s.io/13394 and
// https://issues.k8s.io/13395 for more details.
type streamProtocolV1 struct {
	logger klog.Logger
	StreamOptions

	errorStream  httpstream.Stream
	remoteStdin  httpstream.Stream
	remoteStdout httpstream.Stream
	remoteStderr httpstream.Stream
}

var _ streamProtocolHandler = &streamProtocolV1{}

func newStreamProtocolV1(options StreamOptions) streamProtocolHandler {
	return &streamProtocolV1{
		StreamOptions: options,
	}
}

func (p *streamProtocolV1) stream(logger klog.Logger, conn streamCreator) error {
	doneChan := make(chan struct{}, 2)
	errorChan := make(chan error)

	cp := func(s string, dst io.Writer, src io.Reader) {
		p.logger.V(6).Info("Copying", "data", s)
		defer p.logger.V(6).Info("Done copying", "data", s)
		if _, err := io.Copy(dst, src); err != nil && err != io.EOF {
			p.logger.Error(err, "Error copying", "data", s)
		}
		if s == v1.StreamTypeStdout || s == v1.StreamTypeStderr {
			doneChan <- struct{}{}
		}
	}

	// set up all the streams first
	var err error
	headers := http.Header{}
	headers.Set(v1.StreamType, v1.StreamTypeError)
	p.errorStream, err = conn.CreateStream(headers)
	if err != nil {
		return err
	}
	defer p.errorStream.Reset()

	// Create all the streams first, then start the copy goroutines. The server doesn't start its copy
	// goroutines until it's received all of the streams. If the client creates the stdin stream and
	// immediately begins copying stdin data to the server, it's possible to overwhelm and wedge the
	// spdy frame handler in the server so that it is full of unprocessed frames. The frames aren't
	// getting processed because the server hasn't started its copying, and it won't do that until it
	// gets all the streams. By creating all the streams first, we ensure that the server is ready to
	// process data before the client starts sending any. See https://issues.k8s.io/16373 for more info.
	if p.Stdin != nil {
		headers.Set(v1.StreamType, v1.StreamTypeStdin)
		p.remoteStdin, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
		defer p.remoteStdin.Reset()
	}

	if p.Stdout != nil {
		headers.Set(v1.StreamType, v1.StreamTypeStdout)
		p.remoteStdout, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
		defer p.remoteStdout.Reset()
	}

	if p.Stderr != nil && !p.Tty {
		headers.Set(v1.StreamType, v1.StreamTypeStderr)
		p.remoteStderr, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
		defer p.remoteStderr.Reset()
	}

	// now that all the streams have been created, proceed with reading & copying

	// always read from errorStream
	go func() {
		message, err := io.ReadAll(p.errorStream)
		if err != nil && err != io.EOF {
			errorChan <- fmt.Errorf("Error reading from error stream: %s", err)
			return
		}
		if len(message) > 0 {
			errorChan <- fmt.Errorf("Error executing remote command: %s", message)
			return
		}
	}()

	if p.Stdin != nil {
		// TODO this goroutine will never exit cleanly (the io.Copy never unblocks)
		// because stdin is not closed until the process exits. If we try to call
		// stdin.Close(), it returns no error but doesn't unblock the copy. It will
		// exit when the process exits, instead.
		go cp(v1.StreamTypeStdin, p.remoteStdin, readerWrapper{p.Stdin})
	}

	waitCount := 0
	completedStreams := 0

	if p.Stdout != nil {
		waitCount++
		go cp(v1.StreamTypeStdout, p.Stdout, p.remoteStdout)
	}

	if p.Stderr != nil && !p.Tty {
		waitCount++
		go cp(v1.StreamTypeStderr, p.Stderr, p.remoteStderr)
	}

Loop:
	for {
		select {
		case <-doneChan:
			completedStreams++
			if completedStreams == waitCount {
				break Loop
			}
		case err := <-errorChan:
			return err
		}
	}

	return nil
}
