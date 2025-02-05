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

package filters

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/apiserver/plugin/pkg/audit/buffered"
	"k8s.io/apiserver/plugin/pkg/audit/log"
	"k8s.io/apiserver/plugin/pkg/audit/webhook"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/retry"
)

type fakeAuditSink struct {
	lock   sync.Mutex
	events []*auditinternal.Event
}

func (s *fakeAuditSink) ProcessEvents(evs ...*auditinternal.Event) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	for _, e := range evs {
		event := e.DeepCopy()
		s.events = append(s.events, event)
	}
	return true
}

func (s *fakeAuditSink) Events() []*auditinternal.Event {
	s.lock.Lock()
	defer s.lock.Unlock()
	return append([]*auditinternal.Event{}, s.events...)
}

func (s *fakeAuditSink) Pop(timeout time.Duration) (*auditinternal.Event, error) {
	var result *auditinternal.Event
	err := wait.Poll(50*time.Millisecond, wait.ForeverTestTimeout, wait.ConditionFunc(func() (done bool, err error) {
		s.lock.Lock()
		defer s.lock.Unlock()
		if len(s.events) == 0 {
			return false, nil
		}
		result = s.events[0]
		s.events = s.events[1:]
		return true, nil
	}))
	return result, err
}

func TestConstructResponseWriter(t *testing.T) {
	inner := &responsewriter.FakeResponseWriter{}
	actual := decorateResponseWriter(context.Background(), inner, nil, nil)
	switch v := actual.(type) {
	case *auditResponseWriter:
	default:
		t.Errorf("Expected auditResponseWriter, got %v", reflect.TypeOf(v))
	}
	if innerGot := actual.(responsewriter.UserProvidedDecorator).Unwrap(); inner != innerGot {
		t.Errorf("Expected the decorator to return the inner http.ResponseWriter object")
	}

	actual = decorateResponseWriter(context.Background(), &responsewriter.FakeResponseWriterFlusherCloseNotifier{}, nil, nil)
	//lint:file-ignore SA1019 Keep supporting deprecated http.CloseNotifier
	if _, ok := actual.(http.CloseNotifier); !ok {
		t.Errorf("Expected http.ResponseWriter to implement http.CloseNotifier")
	}
	if _, ok := actual.(http.Flusher); !ok {
		t.Errorf("Expected the wrapper to implement http.Flusher")
	}
	if _, ok := actual.(http.Hijacker); ok {
		t.Errorf("Expected http.ResponseWriter not to implement http.Hijacker")
	}

	actual = decorateResponseWriter(context.Background(), &responsewriter.FakeResponseWriterFlusherCloseNotifierHijacker{}, nil, nil)
	//lint:file-ignore SA1019 Keep supporting deprecated http.CloseNotifier
	if _, ok := actual.(http.CloseNotifier); !ok {
		t.Errorf("Expected http.ResponseWriter to implement http.CloseNotifier")
	}
	if _, ok := actual.(http.Flusher); !ok {
		t.Errorf("Expected the wrapper to implement http.Flusher")
	}
	if _, ok := actual.(http.Hijacker); !ok {
		t.Errorf("Expected http.ResponseWriter to implement http.Hijacker")
	}
}

func TestDecorateResponseWriterWithoutChannel(t *testing.T) {
	ctx := audit.WithAuditContext(context.Background())
	ac := audit.AuditContextFrom(ctx)
	actual := decorateResponseWriter(ctx, &responsewriter.FakeResponseWriter{}, nil, nil)

	// write status. This will not block because firstEventSentCh is nil
	actual.WriteHeader(42)
	if ac.GetEventResponseStatus() == nil {
		t.Fatalf("Expected ResponseStatus to be non-nil")
	}
	if ac.GetEventResponseStatus().Code != 42 {
		t.Errorf("expected status code 42, got %d", ac.GetEventResponseStatus().Code)
	}
}

