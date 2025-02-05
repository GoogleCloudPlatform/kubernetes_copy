/*
Copyright 2014 The Kubernetes Authors.

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

package watch_test

import (
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	. "k8s.io/apimachinery/pkg/watch"
)

type fakeDecoder struct {
	items chan Event
	err   error
}

func (f fakeDecoder) Decode() (action EventType, object runtime.Object, err error) {
	if f.err != nil {
		return "", nil, f.err
	}
	item, open := <-f.items
	if !open {
		return action, nil, io.EOF
	}
	return item.Type, item.Object, nil
}

func (f fakeDecoder) Close() {
	if f.items != nil {
		close(f.items)
	}
}

type fakeReporter struct {
	err error
}

func (f *fakeReporter) AsObject(err error) runtime.Object {
	f.err = err
	return runtime.Unstructured(nil)
}

func TestStreamWatcher(t *testing.T) {
	table := []Event{
		{Type: Added, Object: testType("foo")},
	}

	fd := fakeDecoder{items: make(chan Event, 5)}
	//nolint:logcheck // Intentionally uses the old API.
	sw := NewStreamWatcher(fd, nil)

	for _, item := range table {
		fd.items <- item
		got, open := <-sw.ResultChan()
		if !open {
			t.Errorf("unexpected early close")
		}
		if e, a := item, got; !reflect.DeepEqual(e, a) {
			t.Errorf("expected %v, got %v", e, a)
		}
	}

	sw.Stop()
	_, open := <-sw.ResultChan()
	if open {
		t.Errorf("Unexpected failure to close")
	}
}

func TestStreamWatcherError(t *testing.T) {
	fd := fakeDecoder{err: fmt.Errorf("test error")}
	fr := &fakeReporter{}
	//nolint:logcheck // Intentionally uses the old API.
	sw := NewStreamWatcher(fd, fr)
	evt, ok := <-sw.ResultChan()
	if !ok {
		t.Fatalf("unexpected close")
	}
	if evt.Type != Error || evt.Object != runtime.Unstructured(nil) {
		t.Fatalf("unexpected object: %#v", evt)
	}
	_, ok = <-sw.ResultChan()
	if ok {
		t.Fatalf("unexpected open channel")
	}

	sw.Stop()
	_, ok = <-sw.ResultChan()
	if ok {
		t.Fatalf("unexpected open channel")
	}
}

func TestStreamWatcherRace(t *testing.T) {
	fd := fakeDecoder{err: fmt.Errorf("test error")}
	fr := &fakeReporter{}
	//nolint:logcheck // Intentionally uses the old API.
	sw := NewStreamWatcher(fd, fr)
	time.Sleep(10 * time.Millisecond)
	sw.Stop()
	time.Sleep(10 * time.Millisecond)
	_, ok := <-sw.ResultChan()
	if ok {
		t.Fatalf("unexpected pending send")
	}
}
