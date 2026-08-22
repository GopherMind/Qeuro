//go:build !windows

package tools

import (
	"fmt"
	"os"
	"syscall"
)

// Unix keeps physical canonicalization strict. Unlike the Windows managed-FS
// case, a permission failure here is not an expected limitation of the path
// API and therefore remains a denial.
func checkCanonicalContainment(root, target string) error {
	return strictCanonicalContainment(root, target)
}

// checkHardLink refuses to read or write through a regular file that has more
// than one hard link. A malicious repo could run `ln /etc/passwd project/cfg`
// and obtain an in-root path whose inode actually lives outside the project.
// filepath.EvalSymlinks cannot detect hard links (they are indistinguishable
// from the original name), so inode metadata is the only reliable signal on
// UNIX systems.
func checkHardLink(abs string) error {
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // brand-new file — nothing to hijack
		}
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if info.Mode().IsRegular() && st.Nlink > 1 {
		return fmt.Errorf("outside root: %s has %d hard links; refusing to access a linked inode", abs, st.Nlink)
	}
	return nil
}
