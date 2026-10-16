// Package gguf reads bounded GGUF metadata, never tensor descriptors or weights.
package gguf

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// Header contains only the architecture and attention-sizing integers.
type Header struct {
	Architecture string
	Values       map[string]int64
}

type decoder struct {
	r   *io.LimitedReader
	err error
}

func (d *decoder) number(size int) uint64 {
	var b [8]byte
	if d.err == nil {
		_, d.err = io.ReadFull(d.r, b[:size])
	}
	return binary.LittleEndian.Uint64(b[:])
}

func (d *decoder) text(keep bool) string {
	n := d.number(8)
	if n > 1<<20 {
		d.err = fmt.Errorf("GGUF string limit exceeded")
	}
	if d.err != nil {
		return ""
	}
	if !keep {
		_, d.err = io.CopyN(io.Discard, d.r, int64(n))
		return ""
	}
	b := make([]byte, int(n))
	_, d.err = io.ReadFull(d.r, b)
	return string(b)
}

// value skips arrays without allocation or recursion; nested arrays are unsupported.
func (d *decoder) value(kind uint64, keep bool) (int64, string) {
	if kind == 8 {
		return 0, d.text(keep)
	}
	if kind == 9 {
		element, n := d.number(4), d.number(8)
		if element > 12 || element == 9 || n > 1<<20 {
			d.err = fmt.Errorf("unsupported GGUF array")
		}
		for i := uint64(0); i < n && d.err == nil; i++ {
			d.value(element, false)
		}
		return 0, ""
	}
	size := 0
	switch kind {
	case 0, 1, 7:
		size = 1
	case 2, 3:
		size = 2
	case 4, 5, 6:
		size = 4
	case 10, 11, 12:
		size = 8
	default:
		d.err = fmt.Errorf("unknown GGUF value type")
		return 0, ""
	}
	n := d.number(size)
	switch kind {
	case 0, 2, 4, 10:
		if n <= 1<<63-1 {
			return int64(n), ""
		}
	case 1:
		return int64(int8(n)), ""
	case 3:
		return int64(int16(n)), ""
	case 5:
		return int64(int32(n)), ""
	case 11:
		return int64(n), ""
	}
	return 0, ""
}

// Read accepts little-endian GGUF v2/v3, with at most 4096 keys and 64 MiB of metadata.
func Read(r io.Reader) (Header, error) {
	d := decoder{r: &io.LimitedReader{R: r, N: 64 << 20}}
	h := Header{Values: map[string]int64{}}
	magic, version := d.number(4), d.number(4)
	d.number(8) // Tensor count is intentionally never followed.
	count := d.number(8)
	if magic != 0x46554747 || (version != 2 && version != 3) || count > 4096 {
		return h, fmt.Errorf("unsupported GGUF header")
	}
	seen := map[string]bool{}
	for i := uint64(0); i < count && d.err == nil; i++ {
		key := d.text(true)
		kind := d.number(4)
		if seen[key] {
			return h, fmt.Errorf("duplicate GGUF key")
		}
		seen[key] = true
		_, suffix, _ := strings.Cut(key, ".")
		keep := key == "general.architecture"
		switch suffix {
		case "block_count", "context_length", "embedding_length", "attention.head_count", "attention.head_count_kv", "attention.key_length", "attention.value_length", "attention.sliding_window":
			keep = true
		}
		n, text := d.value(kind, keep)
		if key == "general.architecture" {
			h.Architecture = text
		} else if keep {
			if kind == 8 || kind == 9 || kind == 6 || kind == 7 || kind == 12 || n <= 0 {
				return h, fmt.Errorf("invalid GGUF geometry")
			}
			h.Values[key] = n
		}
	}
	return h, d.err
}