func TestDecorateResponseWriterWithImplicitWrite(t *testing.T) {
	ctx := audit.WithAuditContext(context.Background())
	ac := audit.AuditContextFrom(ctx)
	actual := decorateResponseWriter(ctx, &responsewriter.FakeResponseWriter{}, nil, nil)

	// write status. This will not block because firstEventSentCh is nil
	actual.Write([]byte("foo"))
	if ac.GetEventResponseStatus() == nil {
		t.Fatalf("Expected ResponseStatus to be non-nil")
	}
	if ac.GetEventResponseStatus().Code != 200 {
		t.Errorf("expected status code 200, got %d", ac.GetEventResponseStatus().Code)
	}
}

func TestDecorateResponseWriterChannel(t *testing.T) {
	ctx := audit.WithAuditContext(context.Background())
	sink := &fakeAuditSink{}
	actual := decorateResponseWriter(ctx, &responsewriter.FakeResponseWriter{}, sink, nil)

	done := make(chan struct{})
	go func() {
		t.Log("Writing status code 42")
		actual.WriteHeader(42)
		t.Log("Finished writing status code 42")
		close(done)

		actual.Write([]byte("foo"))
	}()

	// sleep some time to give write the possibility to do wrong stuff
	time.Sleep(100 * time.Millisecond)

	t.Log("Waiting for event in the channel")
	ev1, err := sink.Pop(time.Second)
	if err != nil {
		t.Fatal("Timeout waiting for events")
	}
	t.Logf("Seen event with status %v", ev1.ResponseStatus)

	auditContext := audit.AuditContextFrom(ctx)
	auditContext.Invoke(func(ev *auditinternal.Event) bool {
		if !reflect.DeepEqual(ev, ev1) {
			t.Fatalf("ev1 and ev must be equal")
		}
		return true
	})

	<-done
	t.Log("Seen the go routine finished")

	// write again
	_, err = actual.Write([]byte("foo"))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

type fakeHTTPHandler struct{}

func (*fakeHTTPHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(200)
}

func TestAudit(t *testing.T) {
	shortRunningPath := "/api/v1/namespaces/default/pods/foo"
	longRunningPath := "/api/v1/namespaces/default/pods?watch=true"

	delay := 500 * time.Millisecond

	for _, test := range []struct {
		desc       string
		path       string
		verb       string
		auditID    string
		omitStages []auditinternal.Stage
		handler    func(http.ResponseWriter, *http.Request)
		expected   []auditinternal.Event
		respHeader bool
	}{
		// short running requests with read-only verb
		{
			"read-only empty",
			shortRunningPath,
			"GET",
			"",
			nil,
			func(http.ResponseWriter, *http.Request) {},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "get",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "get",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			false,
		},
		{
			"short running with auditID",
			shortRunningPath,
			"GET",
			uuid.New().String(),
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "get",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "get",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			true,
		},
		{
			"read-only panic",
			shortRunningPath,
			"GET",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				panic("kaboom")
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "get",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StagePanic,
					Verb:           "get",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 500},
				},
			},
			false,
		},
		// short running request with non-read-only verb
		{
			"writing empty",
			shortRunningPath,
			"PUT",
			"",
			nil,
			func(http.ResponseWriter, *http.Request) {},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "update",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "update",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			false,
		},
		{
			"writing sleep",
			shortRunningPath,
			"PUT",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
				time.Sleep(delay)
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "update",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "update",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			true,
		},
		{
			"writing 403+write",
			shortRunningPath,
			"PUT",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(403)
				w.Write([]byte("foo"))
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "update",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "update",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 403},
				},
			},
			true,
		},
		{
			"writing panic",
			shortRunningPath,
			"PUT",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				panic("kaboom")
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "update",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StagePanic,
					Verb:           "update",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 500},
				},
			},
			false,
		},
		{
			"writing write+panic",
			shortRunningPath,
			"PUT",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
				panic("kaboom")
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "update",
					RequestURI: shortRunningPath,
				},
				{
					Stage:          auditinternal.StagePanic,
					Verb:           "update",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 500},
				},
			},
			true,
		},
		// long running requests
		{
			"empty longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(http.ResponseWriter, *http.Request) {},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			false,
		},
		{
			"empty longrunning with audit id",
			longRunningPath,
			"GET",
			uuid.New().String(),
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			true,
		},
		{
			"sleep longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(http.ResponseWriter, *http.Request) {
				time.Sleep(delay)
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			false,
		},
		{
			"sleep+403 longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				time.Sleep(delay)
				w.WriteHeader(403)
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 403},
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 403},
				},
			},
			true,
		},
		{
			"write longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			true,
		},
		{
			"403+write longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(403)
				w.Write([]byte("foo"))
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 403},
				},
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 403},
				},
			},
			true,
		},
		{
			"panic longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				panic("kaboom")
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StagePanic,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 500},
				},
			},
			false,
		},
		{
			"write+panic longrunning",
			longRunningPath,
			"GET",
			"",
			nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
				panic("kaboom")
			},
			[]auditinternal.Event{
				{
					Stage:      auditinternal.StageRequestReceived,
					Verb:       "watch",
					RequestURI: longRunningPath,
				},
				{
					Stage:          auditinternal.StageResponseStarted,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
				{
					Stage:          auditinternal.StagePanic,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 500},
				},
			},
			true,
		},
		{
			"omit RequestReceived",
			shortRunningPath,
			"GET",
			"",
			[]auditinternal.Stage{auditinternal.StageRequestReceived},
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
			},
			[]auditinternal.Event{
				{
					Stage:          auditinternal.StageResponseComplete,
					Verb:           "get",
					RequestURI:     shortRunningPath,
					ResponseStatus: &metav1.Status{Code: 200},
				},
			},
			true,
		},
		{
			"emit Panic only",
			longRunningPath,
			"GET",
			"",
			[]auditinternal.Stage{auditinternal.StageRequestReceived, auditinternal.StageResponseStarted, auditinternal.StageResponseComplete},
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("foo"))
				panic("kaboom")
			},
			[]auditinternal.Event{
				{
					Stage:          auditinternal.StagePanic,
					Verb:           "watch",
					RequestURI:     longRunningPath,
					ResponseStatus: &metav1.Status{Code: 500},
				},
			},
			true,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			sink := &fakeAuditSink{}
			fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, test.omitStages)
			handler := WithAudit(http.HandlerFunc(test.handler), sink, fakeRuleEvaluator, func(r *http.Request, ri *request.RequestInfo) bool {
				// simplified long-running check
				return ri.Verb == "watch"
			})
			handler = WithAuditInit(handler)

			req, _ := http.NewRequest(test.verb, test.path, nil)
			req = withTestContext(req, &user.DefaultInfo{Name: "admin"}, nil)
			if test.auditID != "" {
				req.Header.Add("Audit-ID", test.auditID)
			}
			req.RemoteAddr = "127.0.0.1"

			func() {
				defer func() {
					recover()
				}()
				handler.ServeHTTP(httptest.NewRecorder(), req)
			}()

			events := sink.Events()
			t.Logf("audit log: %v", events)

			if len(events) != len(test.expected) {
				t.Fatalf("Unexpected amount of lines in audit log: %d", len(events))
			}
			expectedID := types.UID("")
			for i, expect := range test.expected {
				event := events[i]
				if event.User.Username != "admin" {
					t.Errorf("Unexpected username: %s", event.User.Username)
				}
				if event.Stage != expect.Stage {
					t.Errorf("Unexpected Stage: %s", event.Stage)
				}
				if event.Verb != expect.Verb {
					t.Errorf("Unexpected Verb: %s", event.Verb)
				}
				if event.RequestURI != expect.RequestURI {
					t.Errorf("Unexpected RequestURI: %s", event.RequestURI)
				}

				if test.auditID != "" && event.AuditID != types.UID(test.auditID) {
					t.Errorf("Unexpected AuditID in audit event, AuditID should be the same with Audit-ID http header")
				}
				if expectedID == types.UID("") {
					expectedID = event.AuditID
				} else if expectedID != event.AuditID {
					t.Errorf("Audits for one request should share the same AuditID, %s differs from %s", expectedID, event.AuditID)
				}
				if event.ObjectRef.APIVersion != "v1" {
					t.Errorf("Unexpected apiVersion: %s", event.ObjectRef.APIVersion)
				}
				if event.ObjectRef.APIGroup != "" {
					t.Errorf("Unexpected apiGroup: %s", event.ObjectRef.APIGroup)
				}
				if (event.ResponseStatus == nil) != (expect.ResponseStatus == nil) {
					t.Errorf("Unexpected ResponseStatus: %v", event.ResponseStatus)
					continue
				}
				if (event.ResponseStatus != nil) && (event.ResponseStatus.Code != expect.ResponseStatus.Code) {
					t.Errorf("Unexpected status code : %d", event.ResponseStatus.Code)
				}
			}
		})
	}
}

