package inferencelog

import (
	"bytes"
	"encoding/json"
)

// DefaultMaxPayloads is the number of most-recent entries that retain
// their full request bodies.
const DefaultMaxPayloads = 100

// MaxPayloadBytes caps a single retained body after media elision.
// Retention is bounded by DefaultMaxPayloads × this.
const MaxPayloadBytes = 256 * 1024

// Payload holds the request bodies retained for one log entry.
type Payload struct {
	// Request is the body the client sent to zzRouter.
	Request json.RawMessage `json:"request,omitempty"`
	// Upstream is the body zzRouter sent to the provider — it differs
	// from Request whenever routing rewrites the model name.
	Upstream json.RawMessage `json:"upstream_request,omitempty"`
	// Response is the reply body as the provider returned it. Empty for
	// streamed replies, which never have a single body.
	Response json.RawMessage `json:"response,omitempty"`
	// Elided lists attached media replaced by placeholders in Request.
	Elided []ElidedBlob `json:"elided,omitempty"`
	// Oversize reports that a body was dropped for exceeding MaxPayloadBytes.
	Oversize bool `json:"oversize,omitempty"`
}

// NewPayload builds a retainable payload from raw request bodies,
// returning nil when there is nothing worth keeping. Attached media is
// replaced by size-annotated placeholders first, so a vision or file
// request keeps its prompt instead of blowing the size cap on binary.
// What survives that and still exceeds MaxPayloadBytes is dropped rather
// than truncated, so a retained body is always valid JSON.
func NewPayload(request, upstream, response []byte) *Payload {
	var p Payload
	p.Request, p.Elided, p.Oversize = retainable(request)
	up, _, upOversize := retainable(upstream)
	resp, _, respOversize := retainable(response)
	p.Response = resp
	p.Oversize = p.Oversize || upOversize || respOversize
	// An upstream body identical to the request adds no information.
	if !bytes.Equal(p.Request, up) {
		p.Upstream = up
	}
	if p.Request == nil && p.Upstream == nil && p.Response == nil && !p.Oversize {
		return nil
	}
	return &p
}

// retainable returns the storable form of a body, the media it elided,
// and whether it was dropped for being oversized.
func retainable(body []byte) (json.RawMessage, []ElidedBlob, bool) {
	if len(body) == 0 {
		return nil, nil, false
	}
	if !json.Valid(body) {
		return nil, nil, false
	}
	// Decoding a body only pays off if there is media to shrink, so a
	// prose body over the cap is dropped without the round trip.
	var elided []ElidedBlob
	if hasElidableBlob(body) {
		body, elided = elideMedia(body)
	}
	if len(body) > MaxPayloadBytes {
		return nil, elided, true
	}
	return json.RawMessage(append([]byte(nil), body...)), elided, false
}

// payloadRing retains payloads for the most recent maxPayloads entries
// that had one. Callers hold the owning Store's lock.
type payloadRing struct {
	ids   []string // ring of retained entry IDs, oldest overwritten first
	head  int
	count int // filled slots, ≤ len(ids)
	items map[string]Payload
}

func newPayloadRing(maxPayloads int) *payloadRing {
	if maxPayloads <= 0 {
		return nil
	}
	return &payloadRing{
		ids:   make([]string, maxPayloads),
		items: make(map[string]Payload, maxPayloads),
	}
}

func (r *payloadRing) add(id string, p Payload) {
	if r == nil {
		return
	}
	if r.count == len(r.ids) {
		delete(r.items, r.ids[r.head])
	} else {
		r.count++
	}
	r.ids[r.head] = id
	r.items[id] = p
	r.head = (r.head + 1) % len(r.ids)
}

// remove drops a payload ahead of its ring slot, called when the entry it
// belongs to is overwritten in the shorter-lived metadata ring.
func (r *payloadRing) remove(id string) {
	if r == nil {
		return
	}
	delete(r.items, id)
}

func (r *payloadRing) get(id string) (Payload, bool) {
	if r == nil {
		return Payload{}, false
	}
	p, ok := r.items[id]
	return p, ok
}

// hasBody reports whether a retained payload actually carries a body — an
// oversize marker is stored to explain itself but has nothing to serve.
func (r *payloadRing) hasBody(id string) bool {
	if r == nil {
		return false
	}
	p, ok := r.items[id]
	return ok && (len(p.Request) > 0 || len(p.Upstream) > 0 || len(p.Response) > 0)
}

func (r *payloadRing) len() int {
	if r == nil {
		return 0
	}
	return len(r.items)
}

// drop releases every retained body, leaving the ring usable so an Add
// racing Stop cannot land on a zero-length ring.
func (r *payloadRing) drop() {
	if r == nil {
		return
	}
	clear(r.items)
	clear(r.ids)
	r.head, r.count = 0, 0
}
