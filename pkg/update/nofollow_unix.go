//go:build unix

package update

import "syscall"

// openNoFollow refuses to open a symlink, so a root process reading a
// file in a directory an unprivileged principal owns cannot be steered
// into opening whatever that principal linked the name to.
const openNoFollow = syscall.O_NOFOLLOW
