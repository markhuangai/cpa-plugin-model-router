package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowYAML(t *testing.T) {
	for _, path := range []string{".github/workflows/test.yml", ".github/workflows/codeql.yml", ".github/workflows/release.yml"} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document yaml.Node
			if err := yaml.Unmarshal(raw, &document); err != nil {
				t.Fatalf("invalid workflow YAML: %v", err)
			}
			if len(document.Content) != 1 || len(document.Content[0].Content) == 0 {
				t.Fatal("workflow YAML is empty")
			}
		})
	}
}

func TestWorkflowRunnersAndActionRuntimes(t *testing.T) {
	allowedRunners := map[string]map[string]bool{
		".github/workflows/test.yml": {
			"ubuntu-latest": true,
		},
		".github/workflows/codeql.yml": {
			"ubuntu-latest": true,
		},
		".github/workflows/release.yml": {
			"ubuntu-latest": true,
		},
	}
	expectedRunners := map[string]int{
		".github/workflows/test.yml":    1,
		".github/workflows/codeql.yml":  1,
		".github/workflows/release.yml": 3,
	}
	allowedActions := map[string]bool{
		"actions/checkout@v7":          true,
		"actions/setup-go@v7":          true,
		"actions/upload-artifact@v7":   true,
		"actions/download-artifact@v8": true,
	}

	for _, path := range []string{".github/workflows/test.yml", ".github/workflows/codeql.yml", ".github/workflows/release.yml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		runnerCount := 0
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "runs-on:") {
				runner := strings.TrimSpace(strings.TrimPrefix(line, "runs-on:"))
				if !allowedRunners[path][runner] {
					t.Errorf("%s uses unsupported runner %q", path, runner)
				}
				runnerCount++
			}
			if strings.HasPrefix(line, "- uses: actions/") {
				action := strings.TrimSpace(strings.TrimPrefix(line, "- uses:"))
				if !allowedActions[action] {
					t.Errorf("%s uses an action without the approved Node 24 runtime: %s", path, action)
				}
			}
		}
		if runnerCount != expectedRunners[path] {
			t.Errorf("%s runner count = %d, want %d", path, runnerCount, expectedRunners[path])
		}
	}
}

func TestWorkflowPinsMinimumCPAIntegration(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/test.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	for _, required := range []string{
		"repository: router-for-me/CLIProxyAPI",
		"ref: 4b5f1eab25fca4b3815369a826e958e7c070a69e",
		"path: .cpa-source",
		`CPA_SOURCE="$GITHUB_WORKSPACE/.cpa-source" go test -tags=integration ./... -count=1`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("test workflow is missing %q", required)
		}
	}
}
