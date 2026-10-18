//go:build unix

package runtimeguard

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type ownerTestInfo struct {
	os.FileInfo
	sys any
}

func (s ownerTestInfo) Sys() any { return s.sys }

func TestOwnedFileUnixTrustBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted")
	data := []byte("synthetic runtime fixture")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ownedFile(path, int64(len(data)))
	if err != nil || string(got) != string(data) {
		t.Fatalf("valid file: %q %v", got, err)
	}
	for _, mode := range []os.FileMode{0620, 0602, 0666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ownedFile(path, 1024); !errors.Is(err, ErrUntrusted) {
			t.Fatalf("mode %o accepted: %v", mode, err)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := *st.Sys().(*syscall.Stat_t)
	sys.Uid++
	if trustedFileOwner(ownerTestInfo{st, &sys}) {
		t.Fatal("foreign UID accepted")
	}
	if trustedFileOwner(ownerTestInfo{st, nil}) {
		t.Fatal("missing ownership metadata accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path  string
		limit int64
	}{{link, 1024}, {dir, 1024}, {path, 1}, {filepath.Join(dir, "missing"), 1024}} {
		if _, err := ownedFile(c.path, c.limit); !errors.Is(err, ErrUntrusted) {
			t.Fatalf("invalid file accepted: %s %v", c.path, err)
		}
	}
}
