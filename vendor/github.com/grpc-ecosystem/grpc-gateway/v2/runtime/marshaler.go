package runtime

import (
	"io"
)

// Marshaler defines a conversion between byte sequence and gRPC payloads / fields.
type Marshaler interface {
	// Marshal marshals "v" into byte sequence.
	Marshal(v interface{}) ([]byte, error)
	// Unmarshal unmarshals "data" into "v".
	// "v" must be a pointer value.
	Unmarshal(data []byte, v interface{}) error
	// NewDecoder returns a Decoder which reads byte sequence from "r".
	NewDecoder(r io.Reader) Decoder
	// NewEncoder returns an Encoder which writes bytes sequence into "w".
	NewEncoder(w io.Writer) Encoder
	// ContentType returns the Content-Type which this marshaler is responsible for.
	// The parameter describes the type which is being marshalled, which can sometimes
	// affect the content type returned.
	ContentType(v interface{}) string
}

// Decoder decodes a byte sequence
type Decoder interface {
	Decode(v interface{}) error
}

// Encoder encodes gRPC payloads / fields into byte sequence.
type Encoder interface {
	Encode(v interface{}) error
}

// DecoderFunc adapts an decoder function into Decoder.
type DecoderFunc func(v interface{}) error

// Decode delegates invocations to the underlying function itself.
func (f DecoderFunc) Decode(v interface{}) error { return f(v) }

// EncoderFunc adapts an encoder function into Encoder
type EncoderFunc func(v interface{}) error

// Encode delegates invocations to the underlying function itself.
func (f EncoderFunc) Encode(v interface{}) error { return f(v) }

// Delimited defines the streaming delimiter.
type Delimited interface {
	// Delimiter returns the record separator for the stream.
	Delimiter() []byte
}

// StreamContentType defines the streaming content type.
type StreamContentType interface {
	// StreamContentType returns the content type for a stream. This shares the
	// same behaviour as for `Marshaler.ContentType`, but is called, if present,
	// in the case of a streamed response.
	StreamContentType(v interface{}) string
}
