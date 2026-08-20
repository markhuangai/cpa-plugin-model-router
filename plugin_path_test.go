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
	want := filepath.Join(plugins, defaultDataFileName)
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
	workingDir := t.TempDir()
	want := filepath.Join(workingDir, "plugins", defaultDataFileName)
	if got := resolveDefaultDataPath("/model-router.so", "/CLIProxyAPI", workingDir); got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
}

func TestResolveDefaultDataPathDoesNotSelectLegacyDatabase(t *testing.T) {
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "data", "model-router-usage.db")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(plugins, defaultDataFileName)
	if got := resolveDefaultDataPath(filepath.Join(plugins, "model-router.so"), "", root); got != want {
		t.Fatalf("default with legacy database = %q, want %q", got, want)
	}
}
