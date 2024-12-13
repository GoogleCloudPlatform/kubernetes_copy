/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package status implements errors returned by gRPC.  These errors are
// serialized and transmitted on the wire between server and client, and allow
// for additional data to be transmitted via the Details field in the status
// proto.  gRPC service handlers should return an error created by this
// package, and gRPC clients should expect a corresponding error to be
// returned from the RPC call.
//
// This package upholds the invariants that a non-nil error may not
// contain an OK code, and an OK code must result in a nil error.
package status

import (
	"errors"
	"fmt"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/anypb"
)

// Status represents an RPC status code, message, and details.  It is immutable
// and should be created with New, Newf, or FromProto.
type Status struct {
	s *spb.Status
}

// NewWithProto returns a new status including details from statusProto.  This
// is meant to be used by the gRPC library only.
func NewWithProto(code codes.Code, message string, statusProto []string) *Status {
	if len(statusProto) != 1 {
		// No grpc-status-details bin header, or multiple; just ignore.
		return &Status{s: &spb.Status{Code: int32(code), Message: message}}
	}
	st := &spb.Status{}
	if err := proto.Unmarshal([]byte(statusProto[0]), st); err != nil {
		// Probably not a google.rpc.Status proto; do not provide details.
		return &Status{s: &spb.Status{Code: int32(code), Message: message}}
	}
	if st.Code == int32(code) {
		// The codes match between the grpc-status header and the
		// grpc-status-details-bin header; use the full details proto.
		return &Status{s: st}
	}
	return &Status{
		s: &spb.Status{
			Code: int32(codes.Internal),
			Message: fmt.Sprintf(
				"grpc-status-details-bin mismatch: grpc-status=%v, grpc-message=%q, grpc-status-details-bin=%+v",
				code, message, st,
			),
		},
	}
}

// New returns a Status representing c and msg.
func New(c codes.Code, msg string) *Status {
	return &Status{s: &spb.Status{Code: int32(c), Message: msg}}
}

// Newf returns New(c, fmt.Sprintf(format, a...)).
func Newf(c codes.Code, format string, a ...any) *Status {
	return New(c, fmt.Sprintf(format, a...))
}

// FromProto returns a Status representing s.
func FromProto(s *spb.Status) *Status {
	return &Status{s: proto.Clone(s).(*spb.Status)}
}

// Err returns an error representing c and msg.  If c is OK, returns nil.
func Err(c codes.Code, msg string) error {
	return New(c, msg).Err()
}

// Errorf returns Error(c, fmt.Sprintf(format, a...)).
func Errorf(c codes.Code, format string, a ...any) error {
	return Err(c, fmt.Sprintf(format, a...))
}

// Code returns the status code contained in s.
func (s *Status) Code() codes.Code {
	if s == nil || s.s == nil {
		return codes.OK
	}
	return codes.Code(s.s.Code)
}

// Message returns the message contained in s.
func (s *Status) Message() string {
	if s == nil || s.s == nil {
		return ""
	}
	return s.s.Message
}

// Proto returns s's status as an spb.Status proto message.
func (s *Status) Proto() *spb.Status {
	if s == nil {
		return nil
	}
	return proto.Clone(s.s).(*spb.Status)
}

// Err returns an immutable error representing s; returns nil if s.Code() is OK.
func (s *Status) Err() error {
	if s.Code() == codes.OK {
		return nil
	}
	return &Error{s: s}
}

// WithDetails returns a new status with the provided details messages appended to the status.
// If any errors are encountered, it returns nil and the first error encountered.
func (s *Status) WithDetails(details ...protoadapt.MessageV1) (*Status, error) {
	if s.Code() == codes.OK {
		return nil, errors.New("no error details for status with code OK")
	}
	// s.Code() != OK implies that s.Proto() != nil.
	p := s.Proto()
	for _, detail := range details {
		m, err := anypb.New(protoadapt.MessageV2Of(detail))
		if err != nil {
			return nil, err
		}
		p.Details = append(p.Details, m)
	}
	return &Status{s: p}, nil
}

// Details returns a slice of details messages attached to the status.
// If a detail cannot be decoded, the error is returned in place of the detail.
// If the detail can be decoded, the proto message returned is of the same
// type that was given to WithDetails().
func (s *Status) Details() []any {
	if s == nil || s.s == nil {
		return nil
	}
	details := make([]any, 0, len(s.s.Details))
	for _, any := range s.s.Details {
		detail, err := any.UnmarshalNew()
		if err != nil {
			details = append(details, err)
			continue
		}
		// The call to MessageV1Of is required to unwrap the proto message if
		// it implemented only the MessageV1 API. The proto message would have
		// been wrapped in a V2 wrapper in Status.WithDetails. V2 messages are
		// added to a global registry used by any.UnmarshalNew().
		// MessageV1Of has the following behaviour:
		// 1. If the given message is a wrapped MessageV1, it returns the
		//   unwrapped value.
		// 2. If the given message already implements MessageV1, it returns it
		//   as is.
		// 3. Else, it wraps the MessageV2 in a MessageV1 wrapper.
		//
		// Since the Status.WithDetails() API only accepts MessageV1, calling
		// MessageV1Of ensures we return the same type that was given to
		// WithDetails:
		// * If the give type implemented only MessageV1, the unwrapping from
		//   point 1 above will restore the type.
		// * If the given type implemented both MessageV1 and MessageV2, point 2
		//   above will ensure no wrapping is performed.
		// * If the given type implemented only MessageV2 and was wrapped using
		//   MessageV1Of before passing to WithDetails(), it would be unwrapped
		//   in WithDetails by calling MessageV2Of(). Point 3 above will ensure
		//   that the type is wrapped in a MessageV1 wrapper again before
		//   returning. Note that protoc-gen-go doesn't generate code which
		//   implements ONLY MessageV2 at the time of writing.
		//
		// NOTE: Status details can also be added using the FromProto method.
		// This could theoretically allow passing a Detail message that only
		// implements the V2 API. In such a case the message will be wrapped in
		// a MessageV1 wrapper when fetched using Details().
		// Since protoc-gen-go generates only code that implements both V1 and
		// V2 APIs for backward compatibility, this is not a concern.
		details = append(details, protoadapt.MessageV1Of(detail))
	}
	return details
}

func (s *Status) String() string {
	return fmt.Sprintf("rpc error: code = %s desc = %s", s.Code(), s.Message())
}

// Error wraps a pointer of a status proto. It implements error and Status,
// and a nil *Error should never be returned by this package.
type Error struct {
	s *Status
}

func (e *Error) Error() string {
	return e.s.String()
}

// GRPCStatus returns the Status represented by se.
func (e *Error) GRPCStatus() *Status {
	return e.s
}

// Is implements future error.Is functionality.
// A Error is equivalent if the code and message are identical.
func (e *Error) Is(target error) bool {
	tse, ok := target.(*Error)
	if !ok {
		return false
	}
	return proto.Equal(e.s.s, tse.s.s)
}

// IsRestrictedControlPlaneCode returns whether the status includes a code
// restricted for control plane usage as defined by gRFC A54.
func IsRestrictedControlPlaneCode(s *Status) bool {
	switch s.Code() {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted, codes.OutOfRange, codes.DataLoss:
		return true
	}
	return false
}
