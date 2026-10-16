package gguf

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func header(entries ...[]byte) []byte {
	var b bytes.Buffer
	for _, v := range []any{uint32(0x46554747), uint32(3), uint64(1), uint64(len(entries))} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	for _, e := range entries {
		b.Write(e)
	}
	return b.Bytes()
}
func entry(key string, kind uint32, values ...any) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(key)))
	b.WriteString(key)
	_ = binary.Write(&b, binary.LittleEndian, kind)
	for _, v := range values {
		if s, ok := v.(string); ok {
			_ = binary.Write(&b, binary.LittleEndian, uint64(len(s)))
			b.WriteString(s)
		} else {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
	}
	return b.Bytes()
}
func validHeader() []byte {
	return header(entry("general.architecture", 8, "llama"), entry("llama.block_count", 4, uint32(32)), entry("tokenizer.tokens", 9, uint32(8), uint64(2), "one", "two"))
}

func TestHeaderStopsBeforeTensors(t *testing.T) {
	data := validHeader()
	// Invalid tensor descriptors must not be parsed. Buffer read-ahead is bounded.
	tensors := bytes.Repeat([]byte{0xff}, 8192)
	r := bytes.NewReader(append(data, tensors...))
	h, err := Read(r)
	require.NoError(t, err)
	require.Equal(t, "llama", h.Architecture)
	require.Equal(t, int64(32), h.Values["llama.block_count"])
	require.Len(t, h.Values, 1)
	require.GreaterOrEqual(t, r.Len(), len(tensors)-4096)
}

// countingReader counts reads reaching the source, the cost a per-element
// read loop would multiply.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestHeaderManyMaxSizeArraysReadInBulk(t *testing.T) {
	const arrays, elements, size = 7, 1 << 20, 8
	parts := []io.Reader{bytes.NewReader(header(make([][]byte, arrays)...))}
	for i := range arrays {
		parts = append(parts, bytes.NewReader(entry(fmt.Sprintf("array.%d", i), 9, uint32(10), uint64(elements))), io.LimitReader(zeros{}, elements*size))
	}
	src := &countingReader{r: io.MultiReader(parts...)}
	_, err := Read(src)
	require.NoError(t, err)
	require.Less(t, src.reads, arrays*elements/64, "skipped array elements must not cost a read each")
}

func TestHeaderKeyLimitAndTruncatedValue(t *testing.T) {
	_, err := Read(bytes.NewReader(header(entry(strings.Repeat("k", 256), 4, uint32(1)))))
	require.NoError(t, err)
	_, err = Read(bytes.NewReader(header(entry(strings.Repeat("k", 257), 4, uint32(1)))))
	require.Error(t, err)
	data := header(entry("llama.block_count", 4, uint32(32)))
	for _, missing := range []int{1, 4} {
		_, err := Read(bytes.NewReader(data[:len(data)-missing]))
		if missing == 4 {
			require.ErrorIs(t, err, io.EOF)
		} else {
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		}
	}
}
func TestHostileAndTruncatedHeaders(t *testing.T) {
	data := validHeader()
	for i := 0; i < len(data); i++ {
		_, err := Read(bytes.NewReader(data[:i]))
		require.Error(t, err, "offset %d", i)
	}
	for _, hostile := range [][]byte{
		header(entry("x", 99)),
		header(entry("x", 9, uint32(99), uint64(0))),
		header(entry("x", 9, uint32(9), uint64(0))),
		header(entry("x", 9, uint32(4), uint64(math.MaxUint64))),
		header(entry("x", 8, uint64(math.MaxUint64))),
		header(entry("llama.block_count", 4, uint32(32)), entry("llama.block_count", 4, uint32(1))),
		header(entry("llama.context_length", 10, uint64(math.MaxUint64))),
		header(entry("llama.context_length", 9, uint32(4), uint64(0))),
	} {
		_, err := Read(bytes.NewReader(hostile))
		require.Error(t, err)
	}
	for _, offset := range []int{4, 16} {
		bad := append([]byte(nil), data...)
		if offset == 4 {
			binary.LittleEndian.PutUint32(bad[offset:], 1)
		} else {
			binary.LittleEndian.PutUint64(bad[offset:], math.MaxUint64)
		}
		_, err := Read(bytes.NewReader(bad))
		require.Error(t, err)
	}
}
func FuzzRead(f *testing.F) {
	f.Add(validHeader())
	f.Add(header(hybridEntries()...))
	for _, key := range hybridKeys() {
		f.Add(header(entry("general.architecture", 8, "qwen35"), entry("qwen35."+key, 9, uint32(4), uint64(math.MaxUint64))))
		f.Add(header(entry("qwen35."+key, 10, uint64(math.MaxUint64))))
		f.Add(header(entry("qwen35."+key, 8, "bad geometry")))
		f.Add(header(entry("qwen35."+key, 4, uint32(1)), entry("qwen35."+key, 4, uint32(1))))
	}
	f.Add(header(entry("qwen35.attention.recurrent_layers", 9, uint32(7), uint64(4), []byte{1, 1, 1, 0})))
	f.Add([]byte("GGUF"))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = Read(bytes.NewReader(data)) })
}

func hybridKeys() []string {
	return []string{"block_count", "nextn_predict_layers", "full_attention_interval", "ssm.conv_kernel", "ssm.group_count", "ssm.time_step_rank", "ssm.state_size", "ssm.inner_size"}
}

func hybridEntries() [][]byte {
	entries := [][]byte{entry("general.architecture", 8, "qwen35")}
	for i, key := range hybridKeys() {
		entries = append(entries, entry("qwen35."+key, 4, uint32(i+1)))
	}
	return entries
}

func TestHybridHeaderBoundsAndTypes(t *testing.T) {
	data := header(hybridEntries()...)
	h, err := Read(bytes.NewReader(data))
	require.NoError(t, err)
	require.Len(t, h.Values, len(hybridKeys()))
	for i := range data {
		_, err := Read(bytes.NewReader(data[:i]))
		require.Error(t, err, "prefix %d", i)
	}
	for _, key := range append(hybridKeys(), "attention.recurrent_layers") {
		key = "qwen35." + key
		for _, bad := range [][]byte{
			entry(key, 8, "4"), entry(key, 9, uint32(4), uint64(0)),
			entry(key, 9, uint32(4), uint64(1<<20+1)), entry(key, 10, uint64(math.MaxUint64)),
			entry(key, 5, int32(-1)), entry(key, 6, float32(4)), entry(key, 7, uint8(1)), entry(key, 12, float64(4)),
		} {
			_, err := Read(bytes.NewReader(header(bad)))
			require.Error(t, err, key)
		}
		_, err := Read(bytes.NewReader(header(entry(key, 4, uint32(1)), entry(key, 4, uint32(1)))))
		require.Error(t, err, "duplicate %s", key)
	}
	_, err = Read(bytes.NewReader(header(entry("general.architecture", 8, "qwen3moe"))))
	require.NoError(t, err)
	_, err = Read(bytes.NewReader(header(entry("qwen35.nextn_predict_layers", 4, uint32(0)))))
	require.NoError(t, err, "zero MTP layers is valid")
}
