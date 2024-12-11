// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.

package zstd

import (
	"errors"
	"io"
	"sync"
)

// ZipMethodWinZip is the method for Zstandard compressed data inside Zip files for WinZip.
// See https://www.winzip.com/win/en/comp_info.html
const ZipMethodWinZip = 93

// ZipMethodPKWare is the original method number used by PKWARE to indicate Zstandard compression.
// Deprecated: This has been deprecated by PKWARE, use ZipMethodWinZip instead for compression.
// See https://pkware.cachefly.net/webdocs/APPNOTE/APPNOTE-6.3.9.TXT
const ZipMethodPKWare = 20

// zipReaderPool is the default reader pool.
var zipReaderPool = sync.Pool{New: func() interface{} {
	z, err := NewReader(nil, WithDecoderLowmem(true), WithDecoderMaxWindow(128<<20), WithDecoderConcurrency(1))
	if err != nil {
		panic(err)
	}
	return z
}}

// newZipReader creates a pooled zip decompressor.
func newZipReader(opts ...DOption) func(r io.Reader) io.ReadCloser {
	pool := &zipReaderPool
	if len(opts) > 0 {
		opts = append([]DOption{WithDecoderLowmem(true), WithDecoderMaxWindow(128 << 20)}, opts...)
		// Force concurrency 1
		opts = append(opts, WithDecoderConcurrency(1))
		// Create our own pool
		pool = &sync.Pool{}
	}
	return func(r io.Reader) io.ReadCloser {
		dec, ok := pool.Get().(*Decoder)
		if ok {
			dec.Reset(r)
		} else {
			d, err := NewReader(r, opts...)
			if err != nil {
				panic(err)
			}
			dec = d
		}
		return &pooledZipReader{dec: dec, pool: pool}
	}
}

type pooledZipReader struct {
	mu   sync.Mutex // guards Close and Read
	pool *sync.Pool
	dec  *Decoder
}

func (r *pooledZipReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dec == nil {
		return 0, errors.New("read after close or EOF")
	}
	dec, err := r.dec.Read(p)
	if err == io.EOF {
		r.dec.Reset(nil)
		r.pool.Put(r.dec)
		r.dec = nil
	}
	return dec, err
}

func (r *pooledZipReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.dec != nil {
		err = r.dec.Reset(nil)
		r.pool.Put(r.dec)
		r.dec = nil
	}
	return err
}

type pooledZipWriter struct {
	mu   sync.Mutex // guards Close and Read
	enc  *Encoder
	pool *sync.Pool
}

func (w *pooledZipWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enc == nil {
		return 0, errors.New("Write after Close")
	}
	return w.enc.Write(p)
}

func (w *pooledZipWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.enc != nil {
		err = w.enc.Close()
		w.pool.Put(w.enc)
		w.enc = nil
	}
	return err
}

// ZipCompressor returns a compressor that can be registered with zip libraries.
// The provided encoder options will be used on all encodes.
func ZipCompressor(opts ...EOption) func(w io.Writer) (io.WriteCloser, error) {
	var pool sync.Pool
	return func(w io.Writer) (io.WriteCloser, error) {
		enc, ok := pool.Get().(*Encoder)
		if ok {
			enc.Reset(w)
		} else {
			var err error
			enc, err = NewWriter(w, opts...)
			if err != nil {
				return nil, err
			}
		}
		return &pooledZipWriter{enc: enc, pool: &pool}, nil
	}
}

// ZipDecompressor returns a decompressor that can be registered with zip libraries.
// See ZipCompressor for example.
// Options can be specified. WithDecoderConcurrency(1) is forced,
// and by default a 128MB maximum decompression window is specified.
// The window size can be overridden if required.
func ZipDecompressor(opts ...DOption) func(r io.Reader) io.ReadCloser {
	return newZipReader(opts...)
}
