//go:build windows

package config

import "os"

type fileOwner struct {
	uid, gid int
}

func getFileOwner(_ os.FileInfo) (fileOwner, bool) {
	return fileOwner{}, false
}
