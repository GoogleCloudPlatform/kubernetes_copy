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
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// streamProtocolV2 implements version 2 of the streaming protocol for attach
// and exec. The original streaming protocol was metav1. As a result, this
// version is referred to as version 2, even though it is the first actual
// numbered version.
type streamProtocolV2 struct {
	StreamOptions

	errorStream  io.Reader
	remoteStdin  io.ReadWriteCloser
	remoteStdout io.Reader
	remoteStderr io.Reader
}

var _ streamProtocolHandler = &streamProtocolV2{}

func newStreamProtocolV2(options StreamOptions) streamProtocolHandler {
	return &streamProtocolV2{
		StreamOptions: options,
	}
}

func (p *streamProtocolV2) createStreams(conn streamCreator) error {
	var err error
	headers := http.Header{}

	// set up error stream
	headers.Set(v1.StreamType, v1.StreamTypeError)
	p.errorStream, err = conn.CreateStream(headers)
	if err != nil {
		return err
	}

	// set up stdin stream
	if p.Stdin != nil {
		headers.Set(v1.StreamType, v1.StreamTypeStdin)
		p.remoteStdin, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
	}

	// set up stdout stream
	if p.Stdout != nil {
		headers.Set(v1.StreamType, v1.StreamTypeStdout)
		p.remoteStdout, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
	}

	// set up stderr stream
	if p.Stderr != nil && !p.Tty {
		headers.Set(v1.StreamType, v1.StreamTypeStderr)
		p.remoteStderr, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *streamProtocolV2) copyStdin(logger klog.Logger) {
	if p.Stdin != nil {
		var once sync.Once

		// copy from client's stdin to container's stdin
		go func() {
			defer runtime.HandleCrashWithLogger(logger)

			// if p.stdin is noninteractive, p.g. `echo abc | kubectl exec -i <pod> -- cat`, make sure
			// we close remoteStdin as soon as the copy from p.stdin to remoteStdin finishes. Otherwise
			// the executed command will remain running.
			defer once.Do(func() { p.remoteStdin.Close() })

			if _, err := io.Copy(p.remoteStdin, readerWrapper{p.Stdin}); err != nil {
				runtime.HandleErrorWithLogger(logger, err, "Copying stdin failed")
			}
		}()

		// read from remoteStdin until the stream is closed. this is essential to
		// be able to exit interactive sessions cleanly and not leak goroutines or
		// hang the client's terminal.
		//
		// TODO we aren't using go-dockerclient any more; revisit this to determine if it's still
		// required by engine-api.
		//
		// go-dockerclient's current hijack implementation
		// (https://github.com/fsouza/go-dockerclient/blob/89f3d56d93788dfe85f864a44f85d9738fca0670/client.go#L564)
		// waits for all three streams (stdin/stdout/stderr) to finish copying
		// before returning. When hijack finishes copying stdout/stderr, it calls
		// Close() on its side of remoteStdin, which allows this copy to complete.
		// When that happens, we must Close() on our side of remoteStdin, to
		// allow the copy in hijack to complete, and hijack to return.
		go func() {
			defer runtime.HandleCrashWithLogger(logger)
			defer once.Do(func() { p.remoteStdin.Close() })

			// this "copy" doesn't actually read anything - it's just here to wait for
			// the server to close remoteStdin.
			if _, err := io.Copy(io.Discard, p.remoteStdin); err != nil {
				runtime.HandleErrorWithLogger(logger, err, "Waiting for server to close stdin failed")
			}
		}()
	}
}

func (p *streamProtocolV2) copyStdout(logger klog.Logger, wg *sync.WaitGroup) {
	if p.Stdout == nil {
		return
	}

	wg.Add(1)
	go func() {
		defer runtime.HandleCrashWithLogger(logger)
		defer wg.Done()
		// make sure, packet in queue can be consumed.
		// block in queue may lead to deadlock in conn.server
		// issue: https://github.com/kubernetes/kubernetes/issues/96339
		defer io.Copy(io.Discard, p.remoteStdout)

		if _, err := io.Copy(p.Stdout, p.remoteStdout); err != nil {
			runtime.HandleErrorWithLogger(logger, err, "Copying stdout failed")
		}
	}()
}

func (p *streamProtocolV2) copyStderr(logger klog.Logger, wg *sync.WaitGroup) {
	if p.Stderr == nil || p.Tty {
		return
	}

	wg.Add(1)
	go func() {
		defer runtime.HandleCrashWithLogger(logger)
		defer wg.Done()
		defer io.Copy(io.Discard, p.remoteStderr)

		if _, err := io.Copy(p.Stderr, p.remoteStderr); err != nil {
			runtime.HandleErrorWithLogger(logger, err, "Copying stderr failed")
		}
	}()
}

func (p *streamProtocolV2) stream(logger klog.Logger, conn streamCreator) error {
	if err := p.createStreams(conn); err != nil {
		return err
	}

	// now that all the streams have been created, proceed with reading & copying

	errorChan := watchErrorStream(logger, p.errorStream, &errorDecoderV2{})

	p.copyStdin(logger)

	var wg sync.WaitGroup
	p.copyStdout(logger, &wg)
	p.copyStderr(logger, &wg)

	// we're waiting for stdout/stderr to finish copying
	wg.Wait()

	// waits for errorStream to finish reading with an error or nil
	return <-errorChan
}

// errorDecoderV2 interprets the error channel data as plain text.
type errorDecoderV2 struct{}

func (d *errorDecoderV2) decode(message []byte) error {
	return fmt.Errorf("error executing remote command: %s", message)
}
