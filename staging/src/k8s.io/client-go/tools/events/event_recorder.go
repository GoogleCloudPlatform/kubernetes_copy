/*
Copyright 2019 The Kubernetes Authors.

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

package events

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/record/util"
	"k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

type recorderImpl struct {
	scheme              *runtime.Scheme
	reportingController string
	reportingInstance   string
	*watch.Broadcaster
	clock clock.Clock
}

var _ EventRecorder = &recorderImpl{}

func (recorder *recorderImpl) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	recorder.eventf(klog.Background(), regarding, related, eventtype, reason, action, note, args...)
}

type recorderImplLogger struct {
	*recorderImpl
	logger klog.Logger
}

var _ EventRecorderLogger = &recorderImplLogger{}

func (recorder *recorderImplLogger) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	recorder.eventf(recorder.logger, regarding, related, eventtype, reason, action, note, args...)
}

func (recorder *recorderImplLogger) WithLogger(logger klog.Logger) EventRecorderLogger {
	return &recorderImplLogger{recorderImpl: recorder.recorderImpl, logger: logger}
}

func (recorder *recorderImpl) eventf(logger klog.Logger, regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	timestamp := metav1.MicroTime{Time: time.Now()}
	message := fmt.Sprintf(note, args...)
	refRegarding, err := reference.GetReference(recorder.scheme, regarding)
	if err != nil {
		logger.Error(err, "Could not construct reference, will not report event", "object", regarding, "eventType", eventtype, "reason", reason, "message", message)
		return
	}

	var refRelated *v1.ObjectReference
	if related != nil {
		refRelated, err = reference.GetReference(recorder.scheme, related)
		if err != nil {
			logger.V(9).Info("Could not construct reference", "object", related, "err", err)
		}
	}
	if !util.ValidateEventType(eventtype) {
		logger.Error(nil, "Unsupported event type", "eventType", eventtype)
		return
	}
	event := recorder.makeEvent(refRegarding, refRelated, timestamp, eventtype, reason, message, recorder.reportingController, recorder.reportingInstance, action)
	go func() {
		defer utilruntime.HandleCrash()
		recorder.Action(watch.Added, event)
	}()
}

func (recorder *recorderImpl) makeEvent(refRegarding *v1.ObjectReference, refRelated *v1.ObjectReference, timestamp metav1.MicroTime, eventtype, reason, message string, reportingController string, reportingInstance string, action string) *eventsv1.Event {
	t := metav1.Time{Time: recorder.clock.Now()}
	namespace := refRegarding.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.GenerateEventName(refRegarding.Name, t.UnixNano()),
			Namespace: namespace,
		},
		EventTime:           timestamp,
		Series:              nil,
		ReportingController: reportingController,
		ReportingInstance:   reportingInstance,
		Action:              action,
		Reason:              reason,
		Regarding:           *refRegarding,
		Related:             refRelated,
		Note:                message,
		Type:                eventtype,
	}
}
