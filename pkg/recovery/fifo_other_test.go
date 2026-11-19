//go:build !unix

package recovery

import "errors"

func mkfifo(string) error { return errors.ErrUnsupported }
