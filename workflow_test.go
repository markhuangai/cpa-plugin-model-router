package main

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var workflowPaths = []string{
	".github/workflows/test.yml",
	".github/workflows/codeql.yml",
	".github/workflows/release.yml",
	".github/workflows/maintenance.yml",
	".github/workflows/test-ai.yml",
	".github/workflows/orm-runtime.yml",
}

func TestWorkflowYAML(t *testing.T) {
	for _, path := range workflowPaths {
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
			"ubuntu-latest":  true,
			"windows-latest": true,
		},
		".github/workflows/codeql.yml": {
			"ubuntu-latest": true,
		},
		".github/workflows/release.yml": {
			"ubuntu-latest": true,
		},
		".github/workflows/orm-runtime.yml": {
			"ubuntu-24.04": true,
		},
	}
	expectedRunners := map[string]int{
		".github/workflows/test.yml":        2,
		".github/workflows/codeql.yml":      1,
		".github/workflows/release.yml":     3,
		".github/workflows/maintenance.yml": 0,
		".github/workflows/test-ai.yml":     0,
		".github/workflows/orm-runtime.yml": 1,
	}
	allowedActions := map[string]bool{
		"actions/checkout@v7":          true,
		"actions/setup-go@v7":          true,
		"actions/upload-artifact@v7":   true,
		"actions/download-artifact@v8": true,
		"actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3": true,
	}

	for _, path := range workflowPaths {
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

type ormWorkflow struct {
	Permissions map[string]string
	Jobs        map[string]struct {
		If      string
		Needs   string
		Uses    string
		With    map[string]any
		Secrets map[string]string
		Steps   []struct {
			Uses string
			With map[string]any
			Env  map[string]string
		}
	}
}

func readORMYAML(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func TestORMWorkflowTrustAndChannel(t *testing.T) {
	secretReference := regexp.MustCompile("\\$\\{\\{ toJSON\\(secrets\\.([A-Z_]+)\\) \\}\\}")
	expectedSecrets := map[string]string{
		"AI_ENDPOINT":      "fixture",
		"ORM_GITHUB_TOKEN": "fixture",
		"ORM_PROXY_KEY":    "fixture",
		"ORM_SESSION_KEY":  "fixture",
	}
	for filename, jobName := range map[string]string{"maintenance.yml": "maintenance", "test-ai.yml": "test"} {
		t.Run(filename, func(t *testing.T) {
			var workflow ormWorkflow
			readORMYAML(t, ".github/workflows/"+filename, &workflow)
			if !reflect.DeepEqual(workflow.Permissions, map[string]string{"contents": "read", "actions": "read"}) {
				t.Fatalf("unexpected caller permissions: %v", workflow.Permissions)
			}
			resolver := workflow.Jobs["runtime"]
			if resolver.Uses != "./.github/workflows/orm-runtime.yml" || resolver.With["workflow-file"] != filename || len(resolver.Secrets) != 0 {
				t.Fatal("caller must resolve the channel without forwarding ORM secrets")
			}
			if filename == "maintenance.yml" && resolver.If != "vars.ORM_ENABLED == 'true'" {
				t.Fatal("maintenance must require explicit activation")
			}
			job := workflow.Jobs[jobName]
			if job.Needs != "runtime" || job.Uses != "markhuangai/opensource-repo-manager/.github/workflows/"+filename+"@v1-prerelease" {
				t.Fatal("runtime selection must precede the floating upstream workflow")
			}
			if job.With["runtime-image"] != "${{ needs.runtime.outputs.runtime-image }}" {
				t.Fatal("caller must pass the verified immutable image")
			}
			if len(job.Secrets) != 2 || job.Secrets["ORM_CONFIGURATION_VARIABLES"] != "${{ toJSON(vars) }}" {
				t.Fatal("unexpected configuration transports")
			}
			payload := job.Secrets["ORM_CONFIGURATION_SECRETS"]
			for _, match := range secretReference.FindAllStringSubmatch(payload, -1) {
				if _, ok := expectedSecrets[match[1]]; !ok {
					t.Fatalf("unexpected forwarded secret %s", match[1])
				}
			}
			var forwarded map[string]string
			if err := json.Unmarshal([]byte(secretReference.ReplaceAllString(payload, "\"fixture\"")), &forwarded); err != nil {
				t.Fatalf("secret map is not JSON after expression resolution: %v", err)
			}
			if !reflect.DeepEqual(forwarded, expectedSecrets) {
				t.Fatalf("unexpected forwarded secrets: %v", forwarded)
			}
		})
	}
	var resolver ormWorkflow
	readORMYAML(t, ".github/workflows/orm-runtime.yml", &resolver)
	steps := resolver.Jobs["resolve"].Steps
	if len(steps) != 2 || steps[0].Uses != "actions/checkout@v7" ||
		steps[0].With["ref"] != "${{ github.workflow_sha }}" || steps[0].With["persist-credentials"] != false {
		t.Fatal("resolver must check out the trusted caller revision without persistent credentials")
	}
	if steps[1].Env["ORM_BOOTSTRAP_GITHUB_TOKEN"] != "${{ github.token }}" || len(steps[1].Env) != 3 {
		t.Fatal("resolver must receive the read-only job token and public selection inputs")
	}
}

func TestORMConfigurationProfilesAndGates(t *testing.T) {
	var configuration struct {
		Github     struct{ Token map[string]string }
		SessionKey map[string]string `yaml:"session_key"`
		Proxy      struct{ URL, Key map[string]string }
		Profiles   map[string]struct {
			SDK           string
			Model, Effort map[string]string
		}
		Roles map[string]struct {
			Profile string
			Skills  []string
		}
		Monitor struct {
			Checks    []string                         `yaml:"required_checks"`
			Reviewers []struct{ Login, Signal string } `yaml:"automated_reviewers"`
		} `yaml:"pr_monitor"`
	}
	readORMYAML(t, ".github/orm.yml", &configuration)
	if configuration.Github.Token["from_secret"] != "ORM_GITHUB_TOKEN" ||
		configuration.SessionKey["from_secret"] != "ORM_SESSION_KEY" ||
		configuration.Proxy.URL["from_secret"] != "AI_ENDPOINT" ||
		configuration.Proxy.Key["from_secret"] != "ORM_PROXY_KEY" {
		t.Fatal("credentials and private endpoint must remain explicit secret references")
	}
	profile := configuration.Profiles["claude"]
	if len(configuration.Profiles) != 1 || profile.SDK != "claude" ||
		profile.Model["from_variable"] != "CLAUDE_MODEL" || profile.Effort["from_variable"] != "CLAUDE_EFFORT" {
		t.Fatal("all roles must preserve the selected Claude SDK model and effort variables")
	}
	if len(configuration.Roles) != 5 {
		t.Fatal("all five supervisor roles must be configured")
	}
	for _, role := range []string{"bootstrap", "investigation", "implementation", "audit", "output_review"} {
		selected := configuration.Roles[role]
		if selected.Profile != "claude" || len(selected.Skills) == 0 || selected.Skills[0] != "project" {
			t.Fatalf("role %s must use the common profile and project skill", role)
		}
	}
	if _, err := os.Stat(".github/orm/skills/project/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(configuration.Monitor.Checks, []string{"test", "windows-sqlite", "analyze", "CodeQL"}) ||
		len(configuration.Monitor.Reviewers) != 1 ||
		configuration.Monitor.Reviewers[0].Login != "chatgpt-codex-connector[bot]" ||
		configuration.Monitor.Reviewers[0].Signal != "codex" {
		t.Fatal("all current-head CI and Codex review must gate readiness")
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
		"ref: 61fdfc341b96178a8dcb53f2efc46cbc341d267c",
		"path: .cpa-source",
		`CPA_SOURCE="$GITHUB_WORKSPACE/.cpa-source" go test -tags=integration ./... -count=1`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("test workflow is missing %q", required)
		}
	}
}
