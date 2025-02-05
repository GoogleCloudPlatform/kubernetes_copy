// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"context"
	"encoding/binary"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd/internal/xxhash"
)

// Decoder provides decoding of zstandard streams.
// The decoder has been designed to operate without allocations after a warmup.
// This means that you should store the decoder for best performance.
// To re-use a stream decoder, use the Reset(r io.Reader) error to switch to another stream.
// A decoder can safely be re-used even if the previous stream failed.
// To release the resources, you must call the Close() function on a decoder.
type Decoder struct {
	o decoderOptions

	// Unreferenced decoders, ready for use.
	decoders chan *blockDec

	// Current read position used for Reader functionality.
	current decoderState

	// sync stream decoding
	syncStream struct {
		decodedFrame uint64
		br           readerWrapper
		enabled      bool
		inFrame      bool
		dstBuf       []byte
	}

	frame *frameDec

	// Custom dictionaries.
	dicts map[uint32]*dict

	// streamWg is the waitgroup for all streams
	streamWg sync.WaitGroup
}

// decoderState is used for maintaining state when the decoder
// is used for streaming.
type decoderState struct {
	// current block being written to stream.
	decodeOutput

	// output in order to be written to stream.
	output chan decodeOutput

	// cancel remaining output.
	cancel context.CancelFunc

	// crc of current frame
	crc *xxhash.Digest

	flushed bool
}

var (
	// Check the interfaces we want to support.
	_ = io.WriterTo(&Decoder{})
	_ = io.Reader(&Decoder{})
)

// NewReader creates a new decoder.
// A nil Reader can be provided in which case Reset can be used to start a decode.
//
// A Decoder can be used in two modes:
//
// 1) As a stream, or
// 2) For stateless decoding using DecodeAll.
//
// Only a single stream can be decoded concurrently, but the same decoder
// can run multiple concurrent stateless decodes. It is even possible to
// use stateless decodes while a stream is being decoded.
//
// The Reset function can be used to initiate a new stream, which will considerably
// reduce the allocations normally caused by NewReader.
func NewReader(r io.Reader, opts ...DOption) (*Decoder, error) {
	initPredefined()
	var d Decoder
	d.o.setDefault()
	for _, o := range opts {
		err := o(&d.o)
		if err != nil {
			return nil, err
		}
	}
	d.current.crc = xxhash.New()
	d.current.flushed = true

	if r == nil {
		d.current.err = ErrDecoderNilInput
	}

	// Transfer option dicts.
	d.dicts = make(map[uint32]*dict, len(d.o.dicts))
	for _, dc := range d.o.dicts {
		d.dicts[dc.id] = dc
	}
	d.o.dicts = nil

	// Create decoders
	d.decoders = make(chan *blockDec, d.o.concurrent)
	for i := 0; i < d.o.concurrent; i++ {
		dec := newBlockDec(d.o.lowMem)
		dec.localFrame = newFrameDec(d.o)
		d.decoders <- dec
	}

	if r == nil {
		return &d, nil
	}
	return &d, d.Reset(r)
}

// Read bytes from the decompressed stream into p.
// Returns the number of bytes written and any error that occurred.
// When the stream is done, io.EOF will be returned.
func (d *Decoder) Read(p []byte) (int, error) {
	var n int
	for {
		if len(d.current.b) > 0 {
			filled := copy(p, d.current.b)
			p = p[filled:]
			d.current.b = d.current.b[filled:]
			n += filled
		}
		if len(p) == 0 {
			break
		}
		if len(d.current.b) == 0 {
			// We have an error and no more data
			if d.current.err != nil {
				break
			}
			if !d.nextBlock(n == 0) {
				return n, d.current.err
			}
		}
	}
	if len(d.current.b) > 0 {
		if debugDecoder {
			println("returning", n, "still bytes left:", len(d.current.b))
		}
		// Only return error at end of block
		return n, nil
	}
	if d.current.err != nil {
		d.drainOutput()
	}
	if debugDecoder {
		println("returning", n, d.current.err, len(d.decoders))
	}
	return n, d.current.err
}

