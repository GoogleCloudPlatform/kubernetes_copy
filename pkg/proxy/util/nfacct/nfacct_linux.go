//go:build linux
// +build linux

/*
Copyright 2024 The Kubernetes Authors.

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

package nfacct

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/pkg/proxy/util"
)

// MaxLength represents the maximum length allowed for the name in a nfacct counter.
const MaxLength = 31

// nf netlink nfacct commands, these should strictly match with the ones defined in kernel headers.
// (definition: https://github.com/torvalds/linux/blob/v6.7/include/uapi/linux/netfilter/nfnetlink_acct.h#L9-L16)
const (
	// NFNL_MSG_ACCT_NEW
	cmdNew = 0
	// NFNL_MSG_ACCT_GET
	cmdGet = 1
)

// nf netlink nfacct attribute, these should strictly match with the ones defined in kernel headers.
// (definition: https://github.com/torvalds/linux/blob/v6.7/include/uapi/linux/netfilter/nfnetlink_acct.h#L24-L35)
const (
	// NFACCT_NAME
	attrName = 1
	// NFACCT_PKTS
	attrPackets = 2
	// NFACCT_BYTES
	attrBytes = 3
)

// runner implements the Interface and depends on the handler for execution.
type runner struct {
	handler handler
}

// New returns a new Interface. If the netfilter_nfacct subsystem is
// not available in the kernel it will return error.
func New() (Interface, error) {
	hndlr, err := newNetlinkHandler()
	if err != nil {
		return nil, err
	}

	rnr, err := newInternal(hndlr)
	if err != nil {
		return nil, err
	}

	// check if nfacct is supported on the current kernel by attempting to retrieve a counter.
	// the following GET call should either succeed or return ENOENT.
	_, err = rnr.Get("IMayExist")
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return nil, ErrNotSupported
	}
	return rnr, nil
}

// newInternal returns a new Interface with the given handler.
func newInternal(hndlr handler) (Interface, error) {
	return &runner{handler: hndlr}, nil

}

// Ensure is part of the interface.
func (r *runner) Ensure(name string) error {
	counter, err := r.Get(name)
	if counter != nil {
		return nil
	}

	if err != nil && errors.Is(err, ErrObjectNotFound) {
		return handleError(r.Add(name))
	} else if err != nil {
		return handleError(err)
	} else {
		return ErrUnexpected
	}
}

// Add is part of the interface.
func (r *runner) Add(name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if len(name) > MaxLength {
		return ErrNameExceedsMaxLength
	}

	req := r.handler.newRequest(cmdNew, unix.NLM_F_REQUEST|unix.NLM_F_CREATE|unix.NLM_F_ACK)
	req.AddData(nl.NewRtAttr(attrName, nl.ZeroTerminated(name)))
	_, err := req.Execute(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return handleError(err)
	}
	return nil
}

// Get is part of the interface.
func (r *runner) Get(name string) (*Counter, error) {
	if len(name) > MaxLength {
		return nil, ErrNameExceedsMaxLength
	}

	req := r.handler.newRequest(cmdGet, unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	req.AddData(nl.NewRtAttr(attrName, nl.ZeroTerminated(name)))
	msgs, err := req.Execute(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return nil, handleError(err)
	}

	var counter *Counter
	for _, msg := range msgs {
		counter, err = decode(msg, true)
		if err != nil {
			return nil, handleError(err)
		}
	}
	return counter, nil
}

// List is part of the interface.
func (r *runner) List() ([]*Counter, error) {
	var err error
	var msgs [][]byte
	err = retry.OnError(util.MaxAttemptsEINTR, util.ShouldRetryOnEINTR, func() error {
		req := r.handler.newRequest(cmdGet, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
		msgs, err = req.Execute(unix.NETLINK_NETFILTER, 0)
		return err
	})

	if err != nil && !errors.Is(err, unix.EINTR) {
		return nil, handleError(err)
	}

	counters := make([]*Counter, 0)
	for _, msg := range msgs {
		counter, err := decode(msg, true)
		if err != nil {
			return nil, handleError(err)
		}
		counters = append(counters, counter)
	}
	return counters, err
}

var ErrObjectNotFound = errors.New("object not found")
var ErrObjectAlreadyExists = errors.New("object already exists")
var ErrNameExceedsMaxLength = fmt.Errorf("object name exceeds the maximum allowed length of %d characters", MaxLength)
var ErrEmptyName = errors.New("object name cannot be empty")
var ErrUnexpected = errors.New("unexpected error")
var ErrNotSupported = errors.New("nfacct sub-system not available")

func handleError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ENOENT):
		return ErrObjectNotFound
	case errors.Is(err, syscall.EBUSY):
		return ErrObjectAlreadyExists
	default:
		return fmt.Errorf("%s: %s", ErrUnexpected.Error(), err.Error())
	}
}

// decode function processes a byte stream, requiring the 'strict' parameter to be true in production and
// false only for testing purposes. If in strict mode and any of the relevant attributes (name, packets, or bytes)
// have not been processed, an error is returned indicating a failure to decode the byte stream.
//
// Parse the netlink message as per the documentation outlined in:
// https://docs.kernel.org/userspace-api/netlink/intro.html
//
// Message Components:
//   - netfilter generic message [4 bytes]
//     struct nfgenmsg (definition: https://github.com/torvalds/linux/blob/v6.7/include/uapi/linux/netfilter/nfnetlink.h#L32-L38)
//   - attributes [variable-sized, must align to 4 bytes from the start of attribute]
//     struct nlattr (definition: https://github.com/torvalds/linux/blob/v6.7/include/uapi/linux/netlink.h#L220-L232)
//
// Attribute Components:
//   - length [2 bytes]
//     length includes bytes for defining the length itself, bytes for defining the type,
//     and the actual bytes of data without any padding.
//   - type [2 bytes]
//   - data [variable-sized]
//   - padding [optional]
//
// Example. Counter{Name: "dummy-metric", Packets: 123, Bytes: 54321} in netlink message:
//
//	struct nfgenmsg{
//	    __u8  nfgen_family: AF_NETLINK
//	    __u8    version:    nl.NFNETLINK_V0
//	    __be16  res_id:     nl.NFNETLINK_V0
//	}
//
//	struct nlattr{
//	    __u16 nla_len:      13
//	    __u16 nla_type:     NFACCT_NAME
//	    char data:          dummy-metric\0
//	}
//
//	(padding:)
//	    data:               \0\0\0
//
//	struct nlattr{
//	    __u16 nla_len:      12
//	    __u16 nla_type:     NFACCT_PKTS
//	    __u64: data:        123
//	}
//
//	struct nlattr{
//	    __u16 nla_len:      12
//	    __u16 nla_type:     NFACCT_BYTES
//	    __u64: data:        54321
//	}
func decode(msg []byte, strict bool) (*Counter, error) {
	counter := &Counter{}
	reader := bytes.NewReader(msg)
	// skip the first 4 bytes (netfilter generic message).
	if _, err := reader.Seek(nl.SizeofNfgenmsg, io.SeekCurrent); err != nil {
		return nil, err
	}

	// attrsProcessed tracks the number of processed attributes.
	var attrsProcessed int

	// length and type of netlink attribute.
	var length, attrType uint16

	// now we are just left with the attributes(struct nlattr) after skipping netlink generic
	// message; we iterate over all the attributes one by one to construct our Counter object.
	for reader.Len() > 0 {
		// netlink attributes are in LTV(length, type and value) format.

		// STEP 1. parse length [2 bytes]
		if err := binary.Read(reader, binary.NativeEndian, &length); err != nil {
			return nil, err
		}

		// STEP 2. parse type   [2 bytes]
		if err := binary.Read(reader, binary.NativeEndian, &attrType); err != nil {
			return nil, err
		}

		// STEP 3. adjust the length
		// adjust the length to consider the header bytes read in step(1) and step(2); the actual
		// length of data will be 4 bytes less than the originally read value.
		length -= 4

		// STEP 4. parse value  [variable sized]
		// The value can assume any data-type. To read it into the appropriate data structure, we need
		// to know the data type in advance. We achieve this by switching on the attribute-type, and we
		// allocate the 'adjusted length' bytes (as done in step(3)) for the data-structure.
		switch attrType {
		case attrName:
			// NFACCT_NAME has a variable size, so we allocate a slice of 'adjusted length' bytes
			// and read the next 'adjusted length' bytes into this slice.
			data := make([]byte, length)
			if err := binary.Read(reader, binary.NativeEndian, data); err != nil {
				return nil, err
			}
			counter.Name = string(data[:length-1])
			attrsProcessed++
		case attrPackets:
			// NFACCT_PKTS holds 8 bytes of data, so we directly read the next 8 bytes into a 64-bit
			// unsigned integer (counter.Packets).
			if err := binary.Read(reader, binary.BigEndian, &counter.Packets); err != nil {
				return nil, err
			}
			attrsProcessed++
		case attrBytes:
			// NFACCT_BYTES holds 8 bytes of data, so we directly read the next 8 bytes into a 64-bit
			// unsigned integer (counter.Bytes).
			if err := binary.Read(reader, binary.BigEndian, &counter.Bytes); err != nil {
				return nil, err
			}
			attrsProcessed++
		default:
			// skip the data part for unknown attribute
			if _, err := reader.Seek(int64(length), io.SeekCurrent); err != nil {
				return nil, err
			}
		}

		// Move past the padding to align with the fixed-size length, always a multiple of 4.
		// If, for instance, the length is 9, skip 3 bytes of padding to reach the start of
		// the next attribute.
		// (ref: https://github.com/torvalds/linux/blob/v6.7/include/uapi/linux/netlink.h#L220-L227)
		if length%4 != 0 {
			padding := 4 - length%4
			if _, err := reader.Seek(int64(padding), io.SeekCurrent); err != nil {
				return nil, err
			}
		}
	}

	// return err if any of the required attribute is not processed.
	if strict && attrsProcessed != 3 {
		return nil, errors.New("failed to decode byte-stream")
	}
	return counter, nil
}
