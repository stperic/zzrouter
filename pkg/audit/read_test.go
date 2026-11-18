package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditPagesAcrossAppendsAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := NewFileSink(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })
	for _, subject := range []string{"one", "two", "three"} {
		require.NoError(t, sink.Emit(t.Context(), Event{Actor: "operator", Action: ActionPersonCreated, Subject: subject}))
	}
	page, err := sink.ReadPage(t.Context(), "", 1)
	require.NoError(t, err)
	require.Equal(t, "three", page.Events[0].Subject)
	require.NotEmpty(t, page.Next)
	require.NoError(t, sink.Emit(t.Context(), Event{Actor: "operator", Action: ActionPersonCreated, Subject: "four"}))
	older, err := sink.ReadPage(t.Context(), page.Next, 2)
	require.NoError(t, err)
	require.Len(t, older.Events, 2)
	require.Equal(t, "two", older.Events[0].Subject)
	require.Equal(t, "one", older.Events[1].Subject)
	require.Empty(t, older.Next)
	require.NoError(t, os.Truncate(path, 0))
	_, err = sink.ReadPage(t.Context(), page.Next, 2)
	require.ErrorIs(t, err, ErrCursor)
	require.NoError(t, sink.Close())
	require.Error(t, sink.Emit(t.Context(), Event{Actor: "operator", Action: ActionPersonCreated, Subject: "five"}))
	_, err = sink.ReadPage(t.Context(), "", 2)
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestAuditReaderBoundsAndMalformedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := NewFileSink(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })
	require.NoError(t, sink.Emit(t.Context(), Event{Actor: "operator", Action: ActionPersonCreated, Subject: "oldest"}))
	_, err = sink.file.WriteString(strings.Repeat("x", readWindow+maxLine) + "\n{invalid record}\n")
	require.NoError(t, err)
	require.NoError(t, sink.Emit(t.Context(), Event{Actor: "operator", Action: ActionPersonCreated, Subject: "newest"}))
	_, err = sink.file.WriteString("{unfinished")
	require.NoError(t, err)
	page, err := sink.ReadPage(t.Context(), "", 100)
	require.NoError(t, err)
	require.Equal(t, "newest", page.Events[0].Subject)
	require.Positive(t, page.Skipped)
	for range 3 {
		page, err = sink.ReadPage(t.Context(), page.Next, 100)
		require.NoError(t, err)
		if len(page.Events) > 0 {
			break
		}
	}
	require.Len(t, page.Events, 1)
	require.Equal(t, "oldest", page.Events[0].Subject)
	_, err = sink.ReadPage(t.Context(), "malformed", 50)
	require.ErrorIs(t, err, ErrCursor)
}
