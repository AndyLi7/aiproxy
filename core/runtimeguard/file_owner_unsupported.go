//go:build !unix

package runtimeguard

import "os"

// No equivalent ownership/ACL verifier is implemented on this platform. The
// ordinary service can compile, but controlled runtime files are never trusted.
func trustedFileOwner(_ os.FileInfo) bool { return false }
