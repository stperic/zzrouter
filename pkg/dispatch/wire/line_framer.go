package wire

import "bytes"

// maxPendingLine bounds how much of an unterminated line lineFramer will
// hold back. SSE and NDJSON frames are newline-delimited and small, so a
// stream that runs this far without a newline is not one of them; the
// framer stops holding and lets the bytes through rather than buffering
// an unbounded amount of a stream it does not understand. 1 MiB is far
// above any real frame (a full tool-call argument list is a few KiB) and
// far below a size worth buffering per in-flight stream.
const maxPendingLine = 1 << 20

// lineFramer re-aligns a byte stream to line boundaries.
//
// Reads land wherever the TCP segments fall, so one SSE frame
// (`data: {...}\n\n`) can arrive as two of them. Everything downstream
// splits on "\n" and parses each line standalone: usage extraction,
// response-model capture, the model-id rewrite and the usage-frame
// strip. A frame split across a read reaches all of them as two
// fragments of invalid JSON, so the terminal usage frame meters as zero
// tokens and the rewrites silently no-op.
//
// next reports the prefix that ends on a line boundary and holds the
// rest until its newline arrives; flush releases that remainder once
// upstream is done. Bytes are never modified or reordered, only delayed,
// and only by the part of a line that has not arrived yet.
//
// A lineFramer is not safe for concurrent use; callers that read and
// close from different goroutines must serialize it.
type lineFramer struct {
	// pending holds the trailing bytes of a line whose newline has not
	// arrived. frame is scratch for joining pending to the current
	// chunk, kept on the struct so a stream that splits every frame
	// does not allocate per read.
	pending []byte
	frame   []byte
}

// next returns the whole-line prefix of everything received so far,
// retaining any trailing partial line for a later call. It returns nil
// when no complete line is available yet.
//
// The result may alias chunk, so it stays valid only until the caller
// next writes into the buffer chunk came from.
func (f *lineFramer) next(chunk []byte) []byte {
	cut := bytes.LastIndexByte(chunk, '\n')
	if cut < 0 {
		if len(f.pending)+len(chunk) > maxPendingLine {
			return f.overflow(chunk)
		}
		f.pending = append(f.pending, chunk...)
		return nil
	}

	// Nothing held: hand back the caller's own bytes and copy only the
	// tail. This is the common case, since most reads end on a frame
	// boundary.
	if len(f.pending) == 0 {
		f.pending = append(f.pending, chunk[cut+1:]...)
		return chunk[:cut+1]
	}

	f.frame = append(append(f.frame[:0], f.pending...), chunk[:cut+1]...)
	f.pending = append(f.pending[:0], chunk[cut+1:]...)
	return f.frame
}

// flush returns the unterminated final line, if any. Call it once the
// upstream is done: a last frame with no trailing newline is all there
// will ever be of it, so it is complete by then. Repeat calls return
// nil.
func (f *lineFramer) flush() []byte {
	out := f.pending
	f.pending = nil
	if len(out) == 0 {
		return nil
	}
	return out
}

// overflow releases everything held plus chunk, for a stream that has
// gone maxPendingLine bytes without a newline. Framing degrades to the
// unframed behaviour for that stretch rather than growing without
// bound.
func (f *lineFramer) overflow(chunk []byte) []byte {
	f.frame = append(append(f.frame[:0], f.pending...), chunk...)
	f.pending = f.pending[:0]
	return f.frame
}
