// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wal

import (
	"encoding/binary"
	"hash"
	"io"
	"os"
	"sync"
	"time"

	"go.etcd.io/etcd/pkg/v3/crc"
	"go.etcd.io/etcd/pkg/v3/ioutil"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
)

// walPageBytes is the alignment for flushing records to the backing Writer.
// It should be a multiple of the minimum sector size so that WAL can safely
// distinguish between torn writes and ordinary data corruption.
const walPageBytes = 8 * minSectorSize

type encoder struct {
	mu sync.Mutex
	bw *ioutil.PageWriter

	crc       hash.Hash32
	buf       []byte
	uint64buf []byte
}

func newEncoder(w io.Writer, prevCrc uint32, pageOffset int) *encoder {
	return &encoder{
		bw:  ioutil.NewPageWriter(w, walPageBytes, pageOffset),
		crc: crc.New(prevCrc, crcTable),
		// 1MB buffer
		buf:       make([]byte, 1024*1024),
		uint64buf: make([]byte, 8),
	}
}

// newFileEncoder creates a new encoder with current file offset for the page writer.
func newFileEncoder(f *os.File, prevCrc uint32) (*encoder, error) {
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	return newEncoder(f, prevCrc, int(offset)), nil
}

func (e *encoder) encode(rec *walpb.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.crc.Write(rec.Data)
	rec.Crc = e.crc.Sum32()
	var (
		data []byte
		err  error
		n    int
	)

	if rec.Size() > len(e.buf) {
		data, err = rec.Marshal()
		if err != nil {
			return err
		}
	} else {
		n, err = rec.MarshalTo(e.buf)
		if err != nil {
			return err
		}
		data = e.buf[:n]
	}

	data, lenField := prepareDataWithPadding(data)

	return write(e.bw, e.uint64buf, data, lenField)
}

func encodeFrameSize(dataBytes int) (lenField uint64, padBytes int) {
	lenField = uint64(dataBytes)
	// force 8 byte alignment so length never gets a torn write
	padBytes = (8 - (dataBytes % 8)) % 8
	if padBytes != 0 {
		lenField |= uint64(0x80|padBytes) << 56
	}
	return lenField, padBytes
}

func (e *encoder) flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bw.Flush()
}

func prepareDataWithPadding(data []byte) ([]byte, uint64) {
	lenField, padBytes := encodeFrameSize(len(data))
	if padBytes != 0 {
		data = append(data, make([]byte, padBytes)...)
	}
	return data, lenField
}

func write(w io.Writer, uint64buf, data []byte, lenField uint64) error {
	// write padding info
	binary.LittleEndian.PutUint64(uint64buf, lenField)

	start := time.Now()
	nv, err := w.Write(uint64buf)
	walWriteBytes.Add(float64(nv))
	if err != nil {
		return err
	}

	// write the record with padding
	n, err := w.Write(data)
	walWriteSec.Observe(time.Since(start).Seconds())
	walWriteBytes.Add(float64(n))
	return err
}
