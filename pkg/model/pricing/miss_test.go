package pricing

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecordMiss_CountsPerProviderModel(t *testing.T) {
	s := NewStore(&testConfig{}, t.TempDir())

	s.RecordMiss("ollama-cloud", "gpt-oss:120b")
	s.RecordMiss("ollama-cloud", "gpt-oss:120b")
	s.RecordMiss("openrouter", "meta-llama/llama-3.3-70b-instruct")

	assert.Equal(t, map[string]int64{
		"ollama-cloud/gpt-oss:120b":                    2,
		"openrouter/meta-llama/llama-3.3-70b-instruct": 1,
	}, s.Misses())
}

// The first non-empty candidate is the response model, which is the id
// worth showing an operator; empty leading candidates must not become
// the key or suppress the record.
func TestRecordMiss_SkipsEmptyCandidates(t *testing.T) {
	s := NewStore(&testConfig{}, t.TempDir())

	s.RecordMiss("groq", "", "fast-chat")
	s.RecordMiss("", "bare-model")
	s.RecordMiss("groq", "", "")

	assert.Equal(t, map[string]int64{
		"groq/fast-chat": 1,
		"bare-model":     1,
	}, s.Misses())
}

func TestRecordMiss_BoundedButKeepsCountingKnownKeys(t *testing.T) {
	s := NewStore(&testConfig{}, t.TempDir())

	s.RecordMiss("p", "first")
	for i := range maxTrackedMisses * 2 {
		s.RecordMiss("p", "model-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	s.RecordMiss("p", "first")

	misses := s.Misses()
	assert.LessOrEqual(t, len(misses), maxTrackedMisses)
	assert.Equal(t, int64(2), misses["p/first"],
		"a key already tracked must keep counting after the cap is hit")
}

func TestRecordMiss_ConcurrentAndNilSafe(t *testing.T) {
	var nilStore *Store
	assert.NotPanics(t, func() {
		nilStore.RecordMiss("p", "m")
		assert.Nil(t, nilStore.Misses())
	})

	s := NewStore(&testConfig{}, t.TempDir())
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RecordMiss("p", "m")
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(50), s.Misses()["p/m"])
}
