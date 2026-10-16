package gguf

import (
	"bytes"
	"encoding/binary"
	"math"
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
	r := bytes.NewReader(append(data, []byte("untrusted tensor descriptors")...))
	h, err := Read(r)
	require.NoError(t, err)
	require.Equal(t, "llama", h.Architecture)
	require.Equal(t, int64(32), h.Values["llama.block_count"])
	require.Len(t, h.Values, 1)
	require.Equal(t, len("untrusted tensor descriptors"), r.Len())
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
	f.Add([]byte("GGUF"))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = Read(bytes.NewReader(data)) })
}
