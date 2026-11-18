package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	// ErrCursor requires a fresh read after log replacement, truncation or restart.
	ErrCursor = errors.New("audit history changed; refresh to start a new page")
	// ErrUnavailable reports a closed or inaccessible audit file.
	ErrUnavailable = errors.New("audit log is unavailable")
)

// Page is a bounded, newest-first view of the current audit file only.
type Page struct {
	Events  []Event `json:"events"`
	Next    string  `json:"next,omitempty"`
	Skipped int     `json:"skipped"`
}

type pageCursor struct {
	Generation string `json:"g"`
	Before     int64  `json:"b"`
	End        int64  `json:"e"`
	Anchor     string `json:"a"`
}

const readWindow = 1 << 20
const maxLine = 64 << 10

// ReadPage reads at most 1 MiB and 100 events. Cursors bind an append-stable byte
// boundary to this sink generation and the snapshot's tail. Rotation, truncation
// or reopening the sink requires refreshing; no caller-selected path is opened.
func (s *FileSink) ReadPage(ctx context.Context, cursor string, limit int) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := Page{Events: []Event{}}
	if s.w == nil || s.file == nil {
		return page, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if limit < 1 || limit > 100 {
		return page, fmt.Errorf("audit limit must be between 1 and 100")
	}
	info, err := s.file.Stat()
	if err != nil {
		return page, err
	}
	current, err := os.Stat(s.path)
	if err != nil || !os.SameFile(info, current) {
		return page, ErrCursor
	}
	position := pageCursor{Generation: s.generation, Before: info.Size(), End: info.Size()}
	if cursor != "" {
		if len(cursor) > 2048 {
			return page, ErrCursor
		}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(data, &position) != nil || position.Generation != s.generation || position.Before < 0 || position.Before > position.End || position.End > info.Size() {
			return page, ErrCursor
		}
	}
	anchor, err := tailDigest(s.file, position.End)
	if err != nil {
		return page, err
	}
	if cursor != "" && position.Anchor != anchor {
		return page, ErrCursor
	}
	position.Anchor = anchor
	start := max(int64(0), position.Before-readWindow)
	data := make([]byte, position.Before-start)
	if _, err := s.file.ReadAt(data, start); err != nil && err != io.EOF {
		return page, err
	}
	end, err := scanWindow(ctx, data, start, limit, &page)
	if err != nil {
		return page, err
	}
	position.Before = start + int64(end)
	// A single oversized line cannot prevent paging beyond this window.
	if position.Before == start+int64(len(data)) && start > 0 {
		position.Before = start
		page.Skipped++
	}
	if position.Before > 0 {
		encoded, err := json.Marshal(position)
		if err != nil {
			return page, err
		}
		page.Next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

func scanWindow(ctx context.Context, data []byte, start int64, limit int, page *Page) (int, error) {
	end := len(data)
	// The writer may have been interrupted mid-record. Never surface that text.
	if end > 0 && data[end-1] != '\n' {
		end = bytes.LastIndexByte(data, '\n') + 1
		page.Skipped++
	}
	for end > 0 && len(page.Events) < limit {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		lineStart := bytes.LastIndexByte(data[:end-1], '\n') + 1
		if lineStart == 0 && start > 0 {
			// Revisit the boundary record in the next bounded window.
			break
		}
		line := data[lineStart : end-1]
		var event Event
		if len(line) > maxLine || json.Unmarshal(line, &event) != nil || validate(event) != nil {
			page.Skipped++
		} else {
			page.Events = append(page.Events, event)
		}
		end = lineStart
	}
	return end, nil
}

func tailDigest(file *os.File, end int64) (string, error) {
	data := make([]byte, min(end, 256))
	if _, err := file.ReadAt(data, end-int64(len(data))); err != nil && err != io.EOF {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
