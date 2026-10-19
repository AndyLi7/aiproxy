//go:build !unix

package runtimeguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformRefusesControlledRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "runtime.sqlite")
	data := []byte("synthetic seed")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if trustedFileOwner(st) {
		t.Fatal("unsupported ownership verifier granted trust")
	}
	if _, err := ownedFile(path, 1024); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("ownedFile: %v", err)
	}
	if _, err := Start(context.Background(), Launch{Seed: path, Executable: path, Members: []Member{{ServiceID: "test"}}}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("Start: %v", err)
	}
	if _, err := OpenControlledStorage(Hello{DatabasePath: path, SeedHash: digest(data)}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("OpenControlledStorage: %v", err)
	}
}
