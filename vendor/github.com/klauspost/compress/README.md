# compress

This package provides various compression algorithms.

* [zstandard](https://github.com/klauspost/compress/tree/master/zstd#zstd) compression and decompression in pure Go.
* [S2](https://github.com/klauspost/compress/tree/master/s2#s2-compression) is a high performance replacement for Snappy.
* Optimized [deflate](https://godoc.org/github.com/klauspost/compress/flate) packages which can be used as a dropin replacement for [gzip](https://godoc.org/github.com/klauspost/compress/gzip), [zip](https://godoc.org/github.com/klauspost/compress/zip) and [zlib](https://godoc.org/github.com/klauspost/compress/zlib).
* [snappy](https://github.com/klauspost/compress/tree/master/snappy) is a drop-in replacement for `github.com/golang/snappy` offering better compression and concurrent streams.
* [huff0](https://github.com/klauspost/compress/tree/master/huff0) and [FSE](https://github.com/klauspost/compress/tree/master/fse) implementations for raw entropy encoding.
* [gzhttp](https://github.com/klauspost/compress/tree/master/gzhttp) Provides client and server wrappers for handling gzipped requests efficiently.
* [pgzip](https://github.com/klauspost/pgzip) is a separate package that provides a very fast parallel gzip implementation.

[![Go Reference](https://pkg.go.dev/badge/klauspost/compress.svg)](https://pkg.go.dev/github.com/klauspost/compress?tab=subdirectories)
[![Go](https://github.com/klauspost/compress/actions/workflows/go.yml/badge.svg)](https://github.com/klauspost/compress/actions/workflows/go.yml)
[![Sourcegraph Badge](https://sourcegraph.com/github.com/klauspost/compress/-/badge.svg)](https://sourcegraph.com/github.com/klauspost/compress?badge)

# changelog

* Sep 23rd, 2024 - [1.17.10](https://github.com/klauspost/compress/releases/tag/v1.17.10)
	* gzhttp: Add TransportAlwaysDecompress option. https://github.com/klauspost/compress/pull/978
	* gzhttp: Add supported decompress request body by @mirecl in https://github.com/klauspost/compress/pull/1002
	* s2: Add EncodeBuffer buffer recycling callback https://github.com/klauspost/compress/pull/982
	* zstd: Improve memory usage on small streaming encodes https://github.com/klauspost/compress/pull/1007
	* flate: read data written with partial flush by @vajexal in https://github.com/klauspost/compress/pull/996

* Jun 12th, 2024 - [1.17.9](https://github.com/klauspost/compress/releases/tag/v1.17.9)
	* s2: Reduce ReadFrom temporary allocations https://github.com/klauspost/compress/pull/949
	* flate, zstd: Shave some bytes off amd64 matchLen by @greatroar in https://github.com/klauspost/compress/pull/963
	* Upgrade zip/zlib to 1.22.4 upstream https://github.com/klauspost/compress/pull/970 https://github.com/klauspost/compress/pull/971
	* zstd: BuildDict fails with RLE table https://github.com/klauspost/compress/pull/951

* Apr 9th, 2024 - [1.17.8](https://github.com/klauspost/compress/releases/tag/v1.17.8)
	* zstd: Reject blocks where reserved values are not 0 https://github.com/klauspost/compress/pull/885
	* zstd: Add RLE detection+encoding https://github.com/klauspost/compress/pull/938

* Feb 21st, 2024 - [1.17.7](https://github.com/klauspost/compress/releases/tag/v1.17.7)
	* s2: Add AsyncFlush method: Complete the block without flushing by @Jille in https://github.com/klauspost/compress/pull/927
	* s2: Fix literal+repeat exceeds dst crash https://github.com/klauspost/compress/pull/930
  
* Feb 5th, 2024 - [1.17.6](https://github.com/klauspost/compress/releases/tag/v1.17.6)
	* zstd: Fix incorrect repeat coding in best mode https://github.com/klauspost/compress/pull/923
	* s2: Fix DecodeConcurrent deadlock on errors https://github.com/klauspost/compress/pull/925
  
* Jan 26th, 2024 - [v1.17.5](https://github.com/klauspost/compress/releases/tag/v1.17.5)
	* flate: Fix reset with dictionary on custom window encodes https://github.com/klauspost/compress/pull/912
	* zstd: Add Frame header encoding and stripping https://github.com/klauspost/compress/pull/908
	* zstd: Limit better/best default window to 8MB https://github.com/klauspost/compress/pull/913
	* zstd: Speed improvements by @greatroar in https://github.com/klauspost/compress/pull/896 https://github.com/klauspost/compress/pull/910
	* s2: Fix callbacks for skippable blocks and disallow 0xfe (Padding) by @Jille in https://github.com/klauspost/compress/pull/916 https://github.com/klauspost/compress/pull/917
https://github.com/klauspost/compress/pull/919 https://github.com/klauspost/compress/pull/918

* Dec 1st, 2023 - [v1.17.4](https://github.com/klauspost/compress/releases/tag/v1.17.4)
	* huff0: Speed up symbol counting by @greatroar in https://github.com/klauspost/compress/pull/887
	* huff0: Remove byteReader by @greatroar in https://github.com/klauspost/compress/pull/886
	* gzhttp: Allow overriding decompression on transport https://github.com/klauspost/compress/pull/892
	* gzhttp: Clamp compression level https://github.com/klauspost/compress/pull/890
	* gzip: Error out if reserved bits are set https://github.com/klauspost/compress/pull/891

* Nov 15th, 2023 - [v1.17.3](https://github.com/klauspost/compress/releases/tag/v1.17.3)
	* fse: Fix max header size https://github.com/klauspost/compress/pull/881
	* zstd: Improve better/best compression https://github.com/klauspost/compress/pull/877
	* gzhttp: Fix missing content type on Close https://github.com/klauspost/compress/pull/883

* Oct 22nd, 2023 - [v1.17.2](https://github.com/klauspost/compress/releases/tag/v1.17.2)
	* zstd: Fix rare *CORRUPTION* output in "best" mode. See https://github.com/klauspost/compress/pull/876

* Oct 14th, 2023 - [v1.17.1](https://github.com/klauspost/compress/releases/tag/v1.17.1)
	* s2: Fix S2 "best" dictionary wrong encoding by @klauspost in https://github.com/klauspost/compress/pull/871
	* flate: Reduce allocations in decompressor and minor code improvements by @fakefloordiv in https://github.com/klauspost/compress/pull/869
	* s2: Fix EstimateBlockSize on 6&7 length input by @klauspost in https://github.com/klauspost/compress/pull/867

* Sept 19th, 2023 - [v1.17.0](https://github.com/klauspost/compress/releases/tag/v1.17.0)
	* Add experimental dictionary builder  https://github.com/klauspost/compress/pull/853
	* Add xerial snappy read/writer https://github.com/klauspost/compress/pull/838
	* flate: Add limited window compression https://github.com/klauspost/compress/pull/843
	* s2: Do 2 overlapping match checks https://github.com/klauspost/compress/pull/839
	* flate: Add amd64 assembly matchlen https://github.com/klauspost/compress/pull/837
	* gzip: Copy bufio.Reader on Reset by @thatguystone in https://github.com/klauspost/compress/pull/860

<details>
	<summary>See changes to v1.16.x</summary>

   
* July 1st, 2023 - [v1.16.7](https://github.com/klauspost/compress/releases/tag/v1.16.7)
	* zstd: Fix default level first dictionary encode https://github.com/klauspost/compress/pull/829
	* s2: add GetBufferCapacity() method by @GiedriusS in https://github.com/klauspost/compress/pull/832

* June 13, 2023 - [v1.16.6](https://github.com/klauspost/compress/releases/tag/v1.16.6)
	* zstd: correctly ignore WithEncoderPadding(1) by @ianlancetaylor in https://github.com/klauspost/compress/pull/806
	* zstd: Add amd64 match length assembly https://github.com/klauspost/compress/pull/824
	* gzhttp: Handle informational headers by @rtribotte in https://github.com/klauspost/compress/pull/815
	* s2: Improve Better compression slightly https://github.com/klauspost/compress/pull/663

* Apr 16, 2023 - [v1.16.5](https://github.com/klauspost/compress/releases/tag/v1.16.5)
	* zstd: readByte needs to use io.ReadFull by @jnoxon in https://github.com/klauspost/compress/pull/802
	* gzip: Fix WriterTo after initial read https://github.com/klauspost/compress/pull/804

* Apr 5, 2023 - [v1.16.4](https://github.com/klauspost/compress/releases/tag/v1.16.4)
	* zstd: Improve zstd best efficiency by @greatroar and @klauspost in https://github.com/klauspost/compress/pull/784
	* zstd: Respect WithAllLitEntropyCompression https://github.com/klauspost/compress/pull/792
	* zstd: Fix amd64 not always detecting corrupt data https://github.com/klauspost/compress/pull/785
	* zstd: Various minor improvements by @greatroar in https://github.com/klauspost/compress/pull/788 https://github.com/klauspost/compress/pull/794 https://github.com/klauspost/compress/pull/795
	* s2: Fix huge block overflow https://github.com/klauspost/compress/pull/779
	* s2: Allow CustomEncoder fallback https://github.com/klauspost/compress/pull/780
	* gzhttp: Support ResponseWriter Unwrap() in gzhttp handler by @jgimenez in https://github.com/klauspost/compress/pull/799

* Mar 13, 2023 - [v1.16.1](https://github.com/klauspost/compress/releases/tag/v1.16.1)
	* zstd: Speed up + improve best encoder by @greatroar in https://github.com/klauspost/compress/pull/776
	* gzhttp: Add optional [BREACH mitigation](https://github.com/klauspost/compress/tree/master/gzhttp#breach-mitigation). https://github.com/klauspost/compress/pull/762 https://github.com/klauspost/compress/pull/768 https://github.com/klauspost/compress/pull/769 https://github.com/klauspost/compress/pull/770 https://github.com/klauspost/compress/pull/767
	* s2: Add Intel LZ4s converter https://github.com/klauspost/compress/pull/766
	* zstd: Minor bug fixes https://github.com/klauspost/compress/pull/771 https://github.com/klauspost/compress/pull/772 https://github.com/klauspost/compress/pull/773
	* huff0: Speed up compress1xDo by @greatroar in https://github.com/klauspost/compress/pull/774

* Feb 26, 2023 - [v1.16.0](https://github.com/klauspost/compress/releases/tag/v1.16.0)
	* s2: Add [Dictionary](https://github.com/klauspost/compress/tree/master/s2#dictionaries) support.  https://github.com/klauspost/compress/pull/685
	* s2: Add Compression Size Estimate.  https://github.com/klauspost/compress/pull/752
	* s2: Add support for custom stream encoder. https://github.com/klauspost/compress/pull/755
	* s2: Add LZ4 block converter. https://github.com/klauspost/compress/pull/748
	* s2: Support io.ReaderAt in ReadSeeker. https://github.com/klauspost/compress/pull/747
	* s2c/s2sx: Use concurrent decoding. https://github.com/klauspost/compress/pull/746
</details>

<details>
	<summary>See changes to v1.15.x</summary>
	
* Jan 21st, 2023 (v1.15.15)
	* deflate: Improve level 7-9 by @klauspost in https://github.com/klauspost/compress/pull/739
	* zstd: Add delta encoding support by @greatroar in https://github.com/klauspost/compress/pull/728
	* zstd: Various speed improvements by @greatroar https://github.com/klauspost/compress/pull/741 https://github.com/klauspost/compress/pull/734 https://github.com/klauspost/compress/pull/736 https://github.com/klauspost/compress/pull/744 https://github.com/klauspost/compress/pull/743 https://github.com/klauspost/compress/pull/745
	* gzhttp: Add SuffixETag() and DropETag() options to prevent ETag collisions on compressed responses by @willbicks in https://github.com/klauspost/compress/pull/740

* Jan 3rd, 2023 (v1.15.14)

	* flate: Improve speed in big stateless blocks https://github.com/klauspost/compress/pull/718
	* zstd: Minor speed tweaks by @greatroar in https://github.com/klauspost/compress/pull/716 https://github.com/klauspost/compress/pull/720
	* export NoGzipResponseWriter for custom ResponseWriter wrappers by @harshavardhana in https://github.com/klauspost/compress/pull/722
	* s2: Add example for indexing and existing stream https://github.com/klauspost/compress/pull/723

* Dec 11, 2022 (v1.15.13)
	* zstd: Add [MaxEncodedSize](https://pkg.go.dev/github.com/klauspost/compress@v1.15.13/zstd#Encoder.MaxEncodedSize) to encoder  https://github.com/klauspost/compress/pull/691
	* zstd: Various tweaks and improvements https://github.com/klauspost/compress/pull/693 https://github.com/klauspost/compress/pull/695 https://github.com/klauspost/compress/pull/696 https://github.com/klauspost/compress/pull/701 https://github.com/klauspost/compress/pull/702 https://github.com/klauspost/compress/pull/703 https://github.com/klauspost/compress/pull/704 https://github.com/klauspost/compress/pull/705 https://github.com/klauspost/compress/pull/706 https://github.com/klauspost/compress/pull/707 https://github.com/klauspost/compress/pull/708

* Oct 26, 2022 (v1.15.12)

	* zstd: Tweak decoder allocs. https://github.com/klauspost/compress/pull/680
	* gzhttp: Always delete `HeaderNoCompression` https://github.com/klauspost/compress/pull/683

* Sept 26, 2022 (v1.15.11)

	* flate: Improve level 1-3 compression  https://github.com/klauspost/compress/pull/678
	* zstd: Improve "best" compression by @nightwolfz in https://github.com/klauspost/compress/pull/677
	* zstd: Fix+reduce decompression allocations https://github.com/klauspost/compress/pull/668
	* zstd: Fix non-effective noescape tag https://github.com/klauspost/compress/pull/667

* Sept 16, 2022 (v1.15.10)

	* zstd: Add [WithDecodeAllCapLimit](https://pkg.go.dev/github.com/klauspost/compress@v1.15.10/zstd#WithDecodeAllCapLimit) https://github.com/klauspost/compress/pull/649
	* Add Go 1.19 - deprecate Go 1.16  https://github.com/klauspost/compress/pull/651
	* flate: Improve level 5+6 compression https://github.com/klauspost/compress/pull/656
	* zstd: Improve "better" compression  https://github.com/klauspost/compress/pull/657
	* s2: Improve "best" compression https://github.com/klauspost/compress/pull/658
	* s2: Improve "better" compression. https://github.com/klauspost/compress/pull/635
	* s2: Slightly faster non-assembly decompression https://github.com/klauspost/compress/pull/646
	* Use arrays for constant size copies https://github.com/klauspost/compress/pull/659

* July 21, 2022 (v1.15.9)

	* zstd: Fix decoder crash on amd64 (no BMI) on invalid input https://github.com/klauspost/compress/pull/645
	* zstd: Disable decoder extended memory copies (amd64) due to possible crashes https://github.com/klauspost/compress/pull/644
	* zstd: Allow single segments up to "max decoded size" by @klauspost in https://github.com/klauspost/compress/pull/643

* July 13, 2022 (v1.15.8)

	* gzip: fix stack exhaustion bug in Reader.Read https://github.com/klauspost/compress/pull/641
	* s2: Add Index header trim/restore https://github.com/klauspost/compress/pull/638
	* zstd: Optimize seqdeq amd64 asm by @greatroar in https://github.com/klauspost/compress/pull/636
	* zstd: Improve decoder memcopy https://github.com/klauspost/compress/pull/637
	* huff0: Pass a single bitReader pointer to asm by @greatroar in https://github.com/klauspost/compress/pull/634
	* zstd: Branchless getBits for amd64 w/o BMI2 by @greatroar in https://github.com/klauspost/compress/pull/640
	* gzhttp: Remove header before writing https://github.com/klauspost/compress/pull/639

* June 29, 2022 (v1.15.7)

	* s2: Fix absolute forward seeks  https://github.com/klauspost/compress/pull/633
	* zip: Merge upstream  https://github.com/klauspost/compress/pull/631
	* zip: Re-add zip64 fix https://github.com/klauspost/compress/pull/624
	* zstd: translate fseDecoder.buildDtable into asm by @WojciechMula in https://github.com/klauspost/compress/pull/598
	* flate: Faster histograms  https://github.com/klauspost/compress/pull/620
	* deflate: Use compound hcode  https://github.com/klauspost/compress/pull/622

* June 3, 2022 (v1.15.6)
	* s2: Improve coding for long, close matches https://github.com/klauspost/compress/pull/613
	* s2c: Add Snappy/S2 stream recompression https://github.com/klauspost/compress/pull/611
	* zstd: Always use configured block size https://github.com/klauspost/compress/pull/605
	* zstd: Fix incorrect hash table placement for dict encoding in default https://github.com/klauspost/compress/pull/606
	* zstd: Apply default config to ZipDecompressor without options https://github.com/klauspost/compress/pull/608
	* gzhttp: Exclude more common archive formats https://github.com/klauspost/compress/pull/612
	* s2: Add ReaderIgnoreCRC https://github.com/klauspost/compress/pull/609
	* s2: Remove sanity load on index creation https://github.com/klauspost/compress/pull/607
	* snappy: Use dedicated function for scoring https://github.com/klauspost/compress/pull/614
	* s2c+s2d: Use official snappy framed extension https://github.com/klauspost/compress/pull/610

* May 25, 2022 (v1.15.5)
	* s2: Add concurrent stream decompression https://github.com/klauspost/compress/pull/602
	* s2: Fix final emit oob read crash on amd64 https://github.com/klauspost/compress/pull/601
	* huff0: asm implementation of Decompress1X by @WojciechMula https://github.com/klauspost/compress/pull/596
	* zstd: Use 1 less goroutine for stream decoding https://github.com/klauspost/compress/pull/588
	* zstd: Copy literal in 16 byte blocks when possible https://github.com/klauspost/compress/pull/592
	* zstd: Speed up when WithDecoderLowmem(false) https://github.com/klauspost/compress/pull/599
	* zstd: faster next state update in BMI2 version of decode by @WojciechMula in https://github.com/klauspost/compress/pull/593
	* huff0: Do not check max size when reading table. https://github.com/klauspost/compress/pull/586
	* flate: Inplace hashing for level 7-9 by @klauspost in https://github.com/klauspost/compress/pull/590


* May 11, 2022 (v1.15.4)
	* huff0: decompress directly into output by @WojciechMula in [#577](https://github.com/klauspost/compress/pull/577)
	* inflate: Keep dict on stack [#581](https://github.com/klauspost/compress/pull/581)
	* zstd: Faster decoding memcopy in asm [#583](https://github.com/klauspost/compress/pull/583)
	* zstd: Fix ignored crc [#580](https://github.com/klauspost/compress/pull/580)

* May 5, 2022 (v1.15.3)
	* zstd: Allow to ignore checksum checking by @WojciechMula [#572](https://github.com/klauspost/compress/pull/572)
	* s2: Fix incorrect seek for io.SeekEnd in [#575](https://github.com/klauspost/compress/pull/575)

* Apr 26, 2022 (v1.15.2)
	* zstd: Add x86-64 assembly for decompression on streams and blocks. Contributed by [@WojciechMula](https://github.com/WojciechMula). Typically 2x faster.  [#528](https://github.com/klauspost/compress/pull/528) [#531](https://github.com/klauspost/compress/pull/531) [#545](https://github.com/klauspost/compress/pull/545) [#537](https://github.com/klauspost/compress/pull/537)
	* zstd: Add options to ZipDecompressor and fixes [#539](https://github.com/klauspost/compress/pull/539)
	* s2: Use sorted search for index [#555](https://github.com/klauspost/compress/pull/555)
	* Minimum version is Go 1.16, added CI test on 1.18.

* Mar 11, 2022 (v1.15.1)
	* huff0: Add x86 assembly of Decode4X by @WojciechMula in [#512](https://github.com/klauspost/compress/pull/512)
	* zstd: Reuse zip decoders in [#514](https://github.com/klauspost/compress/pull/514)
	* zstd: Detect extra block data and report as corrupted in [#520](https://github.com/klauspost/compress/pull/520)
	* zstd: Handle zero sized frame content size stricter in [#521](https://github.com/klauspost/compress/pull/521)
	* zstd: Add stricter block size checks in [#523](https://github.com/klauspost/compress/pull/523)

* Mar 3, 2022 (v1.15.0)
	* zstd: Refactor decoder by @klauspost in [#498](https://github.com/klauspost/compress/pull/498)
	* zstd: Add stream encoding without goroutines by @klauspost in [#505](https://github.com/klauspost/compress/pull/505)
	* huff0: Prevent single blocks exceeding 16 bits by @klauspost in[#507](https://github.com/klauspost/compress/pull/507)
	* flate: Inline literal emission by @klauspost in [#509](https://github.com/klauspost/compress/pull/509)
	* gzhttp: Add zstd to transport by @klauspost in [#400](https://github.com/klauspost/compress/pull/400)
	* gzhttp: Make content-type optional by @klauspost in [#510](https://github.com/klauspost/compress/pull/510)

Both compression and decompression now supports "synchronous" stream operations. This means that whenever "concurrency" is set to 1, they will operate without spawning goroutines.

Stream decompression is now faster on asynchronous, since the goroutine allocation much more effectively splits the workload. On typical streams this will typically use 2 cores fully for decompression. When a stream has finished decoding no goroutines will be left over, so decoders can now safely be pooled and still be garbage collected.

While the release has been extensively tested, it is recommended to testing when upgrading.

</details>

<details>
	<summary>See changes to v1.14.x</summary>
	
* Feb 22, 2022 (v1.14.4)
	* flate: Fix rare huffman only (-2) corruption. [#503](https://github.com/klauspost/compress/pull/503)
	* zip: Update deprecated CreateHeaderRaw to correctly call CreateRaw by @saracen in [#502](https://github.com/klauspost/compress/pull/502)
	* zip: don't read data descriptor early by @saracen in [#501](https://github.com/klauspost/compress/pull/501)  #501
	* huff0: Use static decompression buffer up to 30% faster by @klauspost in [#499](https://github.com/klauspost/compress/pull/499) [#500](https://github.com/klauspost/compress/pull/500)

* Feb 17, 2022 (v1.14.3)
	* flate: Improve fastest levels compression speed ~10% more throughput. [#482](https://github.com/klauspost/compress/pull/482) [#489](https://github.com/klauspost/compress/pull/489) [#490](https://github.com/klauspost/compress/pull/490) [#491](https://github.com/klauspost/compress/pull/491) [#494](https://github.com/klauspost/compress/pull/494)  [#478](https://github.com/klauspost/compress/pull/478)
	* flate: Faster decompression speed, ~5-10%. [#483](https://github.com/klauspost/compress/pull/483)
	* s2: Faster compression with Go v1.18 and amd64 microarch level 3+. [#484](https://github.com/klauspost/compress/pull/484) [#486](https://github.com/klauspost/compress/pull/486)

* Jan 25, 2022 (v1.14.2)
	* zstd: improve header decoder by @dsnet  [#476](https://github.com/klauspost/compress/pull/476)
	* zstd: Add bigger default blocks  [#469](https://github.com/klauspost/compress/pull/469)
	* zstd: Remove unused decompression buffer [#470](https://github.com/klauspost/compress/pull/470)
	* zstd: Fix logically dead code by @ningmingxiao [#472](https://github.com/klauspost/compress/pull/472)
	* flate: Improve level 7-9 [#471](https://github.com/klauspost/compress/pull/471) [#473](https://github.com/klauspost/compress/pull/473)
	* zstd: Add noasm tag for xxhash [#475](https://github.com/klauspost/compress/pull/475)

* Jan 11, 2022 (v1.14.1)
	* s2: Add stream index in [#462](https://github.com/klauspost/compress/pull/462)
	* flate: Speed and efficiency improvements in [#439](https://github.com/klauspost/compress/pull/439) [#461](https://github.com/klauspost/compress/pull/461) [#455](https://github.com/klauspost/compress/pull/455) [#452](https://github.com/klauspost/compress/pull/452) [#458](https://github.com/klauspost/compress/pull/458)
	* zstd: Performance improvement in [#420]( https://github.com/klauspost/compress/pull/420) [#456](https://github.com/klauspost/compress/pull/456) [#437](https://github.com/klauspost/compress/pull/437) [#467](https://github.com/klauspost/compress/pull/467) [#468](https://github.com/klauspost/compress/pull/468)
	* zstd: add arm64 xxhash assembly in [#464](https://github.com/klauspost/compress/pull/464)
	* Add garbled for binaries for s2 in [#445](https://github.com/klauspost/compress/pull/445)
</details>

<details>
	<summary>See changes to v1.13.x</summary>
	
* Aug 30, 2021 (v1.13.5)
	* gz/zlib/flate: Alias stdlib errors [#425](https://github.com/klauspost/compress/pull/425)
	* s2: Add block support to commandline tools [#413](https://github.com/klauspost/compress/pull/413)
	* zstd: pooledZipWriter should return Writers to the same pool [#426](https://github.com/klauspost/compress/pull/426)
	* Removed golang/snappy as external dependency for tests [#421](https://github.com/klauspost/compress/pull/421)

* Aug 12, 2021 (v1.13.4)
	* Add [snappy replacement package](https://github.com/klauspost/compress/tree/master/snappy).
	* zstd: Fix incorrect encoding in "best" mode [#415](https://github.com/klauspost/compress/pull/415)

* Aug 3, 2021 (v1.13.3) 
	* zstd: Improve Best compression [#404](https://github.com/klauspost/compress/pull/404)
	* zstd: Fix WriteTo error forwarding [#411](https://github.com/klauspost/compress/pull/411)
	* gzhttp: Return http.HandlerFunc instead of http.Handler. Unlikely breaking change. [#406](https://github.com/klauspost/compress/pull/406)
	* s2sx: Fix max size error [#399](https://github.com/klauspost/compress/pull/399)
	* zstd: Add optional stream content size on reset [#401](https://github.com/klauspost/compress/pull/401)
	* zstd: use SpeedBestCompression for level >= 10 [#410](https://github.com/klauspost/compress/pull/410)

* Jun 14, 2021 (v1.13.1)
	* s2: Add full Snappy output support  [#396](https://github.com/klauspost/compress/pull/396)
	* zstd: Add configurable [Decoder window](https://pkg.go.dev/github.com/klauspost/compress/zstd#WithDecoderMaxWindow) size [#394](https://github.com/klauspost/compress/pull/394)
	* gzhttp: Add header to skip compression  [#389](https://github.com/klauspost/compress/pull/389)
	* s2: Improve speed with bigger output margin  [#395](https://github.com/klauspost/compress/pull/395)

* Jun 3, 2021 (v1.13.0)
	* Added [gzhttp](https://github.com/klauspost/compress/tree/master/gzhttp#gzip-handler) which allows wrapping HTTP servers and clients with GZIP compressors.
	* zstd: Detect short invalid signatures [#382](https://github.com/klauspost/compress/pull/382)
	* zstd: Spawn decoder goroutine only if needed. [#380](https://github.com/klauspost/compress/pull/380)
</details>


<details>
	<summary>See changes to v1.12.x</summary>
	
* May 25, 2021 (v1.12.3)
	* deflate: Better/faster Huffman encoding [#374](https://github.com/klauspost/compress/pull/374)
	* deflate: Allocate less for history. [#375](https://github.com/klauspost/compress/pull/375)
	* zstd: Forward read errors [#373](https://github.com/klauspost/compress/pull/373) 

* Apr 27, 2021 (v1.12.2)
	* zstd: Improve better/best compression [#360](https://github.com/klauspost/compress/pull/360) [#364](https://github.com/klauspost/compress/pull/364) [#365](https://github.com/klauspost/compress/pull/365)
	* zstd: Add helpers to compress/decompress zstd inside zip files [#363](https://github.com/klauspost/compress/pull/363)
	* deflate: Improve level 5+6 compression [#367](https://github.com/klauspost/compress/pull/367)
	* s2: Improve better/best compression [#358](https://github.com/klauspost/compress/pull/358) [#359](https://github.com/klauspost/compress/pull/358)
	* s2: Load after checking src limit on amd64. [#362](https://github.com/klauspost/compress/pull/362)
	* s2sx: Limit max executable size [#368](https://github.com/klauspost/compress/pull/368) 

* Apr 14, 2021 (v1.12.1)
	* snappy package removed. Upstream added as dependency.
	* s2: Better compression in "best" mode [#353](https://github.com/klauspost/compress/pull/353)
	* s2sx: Add stdin input and detect pre-compressed from signature [#352](https://github.com/klauspost/compress/pull/352)
	* s2c/s2d: Add http as possible input [#348](https://github.com/klauspost/compress/pull/348)
	* s2c/s2d/s2sx: Always truncate when writing files [#352](https://github.com/klauspost/compress/pull/352)
	* zstd: Reduce memory usage further when using [WithLowerEncoderMem](https://pkg.go.dev/github.com/klauspost/compress/zstd#WithLowerEncoderMem) [#346](https://github.com/klauspost/compress/pull/346)
	* s2: Fix potential problem with amd64 assembly and profilers [#349](https://github.com/klauspost/compress/pull/349)
</details>

<details>
	<summary>See changes to v1.11.x</summary>
	
* Mar 26, 2021 (v1.11.13)
	* zstd: Big speedup on small dictionary encodes [#344](https://github.com/klauspost/compress/pull/344) [#345](https://github.com/klauspost/compress/pull/345)
	* zstd: Add [WithLowerEncoderMem](https://pkg.go.dev/github.com/klauspost/compress/zstd#WithLowerEncoderMem) encoder option [#336](https://github.com/klauspost/compress/pull/336)
	* deflate: Improve entropy compression [#338](https://github.com/klauspost/compress/pull/338)
	* s2: Clean up and minor performance improvement in best [#341](https://github.com/klauspost/compress/pull/341)

* Mar 5, 2021 (v1.11.12)
	* s2: Add `s2sx` binary that creates [self extracting archives](https://github.com/klauspost/compress/tree/master/s2#s2sx-self-extracting-archives).
	* s2: Speed up decompression on non-assembly platforms [#328](https://github.com/klauspost/compress/pull/328)

* Mar 1, 2021 (v1.11.9)
	* s2: Add ARM64 decompression assembly. Around 2x output speed. [#324](https://github.com/klauspost/compress/pull/324)
	* s2: Improve "better" speed and efficiency. [#325](https://github.com/klauspost/compress/pull/325)
	* s2: Fix binaries.

* Feb 25, 2021 (v1.11.8)
	* s2: Fixed occasional out-of-bounds write on amd64. Upgrade recommended.
	* s2: Add AMD64 assembly for better mode. 25-50% faster. [#315](https://github.com/klauspost/compress/pull/315)
	* s2: Less upfront decoder allocation. [#322](https://github.com/klauspost/compress/pull/322)
	* zstd: Faster "compression" of incompressible data. [#314](https://github.com/klauspost/compress/pull/314)
	* zip: Fix zip64 headers. [#313](https://github.com/klauspost/compress/pull/313)
  
* Jan 14, 2021 (v1.11.7)
	* Use Bytes() interface to get bytes across packages. [#309](https://github.com/klauspost/compress/pull/309)
	* s2: Add 'best' compression option.  [#310](https://github.com/klauspost/compress/pull/310)
	* s2: Add ReaderMaxBlockSize, changes `s2.NewReader` signature to include varargs. [#311](https://github.com/klauspost/compress/pull/311)
	* s2: Fix crash on small better buffers. [#308](https://github.com/klauspost/compress/pull/308)
	* s2: Clean up decoder. [#312](https://github.com/klauspost/compress/pull/312)

* Jan 7, 2021 (v1.11.6)
	* zstd: Make decoder allocations smaller [#306](https://github.com/klauspost/compress/pull/306)
	* zstd: Free Decoder resources when Reset is called with a nil io.Reader  [#305](https://github.com/klauspost/compress/pull/305)

* Dec 20, 2020 (v1.11.4)
	* zstd: Add Best compression mode [#304](https://github.com/klauspost/compress/pull/304)
	* Add header decoder [#299](https://github.com/klauspost/compress/pull/299)
	* s2: Add uncompressed stream option [#297](https://github.com/klauspost/compress/pull/297)
	* Simplify/speed up small blocks with known max size. [#300](https://github.com/klauspost/compress/pull/300)
	* zstd: Always reset literal dict encoder [#303](https://github.com/klauspost/compress/pull/303)

* Nov 15, 2020 (v1.11.3)
	* inflate: 10-15% faster decompression  [#293](https://github.com/klauspost/compress/pull/293)
	* zstd: Tweak DecodeAll default allocation [#295](https://github.com/klauspost/compress/pull/295)

* Oct 11, 2020 (v1.11.2)
	* s2: Fix out of bounds read in "better" block compression [#291](https://github.com/klauspost/compress/pull/291)

* Oct 1, 2020 (v1.11.1)
	* zstd: Set allLitEntropy true in default configuration [#286](https://github.com/klauspost/compress/pull/286)

* Sept 8, 2020 (v1.11.0)
	* zstd: Add experimental compression [dictionaries](https://github.com/klauspost/compress/tree/master/zstd#dictionaries) [#281](https://github.com/klauspost/compress/pull/281)
	* zstd: Fix mixed Write and ReadFrom calls [#282](https://github.com/klauspost/compress/pull/282)
	* inflate/gz: Limit variable shifts, ~5% faster decompression [#274](https://github.com/klauspost/compress/pull/274)
</details>

<details>
	<summary>See changes to v1.10.x</summary>
 
* July 8, 2020 (v1.10.11) 
	* zstd: Fix extra block when compressing with ReadFrom. [#278](https://github.com/klauspost/compress/pull/278)
	* huff0: Also populate compression table when reading decoding table. [#275](https://github.com/klauspost/compress/pull/275)
	
* June 23, 2020 (v1.10.10) 
	* zstd: Skip entropy compression in fastest mode when no matches. [#270](https://github.com/klauspost/compress/pull/270)
	
* June 16, 2020 (v1.10.9): 
	* zstd: API change for specifying dictionaries. See [#268](https://github.com/klauspost/compress/pull/268)
	* zip: update CreateHeaderRaw to handle zip64 fields. [#266](https://github.com/klauspost/compress/pull/266)
	* Fuzzit tests removed. The service has been purchased and is no longer available.
	
* June 5, 2020 (v1.10.8): 
	* 1.15x faster zstd block decompression. [#265](https://github.com/klauspost/compress/pull/265)
	
* June 1, 2020 (v1.10.7): 
	* Added zstd decompression [dictionary support](https://github.com/klauspost/compress/tree/master/zstd#dictionaries)
	* Increase zstd decompression speed up to 1.19x.  [#259](https://github.com/klauspost/compress/pull/259)
	* Remove internal reset call in zstd compression and reduce allocations. [#263](https://github.com/klauspost/compress/pull/263)
	
* May 21, 2020: (v1.10.6) 
	* zstd: Reduce allocations while decoding. [#258](https://github.com/klauspost/compress/pull/258), [#252](https://github.com/klauspost/compress/pull/252)
	* zstd: Stricter decompression checks.
	
* April 12, 2020: (v1.10.5)
	* s2-commands: Flush output when receiving SIGINT. [#239](https://github.com/klauspost/compress/pull/239)
	
* Apr 8, 2020: (v1.10.4) 
	* zstd: Minor/special case optimizations. [#251](https://github.com/klauspost/compress/pull/251),  [#250](https://github.com/klauspost/compress/pull/250),  [#249](https://github.com/klauspost/compress/pull/249),  [#247](https://github.com/klauspost/compress/pull/247)
* Mar 11, 2020: (v1.10.3) 
	* s2: Use S2 encoder in pure Go mode for Snappy output as well. [#245](https://github.com/klauspost/compress/pull/245)
	* s2: Fix pure Go block encoder. [#244](https://github.com/klauspost/compress/pull/244)
	* zstd: Added "better compression" mode. [#240](https://github.com/klauspost/compress/pull/240)
	* zstd: Improve speed of fastest compression mode by 5-10% [#241](https://github.com/klauspost/compress/pull/241)
	* zstd: Skip creating encoders when not needed. [#238](https://github.com/klauspost/compress/pull/238)
	
* Feb 27, 2020: (v1.10.2) 
	* Close to 50% speedup in inflate (gzip/zip decompression). [#236](https://github.com/klauspost/compress/pull/236) [#234](https://github.com/klauspost/compress/pull/234) [#232](https://github.com/klauspost/compress/pull/232)
	* Reduce deflate level 1-6 memory usage up to 59%. [#227](https://github.com/klauspost/compress/pull/227)
	
* Feb 18, 2020: (v1.10.1)
	* Fix zstd crash when resetting multiple times without sending data. [#226](https://github.com/klauspost/compress/pull/226)
	* deflate: Fix dictionary use on level 1-6. [#224](https://github.com/klauspost/compress/pull/224)
	* Remove deflate writer reference when closing. [#224](https://github.com/klauspost/compress/pull/224)
	
* Feb 4, 2020: (v1.10.0) 
	* Add optional dictionary to [stateless deflate](https://pkg.go.dev/github.com/klauspost/compress/flate?tab=doc#StatelessDeflate). Breaking change, send `nil` for previous behaviour. [#216](https://github.com/klauspost/compress/pull/216)
	* Fix buffer overflow on repeated small block deflate.  [#218](https://github.com/klauspost/compress/pull/218)
	* Allow copying content from an existing ZIP file without decompressing+compressing. [#214](https://github.com/klauspost/compress/pull/214)
	* Added [S2](https://github.com/klauspost/compress/tree/master/s2#s2-compression) AMD64 assembler and various optimizations. Stream speed >10GB/s.  [#186](https://github.com/klauspost/compress/pull/186)

</details>

<details>
	<summary>See changes prior to v1.10.0</summary>

* Jan 20,2020 (v1.9.8) Optimize gzip/deflate with better size estimates and faster table generation. [#207](https://github.com/klauspost/compress/pull/207) by [luyu6056](https://github.com/luyu6056),  [#206](https://github.com/klauspost/compress/pull/206).
* Jan 11, 2020: S2 Encode/Decode will use provided buffer if capacity is big enough. [#204](https://github.com/klauspost/compress/pull/204) 
* Jan 5, 2020: (v1.9.7) Fix another zstd regression in v1.9.5 - v1.9.6 removed.
* Jan 4, 2020: (v1.9.6) Regression in v1.9.5 fixed causing corrupt zstd encodes in rare cases.
* Jan 4, 2020: Faster IO in [s2c + s2d commandline tools](https://github.com/klauspost/compress/tree/master/s2#commandline-tools) compression/decompression. [#192](https://github.com/klauspost/compress/pull/192)
* Dec 29, 2019: Removed v1.9.5 since fuzz tests showed a compatibility problem with the reference zstandard decoder.
* Dec 29, 2019: (v1.9.5) zstd: 10-20% faster block compression. [#199](https://github.com/klauspost/compress/pull/199)
* Dec 29, 2019: [zip](https://godoc.org/github.com/klauspost/compress/zip) package updated with latest Go features
* Dec 29, 2019: zstd: Single segment flag condintions tweaked. [#197](https://github.com/klauspost/compress/pull/197)
* Dec 18, 2019: s2: Faster compression when ReadFrom is used. [#198](https://github.com/klauspost/compress/pull/198)
* Dec 10, 2019: s2: Fix repeat length output when just above at 16MB limit.
* Dec 10, 2019: zstd: Add function to get decoder as io.ReadCloser. [#191](https://github.com/klauspost/compress/pull/191)
* Dec 3, 2019: (v1.9.4) S2: limit max repeat length. [#188](https://github.com/klauspost/compress/pull/188)
* Dec 3, 2019: Add [WithNoEntropyCompression](https://godoc.org/github.com/klauspost/compress/zstd#WithNoEntropyCompression) to zstd [#187](https://github.com/klauspost/compress/pull/187)
* Dec 3, 2019: Reduce memory use for tests. Check for leaked goroutines.
* Nov 28, 2019 (v1.9.3) Less allocations in stateless deflate.
* Nov 28, 2019: 5-20% Faster huff0 decode. Impacts zstd as well. [#184](https://github.com/klauspost/compress/pull/184)
* Nov 12, 2019 (v1.9.2) Added [Stateless Compression](#stateless-compression) for gzip/deflate.
* Nov 12, 2019: Fixed zstd decompression of large single blocks. [#180](https://github.com/klauspost/compress/pull/180)
* Nov 11, 2019: Set default  [s2c](https://github.com/klauspost/compress/tree/master/s2#commandline-tools) block size to 4MB.
* Nov 11, 2019: Reduce inflate memory use by 1KB.
* Nov 10, 2019: Less allocations in deflate bit writer.
* Nov 10, 2019: Fix inconsistent error returned by zstd decoder.
* Oct 28, 2019 (v1.9.1) ztsd: Fix crash when compressing blocks. [#174](https://github.com/klauspost/compress/pull/174)
* Oct 24, 2019 (v1.9.0) zstd: Fix rare data corruption [#173](https://github.com/klauspost/compress/pull/173)
* Oct 24, 2019 zstd: Fix huff0 out of buffer write [#171](https://github.com/klauspost/compress/pull/171) and always return errors [#172](https://github.com/klauspost/compress/pull/172) 
* Oct 10, 2019: Big deflate rewrite, 30-40% faster with better compression [#105](https://github.com/klauspost/compress/pull/105)

</details>

<details>
	<summary>See changes prior to v1.9.0</summary>

* Oct 10, 2019: (v1.8.6) zstd: Allow partial reads to get flushed data. [#169](https://github.com/klauspost/compress/pull/169)
* Oct 3, 2019: Fix inconsistent results on broken zstd streams.
* Sep 25, 2019: Added `-rm` (remove source files) and `-q` (no output except errors) to `s2c` and `s2d` [commands](https://github.com/klauspost/compress/tree/master/s2#commandline-tools)
* Sep 16, 2019: (v1.8.4) Add `s2c` and `s2d` [commandline tools](https://github.com/klauspost/compress/tree/master/s2#commandline-tools).
* Sep 10, 2019: (v1.8.3) Fix s2 decoder [Skip](https://godoc.org/github.com/klauspost/compress/s2#Reader.Skip).
* Sep 7, 2019: zstd: Added [WithWindowSize](https://godoc.org/github.com/klauspost/compress/zstd#WithWindowSize), contributed by [ianwilkes](https://github.com/ianwilkes).
* Sep 5, 2019: (v1.8.2) Add [WithZeroFrames](https://godoc.org/github.com/klauspost/compress/zstd#WithZeroFrames) which adds full zero payload block encoding option.
* Sep 5, 2019: Lazy initialization of zstandard predefined en/decoder tables.
* Aug 26, 2019: (v1.8.1) S2: 1-2% compression increase in "better" compression mode.
* Aug 26, 2019: zstd: Check maximum size of Huffman 1X compressed literals while decoding.
* Aug 24, 2019: (v1.8.0) Added [S2 compression](https://github.com/klauspost/compress/tree/master/s2#s2-compression), a high performance replacement for Snappy. 
* Aug 21, 2019: (v1.7.6) Fixed minor issues found by fuzzer. One could lead to zstd not decompressing.
* Aug 18, 2019: Add [fuzzit](https://fuzzit.dev/) continuous fuzzing.
* Aug 14, 2019: zstd: Skip incompressible data 2x faster.  [#147](https://github.com/klauspost/compress/pull/147)
* Aug 4, 2019 (v1.7.5): Better literal compression. [#146](https://github.com/klauspost/compress/pull/146)
* Aug 4, 2019: Faster zstd compression. [#143](https://github.com/klauspost/compress/pull/143) [#144](https://github.com/klauspost/compress/pull/144)
* Aug 4, 2019: Faster zstd decompression. [#145](https://github.com/klauspost/compress/pull/145) [#143](https://github.com/klauspost/compress/pull/143) [#142](https://github.com/klauspost/compress/pull/142)
* July 15, 2019 (v1.7.4): Fix double EOF block in rare cases on zstd encoder.
* July 15, 2019 (v1.7.3): Minor speedup/compression increase in default zstd encoder.
* July 14, 2019: zstd decoder: Fix decompression error on multiple uses with mixed content.
* July 7, 2019 (v1.7.2): Snappy update, zstd decoder potential race fix.
* June 17, 2019: zstd decompression bugfix.
* June 17, 2019: fix 32 bit builds.
* June 17, 2019: Easier use in modules (less dependencies).
* June 9, 2019: New stronger "default" [zstd](https://github.com/klauspost/compress/tree/master/zstd#zstd) compression mode. Matches zstd default compression ratio.
* June 5, 2019: 20-40% throughput in [zstandard](https://github.com/klauspost/compress/tree/master/zstd#zstd) compression and better compression.
* June 5, 2019: deflate/gzip compression: Reduce memory usage of lower compression levels.
* June 2, 2019: Added [zstandard](https://github.com/klauspost/compress/tree/master/zstd#zstd) compression!
* May 25, 2019: deflate/gzip: 10% faster bit writer, mostly visible in lower levels.
* Apr 22, 2019: [zstd](https://github.com/klauspost/compress/tree/master/zstd#zstd) decompression added.
* Aug 1, 2018: Added [huff0 README](https://github.com/klauspost/compress/tree/master/huff0#huff0-entropy-compression).
* Jul 8, 2018: Added [Performance Update 2018](#performance-update-2018) below.
* Jun 23, 2018: Merged [Go 1.11 inflate optimizations](https://go-review.googlesource.com/c/go/+/102235). Go 1.9 is now required. Backwards compatible version tagged with [v1.3.0](https://github.com/klauspost/compress/releases/tag/v1.3.0).
* Apr 2, 2018: Added [huff0](https://godoc.org/github.com/klauspost/compress/huff0) en/decoder. Experimental for now, API may change.
* Mar 4, 2018: Added [FSE Entropy](https://godoc.org/github.com/klauspost/compress/fse) en/decoder. Experimental for now, API may change.
* Nov 3, 2017: Add compression [Estimate](https://godoc.org/github.com/klauspost/compress#Estimate) function.
* May 28, 2017: Reduce allocations when resetting decoder.
* Apr 02, 2017: Change back to official crc32, since changes were merged in Go 1.7.
* Jan 14, 2017: Reduce stack pressure due to array copies. See [Issue #18625](https://github.com/golang/go/issues/18625).
* Oct 25, 2016: Level 2-4 have been rewritten and now offers significantly better performance than before.
* Oct 20, 2016: Port zlib changes from Go 1.7 to fix zlib writer issue. Please update.
* Oct 16, 2016: Go 1.7 changes merged. Apples to apples this package is a few percent faster, but has a significantly better balance between speed and compression per level. 
* Mar 24, 2016: Always attempt Huffman encoding on level 4-7. This improves base 64 encoded data compression.
* Mar 24, 2016: Small speedup for level 1-3.
* Feb 19, 2016: Faster bit writer, level -2 is 15% faster, level 1 is 4% faster.
* Feb 19, 2016: Handle small payloads faster in level 1-3.
* Feb 19, 2016: Added faster level 2 + 3 compression modes.
* Feb 19, 2016: [Rebalanced compression levels](https://blog.klauspost.com/rebalancing-deflate-compression-levels/), so there is a more even progression in terms of compression. New default level is 5.
* Feb 14, 2016: Snappy: Merge upstream changes. 
* Feb 14, 2016: Snappy: Fix aggressive skipping.
* Feb 14, 2016: Snappy: Update benchmark.
* Feb 13, 2016: Deflate: Fixed assembler problem that could lead to sub-optimal compression.
* Feb 12, 2016: Snappy: Added AMD64 SSE 4.2 optimizations to matching, which makes easy to compress material run faster. Typical speedup is around 25%.
* Feb 9, 2016: Added Snappy package fork. This version is 5-7% faster, much more on hard to compress content.
* Jan 30, 2016: Optimize level 1 to 3 by not considering static dictionary or storing uncompressed. ~4-5% speedup.
* Jan 16, 2016: Optimization on deflate level 1,2,3 compression.
* Jan 8 2016: Merge [CL 18317](https://go-review.googlesource.com/#/c/18317): fix reading, writing of zip64 archives.
* Dec 8 2015: Make level 1 and -2 deterministic even if write size differs.
* Dec 8 2015: Split encoding functions, so hashing and matching can potentially be inlined. 1-3% faster on AMD64. 5% faster on other platforms.
* Dec 8 2015: Fixed rare [one byte out-of bounds read](https://github.com/klauspost/compress/issues/20). Please update!
* Nov 23 2015: Optimization on token writer. ~2-4% faster. Contributed by [@dsnet](https://github.com/dsnet).
* Nov 20 2015: Small optimization to bit writer on 64 bit systems.
* Nov 17 2015: Fixed out-of-bound errors if the underlying Writer returned an error. See [#15](https://github.com/klauspost/compress/issues/15).
* Nov 12 2015: Added [io.WriterTo](https://golang.org/pkg/io/#WriterTo) support to gzip/inflate.
* Nov 11 2015: Merged [CL 16669](https://go-review.googlesource.com/#/c/16669/4): archive/zip: enable overriding (de)compressors per file
* Oct 15 2015: Added skipping on uncompressible data. Random data speed up >5x.

</details>

# deflate usage

The packages are drop-in replacements for standard libraries. Simply replace the import path to use them:

| old import         | new import                              | Documentation
|--------------------|-----------------------------------------|--------------------|
| `compress/gzip`    | `github.com/klauspost/compress/gzip`    | [gzip](https://pkg.go.dev/github.com/klauspost/compress/gzip?tab=doc)
| `compress/zlib`    | `github.com/klauspost/compress/zlib`    | [zlib](https://pkg.go.dev/github.com/klauspost/compress/zlib?tab=doc)
| `archive/zip`      | `github.com/klauspost/compress/zip`     | [zip](https://pkg.go.dev/github.com/klauspost/compress/zip?tab=doc)
| `compress/flate`   | `github.com/klauspost/compress/flate`   | [flate](https://pkg.go.dev/github.com/klauspost/compress/flate?tab=doc)

* Optimized [deflate](https://godoc.org/github.com/klauspost/compress/flate) packages which can be used as a dropin replacement for [gzip](https://godoc.org/github.com/klauspost/compress/gzip), [zip](https://godoc.org/github.com/klauspost/compress/zip) and [zlib](https://godoc.org/github.com/klauspost/compress/zlib).

You may also be interested in [pgzip](https://github.com/klauspost/pgzip), which is a drop in replacement for gzip, which support multithreaded compression on big files and the optimized [crc32](https://github.com/klauspost/crc32) package used by these packages.

The packages contains the same as the standard library, so you can use the godoc for that: [gzip](http://golang.org/pkg/compress/gzip/), [zip](http://golang.org/pkg/archive/zip/),  [zlib](http://golang.org/pkg/compress/zlib/), [flate](http://golang.org/pkg/compress/flate/).

Currently there is only minor speedup on decompression (mostly CRC32 calculation).

Memory usage is typically 1MB for a Writer. stdlib is in the same range. 
If you expect to have a lot of concurrently allocated Writers consider using 
the stateless compress described below.

For compression performance, see: [this spreadsheet](https://docs.google.com/spreadsheets/d/1nuNE2nPfuINCZJRMt6wFWhKpToF95I47XjSsc-1rbPQ/edit?usp=sharing).

To disable all assembly add `-tags=noasm`. This works across all packages.

# Stateless compression

This package offers stateless compression as a special option for gzip/deflate. 
It will do compression but without maintaining any state between Write calls.

This means there will be no memory kept between Write calls, but compression and speed will be suboptimal.

This is only relevant in cases where you expect to run many thousands of compressors concurrently, 
but with very little activity. This is *not* intended for regular web servers serving individual requests.  

Because of this, the size of actual Write calls will affect output size.

In gzip, specify level `-3` / `gzip.StatelessCompression` to enable.

For direct deflate use, NewStatelessWriter and StatelessDeflate are available. See [documentation](https://godoc.org/github.com/klauspost/compress/flate#NewStatelessWriter)

A `bufio.Writer` can of course be used to control write sizes. For example, to use a 4KB buffer:

```go
	// replace 'ioutil.Discard' with your output.
	gzw, err := gzip.NewWriterLevel(ioutil.Discard, gzip.StatelessCompression)
	if err != nil {
		return err
	}
	defer gzw.Close()

	w := bufio.NewWriterSize(gzw, 4096)
	defer w.Flush()
	
	// Write to 'w' 
```

This will only use up to 4KB in memory when the writer is idle. 

Compression is almost always worse than the fastest compression level 
and each write will allocate (a little) memory. 

# Performance Update 2018

It has been a while since we have been looking at the speed of this package compared to the standard library, so I thought I would re-do my tests and give some overall recommendations based on the current state. All benchmarks have been performed with Go 1.10 on my Desktop Intel(R) Core(TM) i7-2600 CPU @3.40GHz. Since I last ran the tests, I have gotten more RAM, which means tests with big files are no longer limited by my SSD.

The raw results are in my [updated spreadsheet](https://docs.google.com/spreadsheets/d/1nuNE2nPfuINCZJRMt6wFWhKpToF95I47XjSsc-1rbPQ/edit?usp=sharing). Due to cgo changes and upstream updates i could not get the cgo version of gzip to compile. Instead I included the [zstd](https://github.com/datadog/zstd) cgo implementation. If I get cgo gzip to work again, I might replace the results in the sheet.

The columns to take note of are: *MB/s* - the throughput. *Reduction* - the data size reduction in percent of the original. *Rel Speed* relative speed compared to the standard library at the same level. *Smaller* - how many percent smaller is the compressed output compared to stdlib. Negative means the output was bigger. *Loss* means the loss (or gain) in compression as a percentage difference of the input.

The `gzstd` (standard library gzip) and `gzkp` (this package gzip) only uses one CPU core. [`pgzip`](https://github.com/klauspost/pgzip), [`bgzf`](https://github.com/biogo/hts/tree/master/bgzf) uses all 4 cores. [`zstd`](https://github.com/DataDog/zstd) uses one core, and is a beast (but not Go, yet).


## Overall differences.

There appears to be a roughly 5-10% speed advantage over the standard library when comparing at similar compression levels.

The biggest difference you will see is the result of [re-balancing](https://blog.klauspost.com/rebalancing-deflate-compression-levels/) the compression levels. I wanted by library to give a smoother transition between the compression levels than the standard library.

This package attempts to provide a more smooth transition, where "1" is taking a lot of shortcuts, "5" is the reasonable trade-off and "9" is the "give me the best compression", and the values in between gives something reasonable in between. The standard library has big differences in levels 1-4, but levels 5-9 having no significant gains - often spending a lot more time than can be justified by the achieved compression.

There are links to all the test data in the [spreadsheet](https://docs.google.com/spreadsheets/d/1nuNE2nPfuINCZJRMt6wFWhKpToF95I47XjSsc-1rbPQ/edit?usp=sharing) in the top left field on each tab.

## Web Content

This test set aims to emulate typical use in a web server. The test-set is 4GB data in 53k files, and is a mixture of (mostly) HTML, JS, CSS.

Since level 1 and 9 are close to being the same code, they are quite close. But looking at the levels in-between the differences are quite big.

Looking at level 6, this package is 88% faster, but will output about 6% more data. For a web server, this means you can serve 88% more data, but have to pay for 6% more bandwidth. You can draw your own conclusions on what would be the most expensive for your case.

## Object files

This test is for typical data files stored on a server. In this case it is a collection of Go precompiled objects. They are very compressible.

The picture is similar to the web content, but with small differences since this is very compressible. Levels 2-3 offer good speed, but is sacrificing quite a bit of compression. 

The standard library seems suboptimal on level 3 and 4 - offering both worse compression and speed than level 6 & 7 of this package respectively.

## Highly Compressible File

This is a JSON file with very high redundancy. The reduction starts at 95% on level 1, so in real life terms we are dealing with something like a highly redundant stream of data, etc.

It is definitely visible that we are dealing with specialized content here, so the results are very scattered. This package does not do very well at levels 1-4, but picks up significantly at level 5 and levels 7 and 8 offering great speed for the achieved compression.

So if you know you content is extremely compressible you might want to go slightly higher than the defaults. The standard library has a huge gap between levels 3 and 4 in terms of speed (2.75x slowdown), so it offers little "middle ground".

## Medium-High Compressible

This is a pretty common test corpus: [enwik9](http://mattmahoney.net/dc/textdata.html). It contains the first 10^9 bytes of the English Wikipedia dump on Mar. 3, 2006. This is a very good test of typical text based compression and more data heavy streams.

We see a similar picture here as in "Web Content". On equal levels some compression is sacrificed for more speed. Level 5 seems to be the best trade-off between speed and size, beating stdlib level 3 in both.

## Medium Compressible

I will combine two test sets, one [10GB file set](http://mattmahoney.net/dc/10gb.html) and a VM disk image (~8GB). Both contain different data types and represent a typical backup scenario.

The most notable thing is how quickly the standard library drops to very low compression speeds around level 5-6 without any big gains in compression. Since this type of data is fairly common, this does not seem like good behavior.


## Un-compressible Content

This is mainly a test of how good the algorithms are at detecting un-compressible input. The standard library only offers this feature with very conservative settings at level 1. Obviously there is no reason for the algorithms to try to compress input that cannot be compressed.  The only downside is that it might skip some compressible data on false detections.


## Huffman only compression

This compression library adds a special compression level, named `HuffmanOnly`, which allows near linear time compression. This is done by completely disabling matching of previous data, and only reduce the number of bits to represent each character. 

This means that often used characters, like 'e' and ' ' (space) in text use the fewest bits to represent, and rare characters like '¤' takes more bits to represent. For more information see [wikipedia](https://en.wikipedia.org/wiki/Huffman_coding) or this nice [video](https://youtu.be/ZdooBTdW5bM).

Since this type of compression has much less variance, the compression speed is mostly unaffected by the input data, and is usually more than *180MB/s* for a single core.

The downside is that the compression ratio is usually considerably worse than even the fastest conventional compression. The compression ratio can never be better than 8:1 (12.5%). 

The linear time compression can be used as a "better than nothing" mode, where you cannot risk the encoder to slow down on some content. For comparison, the size of the "Twain" text is *233460 bytes* (+29% vs. level 1) and encode speed is 144MB/s (4.5x level 1). So in this case you trade a 30% size increase for a 4 times speedup.

For more information see my blog post on [Fast Linear Time Compression](http://blog.klauspost.com/constant-time-gzipzip-compression/).

This is implemented on Go 1.7 as "Huffman Only" mode, though not exposed for gzip.

# Other packages

Here are other packages of good quality and pure Go (no cgo wrappers or autoconverted code):

* [github.com/pierrec/lz4](https://github.com/pierrec/lz4) - strong multithreaded LZ4 compression.
* [github.com/cosnicolaou/pbzip2](https://github.com/cosnicolaou/pbzip2) - multithreaded bzip2 decompression.
* [github.com/dsnet/compress](https://github.com/dsnet/compress) - brotli decompression, bzip2 writer.
* [github.com/ronanh/intcomp](https://github.com/ronanh/intcomp) - Integer compression.
* [github.com/spenczar/fpc](https://github.com/spenczar/fpc) - Float compression.
* [github.com/minio/zipindex](https://github.com/minio/zipindex) - External ZIP directory index.
* [github.com/ybirader/pzip](https://github.com/ybirader/pzip) - Fast concurrent zip archiver and extractor.

# license

This code is licensed under the same conditions as the original Go code. See LICENSE file.
