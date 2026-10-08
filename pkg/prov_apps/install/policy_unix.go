//go:build !windows

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkPolicyPath(path string) (os.FileInfo, error) {
	uid := os.Geteuid()
	if uid == 0 {
		return nil, fmt.Errorf("root cannot establish an override boundary")
	}
	var leaf os.FileInfo
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("policy path contains an untrusted link")
		}
		if int(stat.Uid) == uid || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return nil, fmt.Errorf("policy and parents must be root-owned and protected from service writes: %s", current)
		}
		accessErr := unix.Faccessat(unix.AT_FDCWD, current, unix.W_OK, unix.AT_EACCESS)
		if accessErr == nil {
			return nil, fmt.Errorf("service can write policy path %s", current)
		}
		if !errors.Is(accessErr, unix.EACCES) && !errors.Is(accessErr, unix.EPERM) && !errors.Is(accessErr, unix.EROFS) {
			return nil, fmt.Errorf("cannot establish policy write denial: %w", accessErr)
		}
		if current == path {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("policy must be a regular file")
			}
			leaf = info
		} else if !info.IsDir() {
			return nil, fmt.Errorf("policy parent must be a directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return leaf, nil
}
