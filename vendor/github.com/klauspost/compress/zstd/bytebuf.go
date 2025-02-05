// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"fmt"
	"io"
)

type byteBuffer interface {
	// Read up to 8 bytes.
	// Returns io.ErrUnexpectedEOF if this cannot be satisfied.
	readSmall(n int) ([]byte, error)

	// Read >8 bytes.
	// MAY use the destination slice.
	readBig(n int, dst []byte) ([]byte, error)

	// Read a single byte.
	readByte() (byte, error)

	// Skip n bytes.
	skipN(n int64) error
}

// in-memory buffer
type byteBuf []byte

func (b *byteBuf) readSmall(n int) ([]byte, error) {
	if debugAsserts && n > 8 {
		panic(fmt.Errorf("small read > 8 (%d). use readBig", n))
	}
	bb := *b
	if len(bb) < n {
		return nil, io.ErrUnexpectedEOF
	}
	r := bb[:n]
	*b = bb[n:]
	return r, nil
}

func (b *byteBuf) readBig(n int, dst []byte) ([]byte, error) {
	bb := *b
	if len(bb) < n {
		return nil, io.ErrUnexpectedEOF
	}
	r := bb[:n]
	*b = bb[n:]
	return r, nil
}

func (b *byteBuf) readByte() (byte, error) {
	bb := *b
	if len(bb) < 1 {
		return 0, io.ErrUnexpectedEOF
	}
	r := bb[0]
	*b = bb[1:]
	return r, nil
}

func (b *byteBuf) skipN(n int64) error {
	bb := *b
	if n < 0 {
		return fmt.Errorf("negative skip (%d) requested", n)
	}
	if int64(len(bb)) < n {
		return io.ErrUnexpectedEOF
	}
	*b = bb[n:]
	return nil
}

// wrapper around a reader.
type readerWrapper struct {
	r   io.Reader
	tmp [8]byte
}

func (r *readerWrapper) readSmall(n int) ([]byte, error) {
	if debugAsserts && n > 8 {
		panic(fmt.Errorf("small read > 8 (%d). use readBig", n))
	}
	n2, err := io.ReadFull(r.r, r.tmp[:n])
	// We only really care about the actual bytes read.
	if err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		if debugDecoder {
			println("readSmall: got", n2, "want", n, "err", err)
		}
		return nil, err
	}
	return r.tmp[:n], nil
}

func (r *readerWrapper) readBig(n int, dst []byte) ([]byte, error) {
	if cap(dst) < n {
		dst = make([]byte, n)
	}
	n2, err := io.ReadFull(r.r, dst[:n])
	if err == io.EOF && n > 0 {
		err = io.ErrUnexpectedEOF
	}
	return dst[:n2], err
}

func (r *readerWrapper) readByte() (byte, error) {
	n2, err := io.ReadFull(r.r, r.tmp[:1])
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if n2 != 1 {
		return 0, io.ErrUnexpectedEOF
	}
	return r.tmp[0], nil
}

func (r *readerWrapper) skipN(n int64) error {
	n2, err := io.CopyN(io.Discard, r.r, n)
	if n2 != n {
		err = io.ErrUnexpectedEOF
	}
	return err
}
