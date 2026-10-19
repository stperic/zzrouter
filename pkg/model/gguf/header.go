// Package gguf reads bounded GGUF metadata, never tensor descriptors or weights.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrUnsupported identifies valid metadata whose layout is not modelled.
var ErrUnsupported = errors.New("unsupported GGUF architecture")

// Header contains only the architecture and attention-sizing integers.
type Header struct {
	Architecture string
	Values       map[string]int64
}

type decoder struct {
	r   *bufio.Reader
	err error
}

func (d *decoder) number(size int) uint64 {
	var b [8]byte
	if d.err == nil {
		_, d.err = io.ReadFull(d.r, b[:size])
	}
	return binary.LittleEndian.Uint64(b[:])
}

func (d *decoder) text(keep bool, limit uint64) string {
	n := d.number(8)
	if d.err == nil && n > limit {
		d.err = fmt.Errorf("GGUF string limit exceeded")
	}
	if d.err != nil {
		return ""
	}
	if !keep {
		_, d.err = d.r.Discard(int(n))
		return ""
	}
	b := make([]byte, int(n))
	_, d.err = io.ReadFull(d.r, b)
	return string(b)
}

// value skips arrays without allocation or recursion; nested arrays are unsupported.
func (d *decoder) value(kind uint64, keep bool) (int64, string) {
	if kind == 8 {
		return 0, d.text(keep, 1<<20)
	}
	if kind == 9 {
		element, n := d.number(4), d.number(8)
		if d.err == nil && (element > 12 || element == 9 || n > 1<<20) {
			d.err = fmt.Errorf("unsupported GGUF array")
		}
		if element != 8 && d.err == nil {
			_, d.err = io.CopyN(io.Discard, d.r, int64(n)*int64(valueSize(element)))
		} else {
			for i := uint64(0); i < n && d.err == nil; i++ {
				d.text(false, 1<<20)
			}
		}
		return 0, ""
	}
	size := valueSize(kind)
	if size == 0 && d.err == nil {
		d.err = fmt.Errorf("unknown GGUF value type")
		return 0, ""
	}
	n := d.number(size)
	switch kind {
	case 0, 2, 4, 10:
		if n <= 1<<63-1 {
			return int64(n), ""
		}
		if keep {
			d.err = fmt.Errorf("GGUF integer overflow")
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

func valueSize(kind uint64) int {
	switch kind {
	case 0, 1, 7:
		return 1
	case 2, 3:
		return 2
	case 4, 5, 6:
		return 4
	case 10, 11, 12:
		return 8
	}
	return 0
}

// Read accepts little-endian GGUF v2/v3, with at most 4096 keys and 64 MiB of metadata.
func Read(r io.Reader) (Header, error) {
	return read(r, false)
}

// ReadArchitecture stops at general.architecture without parsing later metadata.
func ReadArchitecture(r io.Reader) (string, error) {
	h, err := read(r, true)
	return h.Architecture, err
}

func read(r io.Reader, architectureOnly bool) (Header, error) {
	d := decoder{r: bufio.NewReader(&io.LimitedReader{R: r, N: 64 << 20})}
	h := Header{Values: map[string]int64{}}
	magic, version := d.number(4), d.number(4)
	d.number(8) // Tensor count is intentionally never followed.
	count := d.number(8)
	if d.err != nil {
		return h, d.err
	}
	if magic != 0x46554747 || (version != 2 && version != 3) || count > 4096 {
		return h, fmt.Errorf("unsupported GGUF header")
	}
	seen := map[string]bool{}
	for i := uint64(0); i < count && d.err == nil; i++ {
		key := d.text(true, 256)
		kind := d.number(4)
		if d.err != nil {
			return h, d.err
		}
		if seen[key] {
			return h, fmt.Errorf("duplicate GGUF key")
		}
		seen[key] = true
		if architectureOnly && key != "general.architecture" {
			d.value(kind, false)
			continue
		}
		_, suffix, _ := strings.Cut(key, ".")
		keep := key == "general.architecture"
		switch suffix {
		case "block_count", "context_length", "embedding_length", "attention.head_count", "attention.head_count_kv", "attention.key_length", "attention.value_length", "attention.sliding_window",
			"nextn_predict_layers", "full_attention_interval", "ssm.conv_kernel", "ssm.group_count", "ssm.time_step_rank", "ssm.state_size", "ssm.inner_size", "attention.recurrent_layers":
			keep = true
		}
		n, text := d.value(kind, keep)
		if d.err != nil {
			return h, d.err
		}
		if key == "general.architecture" {
			if architectureOnly {
				h.Architecture = text
				return h, nil
			}
			switch text {
			case "llama", "qwen2", "qwen3", "qwen3moe", "qwen35":
			default:
				return h, ErrUnsupported
			}
			h.Architecture = text
		} else if keep {
			if suffix == "attention.recurrent_layers" || kind == 8 || kind == 9 || kind == 6 || kind == 7 || kind == 12 || n < 0 || (n == 0 && suffix != "nextn_predict_layers") {
				return h, fmt.Errorf("invalid GGUF geometry")
			}
			h.Values[key] = n
		}
	}
	return h, d.err
}
