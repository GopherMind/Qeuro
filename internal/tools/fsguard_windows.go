//go:build windows

package tools

import "os"

// checkHardLink is a no-op on Windows: creating hard links to protected system
// files requires administrator rights there, and the reparse-point walk in
// ensureInsideRoot already blocks junction/symlink based escapes.
func checkHardLink(string) error { return nil }

// checkCanonicalContainment keeps physical canonicalization as defence in
// depth when Windows exposes it. Some managed/ACL-restricted filesystems return
// ERROR_ACCESS_DENIED from filepath.EvalSymlinks even though Lstat succeeded for
// every component. ensureInsideRoot has already enforced lexical containment
// and rejected every symlink/junction/reparse component before reaching this
// function, so only that permission error may fall back to those two gates.
// Missing paths, malformed volumes, and physical escapes still fail closed.
func checkCanonicalContainment(root, target string) error {
	err := strictCanonicalContainment(root, target)
	if canonicalPermissionFallback(err) {
		return nil
	}
	return err
}

func canonicalPermissionFallback(err error) bool {
	return err != nil && os.IsPermission(err)
}