// Reset will reset the decoder the supplied stream after the current has finished processing.
// Note that this functionality cannot be used after Close has been called.
// Reset can be called with a nil reader to release references to the previous reader.
// After being called with a nil reader, no other operations than Reset or DecodeAll or Close
// should be used.
func (d *Decoder) Reset(r io.Reader) error {
	if d.current.err == ErrDecoderClosed {
		return d.current.err
	}

	d.drainOutput()

	d.syncStream.br.r = nil
	if r == nil {
		d.current.err = ErrDecoderNilInput
		if len(d.current.b) > 0 {
			d.current.b = d.current.b[:0]
		}
		d.current.flushed = true
		return nil
	}

	// If bytes buffer and < 5MB, do sync decoding anyway.
	if bb, ok := r.(byter); ok && bb.Len() < d.o.decodeBufsBelow && !d.o.limitToCap {
		bb2 := bb
		if debugDecoder {
			println("*bytes.Buffer detected, doing sync decode, len:", bb.Len())
		}
		b := bb2.Bytes()
		var dst []byte
		if cap(d.syncStream.dstBuf) > 0 {
			dst = d.syncStream.dstBuf[:0]
		}

		dst, err := d.DecodeAll(b, dst)
		if err == nil {
			err = io.EOF
		}
		// Save output buffer
		d.syncStream.dstBuf = dst
		d.current.b = dst
		d.current.err = err
		d.current.flushed = true
		if debugDecoder {
			println("sync decode to", len(dst), "bytes, err:", err)
		}
		return nil
	}
	// Remove current block.
	d.stashDecoder()
	d.current.decodeOutput = decodeOutput{}
	d.current.err = nil
	d.current.flushed = false
	d.current.d = nil
	d.syncStream.dstBuf = nil

	// Ensure no-one else is still running...
	d.streamWg.Wait()
	if d.frame == nil {
		d.frame = newFrameDec(d.o)
	}

	if d.o.concurrent == 1 {
		return d.startSyncDecoder(r)
	}

	d.current.output = make(chan decodeOutput, d.o.concurrent)
	ctx, cancel := context.WithCancel(context.Background())
	d.current.cancel = cancel
	d.streamWg.Add(1)
	go d.startStreamDecoder(ctx, r, d.current.output)

	return nil
}

// drainOutput will drain the output until errEndOfStream is sent.
func (d *Decoder) drainOutput() {
	if d.current.cancel != nil {
		if debugDecoder {
			println("cancelling current")
		}
		d.current.cancel()
		d.current.cancel = nil
	}
	if d.current.d != nil {
		if debugDecoder {
			printf("re-adding current decoder %p, decoders: %d", d.current.d, len(d.decoders))
		}
		d.decoders <- d.current.d
		d.current.d = nil
		d.current.b = nil
	}
	if d.current.output == nil || d.current.flushed {
		println("current already flushed")
		return
	}
	for v := range d.current.output {
		if v.d != nil {
			if debugDecoder {
				printf("re-adding decoder %p", v.d)
			}
			d.decoders <- v.d
		}
	}
	d.current.output = nil
	d.current.flushed = true
}

// WriteTo writes data to w until there's no more data to write or when an error occurs.
// The return value n is the number of bytes written.
// Any error encountered during the write is also returned.
func (d *Decoder) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for {
		if len(d.current.b) > 0 {
			n2, err2 := w.Write(d.current.b)
			n += int64(n2)
			if err2 != nil && (d.current.err == nil || d.current.err == io.EOF) {
				d.current.err = err2
			} else if n2 != len(d.current.b) {
				d.current.err = io.ErrShortWrite
			}
		}
		if d.current.err != nil {
			break
		}
		d.nextBlock(true)
	}
	err := d.current.err
	if err != nil {
		d.drainOutput()
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}

