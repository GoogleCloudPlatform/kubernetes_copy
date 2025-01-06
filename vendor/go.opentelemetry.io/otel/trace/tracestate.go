// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/trace"

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxListMembers = 32

	listDelimiters  = ","
	memberDelimiter = "="

	errInvalidKey    errorConst = "invalid tracestate key"
	errInvalidValue  errorConst = "invalid tracestate value"
	errInvalidMember errorConst = "invalid tracestate list-member"
	errMemberNumber  errorConst = "too many list-members in tracestate"
	errDuplicate     errorConst = "duplicate list-member in tracestate"
)

type member struct {
	Key   string
	Value string
}

// according to (chr = %x20 / (nblk-char = %x21-2B / %x2D-3C / %x3E-7E) )
// means (chr = %x20-2B / %x2D-3C / %x3E-7E) .
func checkValueChar(v byte) bool {
	return v >= '\x20' && v <= '\x7e' && v != '\x2c' && v != '\x3d'
}

// according to (nblk-chr = %x21-2B / %x2D-3C / %x3E-7E) .
func checkValueLast(v byte) bool {
	return v >= '\x21' && v <= '\x7e' && v != '\x2c' && v != '\x3d'
}

// based on the W3C Trace Context specification
//
//	value    = (0*255(chr)) nblk-chr
//	nblk-chr = %x21-2B / %x2D-3C / %x3E-7E
//	chr      = %x20 / nblk-chr
//
// see https://www.w3.org/TR/trace-context-1/#value
func checkValue(val string) bool {
	n := len(val)
	if n == 0 || n > 256 {
		return false
	}
	for i := 0; i < n-1; i++ {
		if !checkValueChar(val[i]) {
			return false
		}
	}
	return checkValueLast(val[n-1])
}

func checkKeyRemain(key string) bool {
	// ( lcalpha / DIGIT / "_" / "-"/ "*" / "/" )
	for _, v := range key {
		if isAlphaNum(byte(v)) {
			continue
		}
		switch v {
		case '_', '-', '*', '/':
			continue
		}
		return false
	}
	return true
}

// according to
//
//	simple-key = lcalpha (0*255( lcalpha / DIGIT / "_" / "-"/ "*" / "/" ))
//	system-id = lcalpha (0*13( lcalpha / DIGIT / "_" / "-"/ "*" / "/" ))
//
// param n is remain part length, should be 255 in simple-key or 13 in system-id.
func checkKeyPart(key string, n int) bool {
	if len(key) == 0 {
		return false
	}
	first := key[0] // key's first char
	ret := len(key[1:]) <= n
	ret = ret && first >= 'a' && first <= 'z'
	return ret && checkKeyRemain(key[1:])
}

func isAlphaNum(c byte) bool {
	if c >= 'a' && c <= 'z' {
		return true
	}
	return c >= '0' && c <= '9'
}

// according to
//
//	tenant-id = ( lcalpha / DIGIT ) 0*240( lcalpha / DIGIT / "_" / "-"/ "*" / "/" )
//
// param n is remain part length, should be 240 exactly.
func checkKeyTenant(key string, n int) bool {
	if len(key) == 0 {
		return false
	}
	return isAlphaNum(key[0]) && len(key[1:]) <= n && checkKeyRemain(key[1:])
}

// based on the W3C Trace Context specification
//
//	key = simple-key / multi-tenant-key
//	simple-key = lcalpha (0*255( lcalpha / DIGIT / "_" / "-"/ "*" / "/" ))
//	multi-tenant-key = tenant-id "@" system-id
//	tenant-id = ( lcalpha / DIGIT ) (0*240( lcalpha / DIGIT / "_" / "-"/ "*" / "/" ))
//	system-id = lcalpha (0*13( lcalpha / DIGIT / "_" / "-"/ "*" / "/" ))
//	lcalpha    = %x61-7A ; a-z
//
// see https://www.w3.org/TR/trace-context-1/#tracestate-header.
func checkKey(key string) bool {
	tenant, system, ok := strings.Cut(key, "@")
	if !ok {
		return checkKeyPart(key, 255)
	}
	return checkKeyTenant(tenant, 240) && checkKeyPart(system, 13)
}

func newMember(key, value string) (member, error) {
	if !checkKey(key) {
		return member{}, errInvalidKey
	}
	if !checkValue(value) {
		return member{}, errInvalidValue
	}
	return member{Key: key, Value: value}, nil
}

func parseMember(m string) (member, error) {
	key, val, ok := strings.Cut(m, memberDelimiter)
	if !ok {
		return member{}, fmt.Errorf("%w: %s", errInvalidMember, m)
	}
	key = strings.TrimLeft(key, " \t")
	val = strings.TrimRight(val, " \t")
	result, e := newMember(key, val)
	if e != nil {
		return member{}, fmt.Errorf("%w: %s", errInvalidMember, m)
	}
	return result, nil
}

// String encodes member into a string compliant with the W3C Trace Context
// specification.
func (m member) String() string {
	return m.Key + "=" + m.Value
}

