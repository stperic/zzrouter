//go:build !windows

package config

import (
	"os"
	"syscall"
)

type fileOwner struct {
	uid, gid int
}

func getFileOwner(info os.FileInfo) (fileOwner, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fileOwner{uid: int(stat.Uid), gid: int(stat.Gid)}, true
	}
	return fileOwner{}, false
}