func TestAuditNoPanicOnNilUser(t *testing.T) {
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	handler := WithAudit(&fakeHTTPHandler{}, &fakeAuditSink{}, fakeRuleEvaluator, nil)
	req, _ := http.NewRequest("GET", "/api/v1/namespaces/default/pods", nil)
	req = withTestContext(req, nil, nil)
	req.RemoteAddr = "127.0.0.1"
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestAuditLevelNone(t *testing.T) {
	sink := &fakeAuditSink{}
	var handler http.Handler
	handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelNone, nil)
	handler = WithAudit(handler, sink, fakeRuleEvaluator, nil)

	req, _ := http.NewRequest("GET", "/api/v1/namespaces/default/pods", nil)
	req.RemoteAddr = "127.0.0.1"
	req = withTestContext(req, &user.DefaultInfo{Name: "admin"}, nil)

	handler.ServeHTTP(httptest.NewRecorder(), req)
	if len(sink.events) > 0 {
		t.Errorf("Generated events, but should not have: %#v", sink.events)
	}
}

func TestAuditIDHttpHeader(t *testing.T) {
	for _, test := range []struct {
		desc           string
		requestHeader  string
		level          auditinternal.Level
		expectedHeader bool
	}{
		{
			// we always want an audit ID since it can appear in logging/tracing and it is propagated
			// to the aggregated apiserver(s) to improve correlation.
			"http header when there is no audit",
			"",
			auditinternal.LevelNone,
			true,
		},
		{
			// we always want an audit ID since it can appear in logging/tracing and it is propagated
			// to the aggregated apiserver(s) to improve correlation.
			"http header when there is no audit even the request header specified",
			uuid.New().String(),
			auditinternal.LevelNone,
			true,
		},
		{
			"server generated header",
			"",
			auditinternal.LevelRequestResponse,
			true,
		},
		{
			"user provided header",
			uuid.New().String(),
			auditinternal.LevelRequestResponse,
			true,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			sink := &fakeAuditSink{}
			var handler http.Handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(200)
			})
			fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(test.level, nil)
			handler = WithAudit(handler, sink, fakeRuleEvaluator, nil)
			handler = WithAuditInit(handler)

			req, _ := http.NewRequest("GET", "/api/v1/namespaces/default/pods", nil)
			req.RemoteAddr = "127.0.0.1"
			req = withTestContext(req, &user.DefaultInfo{Name: "admin"}, nil)
			if test.requestHeader != "" {
				req.Header.Add("Audit-ID", test.requestHeader)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			resp := w.Result()
			if test.expectedHeader {
				if resp.Header.Get("Audit-ID") == "" {
					t.Errorf("[%s] expected Audit-ID http header returned, but not returned", test.desc)
					return
				}
				// if get Audit-ID returned, it should be the same with the requested one
				if test.requestHeader != "" && resp.Header.Get("Audit-ID") != test.requestHeader {
					t.Errorf("[%s] returned audit http header is not the same with the requested http header, expected: %s, get %s", test.desc, test.requestHeader, resp.Header.Get("Audit-ID"))
				}
			} else {
				if resp.Header.Get("Audit-ID") != "" {
					t.Errorf("[%s] expected no Audit-ID http header returned, but got %s", test.desc, resp.Header.Get("Audit-ID"))
				}
			}
		})
	}
}

