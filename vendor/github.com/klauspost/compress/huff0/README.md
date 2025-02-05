# Huff0 entropy compression

This package provides Huff0 encoding and decoding as used in zstd.
            
[Huff0](https://github.com/Cyan4973/FiniteStateEntropy#new-generation-entropy-coders), 
a Huffman codec designed for modern CPU, featuring OoO (Out of Order) operations on multiple ALU 
(Arithmetic Logic Unit), achieving extremely fast compression and decompression speeds.

This can be used for compressing input with a lot of similar input values to the smallest number of bytes.
This does not perform any multi-byte [dictionary coding](https://en.wikipedia.org/wiki/Dictionary_coder) as LZ coders,
but it can be used as a secondary step to compressors (like Snappy) that does not do entropy encoding. 

* [Godoc documentation](https://godoc.org/github.com/klauspost/compress/huff0)

## News

This is used as part of the [zstandard](https://github.com/klauspost/compress/tree/master/zstd#zstd) compression and decompression package.

This ensures that most functionality is well tested.

# Usage

This package provides a low level interface that allows to compress single independent blocks. 

Each block is separate, and there is no built in integrity checks. 
This means that the caller should keep track of block sizes and also do checksums if needed.  

Compressing a block is done via the [`Compress1X`](https://godoc.org/github.com/klauspost/compress/huff0#Compress1X) and 
[`Compress4X`](https://godoc.org/github.com/klauspost/compress/huff0#Compress4X) functions.
You must provide input and will receive the output and maybe an error.

These error values can be returned:

| Error               | Description                                                                 |
|---------------------|-----------------------------------------------------------------------------|
| `<nil>`             | Everything ok, output is returned                                           |
| `ErrIncompressible` | Returned when input is judged to be too hard to compress                    |
| `ErrUseRLE`         | Returned from the compressor when the input is a single byte value repeated |
| `ErrTooBig`         | Returned if the input block exceeds the maximum allowed size (128 Kib)      |
| `(error)`           | An internal error occurred.                                                 |


As can be seen above some of there are errors that will be returned even under normal operation so it is important to handle these.

To reduce allocations you can provide a [`Scratch`](https://godoc.org/github.com/klauspost/compress/huff0#Scratch) object 
that can be re-used for successive calls. Both compression and decompression accepts a `Scratch` object, and the same 
object can be used for both.   

Be aware, that when re-using a `Scratch` object that the *output* buffer is also re-used, so if you are still using this
you must set the `Out` field in the scratch to nil. The same buffer is used for compression and decompression output.

The `Scratch` object will retain state that allows to re-use previous tables for encoding and decoding.  

## Tables and re-use

Huff0 allows for reusing tables from the previous block to save space if that is expected to give better/faster results. 

The Scratch object allows you to set a [`ReusePolicy`](https://godoc.org/github.com/klauspost/compress/huff0#ReusePolicy) 
that controls this behaviour. See the documentation for details. This can be altered between each block.

Do however note that this information is *not* stored in the output block and it is up to the users of the package to
record whether [`ReadTable`](https://godoc.org/github.com/klauspost/compress/huff0#ReadTable) should be called,
based on the boolean reported back from the CompressXX call. 

If you want to store the table separate from the data, you can access them as `OutData` and `OutTable` on the 
[`Scratch`](https://godoc.org/github.com/klauspost/compress/huff0#Scratch) object.

## Decompressing

The first part of decoding is to initialize the decoding table through [`ReadTable`](https://godoc.org/github.com/klauspost/compress/huff0#ReadTable).
This will initialize the decoding tables. 
You can supply the complete block to `ReadTable` and it will return the data part of the block 
which can be given to the decompressor. 

Decompressing is done by calling the [`Decompress1X`](https://godoc.org/github.com/klauspost/compress/huff0#Scratch.Decompress1X) 
or [`Decompress4X`](https://godoc.org/github.com/klauspost/compress/huff0#Scratch.Decompress4X) function.

For concurrently decompressing content with a fixed table a stateless [`Decoder`](https://godoc.org/github.com/klauspost/compress/huff0#Decoder) can be requested which will remain correct as long as the scratch is unchanged. The capacity of the provided slice indicates the expected output size.

You must provide the output from the compression stage, at exactly the size you got back. If you receive an error back
your input was likely corrupted. 

It is important to note that a successful decoding does *not* mean your output matches your original input. 
There are no integrity checks, so relying on errors from the decompressor does not assure your data is valid.

# Contributing

Contributions are always welcome. Be aware that adding public functions will require good justification and breaking 
changes will likely not be accepted. If in doubt open an issue before writing the PR.