// DecodeAll allows stateless decoding of a blob of bytes.
// Output will be appended to dst, so if the destination size is known
// you can pre-allocate the destination slice to avoid allocations.
// DecodeAll can be used concurrently.
// The Decoder concurrency limits will be respected.
func (d *Decoder) DecodeAll(input, dst []byte) ([]byte, error) {
	if d.decoders == nil {
		return dst, ErrDecoderClosed
	}

	// Grab a block decoder and frame decoder.
	block := <-d.decoders
	frame := block.localFrame
	initialSize := len(dst)
	defer func() {
		if debugDecoder {
			printf("re-adding decoder: %p", block)
		}
		frame.rawInput = nil
		frame.bBuf = nil
		if frame.history.decoders.br != nil {
			frame.history.decoders.br.in = nil
		}
		d.decoders <- block
	}()
	frame.bBuf = input

	for {
		frame.history.reset()
		err := frame.reset(&frame.bBuf)
		if err != nil {
			if err == io.EOF {
				if debugDecoder {
					println("frame reset return EOF")
				}
				return dst, nil
			}
			return dst, err
		}
		if err = d.setDict(frame); err != nil {
			return nil, err
		}
		if frame.WindowSize > d.o.maxWindowSize {
			if debugDecoder {
				println("window size exceeded:", frame.WindowSize, ">", d.o.maxWindowSize)
			}
			return dst, ErrWindowSizeExceeded
		}
		if frame.FrameContentSize != fcsUnknown {
			if frame.FrameContentSize > d.o.maxDecodedSize-uint64(len(dst)-initialSize) {
				if debugDecoder {
					println("decoder size exceeded; fcs:", frame.FrameContentSize, "> mcs:", d.o.maxDecodedSize-uint64(len(dst)-initialSize), "len:", len(dst))
				}
				return dst, ErrDecoderSizeExceeded
			}
			if d.o.limitToCap && frame.FrameContentSize > uint64(cap(dst)-len(dst)) {
				if debugDecoder {
					println("decoder size exceeded; fcs:", frame.FrameContentSize, "> (cap-len)", cap(dst)-len(dst))
				}
				return dst, ErrDecoderSizeExceeded
			}
			if cap(dst)-len(dst) < int(frame.FrameContentSize) {
				dst2 := make([]byte, len(dst), len(dst)+int(frame.FrameContentSize)+compressedBlockOverAlloc)
				copy(dst2, dst)
				dst = dst2
			}
		}

		if cap(dst) == 0 && !d.o.limitToCap {
			// Allocate len(input) * 2 by default if nothing is provided
			// and we didn't get frame content size.
			size := len(input) * 2
			// Cap to 1 MB.
			if size > 1<<20 {
				size = 1 << 20
			}
			if uint64(size) > d.o.maxDecodedSize {
				size = int(d.o.maxDecodedSize)
			}
			dst = make([]byte, 0, size)
		}

		dst, err = frame.runDecoder(dst, block)
		if err != nil {
			return dst, err
		}
		if uint64(len(dst)-initialSize) > d.o.maxDecodedSize {
			return dst, ErrDecoderSizeExceeded
		}
		if len(frame.bBuf) == 0 {
			if debugDecoder {
				println("frame dbuf empty")
			}
			break
		}
	}
	return dst, nil
}

// nextBlock returns the next block.
// If an error occurs d.err will be set.
// Optionally the function can block for new output.
// If non-blocking mode is used the returned boolean will be false
// if no data was available without blocking.
func (d *Decoder) nextBlock(blocking bool) (ok bool) {
	if d.current.err != nil {
		// Keep error state.
		return false
	}
	d.current.b = d.current.b[:0]

	// SYNC:
	if d.syncStream.enabled {
		if !blocking {
			return false
		}
		ok = d.nextBlockSync()
		if !ok {
			d.stashDecoder()
		}
		return ok
	}

	//ASYNC:
	d.stashDecoder()
	if blocking {
		d.current.decodeOutput, ok = <-d.current.output
	} else {
		select {
		case d.current.decodeOutput, ok = <-d.current.output:
		default:
			return false
		}
	}
	if !ok {
		// This should not happen, so signal error state...
		d.current.err = io.ErrUnexpectedEOF
		return false
	}
	next := d.current.decodeOutput
	if next.d != nil && next.d.async.newHist != nil {
		d.current.crc.Reset()
	}
	if debugDecoder {
		var tmp [4]byte
		binary.LittleEndian.PutUint32(tmp[:], uint32(xxhash.Sum64(next.b)))
		println("got", len(d.current.b), "bytes, error:", d.current.err, "data crc:", tmp)
	}

	if d.o.ignoreChecksum {
		return true
	}

	if len(next.b) > 0 {
		d.current.crc.Write(next.b)
	}
	if next.err == nil && next.d != nil && next.d.hasCRC {
		got := uint32(d.current.crc.Sum64())
		if got != next.d.checkCRC {
			if debugDecoder {
				printf("CRC Check Failed: %08x (got) != %08x (on stream)\n", got, next.d.checkCRC)
			}
			d.current.err = ErrCRCMismatch
		} else {
			if debugDecoder {
				printf("CRC ok %08x\n", got)
			}
		}
	}

	return true
}

