// mkbench_go generates fixed-size benchmark images for virtual-media throughput
// testing. Two modes:
//
//   - zero   : writes all-zero bytes (write benchmark target — the host writes
//     into this image so it does not matter what the initial content is, but a
//     uniform zero fill makes the file non-sparse on Windows NTFS and
//     pre-allocates every block so the host never has to wait for file growth).
//   - random : writes deterministic pseudo-random bytes (read benchmark source —
//     non-compressible so no OS or driver optimisation can elide the I/O; the
//     PRNG is seeded with a fixed value so the output is reproducible across
//     runs and platforms).
//
// Usage:
//
//	go run ./scripts/mkbench_go -out bin/bench-read-100m.img -size 100MiB -mode random
//	go run ./scripts/mkbench_go -out bin/bench-write-100m.img -size 100MiB -mode zero
//
// Flags:
//
//	-out  string   output file path (default "bin/bench-read-100m.img")
//	-size string   image size in bytes; accepts suffix B/KiB/MiB/GiB (default "100MiB")
//	-mode string   content: "zero" | "random" (default "random")
//
// The generator is intentionally simple: it writes in 1 MiB chunks so it stays
// within a small memory footprint even for large images.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
)

const chunkSize = 1 << 20 // 1 MiB write buffer

func main() {
	out := flag.String("out", "bin/bench-read-100m.img", "output file path")
	sizeStr := flag.String("size", "100MiB", "image size (e.g. 104857600, 100MiB, 100MB)")
	mode := flag.String("mode", "random", "content mode: zero | random")
	flag.Parse()

	size, err := parseSize(*sizeStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkbench: invalid -size %q: %v\n", *sizeStr, err)
		os.Exit(1)
	}
	if size <= 0 {
		fmt.Fprintf(os.Stderr, "mkbench: -size must be > 0\n")
		os.Exit(1)
	}

	var fill func([]byte)
	switch *mode {
	case "zero":
		fill = func(b []byte) {
			for i := range b {
				b[i] = 0
			}
		}
	case "random":
		// Fixed seed → same bytes every run; fast xorshift64 PRNG (not crypto).
		const seed = int64(0x5EED_BABE_C0FFEE) // fixed seed: same bytes every run
		rng := rand.New(rand.NewSource(seed))  //nolint:gosec // deterministic, not crypto
		fill = func(b []byte) {
			for i := 0; i+8 <= len(b); i += 8 {
				v := rng.Uint64()
				b[i+0] = byte(v)
				b[i+1] = byte(v >> 8)
				b[i+2] = byte(v >> 16)
				b[i+3] = byte(v >> 24)
				b[i+4] = byte(v >> 32)
				b[i+5] = byte(v >> 40)
				b[i+6] = byte(v >> 48)
				b[i+7] = byte(v >> 56)
			}
			// tail bytes (if chunk size is not a multiple of 8 — currently it is)
			for i := len(b) &^ 7; i < len(b); i++ {
				b[i] = byte(rng.Uint32())
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "mkbench: unknown -mode %q (want zero|random)\n", *mode)
		os.Exit(1)
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkbench: create %s: %v\n", *out, err)
		os.Exit(1)
	}

	buf := make([]byte, chunkSize)
	remaining := size
	written := int64(0)
	for remaining > 0 {
		n := int64(chunkSize)
		if remaining < n {
			n = remaining
		}
		fill(buf[:n])
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "mkbench: write %s: %v\n", *out, err)
			os.Exit(1)
		}
		written += n
		remaining -= n
	}

	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "mkbench: close %s: %v\n", *out, err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s (%d bytes, mode=%s)\n", *out, written, *mode)
}

// parseSize parses a size string like "104857600", "100MiB", "100MB", "1GiB".
// Accepted suffixes (case-insensitive): B, KiB, MiB, GiB, KB, MB, GB.
// No suffix is treated as bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Split off any unit suffix.
	upper := strings.ToUpper(s)
	type unit struct {
		suffix string
		mult   int64
	}
	units := []unit{
		{"GIB", 1 << 30},
		{"MIB", 1 << 20},
		{"KIB", 1 << 10},
		{"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000},
		{"KB", 1000},
		{"B", 1},
	}
	mult := int64(1)
	numStr := s
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			numStr = s[:len(s)-len(u.suffix)]
			break
		}
	}

	var n int64
	_, err := fmt.Sscanf(strings.TrimSpace(numStr), "%d", &n)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q as integer", numStr)
	}
	return n * mult, nil
}
