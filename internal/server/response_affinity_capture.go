package server

import "encoding/json"

// responseJSONIDScanner skips arbitrary payload values while retaining only
// the top-level key and identifier. Its memory bound applies to the identifier,
// never to the response body, whose output may precede the id.
type responseJSONIDScanner struct {
	depth                                     int
	started, done, inString, escaped, wantKey bool
	key, value, overflow                      bool
	nextID                                    bool
	token                                     []byte
	id                                        string
}

func (s *responseJSONIDScanner) write(p []byte, limit int) string {
	for _, b := range p {
		if s.done {
			break
		}
		if s.inString {
			s.stringByte(b, limit)
			continue
		}
		switch b {
		case ' ', '\n', '\r', '\t':
			continue
		case '"':
			s.inString, s.escaped, s.overflow = true, false, false
			s.key, s.value = s.depth == 1 && s.wantKey, s.depth == 1 && !s.wantKey && s.nextID
			s.token = s.token[:0]
			if s.key || s.value {
				s.token = append(s.token, b)
			}
		case '{', '[':
			if !s.started {
				s.started, s.wantKey = true, true
				if b != '{' {
					s.done = true
					break
				}
			}
			s.depth++
		case '}', ']':
			s.depth--
			if s.depth == 0 {
				s.done = true
			}
		case ',':
			if s.depth == 1 {
				s.wantKey, s.nextID = true, false
			}
		}
	}
	return s.id
}

func (s *responseJSONIDScanner) stringByte(b byte, limit int) {
	if s.key || s.value {
		if s.key {
			limit = 14
		} // Two ASCII letters use at most two \uXXXX escapes.
		if len(s.token) < limit {
			s.token = append(s.token, b)
		} else {
			s.overflow = true
		}
	}
	if s.escaped {
		s.escaped = false
		return
	}
	if b == '\\' {
		s.escaped = true
		return
	}
	if b != '"' {
		return
	}
	s.inString = false
	if s.key {
		var key string
		s.nextID = !s.overflow && json.Unmarshal(s.token, &key) == nil && key == "id"
		s.wantKey = false
	} else if s.value && !s.overflow && json.Unmarshal(s.token, &s.id) == nil {
		s.done = true
	}
}
