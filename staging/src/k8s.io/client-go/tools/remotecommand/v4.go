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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/client-go/util/exec"
	"k8s.io/klog/v2"
)

// streamProtocolV4 implements version 4 of the streaming protocol for attach
// and exec. This version adds support for exit codes on the error stream through
// the use of metav1.Status instead of plain text messages.
type streamProtocolV4 struct {
	*streamProtocolV3
}

var _ streamProtocolHandler = &streamProtocolV4{}

func newStreamProtocolV4(options StreamOptions) streamProtocolHandler {
	return &streamProtocolV4{
		streamProtocolV3: newStreamProtocolV3(options).(*streamProtocolV3),
	}
}

func (p *streamProtocolV4) createStreams(conn streamCreator) error {
	return p.streamProtocolV3.createStreams(conn)
}

func (p *streamProtocolV4) handleResizes(logger klog.Logger) {
	p.streamProtocolV3.handleResizes(logger)
}

func (p *streamProtocolV4) stream(logger klog.Logger, conn streamCreator) error {
	if err := p.createStreams(conn); err != nil {
		return err
	}

	// now that all the streams have been created, proceed with reading & copying

	errorChan := watchErrorStream(logger, p.errorStream, &errorDecoderV4{})

	p.handleResizes(logger)

	p.copyStdin(logger)

	var wg sync.WaitGroup
	p.copyStdout(logger, &wg)
	p.copyStderr(logger, &wg)

	// we're waiting for stdout/stderr to finish copying
	wg.Wait()

	// waits for errorStream to finish reading with an error or nil
	return <-errorChan
}

// errorDecoderV4 interprets the json-marshaled metav1.Status on the error channel
// and creates an exec.ExitError from it.
type errorDecoderV4 struct{}

func (d *errorDecoderV4) decode(message []byte) error {
	status := metav1.Status{}
	err := json.Unmarshal(message, &status)
	if err != nil {
		return fmt.Errorf("error stream protocol error: %v in %q", err, string(message))
	}
	switch status.Status {
	case metav1.StatusSuccess:
		return nil
	case metav1.StatusFailure:
		if status.Reason == remotecommand.NonZeroExitCodeReason {
			if status.Details == nil {
				return errors.New("error stream protocol error: details must be set")
			}
			for i := range status.Details.Causes {
				c := &status.Details.Causes[i]
				if c.Type != remotecommand.ExitCodeCauseType {
					continue
				}

				rc, err := strconv.ParseUint(c.Message, 10, 8)
				if err != nil {
					return fmt.Errorf("error stream protocol error: invalid exit code value %q", c.Message)
				}
				return exec.CodeExitError{
					Err:  fmt.Errorf("command terminated with exit code %d", rc),
					Code: int(rc),
				}
			}

			return fmt.Errorf("error stream protocol error: no %s cause given", remotecommand.ExitCodeCauseType)
		}
	default:
		return errors.New("error stream protocol error: unknown error")
	}

	return errors.New(status.Message)
}
