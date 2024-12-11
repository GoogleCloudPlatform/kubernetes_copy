//go:build (!amd64 && !arm64) || appengine || !gc || purego || noasm
// +build !amd64,!arm64 appengine !gc purego noasm

package xxhash

// Sum64 computes the 64-bit xxHash digest of b.
func Sum64(b []byte) uint64 {
	// A simpler version would be
	//   d := New()
	//   d.Write(b)
	//   return d.Sum64()
	// but this is faster, particularly for small inputs.

	n := len(b)
	var h uint64

	if n >= 32 {
		v1 := primes[0] + prime2
		v2 := prime2
		v3 := uint64(0)
		v4 := -primes[0]
		for len(b) >= 32 {
			v1 = round(v1, u64(b[0:8:len(b)]))
			v2 = round(v2, u64(b[8:16:len(b)]))
			v3 = round(v3, u64(b[16:24:len(b)]))
			v4 = round(v4, u64(b[24:32:len(b)]))
			b = b[32:len(b):len(b)]
		}
		h = rol1(v1) + rol7(v2) + rol12(v3) + rol18(v4)
		h = mergeRound(h, v1)
		h = mergeRound(h, v2)
		h = mergeRound(h, v3)
		h = mergeRound(h, v4)
	} else {
		h = prime5
	}

	h += uint64(n)

	for ; len(b) >= 8; b = b[8:] {
		k1 := round(0, u64(b[:8]))
		h ^= k1
		h = rol27(h)*prime1 + prime4
	}
	if len(b) >= 4 {
		h ^= uint64(u32(b[:4])) * prime1
		h = rol23(h)*prime2 + prime3
		b = b[4:]
	}
	for ; len(b) > 0; b = b[1:] {
		h ^= uint64(b[0]) * prime5
		h = rol11(h) * prime1
	}

	h ^= h >> 33
	h *= prime2
	h ^= h >> 29
	h *= prime3
	h ^= h >> 32

	return h
}

func writeBlocks(d *Digest, b []byte) int {
	v1, v2, v3, v4 := d.v1, d.v2, d.v3, d.v4
	n := len(b)
	for len(b) >= 32 {
		v1 = round(v1, u64(b[0:8:len(b)]))
		v2 = round(v2, u64(b[8:16:len(b)]))
		v3 = round(v3, u64(b[16:24:len(b)]))
		v4 = round(v4, u64(b[24:32:len(b)]))
		b = b[32:len(b):len(b)]
	}
	d.v1, d.v2, d.v3, d.v4 = v1, v2, v3, v4
	return n - len(b)
}
