package zstd

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"strings"
)

// EOption is an option for creating a encoder.
type EOption func(*encoderOptions) error

// options retains accumulated state of multiple options.
type encoderOptions struct {
	concurrent      int
	level           EncoderLevel
	single          *bool
	pad             int
	blockSize       int
	windowSize      int
	crc             bool
	fullZero        bool
	noEntropy       bool
	allLitEntropy   bool
	customWindow    bool
	customALEntropy bool
	customBlockSize bool
	lowMem          bool
	dict            *dict
}

func (o *encoderOptions) setDefault() {
	*o = encoderOptions{
		concurrent:    runtime.GOMAXPROCS(0),
		crc:           true,
		single:        nil,
		blockSize:     maxCompressedBlockSize,
		windowSize:    8 << 20,
		level:         SpeedDefault,
		allLitEntropy: false,
		lowMem:        false,
	}
}

// encoder returns an encoder with the selected options.
func (o encoderOptions) encoder() encoder {
	switch o.level {
	case SpeedFastest:
		if o.dict != nil {
			return &fastEncoderDict{fastEncoder: fastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}}
		}
		return &fastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}

	case SpeedDefault:
		if o.dict != nil {
			return &doubleFastEncoderDict{fastEncoderDict: fastEncoderDict{fastEncoder: fastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}}}
		}
		return &doubleFastEncoder{fastEncoder: fastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}}
	case SpeedBetterCompression:
		if o.dict != nil {
			return &betterFastEncoderDict{betterFastEncoder: betterFastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}}
		}
		return &betterFastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}
	case SpeedBestCompression:
		return &bestFastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}
	}
	panic("unknown compression level")
}

// WithEncoderCRC will add CRC value to output.
// Output will be 4 bytes larger.
func WithEncoderCRC(b bool) EOption {
	return func(o *encoderOptions) error { o.crc = b; return nil }
}

// WithEncoderConcurrency will set the concurrency,
// meaning the maximum number of encoders to run concurrently.
// The value supplied must be at least 1.
// For streams, setting a value of 1 will disable async compression.
// By default this will be set to GOMAXPROCS.
func WithEncoderConcurrency(n int) EOption {
	return func(o *encoderOptions) error {
		if n <= 0 {
			return fmt.Errorf("concurrency must be at least 1")
		}
		o.concurrent = n
		return nil
	}
}

// WithWindowSize will set the maximum allowed back-reference distance.
// The value must be a power of two between MinWindowSize and MaxWindowSize.
// A larger value will enable better compression but allocate more memory and,
// for above-default values, take considerably longer.
// The default value is determined by the compression level and max 8MB.
func WithWindowSize(n int) EOption {
	return func(o *encoderOptions) error {
		switch {
		case n < MinWindowSize:
			return fmt.Errorf("window size must be at least %d", MinWindowSize)
		case n > MaxWindowSize:
			return fmt.Errorf("window size must be at most %d", MaxWindowSize)
		case (n & (n - 1)) != 0:
			return errors.New("window size must be a power of 2")
		}

		o.windowSize = n
		o.customWindow = true
		if o.blockSize > o.windowSize {
			o.blockSize = o.windowSize
			o.customBlockSize = true
		}
		return nil
	}
}

// WithEncoderPadding will add padding to all output so the size will be a multiple of n.
// This can be used to obfuscate the exact output size or make blocks of a certain size.
// The contents will be a skippable frame, so it will be invisible by the decoder.
// n must be > 0 and <= 1GB, 1<<30 bytes.
// The padded area will be filled with data from crypto/rand.Reader.
// If `EncodeAll` is used with data already in the destination, the total size will be multiple of this.
func WithEncoderPadding(n int) EOption {
	return func(o *encoderOptions) error {
		if n <= 0 {
			return fmt.Errorf("padding must be at least 1")
		}
		// No need to waste our time.
		if n == 1 {
			n = 0
		}
		if n > 1<<30 {
			return fmt.Errorf("padding must less than 1GB (1<<30 bytes) ")
		}
		o.pad = n
		return nil
	}
}

// EncoderLevel predefines encoder compression levels.
// Only use the constants made available, since the actual mapping
// of these values are very likely to change and your compression could change
// unpredictably when upgrading the library.
type EncoderLevel int

const (
	speedNotSet EncoderLevel = iota

	// SpeedFastest will choose the fastest reasonable compression.
	// This is roughly equivalent to the fastest Zstandard mode.
	SpeedFastest

	// SpeedDefault is the default "pretty fast" compression option.
	// This is roughly equivalent to the default Zstandard mode (level 3).
	SpeedDefault

	// SpeedBetterCompression will yield better compression than the default.
	// Currently it is about zstd level 7-8 with ~ 2x-3x the default CPU usage.
	// By using this, notice that CPU usage may go up in the future.
	SpeedBetterCompression

	// SpeedBestCompression will choose the best available compression option.
	// This will offer the best compression no matter the CPU cost.
	SpeedBestCompression

	// speedLast should be kept as the last actual compression option.
	// The is not for external usage, but is used to keep track of the valid options.
	speedLast
)

