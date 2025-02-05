/*
Copyright 2020 The Kubernetes Authors.

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

package audit

import (
	"context"
	"maps"
	"net"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"
)

// The key type is unexported to prevent collisions
type key int

// auditKey is the context key for storing the audit context that is being
// captured and the evaluated policy that applies to the given request.
const auditKey key = iota

// AuditContext holds the information for constructing the audit events for the current request.
type AuditContext struct {
	// RequestAuditConfig is the audit configuration that applies to the request
	RequestAuditConfig RequestAuditConfig

	// lock guards event
	lock sync.Mutex

	// event is the audit Event object that is being captured to be written in
	// the API audit log.
	event *auditinternal.Event
}

// Enabled checks whether auditing is enabled for this audit context.
func (ac *AuditContext) Enabled() bool {
	// Note: An unset Level should be considered Enabled, so that request data (e.g. annotations)
	// can still be captured before the audit policy is evaluated.
	return ac != nil && ac.RequestAuditConfig.Level != auditinternal.LevelNone
}

// Level Get the level of the event.
// Null value level can also be compared.
func (ac *AuditContext) Level() auditinternal.Level {
	var ret auditinternal.Level
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		ret = event.Level
	})
	return ret
}

func (ac *AuditContext) AuditID() types.UID {
	var ret types.UID
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		ret = event.AuditID
	})
	return ret
}

func (ac *AuditContext) EventIsNil() bool {
	var result bool
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		result = event == nil
	})
	return result
}

func (ac *AuditContext) visitEventIfNotNil(f func(event *auditinternal.Event)) {
	ac.lock.Lock()
	defer ac.lock.Unlock()
	if ac.event == nil {
		return
	}
	f(ac.event)
}

func (ac *AuditContext) visitEventIfNotNilLocked(f func(event *auditinternal.Event)) {
	if ac.event == nil {
		return
	}
	f(ac.event)
}

func (ac *AuditContext) LogRequestObject(objMeta metav1.Object, gvr schema.GroupVersionResource, subresource string, obj *runtime.Unknown) {
	ac.visitEventIfNotNil(func(ae *auditinternal.Event) {
		if ae.ObjectRef == nil {
			ae.ObjectRef = &auditinternal.ObjectReference{}
		}

		if objMeta != nil {
			if len(ae.ObjectRef.Namespace) == 0 {
				ae.ObjectRef.Namespace = objMeta.GetNamespace()
			}
			if len(ae.ObjectRef.Name) == 0 {
				ae.ObjectRef.Name = objMeta.GetName()
			}
			if len(ae.ObjectRef.UID) == 0 {
				ae.ObjectRef.UID = objMeta.GetUID()
			}
			if len(ae.ObjectRef.ResourceVersion) == 0 {
				ae.ObjectRef.ResourceVersion = objMeta.GetResourceVersion()
			}
		}
		if len(ae.ObjectRef.APIVersion) == 0 {
			ae.ObjectRef.APIGroup = gvr.Group
			ae.ObjectRef.APIVersion = gvr.Version
		}
		if len(ae.ObjectRef.Resource) == 0 {
			ae.ObjectRef.Resource = gvr.Resource
		}
		if len(ae.ObjectRef.Subresource) == 0 {
			ae.ObjectRef.Subresource = subresource
		}

		if ae.Level.Less(auditinternal.LevelRequest) {
			return
		}
		ae.RequestObject = obj
	})
}

func (ac *AuditContext) LogImpersonatedUser(user user.Info) {
	ac.visitEventIfNotNil(func(ev *auditinternal.Event) {
		if ev == nil || ev.Level.Less(auditinternal.LevelMetadata) {
			return
		}
		ev.ImpersonatedUser = &authnv1.UserInfo{
			Username: user.GetName(),
		}
		ev.ImpersonatedUser.Groups = user.GetGroups()
		ev.ImpersonatedUser.UID = user.GetUID()
		ev.ImpersonatedUser.Extra = map[string]authnv1.ExtraValue{}
		for k, v := range user.GetExtra() {
			ev.ImpersonatedUser.Extra[k] = authnv1.ExtraValue(v)
		}
	})
}

func (ac *AuditContext) LogRequestMetadata(uri string, agent string, sourceIps []net.IP, requestReceivedTimestamp time.Time, level auditinternal.Level, attribs authorizer.Attributes) {
	ac.visitEventIfNotNil(func(ev *auditinternal.Event) {
		ev.RequestReceivedTimestamp = metav1.NewMicroTime(requestReceivedTimestamp)
		ev.Verb = attribs.GetVerb()
		ev.RequestURI = uri
		ev.UserAgent = agent
		ev.Level = level

		ips := sourceIps
		ev.SourceIPs = make([]string, len(ips))
		for i := range ips {
			ev.SourceIPs[i] = ips[i].String()
		}

		if user := attribs.GetUser(); user != nil {
			ev.User.Username = user.GetName()
			ev.User.Extra = map[string]authnv1.ExtraValue{}
			for k, v := range user.GetExtra() {
				ev.User.Extra[k] = authnv1.ExtraValue(v)
			}
			ev.User.Groups = user.GetGroups()
			ev.User.UID = user.GetUID()
		}

		if attribs.IsResourceRequest() {
			ev.ObjectRef = &auditinternal.ObjectReference{
				Namespace:   attribs.GetNamespace(),
				Name:        attribs.GetName(),
				Resource:    attribs.GetResource(),
				Subresource: attribs.GetSubresource(),
				APIGroup:    attribs.GetAPIGroup(),
				APIVersion:  attribs.GetAPIVersion(),
			}
		}
	})
}

func (ac *AuditContext) LogResponseObject(status *metav1.Status, obj *runtime.Unknown) {
	ac.visitEventIfNotNil(func(ae *auditinternal.Event) {
		if status != nil {
			// selectively copy the bounded fields.
			ae.ResponseStatus = &metav1.Status{
				Status:  status.Status,
				Message: status.Message,
				Reason:  status.Reason,
				Details: status.Details,
				Code:    status.Code,
			}
		}
		if ae.Level.Less(auditinternal.LevelRequestResponse) {
			return
		}
		ae.ResponseObject = obj
	})
}

// LogRequestPatch fills in the given patch as the request object into an audit event.
func (ac *AuditContext) LogRequestPatch(patch []byte) {
	ac.visitEventIfNotNil(func(ae *auditinternal.Event) {
		ae.RequestObject = &runtime.Unknown{
			Raw:         patch,
			ContentType: runtime.ContentTypeJSON,
		}
	})
}

func (ac *AuditContext) GetEventAnnotation(key string) (string, bool) {
	var val string
	var ok bool
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		val, ok = event.Annotations[key]
	})
	return val, ok
}

func (ac *AuditContext) GetEventLevel() auditinternal.Level {
	var level auditinternal.Level
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		level = event.Level
	})
	return level
}

func (ac *AuditContext) SetEventLevel(level auditinternal.Level) {
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		event.Level = level
	})
}

func (ac *AuditContext) SetEventStage(stage auditinternal.Stage) {
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		event.Stage = stage
	})
}

func (ac *AuditContext) GetEventStage() auditinternal.Stage {
	var stage auditinternal.Stage
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		stage = event.Stage
	})
	return stage
}

func (ac *AuditContext) SetEventStageTimestamp(timestamp metav1.MicroTime) {
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		event.StageTimestamp = timestamp
	})
}

func (ac *AuditContext) GetEventResponseStatus() *metav1.Status {
	var status *metav1.Status
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		status = event.ResponseStatus
	})
	return status
}

func (ac *AuditContext) GetEventRequestReceivedTimestamp() metav1.MicroTime {
	var timestamp metav1.MicroTime
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		timestamp = event.RequestReceivedTimestamp
	})
	return timestamp
}

func (ac *AuditContext) GetEventStageTimestamp() metav1.MicroTime {
	var timestamp metav1.MicroTime
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		timestamp = event.StageTimestamp
	})
	return timestamp
}

func (ac *AuditContext) SetEventResponseStatus(status *metav1.Status) {
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		event.ResponseStatus = status
	})
}

func (ac *AuditContext) GetEventAnnotations() map[string]string {
	var annotations map[string]string
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		annotations = maps.Clone(event.Annotations)
	})
	return annotations
}

func (ac *AuditContext) Invoke(f func(e *auditinternal.Event) bool) bool {
	var done bool
	ac.visitEventIfNotNil(func(event *auditinternal.Event) {
		done = f(ac.event)
	})
	return done
}

// AddAuditAnnotation sets the audit annotation for the given key, value pair.
// It is safe to call at most parts of request flow that come after WithAuditAnnotations.
// The notable exception being that this function must not be called via a
// defer statement (i.e. after ServeHTTP) in a handler that runs before WithAudit
// as at that point the audit event has already been sent to the audit sink.
// Handlers that are unaware of their position in the overall request flow should
// prefer AddAuditAnnotation over LogAnnotation to avoid dropping annotations.
func AddAuditAnnotation(ctx context.Context, key, value string) {
	ac := AuditContextFrom(ctx)
	if !ac.Enabled() {
		return
	}

	ac.lock.Lock()
	defer ac.lock.Unlock()

	addAuditAnnotationLocked(ac, key, value)
}

// AddAuditAnnotations is a bulk version of AddAuditAnnotation. Refer to AddAuditAnnotation for
// restrictions on when this can be called.
// keysAndValues are the key-value pairs to add, and must have an even number of items.
func AddAuditAnnotations(ctx context.Context, keysAndValues ...string) {
	ac := AuditContextFrom(ctx)
	if !ac.Enabled() {
		return
	}

	ac.lock.Lock()
	defer ac.lock.Unlock()

	if len(keysAndValues)%2 != 0 {
		klog.Errorf("Dropping mismatched audit annotation %q", keysAndValues[len(keysAndValues)-1])
	}
	for i := 0; i < len(keysAndValues); i += 2 {
		addAuditAnnotationLocked(ac, keysAndValues[i], keysAndValues[i+1])
	}
}

// AddAuditAnnotationsMap is a bulk version of AddAuditAnnotation. Refer to AddAuditAnnotation for
// restrictions on when this can be called.
func AddAuditAnnotationsMap(ctx context.Context, annotations map[string]string) {
	ac := AuditContextFrom(ctx)
	if !ac.Enabled() {
		return
	}

	ac.lock.Lock()
	defer ac.lock.Unlock()

	for k, v := range annotations {
		addAuditAnnotationLocked(ac, k, v)
	}
}

// addAuditAnnotationLocked records the audit annotation on the event.
func addAuditAnnotationLocked(ac *AuditContext, key, value string) {
	ac.visitEventIfNotNilLocked(func(ae *auditinternal.Event) {
		if ae.Annotations == nil {
			ae.Annotations = make(map[string]string)
		}
		if v, ok := ae.Annotations[key]; ok && v != value {
			klog.Warningf("Failed to set annotations[%q] to %q for audit:%q, it has already been set to %q", key, value, ae.AuditID, ae.Annotations[key])
			return
		}
		ae.Annotations[key] = value
	})
}

// WithAuditContext returns a new context that stores the AuditContext.
func WithAuditContext(parent context.Context) context.Context {
	if AuditContextFrom(parent) != nil {
		return parent // Avoid double registering.
	}

	return genericapirequest.WithValue(parent, auditKey, &AuditContext{
		event: &auditinternal.Event{},
	})
}

// AuditContextFrom returns the pair of the audit configuration object
// that applies to the given request and the audit event that is going to
// be written to the API audit log.
func AuditContextFrom(ctx context.Context) *AuditContext {
	ev, _ := ctx.Value(auditKey).(*AuditContext)
	return ev
}

// WithAuditID sets the AuditID on the AuditContext. The AuditContext must already be present in the
// request context. If the specified auditID is empty, no value is set.
func WithAuditID(ctx context.Context, auditID types.UID) {
	if auditID == "" {
		return
	}
	if ac := AuditContextFrom(ctx); ac != nil {
		ac.visitEventIfNotNil(func(event *auditinternal.Event) {
			event.AuditID = auditID
		})
	}
}

// AuditIDFrom returns the value of the audit ID from the request context, along with whether
// auditing is enabled.
func AuditIDFrom(ctx context.Context) (types.UID, bool) {
	if ac := AuditContextFrom(ctx); ac != nil {
		var ret types.UID
		ac.visitEventIfNotNil(func(event *auditinternal.Event) {
			ret = event.AuditID
		})
		return ret, true
	}
	return "", false
}

// GetAuditIDTruncated returns the audit ID (truncated) from the request context.
// If the length of the Audit-ID value exceeds the limit, we truncate it to keep
// the first N (maxAuditIDLength) characters.
// This is intended to be used in logging only.
func GetAuditIDTruncated(ctx context.Context) string {
	auditID, ok := AuditIDFrom(ctx)
	if !ok {
		return ""
	}

	// if the user has specified a very long audit ID then we will use the first N characters
	// Note: assuming Audit-ID header is in ASCII
	const maxAuditIDLength = 64
	if len(auditID) > maxAuditIDLength {
		auditID = auditID[:maxAuditIDLength]
	}

	return string(auditID)
}