func (d *Decoder) nextBlockSync() (ok bool) {
	if d.current.d == nil {
		d.current.d = <-d.decoders
	}
	for len(d.current.b) == 0 {
		if !d.syncStream.inFrame {
			d.frame.history.reset()
			d.current.err = d.frame.reset(&d.syncStream.br)
			if d.current.err == nil {
				d.current.err = d.setDict(d.frame)
			}
			if d.current.err != nil {
				return false
			}
			if d.frame.WindowSize > d.o.maxDecodedSize || d.frame.WindowSize > d.o.maxWindowSize {
				d.current.err = ErrDecoderSizeExceeded
				return false
			}

			d.syncStream.decodedFrame = 0
			d.syncStream.inFrame = true
		}
		d.current.err = d.frame.next(d.current.d)
		if d.current.err != nil {
			return false
		}
		d.frame.history.ensureBlock()
		if debugDecoder {
			println("History trimmed:", len(d.frame.history.b), "decoded already:", d.syncStream.decodedFrame)
		}
		histBefore := len(d.frame.history.b)
		d.current.err = d.current.d.decodeBuf(&d.frame.history)

		if d.current.err != nil {
			println("error after:", d.current.err)
			return false
		}
		d.current.b = d.frame.history.b[histBefore:]
		if debugDecoder {
			println("history after:", len(d.frame.history.b))
		}

		// Check frame size (before CRC)
		d.syncStream.decodedFrame += uint64(len(d.current.b))
		if d.syncStream.decodedFrame > d.frame.FrameContentSize {
			if debugDecoder {
				printf("DecodedFrame (%d) > FrameContentSize (%d)\n", d.syncStream.decodedFrame, d.frame.FrameContentSize)
			}
			d.current.err = ErrFrameSizeExceeded
			return false
		}

		// Check FCS
		if d.current.d.Last && d.frame.FrameContentSize != fcsUnknown && d.syncStream.decodedFrame != d.frame.FrameContentSize {
			if debugDecoder {
				printf("DecodedFrame (%d) != FrameContentSize (%d)\n", d.syncStream.decodedFrame, d.frame.FrameContentSize)
			}
			d.current.err = ErrFrameSizeMismatch
			return false
		}

		// Update/Check CRC
		if d.frame.HasCheckSum {
			if !d.o.ignoreChecksum {
				d.frame.crc.Write(d.current.b)
			}
			if d.current.d.Last {
				if !d.o.ignoreChecksum {
					d.current.err = d.frame.checkCRC()
				} else {
					d.current.err = d.frame.consumeCRC()
				}
				if d.current.err != nil {
					println("CRC error:", d.current.err)
					return false
				}
			}
		}
		d.syncStream.inFrame = !d.current.d.Last
	}
	return true
}

func (d *Decoder) stashDecoder() {
	if d.current.d != nil {
		if debugDecoder {
			printf("re-adding current decoder %p", d.current.d)
		}
		d.decoders <- d.current.d
		d.current.d = nil
	}
}

// Close will release all resources.
// It is NOT possible to reuse the decoder after this.
func (d *Decoder) Close() {
	if d.current.err == ErrDecoderClosed {
		return
	}
	d.drainOutput()
	if d.current.cancel != nil {
		d.current.cancel()
		d.streamWg.Wait()
		d.current.cancel = nil
	}
	if d.decoders != nil {
		close(d.decoders)
		for dec := range d.decoders {
			dec.Close()
		}
		d.decoders = nil
	}
	if d.current.d != nil {
		d.current.d.Close()
		d.current.d = nil
	}
	d.current.err = ErrDecoderClosed
}

// IOReadCloser returns the decoder as an io.ReadCloser for convenience.
// Any changes to the decoder will be reflected, so the returned ReadCloser
// can be reused along with the decoder.
// io.WriterTo is also supported by the returned ReadCloser.
func (d *Decoder) IOReadCloser() io.ReadCloser {
	return closeWrapper{d: d}
}

// closeWrapper wraps a function call as a closer.
type closeWrapper struct {
	d *Decoder
}

// WriteTo forwards WriteTo calls to the decoder.
func (c closeWrapper) WriteTo(w io.Writer) (n int64, err error) {
	return c.d.WriteTo(w)
}

// Read forwards read calls to the decoder.
func (c closeWrapper) Read(p []byte) (n int, err error) {
	return c.d.Read(p)
}

// Close closes the decoder.
func (c closeWrapper) Close() error {
	c.d.Close()
	return nil
}

type decodeOutput struct {
	d   *blockDec
	b   []byte
	err error
}

