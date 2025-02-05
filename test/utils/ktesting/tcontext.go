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
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/gomega"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/test/utils/format"
	"k8s.io/kubernetes/test/utils/ktesting/initoption"
	"k8s.io/kubernetes/test/utils/ktesting/internal"
)

// Underlier is the additional interface implemented by the per-test LogSink
// behind [TContext.Logger]. Together with [initoption.BufferLogs] it can be
// used to capture log output in memory to check it in tests.
type Underlier = ktesting.Underlier

// CleanupGracePeriod is the time that a [TContext] gets canceled before the
// deadline of its underlying test suite (usually determined via "go test
// -timeout"). This gives the running test(s) time to fail with an informative
// timeout error. After that, all cleanup callbacks then have the remaining
// time to complete before the test binary is killed.
//
// For this to work, each blocking calls in a test must respect the
// cancellation of the [TContext].
//
// When using Ginkgo to manage the test suite and running tests, the
// CleanupGracePeriod is ignored because Ginkgo itself manages timeouts.
const CleanupGracePeriod = 5 * time.Second

// TContext combines [context.Context], [TB] and some additional
// methods.  Log output is associated with the current test. Errors ([Error],
// [Errorf]) are recorded with "ERROR" as prefix, fatal errors ([Fatal],
// [Fatalf]) with "FATAL ERROR".
//
// TContext provides features offered by Ginkgo also when using normal Go [testing]:
//   - The context contains a deadline that expires soon enough before
//     the overall timeout that cleanup code can still run.
//   - Cleanup callbacks can get their own, separate contexts when
//     registered via [CleanupCtx].
//   - CTRL-C aborts, prints a progress report, and then cleans up
//     before terminating.
//   - SIGUSR1 prints a progress report without aborting.
//
// Progress reporting is more informative when doing polling with
// [gomega.Eventually] and [gomega.Consistently]. Without that, it
// can only report which tests are active.
type TContext interface {
	context.Context
	TB

	// Cancel can be invoked to cancel the context before the test is completed.
	// Tests which use the context to control goroutines and then wait for
	// termination of those goroutines must call Cancel to avoid a deadlock.
	//
	// The cause, if non-empty, is turned into an error which is equivalend
	// to context.Canceled. context.Cause will return that error for the
	// context.
	Cancel(cause string)

	// Cleanup registers a callback that will get invoked when the test
	// has finished. Callbacks get invoked in last-in-first-out order (LIFO).
	//
	// Beware of context cancellation. The following cleanup code
	// will use a canceled context, which is not desirable:
	//
	//    tCtx.Cleanup(func() { /* do something with tCtx */ })
	//    tCtx.Cancel()
	//
	// A safer way to run cleanup code is:
	//
	//    tCtx.CleanupCtx(func (tCtx ktesting.TContext) { /* do something with cleanup tCtx */ })
	Cleanup(func())

	// CleanupCtx is an alternative for Cleanup. The callback is passed a
	// new TContext with the same logger and clients as the one CleanupCtx
	// was invoked for.
	CleanupCtx(func(TContext))

	// Expect wraps [gomega.Expect] such that a failure will be reported via
	// [TContext.Fatal]. As with [gomega.Expect], additional values
	// may get passed. Those values then all must be nil for the assertion
	// to pass. This can be used with functions which return a value
	// plus error:
	//
	//     myAmazingThing := func(int, error) { ...}
	//     tCtx.Expect(myAmazingThing()).Should(gomega.Equal(1))
	Expect(actual interface{}, extra ...interface{}) gomega.Assertion

	// ExpectNoError asserts that no error has occurred.
	//
	// As in [gomega], the optional explanation can be:
	//   - a [fmt.Sprintf] format string plus its argument
	//   - a function returning a string, which will be called
	//     lazy to construct the explanation if needed
	//
	// If an explanation is provided, then it replaces the default "Unexpected
	// error" in the failure message. It's combined with additional details by
	// adding a colon at the end, as when wrapping an error. Therefore it should
	// not end with a punctuation mark or line break.
	//
	// Using ExpectNoError instead of the corresponding Gomega or testify
	// assertions has the advantage that the failure message is short (good for
	// aggregation in https://go.k8s.io/triage) with more details captured in the
	// test log output (good when investigating one particular failure).
	ExpectNoError(err error, explain ...interface{})

	// Logger returns a logger for the current test. This is a shortcut
	// for calling klog.FromContext.
	//
	// Output emitted via this logger and the TB interface (like Logf)
	// is formatted consistently. The TB interface generates a single
	// message string, while Logger enables structured logging and can
	// be passed down into code which expects a logger.
	//
	// To skip intermediate helper functions during stack unwinding,
	// TB.Helper can be called in those functions.
	Logger() klog.Logger

	// TB returns the underlying TB. This can be used to "break the glass"
	// and cast back into a testing.T or TB. Calling TB is necessary
	// because TContext wraps the underlying TB.
	TB() TB

	// RESTConfig returns a config for a rest client with the UserAgent set
	// to include the current test name or nil if not available. Several
	// typed clients using this config are available through [Client],
	// [Dynamic], [APIExtensions].
	RESTConfig() *rest.Config

	RESTMapper() *restmapper.DeferredDiscoveryRESTMapper
	Client() clientset.Interface
	Dynamic() dynamic.Interface
	APIExtensions() apiextensions.Interface

	// The following methods must be implemented by every implementation
	// of TContext to ensure that the leaf TContext is used, not some
	// embedded TContext:
	// - CleanupCtx
	// - Expect
	// - ExpectNoError
	// - Logger
	//
	// Usually these methods would be stand-alone functions with a TContext
	// parameter. Offering them as methods simplifies the test code.
}

