package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultDataPath(t *testing.T) {
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "data", defaultDataFileName)
	if got := resolveDefaultDataPath(filepath.Join(plugins, "linux", "amd64", "model-router.so"), "", ""); got != want {
		t.Fatalf("plugin path default = %q, want %q", got, want)
	}
	if got := resolveDefaultDataPath("", filepath.Join(root, "CLIProxyAPI"), ""); got != want {
		t.Fatalf("executable path default = %q, want %q", got, want)
	}
	if got := resolveDefaultDataPath("", "", root); got != want {
		t.Fatalf("working directory default = %q, want %q", got, want)
	}
}

func TestResolveDefaultDataPathFallsBackOutsideCPALayout(t *testing.T) {
	if got := resolveDefaultDataPath("/model-router.so", "/CLIProxyAPI", "/"); got != legacyDefaultDataPath {
		t.Fatalf("fallback = %q, want %q", got, legacyDefaultDataPath)
	}
}
