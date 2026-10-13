package jobs

import (
	"bufio"
	"io"
	"strings"
)

// ReadSSE frames a job stream. The caller owns body cancellation, decoding
// and terminal interpretation. A true callback result stops reading.
func ReadSSE(r io.Reader, consume func(string, []byte) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var name string
	var data strings.Builder
	dispatch := func() (bool, error) {
		defer func() { name = ""; data.Reset() }()
		if data.Len() == 0 {
			return false, nil
		}
		if name == "" {
			name = "progress"
		}
		return consume(name, []byte(data.String()))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if stop, err := dispatch(); stop || err != nil {
				return err
			}
		} else if value, ok := strings.CutPrefix(line, "event:"); ok {
			name = strings.TrimSpace(value)
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len()+len(value) > 1024*1024 {
				return bufio.ErrTooLong
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := dispatch()
	return err
}