// TB is the interface common to [testing.T], [testing.B], [testing.F] and
// [github.com/onsi/ginkgo/v2]. In contrast to [testing.TB], it can be
// implemented also outside of the testing package.
type TB interface {
	Cleanup(func())
	Error(args ...any)
	Errorf(format string, args ...any)
	Fail()
	FailNow()
	Failed() bool
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Helper()
	Log(args ...any)
	Logf(format string, args ...any)
	Name() string
	Setenv(key, value string)
	Skip(args ...any)
	SkipNow()
	Skipf(format string, args ...any)
	Skipped() bool
	TempDir() string
}

// ContextTB adds support for cleanup callbacks with explicit context
// parameter. This is used when integrating with Ginkgo: then CleanupCtx
// gets implemented via ginkgo.DeferCleanup.
type ContextTB interface {
	TB
	CleanupCtx(func(ctx context.Context))
}

// Init can be called in a unit or integration test to create
// a test context which:
// - has a per-test logger with verbosity derived from the -v command line flag
// - gets canceled when the test finishes (via [TB.Cleanup])
//
// Note that the test context supports the interfaces of [TB] and
// [context.Context] and thus can be used like one of those where needed.
// It also has additional methods for retrieving the logger and canceling
// the context early, which can be useful in tests which want to wait
// for goroutines to terminate after cancellation.
//
// If the [TB] implementation also implements [ContextTB], then
// [TContext.CleanupCtx] uses [ContextTB.CleanupCtx] and uses
// the context passed into that callback. This can be used to let
// Ginkgo create a fresh context for cleanup code.
//
// Can be called more than once per test to get different contexts with
// independent cancellation. The default behavior describe above can be
// modified via optional functional options defined in [initoption].
func Init(tb TB, opts ...InitOption) TContext {
	tb.Helper()

	c := internal.InitConfig{
		PerTestOutput: true,
	}
	for _, opt := range opts {
		opt(&c)
	}

	// We don't need a Deadline implementation, testing.B doesn't have it.
	// But if we have one, we'll use it to set a timeout shortly before
	// the deadline. This needs to come before we wrap tb.
	deadlineTB, deadlineOK := tb.(interface {
		Deadline() (time.Time, bool)
	})

	// If testing has been interrupted, immediately fail all new tests.
	// Giving them a canceled context also works, but only if the tests
	// actually check it.
	ctx := interruptCtx
	if ctx.Err() != nil {
		tb.Fatalf("testing has been interrupted: %v", context.Cause(ctx))
	}

	if c.PerTestOutput {
		config := ktesting.NewConfig(
			ktesting.AnyToString(func(v interface{}) string {
				// For basic types where the string
				// representation is "obvious" we use
				// fmt.Sprintf because format.Object always
				// adds a <"type"> prefix, which is too long
				// for simple values.
				switch v := v.(type) {
				case int, int32, int64, uint, uint32, uint64, float32, float64, bool:
					return fmt.Sprintf("%v", v)
				case string:
					return v
				default:
					return strings.TrimSpace(format.Object(v, 1))
				}
			}),
			ktesting.VerbosityFlagName("v"),
			ktesting.VModuleFlagName("vmodule"),
			ktesting.BufferLogs(c.BufferLogs),
		)

		// Copy klog settings instead of making the ktesting logger
		// configurable directly.
		var fs flag.FlagSet
		config.AddFlags(&fs)
		for _, name := range []string{"v", "vmodule"} {
			from := flag.CommandLine.Lookup(name)
			to := fs.Lookup(name)
			if err := to.Value.Set(from.Value.String()); err != nil {
				panic(err)
			}
		}

		// Ensure consistent logging: this klog.Logger writes to tb, adding the
		// date/time header, and our own wrapper emulates that behavior for
		// Log/Logf/...
		logger := ktesting.NewLogger(tb, config)
		ctx = klog.NewContext(interruptCtx, logger)

		tb = withKlogHeader(tb)
	}

	if deadlineOK {
		if deadline, ok := deadlineTB.Deadline(); ok {
			timeLeft := time.Until(deadline)
			timeLeft -= CleanupGracePeriod
			ctx, cancel := withTimeout(ctx, tb, timeLeft, fmt.Sprintf("test suite deadline (%s) is close, need to clean up before the %s cleanup grace period", deadline.Truncate(time.Second), CleanupGracePeriod))
			tCtx := tContext{
				Context:   ctx,
				testingTB: testingTB{TB: tb},
				cancel:    cancel,
			}
			return tCtx
		}
	}
	return WithCancel(InitCtx(ctx, tb))
}

