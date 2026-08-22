//go:build !windows

package tools

import "os"

func isSymlinkOrReparse(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}
