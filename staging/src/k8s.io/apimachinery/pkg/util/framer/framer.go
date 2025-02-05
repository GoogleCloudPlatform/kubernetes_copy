/*
Copyright 2015 The Kubernetes Authors.

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

// Package framer implements simple frame decoding techniques for an io.ReadCloser
package framer

import (
	"encoding/binary"
	"encoding/json"
	"io"
)

type lengthDelimitedFrameWriter struct {
	w io.Writer
	h [4]byte
}

func NewLengthDelimitedFrameWriter(w io.Writer) io.Writer {
	return &lengthDelimitedFrameWriter{w: w}
}

// Write writes a single frame to the nested writer, prepending it with the length
// in bytes of data (as a 4 byte, bigendian uint32).
func (w *lengthDelimitedFrameWriter) Write(data []byte) (int, error) {
	binary.BigEndian.PutUint32(w.h[:], uint32(len(data)))
	n, err := w.w.Write(w.h[:])
	if err != nil {
		return 0, err
	}
	if n != len(w.h) {
		return 0, io.ErrShortWrite
	}
	return w.w.Write(data)
}

type lengthDelimitedFrameReader struct {
	r         io.ReadCloser
	remaining int
}

// NewLengthDelimitedFrameReader returns an io.Reader that will decode length-prefixed
// frames off of a stream.
//
// The protocol is:
//
//	stream: message ...
//	message: prefix body
//	prefix: 4 byte uint32 in BigEndian order, denotes length of body
//	body: bytes (0..prefix)
//
// If the buffer passed to Read is not long enough to contain an entire frame, io.ErrShortRead
// will be returned along with the number of bytes read.
func NewLengthDelimitedFrameReader(r io.ReadCloser) io.ReadCloser {
	return &lengthDelimitedFrameReader{r: r}
}

// Read attempts to read an entire frame into data. If that is not possible, io.ErrShortBuffer
// is returned and subsequent calls will attempt to read the last frame. A frame is complete when
// err is nil.
func (r *lengthDelimitedFrameReader) Read(data []byte) (int, error) {
	if r.remaining <= 0 {
		header := [4]byte{}
		n, err := io.ReadAtLeast(r.r, header[:4], 4)
		if err != nil {
			return 0, err
		}
		if n != 4 {
			return 0, io.ErrUnexpectedEOF
		}
		frameLength := int(binary.BigEndian.Uint32(header[:]))
		r.remaining = frameLength
	}

	expect := r.remaining
	max := expect
	if max > len(data) {
		max = len(data)
	}
	n, err := io.ReadAtLeast(r.r, data[:max], int(max))
	r.remaining -= n
	if err != nil {
		return n, err
	}
	if r.remaining > 0 {
		return n, io.ErrShortBuffer
	}
	if n != expect {
		return n, io.ErrUnexpectedEOF
	}

	return n, nil
}

func (r *lengthDelimitedFrameReader) Close() error {
	return r.r.Close()
}

type jsonFrameReader struct {
	r         io.ReadCloser
	decoder   *json.Decoder
	remaining []byte
}

// NewJSONFramedReader returns an io.Reader that will decode individual JSON objects off
// of a wire.
//
// The boundaries between each frame are valid JSON objects. A JSON parsing error will terminate
// the read.
func NewJSONFramedReader(r io.ReadCloser) io.ReadCloser {
	return &jsonFrameReader{
		r:       r,
		decoder: json.NewDecoder(r),
	}
}

// ReadFrame decodes the next JSON object in the stream, or returns an error. The returned
// byte slice will be modified the next time ReadFrame is invoked and should not be altered.
func (r *jsonFrameReader) Read(data []byte) (int, error) {
	// Return whatever remaining data exists from an in progress frame
	if n := len(r.remaining); n > 0 {
		if n <= len(data) {
			//nolint:staticcheck // SA4006,SA4010 underlying array of data is modified here.
			data = append(data[0:0], r.remaining...)
			r.remaining = nil
			return n, nil
		}

		n = len(data)
		//nolint:staticcheck // SA4006,SA4010 underlying array of data is modified here.
		data = append(data[0:0], r.remaining[:n]...)
		r.remaining = r.remaining[n:]
		return n, io.ErrShortBuffer
	}

	// RawMessage#Unmarshal appends to data - we reset the slice down to 0 and will either see
	// data written to data, or be larger than data and a different array.
	m := json.RawMessage(data[:0])
	if err := r.decoder.Decode(&m); err != nil {
		return 0, err
	}

	// If capacity of data is less than length of the message, decoder will allocate a new slice
	// and set m to it, which means we need to copy the partial result back into data and preserve
	// the remaining result for subsequent reads.
	if len(m) > cap(data) {
		copy(data, m)
		r.remaining = m[len(data):]
		return len(data), io.ErrShortBuffer
	}

	if len(m) > len(data) {
		// The bytes beyond len(data) were stored in data's underlying array, which we do
		// not own after this function returns.
		r.remaining = append([]byte(nil), m[len(data):]...)
		return len(data), io.ErrShortBuffer
	}

	return len(m), nil
}

func (r *jsonFrameReader) Close() error {
	return r.r.Close()
}