type InitOption = initoption.InitOption

// InitCtx is a variant of [Init] which uses an already existing context and
// whatever logger and timeouts are stored there.
// Functional options are part of the API, but currently
// there are none which have an effect.
func InitCtx(ctx context.Context, tb TB, _ ...InitOption) TContext {
	tCtx := tContext{
		Context:   ctx,
		testingTB: testingTB{TB: tb},
	}
	return tCtx
}

// WithTB constructs a new TContext with a different TB instance.
// This can be used to set up some of the context, in particular
// clients, in the root test and then run sub-tests:
//
//	func TestSomething(t *testing.T) {
//	   tCtx := ktesting.Init(t)
//	   ...
//	   tCtx = ktesting.WithRESTConfig(tCtx, config)
//
//	   t.Run("sub", func (t *testing.T) {
//	       tCtx := ktesting.WithTB(tCtx, t)
//	       ...
//	   })
//
// WithTB sets up cancellation for the sub-test.
func WithTB(parentCtx TContext, tb TB) TContext {
	tCtx := InitCtx(parentCtx, tb)
	tCtx = WithCancel(tCtx)
	tCtx = WithClients(tCtx,
		parentCtx.RESTConfig(),
		parentCtx.RESTMapper(),
		parentCtx.Client(),
		parentCtx.Dynamic(),
		parentCtx.APIExtensions(),
	)
	return tCtx
}

