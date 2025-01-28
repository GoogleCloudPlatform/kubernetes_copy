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

package testutil

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

// FakeRecorder is used as a fake during testing.
type FakeRecorder struct {
	sync.Mutex
	Events []*eventsv1.Event
	clock  clock.Clock
}

// Event emits a fake event to the fake recorder
func (f *FakeRecorder) Event(obj runtime.Object, eventtype, reason, message string) {
	f.generateEvent(obj, eventtype, reason, message)
}

// Eventf emits a fake formatted event to the fake recorder
func (f *FakeRecorder) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	f.Event(regarding, eventtype, reason, fmt.Sprintf(note, args...))
}

func (f *FakeRecorder) generateEvent(regarding runtime.Object, eventtype, reason, note string) {
	f.Lock()
	defer f.Unlock()
	ctx := context.TODO()
	ref, err := ref.GetReference(legacyscheme.Scheme, regarding)
	if err != nil {
		klog.FromContext(ctx).Error(err, "Encountered error while getting reference")
		return
	}
	event := f.makeEvent(ref, eventtype, reason, note)

	if f.Events != nil {
		f.Events = append(f.Events, event)
	}
}

func (f *FakeRecorder) makeEvent(ref *v1.ObjectReference, eventtype, reason, note string) *eventsv1.Event {
	t := metav1.Time{Time: f.clock.Now()}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}

	clientref := v1.ObjectReference{
		Kind:            ref.Kind,
		Namespace:       ref.Namespace,
		Name:            ref.Name,
		UID:             ref.UID,
		APIVersion:      ref.APIVersion,
		ResourceVersion: ref.ResourceVersion,
		FieldPath:       ref.FieldPath,
	}

	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%v.%x", ref.Name, t.UnixNano()),
			Namespace: namespace,
		},
		Regarding: clientref,
		Reason:    reason,
		Type:      eventtype,
	}
}

// NewFakeRecorder returns a pointer to a newly constructed FakeRecorder.
func NewFakeRecorder() *FakeRecorder {
	return &FakeRecorder{
		Events: []*eventsv1.Event{},
		clock:  testingclock.NewFakeClock(time.Now()),
	}
}
