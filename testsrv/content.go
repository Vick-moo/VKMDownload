// Package testsrv implements a controllable, deliberately hostile HTTP/1.1
// server used to exercise the download engine against misbehaving peers.
//
// Every hostile behaviour is opt-in through Config so that a single test can
// isolate exactly one protocol violation.
package testsrv

import (
	"math/rand/v2"
)

// PatternSize is the period, in bytes, of the deterministic content stream.
//
// The stream is built from two components so that no realistic download can
// accidentally look correct when it is not:
//
//   - a pseudo-random 16 MiB pattern, indexed by absolute offset modulo
//     PatternSize, which catches a worker writing to the wrong offset;
//   - a per-"block" XOR mask derived from offset/PatternSize and the seed,
//     which breaks the 16 MiB period so that an offset error of exactly one
//     period is still detected.
const PatternSize = 16 << 20 // 16 MiB

var pattern [PatternSize]byte

func init() {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i*31 + 7)
	}
	c := rand.NewChaCha8(seed)
	for i := 0; i < PatternSize; {
		v := c.Uint64()
		for b := 0; b < 8 && i < PatternSize; b++ {
			pattern[i] = byte(v >> (8 * b))
			i++
		}
	}
}

// Fill writes the deterministic content of the byte range [off, off+len(dst))
// into dst.
//
// The server uses it to produce response bodies and the tests use it to build
// the expected file, so the two can never drift apart.
func Fill(seed int64, off int64, dst []byte) {
	if len(dst) == 0 {
		return
	}
	// The seed shifts where in the pattern the stream starts and perturbs the
	// per-block mask, so two servers with different seeds never agree.
	phase := uint64(seed)*0x9E3779B97F4A7C15 + 0xD1B54A32D192ED03
	mix := uint64(seed) * 0xC2B2AE3D27D4EB4F

	start := uint64(off)
	for i := 0; i < len(dst); {
		abs := start + uint64(i)
		blk := abs / PatternSize
		blkEnd := (blk + 1) * PatternSize

		p := (abs + phase) % PatternSize
		n := uint64(len(dst) - i)
		if m := PatternSize - p; n > m {
			n = m
		}
		if m := blkEnd - abs; n > m {
			n = m
		}

		seg := dst[i : i+int(n)]
		copy(seg, pattern[p:p+n])

		mask := byte(mix>>(8*(blk%8))) ^ byte((blk*0x9E3779B9)>>24)
		for j := range seg {
			seg[j] ^= mask
		}
		i += int(n)
	}
}

// Expected builds the full expected content of a resource. It is intended for
// tests and modest sizes; prefer comparing streamed chunks for large files.
func Expected(seed, size int64) []byte {
	if size <= 0 {
		return nil
	}
	b := make([]byte, size)
	Fill(seed, 0, b)
	return b
}