// TraceState provides additional vendor-specific trace identification
// information across different distributed tracing systems. It represents an
// immutable list consisting of key/value pairs, each pair is referred to as a
// list-member.
//
// TraceState conforms to the W3C Trace Context specification
// (https://www.w3.org/TR/trace-context-1). All operations that create or copy
// a TraceState do so by validating all input and will only produce TraceState
// that conform to the specification. Specifically, this means that all
// list-member's key/value pairs are valid, no duplicate list-members exist,
// and the maximum number of list-members (32) is not exceeded.
type TraceState struct { //nolint:revive // revive complains about stutter of `trace.TraceState`
	// list is the members in order.
	list []member
}

var _ json.Marshaler = TraceState{}

// ParseTraceState attempts to decode a TraceState from the passed
// string. It returns an error if the input is invalid according to the W3C
// Trace Context specification.
func ParseTraceState(ts string) (TraceState, error) {
	if ts == "" {
		return TraceState{}, nil
	}

	wrapErr := func(err error) error {
		return fmt.Errorf("failed to parse tracestate: %w", err)
	}

	var members []member
	found := make(map[string]struct{})
	for ts != "" {
		var memberStr string
		memberStr, ts, _ = strings.Cut(ts, listDelimiters)
		if len(memberStr) == 0 {
			continue
		}

		m, err := parseMember(memberStr)
		if err != nil {
			return TraceState{}, wrapErr(err)
		}

		if _, ok := found[m.Key]; ok {
			return TraceState{}, wrapErr(errDuplicate)
		}
		found[m.Key] = struct{}{}

		members = append(members, m)
		if n := len(members); n > maxListMembers {
			return TraceState{}, wrapErr(errMemberNumber)
		}
	}

	return TraceState{list: members}, nil
}

// MarshalJSON marshals the TraceState into JSON.
func (ts TraceState) MarshalJSON() ([]byte, error) {
	return json.Marshal(ts.String())
}

// String encodes the TraceState into a string compliant with the W3C
// Trace Context specification. The returned string will be invalid if the
// TraceState contains any invalid members.
func (ts TraceState) String() string {
	if len(ts.list) == 0 {
		return ""
	}
	var n int
	n += len(ts.list)     // member delimiters: '='
	n += len(ts.list) - 1 // list delimiters: ','
	for _, mem := range ts.list {
		n += len(mem.Key)
		n += len(mem.Value)
	}

	var sb strings.Builder
	sb.Grow(n)
	_, _ = sb.WriteString(ts.list[0].Key)
	_ = sb.WriteByte('=')
	_, _ = sb.WriteString(ts.list[0].Value)
	for i := 1; i < len(ts.list); i++ {
		_ = sb.WriteByte(listDelimiters[0])
		_, _ = sb.WriteString(ts.list[i].Key)
		_ = sb.WriteByte('=')
		_, _ = sb.WriteString(ts.list[i].Value)
	}
	return sb.String()
}

// Get returns the value paired with key from the corresponding TraceState
// list-member if it exists, otherwise an empty string is returned.
func (ts TraceState) Get(key string) string {
	for _, member := range ts.list {
		if member.Key == key {
			return member.Value
		}
	}

	return ""
}

// Walk walks all key value pairs in the TraceState by calling f
// Iteration stops if f returns false.
func (ts TraceState) Walk(f func(key, value string) bool) {
	for _, m := range ts.list {
		if !f(m.Key, m.Value) {
			break
		}
	}
}

// Insert adds a new list-member defined by the key/value pair to the
// TraceState. If a list-member already exists for the given key, that
// list-member's value is updated. The new or updated list-member is always
// moved to the beginning of the TraceState as specified by the W3C Trace
// Context specification.
//
// If key or value are invalid according to the W3C Trace Context
// specification an error is returned with the original TraceState.
//
// If adding a new list-member means the TraceState would have more members
// then is allowed, the new list-member will be inserted and the right-most
// list-member will be dropped in the returned TraceState.
func (ts TraceState) Insert(key, value string) (TraceState, error) {
	m, err := newMember(key, value)
	if err != nil {
		return ts, err
	}
	n := len(ts.list)
	found := n
	for i := range ts.list {
		if ts.list[i].Key == key {
			found = i
		}
	}
	cTS := TraceState{}
	if found == n && n < maxListMembers {
		cTS.list = make([]member, n+1)
	} else {
		cTS.list = make([]member, n)
	}
	cTS.list[0] = m
	// When the number of members exceeds capacity, drop the "right-most".
	copy(cTS.list[1:], ts.list[0:found])
	if found < n {
		copy(cTS.list[1+found:], ts.list[found+1:])
	}
	return cTS, nil
}

// Delete returns a copy of the TraceState with the list-member identified by
// key removed.
func (ts TraceState) Delete(key string) TraceState {
	members := make([]member, ts.Len())
	copy(members, ts.list)
	for i, member := range ts.list {
		if member.Key == key {
			members = append(members[:i], members[i+1:]...)
			// TraceState should contain no duplicate members.
			break
		}
	}
	return TraceState{list: members}
}

// Len returns the number of list-members in the TraceState.
func (ts TraceState) Len() int {
	return len(ts.list)
}
