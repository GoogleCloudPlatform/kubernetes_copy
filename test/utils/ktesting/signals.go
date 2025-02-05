/*
Copyright 2023 The Kubernetes Authors.

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

package ktesting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
)

var (
	interruptCtx context.Context

	defaultProgressReporter = new(progressReporter)
	defaultSignalChannel    chan os.Signal
)

const ginkgoSpecContextKey = "GINKGO_SPEC_CONTEXT"

type ginkgoReporter interface {
	AttachProgressReporter(reporter func() string) func()
}

func init() {
	// os.Stderr gets redirected by "go test". "go test -v" has to be
	// used to see the output while a test runs. Opening /dev/tty avoids
	// the redirection, but isn't always possible.
	console, err := os.OpenFile("/dev/tty", os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		console = os.Stderr
		fmt.Fprintf(console, "Cannot use /dev/tty: %v", err)
	}

	// Setting up signals is intentionally done in an init function because
	// then importing ktesting in a unit or integration test is sufficient
	// to activate the signal behavior.
	signalCtx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	cancelCtx, cancel := context.WithCancelCause(context.Background())
	go func() {
		<-signalCtx.Done()
		fmt.Fprintf(console, "\n\nINFO: canceling test context(s): received interrupt signal\n\n")
		cancel(errors.New("received interrupt signal"))
	}()

	// This reimplements the contract between Ginkgo and Gomega for progress reporting.
	// When using Ginkgo contexts, Ginkgo will implement it. This here is for "go test"
	// in combination with Gomega.
	//
	// nolint:staticcheck // It complains about using a plain string. This can only be fixed
	// by Ginkgo and Gomega formalizing this interface and define a type (somewhere...
	// probably cannot be in either Ginkgo or Gomega).
	interruptCtx = context.WithValue(cancelCtx, ginkgoSpecContextKey, defaultProgressReporter)

	defaultSignalChannel = make(chan os.Signal, 1)
	// progressSignals will be empty on Windows.
	if len(progressSignals) > 0 {
		signal.Notify(defaultSignalChannel, progressSignals...)
	}

	defaultProgressReporter.setOutput(console)
	go defaultProgressReporter.run(interruptCtx, defaultSignalChannel)
}

type progressReporter struct {
	mutex           sync.Mutex
	reporterCounter int64
	reporters       map[int64]func() string
	out             io.Writer
}

var _ ginkgoReporter = &progressReporter{}

func (p *progressReporter) setOutput(out io.Writer) io.Writer {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	oldOut := p.out
	p.out = out
	return oldOut
}

// AttachProgressReporter implements Gomega's contextWithAttachProgressReporter.
func (p *progressReporter) AttachProgressReporter(reporter func() string) func() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// TODO (?): identify the caller and record that for dumpProgress.
	p.reporterCounter++
	id := p.reporterCounter
	if p.reporters == nil {
		p.reporters = make(map[int64]func() string)
	}
	p.reporters[id] = reporter
	return func() {
		p.detachProgressReporter(id)
	}
}

func (p *progressReporter) detachProgressReporter(id int64) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	delete(p.reporters, id)
}

func (p *progressReporter) run(ctx context.Context, progressSignalChannel chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-progressSignalChannel:
			p.dumpProgress()
		}
	}
}

// dumpProgress is less useful than the Ginkgo progress report. We can't fix
// that we don't know which tests are currently running and instead have to
// rely on "go test -v" for that.
//
// But perhaps dumping goroutines and their callstacks is useful anyway?  TODO:
// look at how Ginkgo does it and replicate some of it.
func (p *progressReporter) dumpProgress() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	var buffer strings.Builder
	buffer.WriteString("You requested a progress report.\n")
	if len(p.reporters) == 0 {
		buffer.WriteString("Currently there is no information about test progress available.\n")
	}
	for _, reporter := range p.reporters {
		report := reporter()
		buffer.WriteRune('\n')
		buffer.WriteString(report)
		if !strings.HasSuffix(report, "\n") {
			buffer.WriteRune('\n')
		}
	}

	_, _ = p.out.Write([]byte(buffer.String()))
}
