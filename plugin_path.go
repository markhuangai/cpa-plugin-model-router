package main

import (
	"os"
	"path/filepath"
	"strings"
)

const defaultDataFileName = "model-router.db"

var defaultDataPathResolver = resolvedDefaultDataPath

func resolvedDefaultDataPath() string {
	modulePath, _ := loadedPluginPath()
	executablePath, _ := os.Executable()
	workingDir, _ := os.Getwd()
	return resolveDefaultDataPath(modulePath, executablePath, workingDir)
}

func resolveDefaultDataPath(modulePath, executablePath, workingDir string) string {
	if root, ok := cpaRootFromPluginPath(modulePath, workingDir); ok {
		return filepath.Join(root, "plugins", defaultDataFileName)
	}
	if root, ok := cpaRootWithPluginsDir(filepath.Dir(strings.TrimSpace(executablePath))); ok {
		return filepath.Join(root, "plugins", defaultDataFileName)
	}
	if absolute, err := filepath.Abs(strings.TrimSpace(workingDir)); err == nil {
		if root, ok := cpaRootWithPluginsDir(absolute); ok {
			return filepath.Join(root, "plugins", defaultDataFileName)
		}
		return filepath.Join(absolute, "plugins", defaultDataFileName)
	}
	return filepath.Join("plugins", defaultDataFileName)
}

func cpaRootFromPluginPath(modulePath, workingDir string) (string, bool) {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" {
		return "", false
	}
	if !filepath.IsAbs(modulePath) {
		if strings.TrimSpace(workingDir) == "" {
			return "", false
		}
		modulePath = filepath.Join(workingDir, modulePath)
	}
	for directory := filepath.Dir(filepath.Clean(modulePath)); ; directory = filepath.Dir(directory) {
		if strings.EqualFold(filepath.Base(directory), "plugins") {
			root := filepath.Dir(directory)
			if root != directory && filepath.Dir(root) != root {
				return root, true
			}
			return "", false
		}
		if filepath.Dir(directory) == directory {
			return "", false
		}
	}
}

func cpaRootWithPluginsDir(candidate string) (string, bool) {
	candidate = filepath.Clean(strings.TrimSpace(candidate))
	if candidate == "." || filepath.Dir(candidate) == candidate {
		return "", false
	}
	info, err := os.Stat(filepath.Join(candidate, "plugins"))
	if err != nil || !info.IsDir() {
		return "", false
	}
	return candidate, true
}
