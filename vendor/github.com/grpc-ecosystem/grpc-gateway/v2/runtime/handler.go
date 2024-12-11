package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ForwardResponseStream forwards the stream from gRPC server to REST client.
func ForwardResponseStream(ctx context.Context, mux *ServeMux, marshaler Marshaler, w http.ResponseWriter, req *http.Request, recv func() (proto.Message, error), opts ...func(context.Context, http.ResponseWriter, proto.Message) error) {
	rc := http.NewResponseController(w)
	md, ok := ServerMetadataFromContext(ctx)
	if !ok {
		grpclog.Error("Failed to extract ServerMetadata from context")
		http.Error(w, "unexpected error", http.StatusInternalServerError)
		return
	}
	handleForwardResponseServerMetadata(w, mux, md)

	w.Header().Set("Transfer-Encoding", "chunked")
	if err := handleForwardResponseOptions(ctx, w, nil, opts); err != nil {
		HTTPError(ctx, mux, marshaler, w, req, err)
		return
	}

	var delimiter []byte
	if d, ok := marshaler.(Delimited); ok {
		delimiter = d.Delimiter()
	} else {
		delimiter = []byte("\n")
	}

	var wroteHeader bool
	for {
		resp, err := recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			handleForwardResponseStreamError(ctx, wroteHeader, marshaler, w, req, mux, err, delimiter)
			return
		}
		if err := handleForwardResponseOptions(ctx, w, resp, opts); err != nil {
			handleForwardResponseStreamError(ctx, wroteHeader, marshaler, w, req, mux, err, delimiter)
			return
		}

		respRw, err := mux.forwardResponseRewriter(ctx, resp)
		if err != nil {
			grpclog.Errorf("Rewrite error: %v", err)
			handleForwardResponseStreamError(ctx, wroteHeader, marshaler, w, req, mux, err, delimiter)
			return
		}

		if !wroteHeader {
			w.Header().Set("Content-Type", marshaler.ContentType(respRw))
		}

		var buf []byte
		httpBody, isHTTPBody := respRw.(*httpbody.HttpBody)
		switch {
		case respRw == nil:
			buf, err = marshaler.Marshal(errorChunk(status.New(codes.Internal, "empty response")))
		case isHTTPBody:
			buf = httpBody.GetData()
		default:
			result := map[string]interface{}{"result": respRw}
			if rb, ok := respRw.(responseBody); ok {
				result["result"] = rb.XXX_ResponseBody()
			}

			buf, err = marshaler.Marshal(result)
		}

		if err != nil {
			grpclog.Errorf("Failed to marshal response chunk: %v", err)
			handleForwardResponseStreamError(ctx, wroteHeader, marshaler, w, req, mux, err, delimiter)
			return
		}
		if _, err := w.Write(buf); err != nil {
			grpclog.Errorf("Failed to send response chunk: %v", err)
			return
		}
		wroteHeader = true
		if _, err := w.Write(delimiter); err != nil {
			grpclog.Errorf("Failed to send delimiter chunk: %v", err)
			return
		}
		err = rc.Flush()
		if err != nil {
			if errors.Is(err, http.ErrNotSupported) {
				grpclog.Errorf("Flush not supported in %T", w)
				http.Error(w, "unexpected type of web server", http.StatusInternalServerError)
				return
			}
			grpclog.Errorf("Failed to flush response to client: %v", err)
			return
		}
	}
}

func handleForwardResponseServerMetadata(w http.ResponseWriter, mux *ServeMux, md ServerMetadata) {
	for k, vs := range md.HeaderMD {
		if h, ok := mux.outgoingHeaderMatcher(k); ok {
			for _, v := range vs {
				w.Header().Add(h, v)
			}
		}
	}
}

func handleForwardResponseTrailerHeader(w http.ResponseWriter, mux *ServeMux, md ServerMetadata) {
	for k := range md.TrailerMD {
		if h, ok := mux.outgoingTrailerMatcher(k); ok {
			w.Header().Add("Trailer", textproto.CanonicalMIMEHeaderKey(h))
		}
	}
}

