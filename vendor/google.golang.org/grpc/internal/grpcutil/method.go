/*
 *
 * Copyright 2018 gRPC authors.
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

package grpcutil

import (
	"errors"
	"strings"
)

// ParseMethod splits service and method from the input. It expects format
// "/service/method".
func ParseMethod(methodName string) (service, method string, _ error) {
	if !strings.HasPrefix(methodName, "/") {
		return "", "", errors.New("invalid method name: should start with /")
	}
	methodName = methodName[1:]

	pos := strings.LastIndex(methodName, "/")
	if pos < 0 {
		return "", "", errors.New("invalid method name: suffix /method is missing")
	}
	return methodName[:pos], methodName[pos+1:], nil
}

// baseContentType is the base content-type for gRPC.  This is a valid
// content-type on its own, but can also include a content-subtype such as
// "proto" as a suffix after "+" or ";".  See
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests
// for more details.
const baseContentType = "application/grpc"

// ContentSubtype returns the content-subtype for the given content-type.  The
// given content-type must be a valid content-type that starts with
// "application/grpc". A content-subtype will follow "application/grpc" after a
// "+" or ";". See
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests for
// more details.
//
// If contentType is not a valid content-type for gRPC, the boolean
// will be false, otherwise true. If content-type == "application/grpc",
// "application/grpc+", or "application/grpc;", the boolean will be true,
// but no content-subtype will be returned.
//
// contentType is assumed to be lowercase already.
func ContentSubtype(contentType string) (string, bool) {
	if contentType == baseContentType {
		return "", true
	}
	if !strings.HasPrefix(contentType, baseContentType) {
		return "", false
	}
	// guaranteed since != baseContentType and has baseContentType prefix
	switch contentType[len(baseContentType)] {
	case '+', ';':
		// this will return true for "application/grpc+" or "application/grpc;"
		// which the previous validContentType function tested to be valid, so we
		// just say that no content-subtype is specified in this case
		return contentType[len(baseContentType)+1:], true
	default:
		return "", false
	}
}

// ContentType builds full content type with the given sub-type.
//
// contentSubtype is assumed to be lowercase
func ContentType(contentSubtype string) string {
	if contentSubtype == "" {
		return baseContentType
	}
	return baseContentType + "+" + contentSubtype
}
