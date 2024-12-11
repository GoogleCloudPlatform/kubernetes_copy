package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/status"
)

// ErrorHandlerFunc is the signature used to configure error handling.
type ErrorHandlerFunc func(context.Context, *ServeMux, Marshaler, http.ResponseWriter, *http.Request, error)

// StreamErrorHandlerFunc is the signature used to configure stream error handling.
type StreamErrorHandlerFunc func(context.Context, error) *status.Status

// RoutingErrorHandlerFunc is the signature used to configure error handling for routing errors.
type RoutingErrorHandlerFunc func(context.Context, *ServeMux, Marshaler, http.ResponseWriter, *http.Request, int)

// HTTPStatusError is the error to use when needing to provide a different HTTP status code for an error
// passed to the DefaultRoutingErrorHandler.
type HTTPStatusError struct {
	HTTPStatus int
	Err        error
}

func (e *HTTPStatusError) Error() string {
	return e.Err.Error()
}

// HTTPStatusFromCode converts a gRPC error code into the corresponding HTTP response status.
// See: https://github.com/googleapis/googleapis/blob/master/google/rpc/code.proto
func HTTPStatusFromCode(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499
	case codes.Unknown:
		return http.StatusInternalServerError
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.FailedPrecondition:
		// Note, this deliberately doesn't translate to the similarly named '412 Precondition Failed' HTTP response status.
		return http.StatusBadRequest
	case codes.Aborted:
		return http.StatusConflict
	case codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Internal:
		return http.StatusInternalServerError
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DataLoss:
		return http.StatusInternalServerError
	default:
		grpclog.Warningf("Unknown gRPC error code: %v", code)
		return http.StatusInternalServerError
	}
}

// HTTPError uses the mux-configured error handler.
func HTTPError(ctx context.Context, mux *ServeMux, marshaler Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	mux.errorHandler(ctx, mux, marshaler, w, r, err)
}

// HTTPStreamError uses the mux-configured stream error handler to notify error to the client without closing the connection.
func HTTPStreamError(ctx context.Context, mux *ServeMux, marshaler Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	st := mux.streamErrorHandler(ctx, err)
	msg := errorChunk(st)
	buf, err := marshaler.Marshal(msg)
	if err != nil {
		grpclog.Errorf("Failed to marshal an error: %v", err)
		return
	}
	if _, err := w.Write(buf); err != nil {
		grpclog.Errorf("Failed to notify error to client: %v", err)
		return
	}
}

// DefaultHTTPErrorHandler is the default error handler.
// If "err" is a gRPC Status, the function replies with the status code mapped by HTTPStatusFromCode.
// If "err" is a HTTPStatusError, the function replies with the status code provide by that struct. This is
// intended to allow passing through of specific statuses via the function set via WithRoutingErrorHandler
// for the ServeMux constructor to handle edge cases which the standard mappings in HTTPStatusFromCode
// are insufficient for.
// If otherwise, it replies with http.StatusInternalServerError.
//
// The response body written by this function is a Status message marshaled by the Marshaler.
func DefaultHTTPErrorHandler(ctx context.Context, mux *ServeMux, marshaler Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	// return Internal when Marshal failed
	const fallback = `{"code": 13, "message": "failed to marshal error message"}`
	const fallbackRewriter = `{"code": 13, "message": "failed to rewrite error message"}`

	var customStatus *HTTPStatusError
	if errors.As(err, &customStatus) {
		err = customStatus.Err
	}

	s := status.Convert(err)

	w.Header().Del("Trailer")
	w.Header().Del("Transfer-Encoding")

	respRw, err := mux.forwardResponseRewriter(ctx, s.Proto())
	if err != nil {
		grpclog.Errorf("Failed to rewrite error message %q: %v", s, err)
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, fallbackRewriter); err != nil {
			grpclog.Errorf("Failed to write response: %v", err)
		}
		return
	}

	contentType := marshaler.ContentType(respRw)
	w.Header().Set("Content-Type", contentType)

	if s.Code() == codes.Unauthenticated {
		w.Header().Set("WWW-Authenticate", s.Message())
	}

	buf, merr := marshaler.Marshal(respRw)
	if merr != nil {
		grpclog.Errorf("Failed to marshal error message %q: %v", s, merr)
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, fallback); err != nil {
			grpclog.Errorf("Failed to write response: %v", err)
		}
		return
	}

	md, ok := ServerMetadataFromContext(ctx)
	if !ok {
		grpclog.Error("Failed to extract ServerMetadata from context")
	}

	handleForwardResponseServerMetadata(w, mux, md)

	// RFC 7230 https://tools.ietf.org/html/rfc7230#section-4.1.2
	// Unless the request includes a TE header field indicating "trailers"
	// is acceptable, as described in Section 4.3, a server SHOULD NOT
	// generate trailer fields that it believes are necessary for the user
	// agent to receive.
	doForwardTrailers := requestAcceptsTrailers(r)

	if doForwardTrailers {
		handleForwardResponseTrailerHeader(w, mux, md)
		w.Header().Set("Transfer-Encoding", "chunked")
	}

	st := HTTPStatusFromCode(s.Code())
	if customStatus != nil {
		st = customStatus.HTTPStatus
	}

	w.WriteHeader(st)
	if _, err := w.Write(buf); err != nil {
		grpclog.Errorf("Failed to write response: %v", err)
	}

	if doForwardTrailers {
		handleForwardResponseTrailer(w, mux, md)
	}
}

func DefaultStreamErrorHandler(_ context.Context, err error) *status.Status {
	return status.Convert(err)
}

// DefaultRoutingErrorHandler is our default handler for routing errors.
// By default http error codes mapped on the following error codes:
//
//	NotFound -> grpc.NotFound
//	StatusBadRequest -> grpc.InvalidArgument
//	MethodNotAllowed -> grpc.Unimplemented
//	Other -> grpc.Internal, method is not expecting to be called for anything else
func DefaultRoutingErrorHandler(ctx context.Context, mux *ServeMux, marshaler Marshaler, w http.ResponseWriter, r *http.Request, httpStatus int) {
	sterr := status.Error(codes.Internal, "Unexpected routing error")
	switch httpStatus {
	case http.StatusBadRequest:
		sterr = status.Error(codes.InvalidArgument, http.StatusText(httpStatus))
	case http.StatusMethodNotAllowed:
		sterr = status.Error(codes.Unimplemented, http.StatusText(httpStatus))
	case http.StatusNotFound:
		sterr = status.Error(codes.NotFound, http.StatusText(httpStatus))
	}
	mux.errorHandler(ctx, mux, marshaler, w, r, sterr)
}