func handleForwardResponseTrailer(w http.ResponseWriter, mux *ServeMux, md ServerMetadata) {
	for k, vs := range md.TrailerMD {
		if h, ok := mux.outgoingTrailerMatcher(k); ok {
			for _, v := range vs {
				w.Header().Add(h, v)
			}
		}
	}
}

// responseBody interface contains method for getting field for marshaling to the response body
// this method is generated for response struct from the value of `response_body` in the `google.api.HttpRule`
type responseBody interface {
	XXX_ResponseBody() interface{}
}

// ForwardResponseMessage forwards the message "resp" from gRPC server to REST client.
func ForwardResponseMessage(ctx context.Context, mux *ServeMux, marshaler Marshaler, w http.ResponseWriter, req *http.Request, resp proto.Message, opts ...func(context.Context, http.ResponseWriter, proto.Message) error) {
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
	doForwardTrailers := requestAcceptsTrailers(req)

	if doForwardTrailers {
		handleForwardResponseTrailerHeader(w, mux, md)
		w.Header().Set("Transfer-Encoding", "chunked")
	}

	contentType := marshaler.ContentType(resp)
	w.Header().Set("Content-Type", contentType)

	if err := handleForwardResponseOptions(ctx, w, resp, opts); err != nil {
		HTTPError(ctx, mux, marshaler, w, req, err)
		return
	}
	respRw, err := mux.forwardResponseRewriter(ctx, resp)
	if err != nil {
		grpclog.Errorf("Rewrite error: %v", err)
		HTTPError(ctx, mux, marshaler, w, req, err)
		return
	}
	var buf []byte
	if rb, ok := respRw.(responseBody); ok {
		buf, err = marshaler.Marshal(rb.XXX_ResponseBody())
	} else {
		buf, err = marshaler.Marshal(respRw)
	}
	if err != nil {
		grpclog.Errorf("Marshal error: %v", err)
		HTTPError(ctx, mux, marshaler, w, req, err)
		return
	}

	if !doForwardTrailers {
		w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	}

	if _, err = w.Write(buf); err != nil {
		grpclog.Errorf("Failed to write response: %v", err)
	}

	if doForwardTrailers {
		handleForwardResponseTrailer(w, mux, md)
	}
}

func requestAcceptsTrailers(req *http.Request) bool {
	te := req.Header.Get("TE")
	return strings.Contains(strings.ToLower(te), "trailers")
}

func handleForwardResponseOptions(ctx context.Context, w http.ResponseWriter, resp proto.Message, opts []func(context.Context, http.ResponseWriter, proto.Message) error) error {
	if len(opts) == 0 {
		return nil
	}
	for _, opt := range opts {
		if err := opt(ctx, w, resp); err != nil {
			return fmt.Errorf("error handling ForwardResponseOptions: %w", err)
		}
	}
	return nil
}

func handleForwardResponseStreamError(ctx context.Context, wroteHeader bool, marshaler Marshaler, w http.ResponseWriter, req *http.Request, mux *ServeMux, err error, delimiter []byte) {
	st := mux.streamErrorHandler(ctx, err)
	msg := errorChunk(st)
	if !wroteHeader {
		w.Header().Set("Content-Type", marshaler.ContentType(msg))
		w.WriteHeader(HTTPStatusFromCode(st.Code()))
	}
	buf, err := marshaler.Marshal(msg)
	if err != nil {
		grpclog.Errorf("Failed to marshal an error: %v", err)
		return
	}
	if _, err := w.Write(buf); err != nil {
		grpclog.Errorf("Failed to notify error to client: %v", err)
		return
	}
	if _, err := w.Write(delimiter); err != nil {
		grpclog.Errorf("Failed to send delimiter chunk: %v", err)
		return
	}
}

func errorChunk(st *status.Status) map[string]proto.Message {
	return map[string]proto.Message{"error": st.Proto()}
}
