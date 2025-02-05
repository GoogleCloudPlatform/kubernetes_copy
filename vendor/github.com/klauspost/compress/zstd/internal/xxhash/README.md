# xxhash

VENDORED: Go to [github.com/cespare/xxhash](https://github.com/cespare/xxhash) for original package.

xxhash is a Go implementation of the 64-bit [xxHash] algorithm, XXH64. This is a
high-quality hashing algorithm that is much faster than anything in the Go
standard library.

This package provides a straightforward API:

```
func Sum64(b []byte) uint64
func Sum64String(s string) uint64
type Digest struct{ ... }
    func New() *Digest
```

The `Digest` type implements hash.Hash64. Its key methods are:

```
func (*Digest) Write([]byte) (int, error)
func (*Digest) WriteString(string) (int, error)
func (*Digest) Sum64() uint64
```

The package is written with optimized pure Go and also contains even faster
assembly implementations for amd64 and arm64. If desired, the `purego` build tag
opts into using the Go code even on those architectures.

[xxHash]: http://cyan4973.github.io/xxHash/

## Compatibility

This package is in a module and the latest code is in version 2 of the module.
You need a version of Go with at least "minimal module compatibility" to use
github.com/cespare/xxhash/v2:

* 1.9.7+ for Go 1.9
* 1.10.3+ for Go 1.10
* Go 1.11 or later

I recommend using the latest release of Go.

## Benchmarks

Here are some quick benchmarks comparing the pure-Go and assembly
implementations of Sum64.

| input size | purego    | asm       |
| ---------- | --------- | --------- |
| 4 B        |  1.3 GB/s |  1.2 GB/s |
| 16 B       |  2.9 GB/s |  3.5 GB/s |
| 100 B      |  6.9 GB/s |  8.1 GB/s |
| 4 KB       | 11.7 GB/s | 16.7 GB/s |
| 10 MB      | 12.0 GB/s | 17.3 GB/s |

These numbers were generated on Ubuntu 20.04 with an Intel Xeon Platinum 8252C
CPU using the following commands under Go 1.19.2:

```
benchstat <(go test -tags purego -benchtime 500ms -count 15 -bench 'Sum64$')
benchstat <(go test -benchtime 500ms -count 15 -bench 'Sum64$')
```

## Projects using this package

- [InfluxDB](https://github.com/influxdata/influxdb)
- [Prometheus](https://github.com/prometheus/prometheus)
- [VictoriaMetrics](https://github.com/VictoriaMetrics/VictoriaMetrics)
- [FreeCache](https://github.com/coocood/freecache)
- [FastCache](https://github.com/VictoriaMetrics/fastcache)
