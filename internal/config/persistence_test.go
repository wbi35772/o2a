package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveAtomicKeepsExistingConfigPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := SaveAtomic(path, Config{Listen: "127.0.0.1:8080"}); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	assertOwnerOnly(t, path)
	assertOwnerOnly(t, path+".bak")
}

func TestSaveAtomicCreatesPrivateConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := SaveAtomic(path, Config{Listen: "127.0.0.1:8080"}); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	assertOwnerOnly(t, path)
}

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("%s is accessible by group or others: %04o", path, perm)
	}
}
