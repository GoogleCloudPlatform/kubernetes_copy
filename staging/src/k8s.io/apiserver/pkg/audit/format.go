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
	"fmt"
	"strconv"
	"strings"
	"time"

	auditinternal "k8s.io/apiserver/pkg/apis/audit"
)

// EventString creates a 1-line text representation of an audit event, using a subset of the
// information in the event struct.
func EventString(ev *auditinternal.Event) string {
	username := "<none>"
	groups := "<none>"
	if len(ev.User.Username) > 0 {
		username = ev.User.Username
		if len(ev.User.Groups) > 0 {
			groups = auditStringSlice(ev.User.Groups)
		}
	}
	asuser := "<self>"
	asgroups := "<lookup>"
	if ev.ImpersonatedUser != nil {
		asuser = ev.ImpersonatedUser.Username
		if ev.ImpersonatedUser.Groups != nil {
			asgroups = auditStringSlice(ev.ImpersonatedUser.Groups)
		}
	}

	namespace := "<none>"
	if ev.ObjectRef != nil && len(ev.ObjectRef.Namespace) != 0 {
		namespace = ev.ObjectRef.Namespace
	}

	response := "<deferred>"
	if ev.ResponseStatus != nil {
		response = strconv.Itoa(int(ev.ResponseStatus.Code))
	}

	ip := "<unknown>"
	if len(ev.SourceIPs) > 0 {
		ip = ev.SourceIPs[0]
	}

	eventStr := fmt.Sprintf("%s AUDIT: id=%q stage=%q ip=%q method=%q user=%q groups=%q as=%q asgroups=%q user-agent=%q namespace=%q uri=%q response=\"%s\"",
		ev.RequestReceivedTimestamp.Format(time.RFC3339Nano), ev.AuditID, ev.Stage, ip, ev.Verb, username, groups, asuser, asgroups, ev.UserAgent, namespace, ev.RequestURI, response)
	if !ev.TLSExpiration.Time.IsZero() {
		eventStr = fmt.Sprintf("%s tls-expiration=\"%s\"", eventStr, ev.TLSExpiration.Format(time.RFC3339Nano))
	}
	return eventStr
}

func auditStringSlice(inList []string) string {
	quotedElements := make([]string, len(inList))
	for i, in := range inList {
		quotedElements[i] = fmt.Sprintf("%q", in)
	}
	return strings.Join(quotedElements, ",")
}