// WithContext constructs a new TContext with a different Context instance.
// This can be used in callbacks which receive a Context, for example
// from Gomega:
//
//	gomega.Eventually(tCtx, func(ctx context.Context) {
//	   tCtx := ktesting.WithContext(tCtx, ctx)
//	   ...
//
// This is important because the Context in the callback could have
// a different deadline than in the parent TContext.
func WithContext(parentCtx TContext, ctx context.Context) TContext {
	tCtx := InitCtx(ctx, parentCtx.TB())
	tCtx = WithClients(tCtx,
		parentCtx.RESTConfig(),
		parentCtx.RESTMapper(),
		parentCtx.Client(),
		parentCtx.Dynamic(),
		parentCtx.APIExtensions(),
	)
	return tCtx
}

// WithValue wraps context.WithValue such that the result is again a TContext.
func WithValue(parentCtx TContext, key, val any) TContext {
	ctx := context.WithValue(parentCtx, key, val)
	return WithContext(parentCtx, ctx)
}

type tContext struct {
	context.Context
	testingTB
	cancel func(cause string)
}

// testingTB is needed to avoid a name conflict
// between field and method in tContext.
type testingTB struct {
	TB
}

func (tCtx tContext) Cancel(cause string) {
	if tCtx.cancel != nil {
		tCtx.cancel(cause)
	}
}

func (tCtx tContext) CleanupCtx(cb func(TContext)) {
	tCtx.Helper()
	cleanupCtx(tCtx, cb)
}

func (tCtx tContext) Expect(actual interface{}, extra ...interface{}) gomega.Assertion {
	tCtx.Helper()
	return expect(tCtx, actual, extra...)
}

func (tCtx tContext) ExpectNoError(err error, explain ...interface{}) {
	tCtx.Helper()
	expectNoError(tCtx, err, explain...)
}

func cleanupCtx(tCtx TContext, cb func(TContext)) {
	tCtx.Helper()

	if tb, ok := tCtx.TB().(ContextTB); ok {
		// Use context from base TB (most likely Ginkgo).
		tb.CleanupCtx(func(ctx context.Context) {
			tCtx := WithContext(tCtx, ctx)
			cb(tCtx)
		})
		return
	}

	tCtx.Cleanup(func() {
		// Use new context. This is the code path for "go test". The
		// context then has *no* deadline. In the code path above for
		// Ginkgo, Ginkgo is more sophisticated and also applies
		// timeouts to cleanup calls which accept a context.
		childCtx := WithContext(tCtx, context.WithoutCancel(tCtx))
		cb(childCtx)
	})
}

func (tCtx tContext) Logger() klog.Logger {
	return klog.FromContext(tCtx)
}

func (tCtx tContext) Error(args ...any) {
	tCtx.Helper()
	args = append([]any{"ERROR:"}, args...)
	tCtx.testingTB.Error(args...)
}

func (tCtx tContext) Errorf(format string, args ...any) {
	tCtx.Helper()
	error := fmt.Sprintf(format, args...)
	error = "ERROR: " + error
	tCtx.testingTB.Error(error)
}

func (tCtx tContext) Fatal(args ...any) {
	tCtx.Helper()
	args = append([]any{"FATAL ERROR:"}, args...)
	tCtx.testingTB.Fatal(args...)
}

func (tCtx tContext) Fatalf(format string, args ...any) {
	tCtx.Helper()
	error := fmt.Sprintf(format, args...)
	error = "FATAL ERROR: " + error
	tCtx.testingTB.Fatal(error)
}

func (tCtx tContext) TB() TB {
	// Might have to unwrap twice, depending on how
	// this tContext was constructed.
	tb := tCtx.testingTB.TB
	if k, ok := tb.(klogTB); ok {
		return k.TB
	}
	return tb
}

func (tCtx tContext) RESTConfig() *rest.Config {
	return nil
}

func (tCtx tContext) RESTMapper() *restmapper.DeferredDiscoveryRESTMapper {
	return nil
}

func (tCtx tContext) Client() clientset.Interface {
	return nil
}

func (tCtx tContext) Dynamic() dynamic.Interface {
	return nil
}

func (tCtx tContext) APIExtensions() apiextensions.Interface {
	return nil
}
