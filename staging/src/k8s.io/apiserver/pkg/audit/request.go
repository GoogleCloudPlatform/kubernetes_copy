/*
Copyright 2017 The Kubernetes Authors.

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
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/klog/v2"
)

const (
	maxUserAgentLength      = 1024
	userAgentTruncateSuffix = "...TRUNCATED"
)

func LogRequestMetadata(ctx context.Context, req *http.Request, requestReceivedTimestamp time.Time, level auditinternal.Level, attribs authorizer.Attributes) {
	ac := AuditContextFrom(ctx)
	if !ac.Enabled() {
		return
	}
	ev := &ac.Event

	ev.RequestReceivedTimestamp = metav1.NewMicroTime(requestReceivedTimestamp)
	ev.Verb = attribs.GetVerb()
	ev.RequestURI = req.URL.RequestURI()
	ev.UserAgent = maybeTruncateUserAgent(req)
	ev.Level = level

	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		ev.TLSExpiration = metav1.NewMicroTime(req.TLS.PeerCertificates[0].NotAfter)
	}

	ips := utilnet.SourceIPs(req)
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
}

// LogImpersonatedUser fills in the impersonated user attributes into an audit event.
func LogImpersonatedUser(ae *auditinternal.Event, user user.Info) {
	if ae == nil || ae.Level.Less(auditinternal.LevelMetadata) {
		return
	}
	ae.ImpersonatedUser = &authnv1.UserInfo{
		Username: user.GetName(),
	}
	ae.ImpersonatedUser.Groups = user.GetGroups()
	ae.ImpersonatedUser.UID = user.GetUID()
	ae.ImpersonatedUser.Extra = map[string]authnv1.ExtraValue{}
	for k, v := range user.GetExtra() {
		ae.ImpersonatedUser.Extra[k] = authnv1.ExtraValue(v)
	}
}

// LogRequestObject fills in the request object into an audit event. The passed runtime.Object
// will be converted to the given gv.
func LogRequestObject(ctx context.Context, obj runtime.Object, objGV schema.GroupVersion, gvr schema.GroupVersionResource, subresource string, s runtime.NegotiatedSerializer) {
	ae := AuditEventFrom(ctx)
	if ae == nil || ae.Level.Less(auditinternal.LevelMetadata) {
		return
	}

	// complete ObjectRef
	if ae.ObjectRef == nil {
		ae.ObjectRef = &auditinternal.ObjectReference{}
	}

	// meta.Accessor is more general than ObjectMetaAccessor, but if it fails, we can just skip setting these bits
	if meta, err := meta.Accessor(obj); err == nil {
		if len(ae.ObjectRef.Namespace) == 0 {
			ae.ObjectRef.Namespace = meta.GetNamespace()
		}
		if len(ae.ObjectRef.Name) == 0 {
			ae.ObjectRef.Name = meta.GetName()
		}
		if len(ae.ObjectRef.UID) == 0 {
			ae.ObjectRef.UID = meta.GetUID()
		}
		if len(ae.ObjectRef.ResourceVersion) == 0 {
			ae.ObjectRef.ResourceVersion = meta.GetResourceVersion()
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

	if shouldOmitManagedFields(ctx) {
		copy, ok, err := copyWithoutManagedFields(obj)
		if err != nil {
			klog.ErrorS(err, "Error while dropping managed fields from the request", "auditID", ae.AuditID)
		}
		if ok {
			obj = copy
		}
	}

	// TODO(audit): hook into the serializer to avoid double conversion
	var err error
	ae.RequestObject, err = encodeObject(obj, objGV, s)
	if err != nil {
		// TODO(audit): add error slice to audit event struct
		klog.ErrorS(err, "Encoding failed of request object", "auditID", ae.AuditID, "gvr", gvr.String(), "obj", obj)
		return
	}
}

// LogRequestPatch fills in the given patch as the request object into an audit event.
func LogRequestPatch(ctx context.Context, patch []byte) {
	ae := AuditEventFrom(ctx)
	if ae == nil || ae.Level.Less(auditinternal.LevelRequest) {
		return
	}

	ae.RequestObject = &runtime.Unknown{
		Raw:         patch,
		ContentType: runtime.ContentTypeJSON,
	}
}

// LogResponseObject fills in the response object into an audit event. The passed runtime.Object
// will be converted to the given gv.
func LogResponseObject(ctx context.Context, obj runtime.Object, gv schema.GroupVersion, s runtime.NegotiatedSerializer) {
	ae := AuditEventFrom(ctx)
	if ae == nil || ae.Level.Less(auditinternal.LevelMetadata) {
		return
	}
	if status, ok := obj.(*metav1.Status); ok {
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

	if shouldOmitManagedFields(ctx) {
		copy, ok, err := copyWithoutManagedFields(obj)
		if err != nil {
			klog.ErrorS(err, "Error while dropping managed fields from the response", "auditID", ae.AuditID)
		}
		if ok {
			obj = copy
		}
	}

	// TODO(audit): hook into the serializer to avoid double conversion
	var err error
	ae.ResponseObject, err = encodeObject(obj, gv, s)
	if err != nil {
		klog.ErrorS(err, "Encoding failed of response object", "auditID", ae.AuditID, "obj", obj)
	}
}

func encodeObject(obj runtime.Object, gv schema.GroupVersion, serializer runtime.NegotiatedSerializer) (*runtime.Unknown, error) {
	const mediaType = runtime.ContentTypeJSON
	info, ok := runtime.SerializerInfoForMediaType(serializer.SupportedMediaTypes(), mediaType)
	if !ok {
		return nil, fmt.Errorf("unable to locate encoder -- %q is not a supported media type", mediaType)
	}

	enc := serializer.EncoderForVersion(info.Serializer, gv)
	var buf bytes.Buffer
	if err := enc.Encode(obj, &buf); err != nil {
		return nil, fmt.Errorf("encoding failed: %v", err)
	}

	return &runtime.Unknown{
		Raw:         buf.Bytes(),
		ContentType: mediaType,
	}, nil
}

// truncate User-Agent if too long, otherwise return it directly.
func maybeTruncateUserAgent(req *http.Request) string {
	ua := req.UserAgent()
	if len(ua) > maxUserAgentLength {
		ua = ua[:maxUserAgentLength] + userAgentTruncateSuffix
	}

	return ua
}

// copyWithoutManagedFields will make a deep copy of the specified object and
// will discard the managed fields from the copy.
// The specified object is expected to be a meta.Object or a "list".
// The specified object obj is treated as readonly and hence not mutated.
// On return, an error is set if the function runs into any error while
// removing the managed fields, the boolean value is true if the copy has
// been made successfully, otherwise false.
func copyWithoutManagedFields(obj runtime.Object) (runtime.Object, bool, error) {
	isAccessor := true
	if _, err := meta.Accessor(obj); err != nil {
		isAccessor = false
	}
	isList := meta.IsListType(obj)
	_, isTable := obj.(*metav1.Table)
	if !isAccessor && !isList && !isTable {
		return nil, false, nil
	}

	// TODO a deep copy isn't really needed here, figure out how we can reliably
	//  use shallow copy here to omit the manageFields.
	copy := obj.DeepCopyObject()

	if isAccessor {
		if err := removeManagedFields(copy); err != nil {
			return nil, false, err
		}
	}

	if isList {
		if err := meta.EachListItem(copy, removeManagedFields); err != nil {
			return nil, false, err
		}
	}

	if isTable {
		table := copy.(*metav1.Table)
		for i := range table.Rows {
			rowObj := table.Rows[i].Object
			if err := removeManagedFields(rowObj.Object); err != nil {
				return nil, false, err
			}
		}
	}

	return copy, true, nil
}

func removeManagedFields(obj runtime.Object) error {
	if obj == nil {
		return nil
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	accessor.SetManagedFields(nil)
	return nil
}

func shouldOmitManagedFields(ctx context.Context) bool {
	if auditContext := AuditContextFrom(ctx); auditContext != nil {
		return auditContext.RequestAuditConfig.OmitManagedFields
	}

	// If we can't decide, return false to maintain current behavior which is
	// to retain the manage fields in the audit.
	return false
}