// EncoderLevelFromString will convert a string representation of an encoding level back
// to a compression level. The compare is not case sensitive.
// If the string wasn't recognized, (false, SpeedDefault) will be returned.
func EncoderLevelFromString(s string) (bool, EncoderLevel) {
	for l := speedNotSet + 1; l < speedLast; l++ {
		if strings.EqualFold(s, l.String()) {
			return true, l
		}
	}
	return false, SpeedDefault
}

// EncoderLevelFromZstd will return an encoder level that closest matches the compression
// ratio of a specific zstd compression level.
// Many input values will provide the same compression level.
func EncoderLevelFromZstd(level int) EncoderLevel {
	switch {
	case level < 3:
		return SpeedFastest
	case level >= 3 && level < 6:
		return SpeedDefault
	case level >= 6 && level < 10:
		return SpeedBetterCompression
	default:
		return SpeedBestCompression
	}
}

// String provides a string representation of the compression level.
func (e EncoderLevel) String() string {
	switch e {
	case SpeedFastest:
		return "fastest"
	case SpeedDefault:
		return "default"
	case SpeedBetterCompression:
		return "better"
	case SpeedBestCompression:
		return "best"
	default:
		return "invalid"
	}
}

// WithEncoderLevel specifies a predefined compression level.
func WithEncoderLevel(l EncoderLevel) EOption {
	return func(o *encoderOptions) error {
		switch {
		case l <= speedNotSet || l >= speedLast:
			return fmt.Errorf("unknown encoder level")
		}
		o.level = l
		if !o.customWindow {
			switch o.level {
			case SpeedFastest:
				o.windowSize = 4 << 20
				if !o.customBlockSize {
					o.blockSize = 1 << 16
				}
			case SpeedDefault:
				o.windowSize = 8 << 20
			case SpeedBetterCompression:
				o.windowSize = 8 << 20
			case SpeedBestCompression:
				o.windowSize = 8 << 20
			}
		}
		if !o.customALEntropy {
			o.allLitEntropy = l > SpeedDefault
		}

		return nil
	}
}

// WithZeroFrames will encode 0 length input as full frames.
// This can be needed for compatibility with zstandard usage,
// but is not needed for this package.
func WithZeroFrames(b bool) EOption {
	return func(o *encoderOptions) error {
		o.fullZero = b
		return nil
	}
}

// WithAllLitEntropyCompression will apply entropy compression if no matches are found.
// Disabling this will skip incompressible data faster, but in cases with no matches but
// skewed character distribution compression is lost.
// Default value depends on the compression level selected.
func WithAllLitEntropyCompression(b bool) EOption {
	return func(o *encoderOptions) error {
		o.customALEntropy = true
		o.allLitEntropy = b
		return nil
	}
}

// WithNoEntropyCompression will always skip entropy compression of literals.
// This can be useful if content has matches, but unlikely to benefit from entropy
// compression. Usually the slight speed improvement is not worth enabling this.
func WithNoEntropyCompression(b bool) EOption {
	return func(o *encoderOptions) error {
		o.noEntropy = b
		return nil
	}
}

// WithSingleSegment will set the "single segment" flag when EncodeAll is used.
// If this flag is set, data must be regenerated within a single continuous memory segment.
// In this case, Window_Descriptor byte is skipped, but Frame_Content_Size is necessarily present.
// As a consequence, the decoder must allocate a memory segment of size equal or larger than size of your content.
// In order to preserve the decoder from unreasonable memory requirements,
// a decoder is allowed to reject a compressed frame which requests a memory size beyond decoder's authorized range.
// For broader compatibility, decoders are recommended to support memory sizes of at least 8 MB.
// This is only a recommendation, each decoder is free to support higher or lower limits, depending on local limitations.
// If this is not specified, block encodes will automatically choose this based on the input size and the window size.
// This setting has no effect on streamed encodes.
func WithSingleSegment(b bool) EOption {
	return func(o *encoderOptions) error {
		o.single = &b
		return nil
	}
}

// WithLowerEncoderMem will trade in some memory cases trade less memory usage for
// slower encoding speed.
// This will not change the window size which is the primary function for reducing
// memory usage. See WithWindowSize.
func WithLowerEncoderMem(b bool) EOption {
	return func(o *encoderOptions) error {
		o.lowMem = b
		return nil
	}
}

// WithEncoderDict allows to register a dictionary that will be used for the encode.
//
// The slice dict must be in the [dictionary format] produced by
// "zstd --train" from the Zstandard reference implementation.
//
// The encoder *may* choose to use no dictionary instead for certain payloads.
//
// [dictionary format]: https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#dictionary-format
func WithEncoderDict(dict []byte) EOption {
	return func(o *encoderOptions) error {
		d, err := loadDict(dict)
		if err != nil {
			return err
		}
		o.dict = d
		return nil
	}
}

// WithEncoderDictRaw registers a dictionary that may be used by the encoder.
//
// The slice content may contain arbitrary data. It will be used as an initial
// history.
func WithEncoderDictRaw(id uint32, content []byte) EOption {
	return func(o *encoderOptions) error {
		if bits.UintSize > 32 && uint(len(content)) > dictMaxLength {
			return fmt.Errorf("dictionary of size %d > 2GiB too large", len(content))
		}
		o.dict = &dict{id: id, content: content, offsets: [3]int{1, 4, 8}}
		return nil
	}
}
