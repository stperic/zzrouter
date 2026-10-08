//go:build !unix

package update

// openNoFollow has no equivalent flag here. The privilege split it
// guards is a systemd arrangement and does not exist on this platform:
// nothing runs the handoff, so nothing reads a request written by
// another principal.
const openNoFollow = 0