func (d *Decoder) startSyncDecoder(r io.Reader) error {
	d.frame.history.reset()
	d.syncStream.br = readerWrapper{r: r}
	d.syncStream.inFrame = false
	d.syncStream.enabled = true
	d.syncStream.decodedFrame = 0
	return nil
}

// Create Decoder:
// ASYNC:
// Spawn 3 go routines.
// 0: Read frames and decode block literals.
// 1: Decode sequences.
// 2: Execute sequences, send to output.
func (d *Decoder) startStreamDecoder(ctx context.Context, r io.Reader, output chan decodeOutput) {
	defer d.streamWg.Done()
	br := readerWrapper{r: r}

	var seqDecode = make(chan *blockDec, d.o.concurrent)
	var seqExecute = make(chan *blockDec, d.o.concurrent)

	// Async 1: Decode sequences...
	go func() {
		var hist history
		var hasErr bool

		for block := range seqDecode {
			if hasErr {
				if block != nil {
					seqExecute <- block
				}
				continue
			}
			if block.async.newHist != nil {
				if debugDecoder {
					println("Async 1: new history, recent:", block.async.newHist.recentOffsets)
				}
				hist.reset()
				hist.decoders = block.async.newHist.decoders
				hist.recentOffsets = block.async.newHist.recentOffsets
				hist.windowSize = block.async.newHist.windowSize
				if block.async.newHist.dict != nil {
					hist.setDict(block.async.newHist.dict)
				}
			}
			if block.err != nil || block.Type != blockTypeCompressed {
				hasErr = block.err != nil
				seqExecute <- block
				continue
			}

			hist.decoders.literals = block.async.literals
			block.err = block.prepareSequences(block.async.seqData, &hist)
			if debugDecoder && block.err != nil {
				println("prepareSequences returned:", block.err)
			}
			hasErr = block.err != nil
			if block.err == nil {
				block.err = block.decodeSequences(&hist)
				if debugDecoder && block.err != nil {
					println("decodeSequences returned:", block.err)
				}
				hasErr = block.err != nil
				//				block.async.sequence = hist.decoders.seq[:hist.decoders.nSeqs]
				block.async.seqSize = hist.decoders.seqSize
			}
			seqExecute <- block
		}
		close(seqExecute)
		hist.reset()
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	// Async 3: Execute sequences...
	frameHistCache := d.frame.history.b
	go func() {
		var hist history
		var decodedFrame uint64
		var fcs uint64
		var hasErr bool
		for block := range seqExecute {
			out := decodeOutput{err: block.err, d: block}
			if block.err != nil || hasErr {
				hasErr = true
				output <- out
				continue
			}
			if block.async.newHist != nil {
				if debugDecoder {
					println("Async 2: new history")
				}
				hist.reset()
				hist.windowSize = block.async.newHist.windowSize
				hist.allocFrameBuffer = block.async.newHist.allocFrameBuffer
				if block.async.newHist.dict != nil {
					hist.setDict(block.async.newHist.dict)
				}

				if cap(hist.b) < hist.allocFrameBuffer {
					if cap(frameHistCache) >= hist.allocFrameBuffer {
						hist.b = frameHistCache
					} else {
						hist.b = make([]byte, 0, hist.allocFrameBuffer)
						println("Alloc history sized", hist.allocFrameBuffer)
					}
				}
				hist.b = hist.b[:0]
				fcs = block.async.fcs
				decodedFrame = 0
			}
			do := decodeOutput{err: block.err, d: block}
			switch block.Type {
			case blockTypeRLE:
				if debugDecoder {
					println("add rle block length:", block.RLESize)
				}

				if cap(block.dst) < int(block.RLESize) {
					if block.lowMem {
						block.dst = make([]byte, block.RLESize)
					} else {
						block.dst = make([]byte, maxCompressedBlockSize)
					}
				}
				block.dst = block.dst[:block.RLESize]
				v := block.data[0]
				for i := range block.dst {
					block.dst[i] = v
				}
				hist.append(block.dst)
				do.b = block.dst
			case blockTypeRaw:
				if debugDecoder {
					println("add raw block length:", len(block.data))
				}
				hist.append(block.data)
				do.b = block.data
			case blockTypeCompressed:
				if debugDecoder {
					println("execute with history length:", len(hist.b), "window:", hist.windowSize)
				}
				hist.decoders.seqSize = block.async.seqSize
				hist.decoders.literals = block.async.literals
				do.err = block.executeSequences(&hist)
				hasErr = do.err != nil
				if debugDecoder && hasErr {
					println("executeSequences returned:", do.err)
				}
				do.b = block.dst
			}
			if !hasErr {
				decodedFrame += uint64(len(do.b))
				if decodedFrame > fcs {
					println("fcs exceeded", block.Last, fcs, decodedFrame)
					do.err = ErrFrameSizeExceeded
					hasErr = true
				} else if block.Last && fcs != fcsUnknown && decodedFrame != fcs {
					do.err = ErrFrameSizeMismatch
					hasErr = true
				} else {
					if debugDecoder {
						println("fcs ok", block.Last, fcs, decodedFrame)
					}
				}
			}
			output <- do
		}
		close(output)
		frameHistCache = hist.b
		wg.Done()
		if debugDecoder {
			println("decoder goroutines finished")
		}
		hist.reset()
	}()

	var hist history
decodeStream:
	for {
		var hasErr bool
		hist.reset()
		decodeBlock := func(block *blockDec) {
			if hasErr {
				if block != nil {
					seqDecode <- block
				}
				return
			}
			if block.err != nil || block.Type != blockTypeCompressed {
				hasErr = block.err != nil
				seqDecode <- block
				return
			}

			remain, err := block.decodeLiterals(block.data, &hist)
			block.err = err
			hasErr = block.err != nil
			if err == nil {
				block.async.literals = hist.decoders.literals
				block.async.seqData = remain
			} else if debugDecoder {
				println("decodeLiterals error:", err)
			}
			seqDecode <- block
		}
		frame := d.frame
		if debugDecoder {
			println("New frame...")
		}
		var historySent bool
		frame.history.reset()
		err := frame.reset(&br)
		if debugDecoder && err != nil {
			println("Frame decoder returned", err)
		}
		if err == nil {
			err = d.setDict(frame)
		}
		if err == nil && d.frame.WindowSize > d.o.maxWindowSize {
			if debugDecoder {
				println("decoder size exceeded, fws:", d.frame.WindowSize, "> mws:", d.o.maxWindowSize)
			}

			err = ErrDecoderSizeExceeded
		}
		if err != nil {
			select {
			case <-ctx.Done():
			case dec := <-d.decoders:
				dec.sendErr(err)
				decodeBlock(dec)
			}
			break decodeStream
		}

		// Go through all blocks of the frame.
		for {
			var dec *blockDec
			select {
			case <-ctx.Done():
				break decodeStream
			case dec = <-d.decoders:
				// Once we have a decoder, we MUST return it.
			}
			err := frame.next(dec)
			if !historySent {
				h := frame.history
				if debugDecoder {
					println("Alloc History:", h.allocFrameBuffer)
				}
				hist.reset()
				if h.dict != nil {
					hist.setDict(h.dict)
				}
				dec.async.newHist = &h
				dec.async.fcs = frame.FrameContentSize
				historySent = true
			} else {
				dec.async.newHist = nil
			}
			if debugDecoder && err != nil {
				println("next block returned error:", err)
			}
			dec.err = err
			dec.hasCRC = false
			if dec.Last && frame.HasCheckSum && err == nil {
				crc, err := frame.rawInput.readSmall(4)
				if len(crc) < 4 {
					if err == nil {
						err = io.ErrUnexpectedEOF

					}
					println("CRC missing?", err)
					dec.err = err
				} else {
					dec.checkCRC = binary.LittleEndian.Uint32(crc)
					dec.hasCRC = true
					if debugDecoder {
						printf("found crc to check: %08x\n", dec.checkCRC)
					}
				}
			}
			err = dec.err
			last := dec.Last
			decodeBlock(dec)
			if err != nil {
				break decodeStream
			}
			if last {
				break
			}
		}
	}
	close(seqDecode)
	wg.Wait()
	hist.reset()
	d.frame.history.b = frameHistCache
}

func (d *Decoder) setDict(frame *frameDec) (err error) {
	dict, ok := d.dicts[frame.DictionaryID]
	if ok {
		if debugDecoder {
			println("setting dict", frame.DictionaryID)
		}
		frame.history.setDict(dict)
	} else if frame.DictionaryID != 0 {
		// A zero or missing dictionary id is ambiguous:
		// either dictionary zero, or no dictionary. In particular,
		// zstd --patch-from uses this id for the source file,
		// so only return an error if the dictionary id is not zero.
		err = ErrUnknownDictionary
	}
	return err
}