func withTestContext(req *http.Request, user user.Info, ae *auditinternal.Event) *http.Request {
	ctx := audit.WithAuditContext(req.Context())
	if user != nil {
		ctx = request.WithUser(ctx, user)
	}
	if ae != nil {
		ac := audit.AuditContextFrom(ctx)
		ac.Invoke(func(ev *auditinternal.Event) bool {
			*ev = *ae
			return true
		})
	}
	if info, err := newTestRequestInfoResolver().NewRequestInfo(req); err == nil {
		ctx = request.WithRequestInfo(ctx, info)
	}
	return req.WithContext(ctx)
}

type fakeAuditFile struct{}

func (s fakeAuditFile) Write(p []byte) (n int, err error) {
	time.Sleep(time.Duration(rand.Int63n(10000)))
	return len(p), nil
}

type fakeAuditWebhookAuditBackend struct {
}

func (f fakeAuditWebhookAuditBackend) RoundTrip(r *http.Request) (*http.Response, error) {
	time.Sleep(time.Duration(rand.Int63n(10000)))
	return &http.Response{
		StatusCode: http.StatusOK,
	}, nil
}

// Test case for https://github.com/kubernetes/kubernetes/issues/120507
// to test for race conditions in audit backends use the following command:
// `go test ./ -race --run=TestAuditBackendRaceCondition -v`
func TestAuditBackendRaceCondition(t *testing.T) {
	defaultFakeLogBackend := log.NewBackend(fakeAuditFile{}, log.FormatJson, auditv1.SchemeGroupVersion)
	testCases := []struct {
		name           string
		backendBuilder func() audit.Backend
	}{
		{
			"log audit backend",
			func() audit.Backend {
				return defaultFakeLogBackend
			},
		},
		{
			"buffered audit backend",
			func() audit.Backend {
				backend := buffered.NewBackend(defaultFakeLogBackend, buffered.BatchConfig{
					BufferSize:     10000,
					MaxBatchSize:   1,
					ThrottleEnable: false,
					AsyncDelegate:  false,
				})
				err := backend.Run(wait.NeverStop)
				if err != nil {
					t.Fatal(err)
				}
				return backend
			},
		},
		{
			name: "webhook audit backend",
			backendBuilder: func() audit.Backend {
				codecFactory := audit.Codecs
				codec := codecFactory.LegacyCodec(auditv1.SchemeGroupVersion)
				negotiatedSerializer := serializer.NegotiatedSerializerWrapper(runtime.SerializerInfo{Serializer: codec})
				client, err := rest.NewRESTClient(&url.URL{}, "/hello", rest.ClientContentConfig{
					ContentType: "application/json",
					Negotiator:  runtime.NewClientNegotiator(negotiatedSerializer, auditv1.SchemeGroupVersion),
				}, flowcontrol.NewTokenBucketRateLimiter(100, 200), &http.Client{Transport: fakeAuditWebhookAuditBackend{}})
				if err != nil {
					t.Fatal(err)
				}
				return webhook.NewDynamicBackend(client, retry.DefaultBackoff)
			},
		},
		{
			"union audit backend",
			func() audit.Backend {
				return audit.Union(defaultFakeLogBackend, defaultFakeLogBackend)
			},
		},
	}
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	longRunningCheck := func(r *http.Request, ri *request.RequestInfo) bool { return false }
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait.ForeverTestTimeout)
			defer cancel()
			for {
				select {
				case <-ctx.Done():
					// finished the test
					return
				default:
				}
				serveStarted := make(chan struct{})
				req, _ := http.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/foo", nil)
				req = withTestContext(req, &user.DefaultInfo{Name: "admin"}, nil)
				backend := tc.backendBuilder()
				go func() {
					<-serveStarted
					for {
						select {
						case <-ctx.Done():
							// finished the test
							backend.Shutdown()
							return
						default:
						}
						audit.AddAuditAnnotations(req.Context(), "a", "b")
					}
				}()
				realHandler := http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
					close(serveStarted)
					// mock some business logic
					time.Sleep(time.Millisecond)
				})
				handler := WithAudit(realHandler, backend, fakeRuleEvaluator, longRunningCheck)
				handler = WithAuditInit(handler)
				serveFinished := make(chan struct{})
				go func() {
					defer close(serveFinished)
					handler.ServeHTTP(httptest.NewRecorder(), req)
				}()
				<-serveFinished
			}
		})
	}
}
