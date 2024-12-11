# Finite State Entropy

This package provides Finite State Entropy encoding and decoding.
            
Finite State Entropy (also referenced as [tANS](https://en.wikipedia.org/wiki/Asymmetric_numeral_systems#tANS)) 
encoding provides a fast near-optimal symbol encoding/decoding
for byte blocks as implemented in [zstandard](https://github.com/facebook/zstd).

This can be used for compressing input with a lot of similar input values to the smallest number of bytes.
This does not perform any multi-byte [dictionary coding](https://en.wikipedia.org/wiki/Dictionary_coder) as LZ coders,
but it can be used as a secondary step to compressors (like Snappy) that does not do entropy encoding. 

* [Godoc documentation](https://godoc.org/github.com/klauspost/compress/fse)

## News

 * Feb 2018: First implementation released. Consider this beta software for now.

# Usage

This package provides a low level interface that allows to compress single independent blocks. 

Each block is separate, and there is no built in integrity checks. 
This means that the caller should keep track of block sizes and also do checksums if needed.  

Compressing a block is done via the [`Compress`](https://godoc.org/github.com/klauspost/compress/fse#Compress) function.
You must provide input and will receive the output and maybe an error.

These error values can be returned:

| Error               | Description                                                                 |
|---------------------|-----------------------------------------------------------------------------|
| `<nil>`             | Everything ok, output is returned                                           |
| `ErrIncompressible` | Returned when input is judged to be too hard to compress                    |
| `ErrUseRLE`         | Returned from the compressor when the input is a single byte value repeated |
| `(error)`           | An internal error occurred.                                                 |

As can be seen above there are errors that will be returned even under normal operation so it is important to handle these.

To reduce allocations you can provide a [`Scratch`](https://godoc.org/github.com/klauspost/compress/fse#Scratch) object 
that can be re-used for successive calls. Both compression and decompression accepts a `Scratch` object, and the same 
object can be used for both.   

Be aware, that when re-using a `Scratch` object that the *output* buffer is also re-used, so if you are still using this
you must set the `Out` field in the scratch to nil. The same buffer is used for compression and decompression output.

Decompressing is done by calling the [`Decompress`](https://godoc.org/github.com/klauspost/compress/fse#Decompress) function.
You must provide the output from the compression stage, at exactly the size you got back. If you receive an error back
your input was likely corrupted. 

It is important to note that a successful decoding does *not* mean your output matches your original input. 
There are no integrity checks, so relying on errors from the decompressor does not assure your data is valid.

For more detailed usage, see examples in the [godoc documentation](https://godoc.org/github.com/klauspost/compress/fse#pkg-examples).

# Performance

A lot of factors are affecting speed. Block sizes and compressibility of the material are primary factors.  
All compression functions are currently only running on the calling goroutine so only one core will be used per block.  

The compressor is significantly faster if symbols are kept as small as possible. The highest byte value of the input
is used to reduce some of the processing, so if all your input is above byte value 64 for instance, it may be 
beneficial to transpose all your input values down by 64.   

With moderate block sizes around 64k speed are typically 200MB/s per core for compression and 
around 300MB/s decompression speed. 

The same hardware typically does Huffman (deflate) encoding at 125MB/s and decompression at 100MB/s. 

# Plans

At one point, more internals will be exposed to facilitate more "expert" usage of the components. 

A streaming interface is also likely to be implemented. Likely compatible with [FSE stream format](https://github.com/Cyan4973/FiniteStateEntropy/blob/dev/programs/fileio.c#L261).  

# Contributing

Contributions are always welcome. Be aware that adding public functions will require good justification and breaking 
changes will likely not be accepted. If in doubt open an issue before writing the PR.  