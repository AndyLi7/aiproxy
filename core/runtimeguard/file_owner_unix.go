//go:build unix

package runtimeguard

import (
	"os"
	"syscall"
)

// The controlled runtime requires a file owned by this effective UID and not
// writable by its group or other users. Preserve the Unix bootstrap boundary.
func trustedFileOwner(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Geteuid() && st.Mode().Perm()&0022 == 0
}
