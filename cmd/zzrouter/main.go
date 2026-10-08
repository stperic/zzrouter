// zzrouter is the client binary. The cobra tree lives in
// internal/cli/clientcli so it's testable and reusable.
package main

import (
	"os"

	"github.com/stperic/zzrouter/internal/cli/clientcli"
)

func main() {
	os.Exit(clientcli.Execute())
}
