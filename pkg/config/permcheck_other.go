//go:build !windows

package config

import "os"

func isWorldReadableImpl(info os.FileInfo) bool {
	return info.Mode()&0004 != 0
}

func isWorldReadablePathImpl(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return isWorldReadableImpl(info)
}
