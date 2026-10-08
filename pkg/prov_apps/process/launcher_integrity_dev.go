//go:build !release

package process

// Dev build: a missing LauncherSHA256 degrades to a warn log so
// go run / go test / make dev stay frictionless.
const devLauncherBypass = true
