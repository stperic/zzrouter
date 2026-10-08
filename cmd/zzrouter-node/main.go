// zzrouter-node is the server binary. The cobra tree and all boot
// concerns (umask, panic net, early log-file routing) live in
// internal/cli/servercli so they're testable and reusable.
package main

import (
	"os"

	"github.com/stperic/zzrouter/internal/cli/servercli"
)

func main() {
	os.Exit(servercli.Execute())
}
