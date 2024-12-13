package restful

// Copyright 2013 Ernest Micklei. All rights reserved.
// Use of this source code is governed by a license
// that can be found in the LICENSE file.

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

// OBSOLETE : use restful.DefaultContainer.EnableContentEncoding(true) to change this setting.
var EnableContentEncoding = false

// CompressingResponseWriter is a http.ResponseWriter that can perform content encoding (gzip and zlib)
type CompressingResponseWriter struct {
	writer     http.ResponseWriter
	compressor io.WriteCloser
	encoding   string
}

// Header is part of http.ResponseWriter interface
func (c *CompressingResponseWriter) Header() http.Header {
	return c.writer.Header()
}

// WriteHeader is part of http.ResponseWriter interface
func (c *CompressingResponseWriter) WriteHeader(status int) {
	c.writer.WriteHeader(status)
}

// Write is part of http.ResponseWriter interface
// It is passed through the compressor
func (c *CompressingResponseWriter) Write(bytes []byte) (int, error) {
	if c.isCompressorClosed() {
		return -1, errors.New("Compressing error: tried to write data using closed compressor")
	}
	return c.compressor.Write(bytes)
}

// CloseNotify is part of http.CloseNotifier interface
func (c *CompressingResponseWriter) CloseNotify() <-chan bool {
	return c.writer.(http.CloseNotifier).CloseNotify()
}

// Flush is part of http.Flusher interface. Noop if the underlying writer doesn't support it.
func (c *CompressingResponseWriter) Flush() {
	flusher, ok := c.writer.(http.Flusher)
	if !ok {
		// writer doesn't support http.Flusher interface
		return
	}
	flusher.Flush()
}

// Close the underlying compressor
func (c *CompressingResponseWriter) Close() error {
	if c.isCompressorClosed() {
		return errors.New("Compressing error: tried to close already closed compressor")
	}

	c.compressor.Close()
	if ENCODING_GZIP == c.encoding {
		currentCompressorProvider.ReleaseGzipWriter(c.compressor.(*gzip.Writer))
	}
	if ENCODING_DEFLATE == c.encoding {
		currentCompressorProvider.ReleaseZlibWriter(c.compressor.(*zlib.Writer))
	}
	// gc hint needed?
	c.compressor = nil
	return nil
}

func (c *CompressingResponseWriter) isCompressorClosed() bool {
	return nil == c.compressor
}

// Hijack implements the Hijacker interface
// This is especially useful when combining Container.EnabledContentEncoding
// in combination with websockets (for instance gorilla/websocket)
func (c *CompressingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := c.writer.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("ResponseWriter doesn't support Hijacker interface")
	}
	return hijacker.Hijack()
}

// WantsCompressedResponse reads the Accept-Encoding header to see if and which encoding is requested.
// It also inspects the httpWriter whether its content-encoding is already set (non-empty).
func wantsCompressedResponse(httpRequest *http.Request, httpWriter http.ResponseWriter) (bool, string) {
	if contentEncoding := httpWriter.Header().Get(HEADER_ContentEncoding); contentEncoding != "" {
		return false, ""
	}
	header := httpRequest.Header.Get(HEADER_AcceptEncoding)
	gi := strings.Index(header, ENCODING_GZIP)
	zi := strings.Index(header, ENCODING_DEFLATE)
	// use in order of appearance
	if gi == -1 {
		return zi != -1, ENCODING_DEFLATE
	} else if zi == -1 {
		return gi != -1, ENCODING_GZIP
	} else {
		if gi < zi {
			return true, ENCODING_GZIP
		}
		return true, ENCODING_DEFLATE
	}
}

// NewCompressingResponseWriter create a CompressingResponseWriter for a known encoding = {gzip,deflate}
func NewCompressingResponseWriter(httpWriter http.ResponseWriter, encoding string) (*CompressingResponseWriter, error) {
	httpWriter.Header().Set(HEADER_ContentEncoding, encoding)
	c := new(CompressingResponseWriter)
	c.writer = httpWriter
	var err error
	if ENCODING_GZIP == encoding {
		w := currentCompressorProvider.AcquireGzipWriter()
		w.Reset(httpWriter)
		c.compressor = w
		c.encoding = ENCODING_GZIP
	} else if ENCODING_DEFLATE == encoding {
		w := currentCompressorProvider.AcquireZlibWriter()
		w.Reset(httpWriter)
		c.compressor = w
		c.encoding = ENCODING_DEFLATE
	} else {
		return nil, errors.New("Unknown encoding:" + encoding)
	}
	return c, err
}
