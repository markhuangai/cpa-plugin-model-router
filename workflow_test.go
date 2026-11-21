package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var workflowPaths = []string{
	".github/workflows/test.yml",
	".github/workflows/codeql.yml",
	".github/workflows/ai-pr-review.yml",
	".github/workflows/release.yml",
	".github/workflows/maintenance.yml",
	".github/workflows/test-ai.yml",
	".github/workflows/orm-agent.yml",
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
		".github/workflows/ai-pr-review.yml": {
			"ubuntu-latest": true,
		},
		".github/workflows/release.yml": {
			"ubuntu-latest": true,
		},
	}
	expectedRunners := map[string]int{
		".github/workflows/test.yml":         2,
		".github/workflows/codeql.yml":       2,
		".github/workflows/ai-pr-review.yml": 1,
		".github/workflows/release.yml":      3,
		".github/workflows/maintenance.yml":  0,
		".github/workflows/test-ai.yml":      0,
		".github/workflows/orm-agent.yml":    0,
	}
	allowedActions := map[string]bool{
		"actions/checkout@v7":          true,
		"actions/setup-go@v7":          true,
		"actions/setup-node@v7":        true,
		"actions/upload-artifact@v7":   true,
		"actions/download-artifact@v8": true,
		"actions/github-script@v9":     true,
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
	Name        string
	RunName     string `yaml:"run-name"`
	On          map[string]any
	Permissions map[string]string
	Jobs        map[string]struct {
		Name        string
		If          string
		Needs       string
		Uses        string
		With        map[string]any
		Secrets     map[string]string
		Permissions map[string]string
		Concurrency map[string]any
		Strategy    struct {
			FailFast bool `yaml:"fail-fast"`
			Matrix   struct {
				Include []struct {
					Language  string
					BuildMode string `yaml:"build-mode"`
				}
			}
		}
		Steps []struct {
			Name string
			Uses string
			If   string
			Run  string
			With map[string]any
			Env  map[string]string
		}
	}
}

func TestCodeQLLanguageMatrixAndAggregate(t *testing.T) {
	var workflow ormWorkflow
	readORMYAML(t, ".github/workflows/codeql.yml", &workflow)
	if workflow.Name != "CodeQL" {
		t.Fatal("CodeQL must have the documented workflow name")
	}
	languages := workflow.Jobs["analyze"]
	modes := map[string]string{}
	for _, entry := range languages.Strategy.Matrix.Include {
		modes[entry.Language] = entry.BuildMode
	}
	if len(languages.Strategy.Matrix.Include) != 3 || languages.Strategy.FailFast ||
		!reflect.DeepEqual(modes, map[string]string{"actions": "none", "go": "manual", "javascript-typescript": "none"}) {
		t.Fatalf("all three independent language analyses are required: %v", modes)
	}
	if languages.Name != "Analyze (${{ matrix.language }})" {
		t.Fatal("language job names must match the aggregate gate")
	}
	var initialized, built, analyzed bool
	matrixIdentity := "${{ matrix.language == 'go' && '{}' || toJSON(matrix) }}"
	for _, step := range languages.Steps {
		switch step.Uses {
		case "github/codeql-action/init@v4":
			initialized = step.With["languages"] == "${{ matrix.language }}" && step.With["build-mode"] == "${{ matrix.build-mode }}" &&
				step.With["matrix"] == matrixIdentity
		case "github/codeql-action/analyze@v4":
			analyzed = step.With["category"] == "${{ matrix.language == 'go' && '.github/workflows/codeql.yml:analyze' || format('/language:{0}', matrix.language) }}" &&
				step.With["matrix"] == matrixIdentity
		}
		if step.Name == "Build plugin" {
			built = step.If == "matrix.language == 'go'" && strings.Contains(step.Run, "CGO_ENABLED=1 go build -trimpath -buildmode=c-shared")
		}
	}
	if !initialized || !built || !analyzed {
		t.Fatal("CodeQL v4 must retain native CGO extraction, the existing Go scan identity, and distinct language categories")
	}
	aggregate := workflow.Jobs["gate"]
	if aggregate.Name != "analyze" || aggregate.Needs != "analyze" || aggregate.If != "always()" || len(aggregate.Steps) != 1 ||
		!reflect.DeepEqual(aggregate.Permissions, map[string]string{"actions": "read"}) {
		t.Fatal("the existing analyze check must aggregate every language even after a failure")
	}
}

func TestCodeQLAggregateExecutionPolicy(t *testing.T) {
	var workflow ormWorkflow
	readORMYAML(t, ".github/workflows/codeql.yml", &workflow)
	script, ok := workflow.Jobs["gate"].Steps[0].With["script"].(string)
	if !ok {
		t.Fatal("aggregate check script is missing")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node.js is required for workflow policy tests")
	}
	job := func(language string, attempt int, conclusion string) map[string]any {
		return map[string]any{"name": "Analyze (" + language + ")", "run_attempt": attempt, "status": "completed", "conclusion": conclusion}
	}
	passed := func() []map[string]any {
		return []map[string]any{job("actions", 1, "success"), job("go", 1, "success"), job("javascript-typescript", 1, "success")}
	}
	cases := []struct {
		name string
		jobs []map[string]any
		fail bool
	}{
		{"all languages passed", passed(), false},
		{"missing Actions analysis", passed()[1:], true},
		{"missing Go analysis", []map[string]any{job("actions", 1, "success"), job("javascript-typescript", 1, "success")}, true},
		{"missing JavaScript analysis", passed()[:2], true},
		{"skipped analysis", []map[string]any{job("actions", 1, "success"), job("go", 1, "skipped"), job("javascript-typescript", 1, "success")}, true},
		{"latest rerun failed", append(passed(), job("go", 2, "failure")), true},
		{"latest rerun repaired", []map[string]any{job("actions", 1, "success"), job("go", 1, "failure"), job("go", 2, "success"), job("javascript-typescript", 1, "success")}, false},
		{"duplicate latest execution", append(passed(), job("go", 1, "success")), true},
		{"future attempt cannot override current failure", append(passed(), job("go", 2, "failure"), job("go", 3, "success")), true},
	}
	program := `
import { readFileSync } from 'node:fs';
const { script, jobs } = JSON.parse(readFileSync(0, 'utf8'));
const failures = [];
const github = { rest: { actions: { listJobsForWorkflowRun: null } }, paginate: async () => jobs };
const context = { repo: { owner: 'fixture', repo: 'fixture' }, runId: 707 };
const core = { setFailed: message => failures.push(message) };
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
await new AsyncFunction('github', 'context', 'core', script)(github, context, core);
process.stdout.write(JSON.stringify(failures));
`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{"script": script, "jobs": tc.jobs})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(node, "--input-type=module", "--eval", program)
			cmd.Env = append(os.Environ(), "GITHUB_RUN_ATTEMPT=2")
			cmd.Stdin = strings.NewReader(string(input))
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("aggregate policy execution failed: %v\n%s", err, output)
			}
			var failures []string
			if err := json.Unmarshal(output, &failures); err != nil {
				t.Fatal(err)
			}
			if (len(failures) > 0) != tc.fail {
				t.Fatalf("unexpected aggregate result: %v", failures)
			}
		})
	}
}

func TestAIReviewerDirectActionAndTrust(t *testing.T) {
	var workflow ormWorkflow
	readORMYAML(t, ".github/workflows/ai-pr-review.yml", &workflow)
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("automatic reviews must use an unprivileged PR event")
	}
	if _, ok := workflow.On["pull_request_target"]; ok {
		t.Fatal("automatic reviews must not check out PR code through a privileged event")
	}
	if !reflect.DeepEqual(workflow.Permissions, map[string]string{"contents": "read", "pull-requests": "read"}) {
		t.Fatal("review job token must remain read-only")
	}
	job := workflow.Jobs["review"]
	if !strings.Contains(job.If, "github.event.pull_request.head.repo.full_name == github.repository") ||
		!strings.Contains(job.If, "github.event_name == 'workflow_dispatch'") {
		t.Fatal("fork reviews require an explicit maintainer dispatch")
	}
	if len(workflow.Jobs) != 1 || len(job.Steps) != 2 || job.Concurrency["cancel-in-progress"] != true {
		t.Fatal("review must call the packaged action directly with per-PR cancellation")
	}
	checkout, review := job.Steps[0], job.Steps[1]
	if checkout.Uses != "actions/checkout@v7" || checkout.With["persist-credentials"] != false || checkout.With["fetch-depth"] != 0 ||
		checkout.With["ref"] != "${{ github.event.pull_request.head.sha || format('refs/pull/{0}/head', inputs.pull-request-number) }}" {
		t.Fatal("review requires a full-history PR revision without persisted credentials")
	}
	if checkout.Run != "" || review.Run != "" || review.Uses != "markhuangai/ai-pr-reviewer@v1-prerelease" {
		t.Fatal("privileged review must not execute repository setup or build scripts")
	}
	for key, expected := range map[string]string{
		"executor": "claude", "github-pat": "${{ secrets.PR_REVIEW_PAT }}",
		"ai-base-url": "${{ secrets.AI_ENDPOINT }}", "ai-secret": "${{ secrets.ORM_PROXY_KEY }}",
		"model": "${{ vars.CLAUDE_MODEL }}", "effort": "${{ vars.CLAUDE_EFFORT }}", "auto-approve": "false",
	} {
		if review.With[key] != expected {
			t.Fatalf("review input %s must be %q", key, expected)
		}
	}
	prompts, ok := review.With["review-prompts"].(string)
	if !ok {
		t.Fatal("review prompts are missing")
	}
	var goals []struct{ Prompt string }
	if err := json.Unmarshal([]byte(prompts), &goals); err != nil || len(goals) != 3 {
		t.Fatalf("review prompts must define three valid goals: %v", err)
	}
	for _, goal := range goals {
		if strings.TrimSpace(goal.Prompt) == "" {
			t.Fatal("review goals must not be empty")
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

func TestORMWorkflowDefaultsAndTrust(t *testing.T) {
	for _, caller := range []struct {
		file, job, upstream, guard, actions string
		inputs                              map[string]any
	}{
		{
			file:     "maintenance.yml",
			job:      "maintenance",
			upstream: "maintenance.yml",
			guard:    "vars.ORM_ENABLED == 'true'",
			actions:  "read",
			inputs:   map[string]any{"issue-number": "${{ inputs.issue-number || '' }}"},
		},
		{
			file:     "test-ai.yml",
			job:      "test",
			upstream: "test-ai.yml",
			guard:    "github.ref == format('refs/heads/{0}', github.event.repository.default_branch)",
			actions:  "write",
			inputs:   map[string]any{"role": "${{ inputs.role }}"},
		},
		{
			file:     "orm-agent.yml",
			job:      "agent",
			upstream: "test-ai.yml",
			guard:    "github.run_attempt == '1'",
			actions:  "write",
			inputs: map[string]any{
				"issue-number": "${{ inputs.issue }}",
				"turn":         "${{ inputs.turn }}",
				"plan":         "${{ inputs.plan }}",
			},
		},
	} {
		t.Run(caller.file, func(t *testing.T) {
			var workflow ormWorkflow
			readORMYAML(t, ".github/workflows/"+caller.file, &workflow)
			job := workflow.Jobs[caller.job]
			if !reflect.DeepEqual(workflow.Permissions, map[string]string{"contents": "read", "actions": caller.actions}) || len(job.Permissions) != 0 {
				t.Fatalf("unexpected caller permissions: %v", workflow.Permissions)
			}
			if len(workflow.Jobs) != 1 || job.Needs != "" ||
				job.Uses != "markhuangai/opensource-repo-manager/.github/workflows/"+caller.upstream+"@v1-prerelease" {
				t.Fatal("caller must use the upstream prerelease workflow directly")
			}
			caller.inputs["runner"] = "ubuntu-24.04"
			if job.If != caller.guard || !reflect.DeepEqual(job.With, caller.inputs) {
				t.Fatal("direct caller must preserve admission guards and use upstream runtime defaults")
			}
			checkORMSecretMap(t, job.Secrets["ORM_CONFIGURATION_SECRETS"], map[string]string{
				"ORM_GITHUB_TOKEN": "ORM_GITHUB_TOKEN",
				"ORM_SESSION_KEY":  "ORM_SESSION_KEY",
			})
			delete(job.Secrets, "ORM_CONFIGURATION_SECRETS")
			wantSecrets := map[string]string{"ORM_CONFIGURATION_VARIABLES": "${{ toJSON(vars) }}"}
			if caller.upstream == "test-ai.yml" {
				checkORMSecretMap(t, job.Secrets["ORM_AGENT_PROFILES"], map[string]string{"claude": "CLAUDE_SETTINGS_JSON"})
				delete(job.Secrets, "ORM_AGENT_PROFILES")
				wantSecrets["ORM_CLAUDE_API_KEY"] = "${{ secrets.ANTHROPIC_API_KEY }}"
				wantSecrets["ORM_AGENT_READ_TOKEN"] = "${{ secrets.GITHUB_TOKEN }}"
			}
			if caller.file == "orm-agent.yml" {
				wantSecrets["ORM_AGENT_WORK_TOKEN"] = "${{ secrets.REMOTE_DEV_PAT }}"
			}
			if !reflect.DeepEqual(job.Secrets, wantSecrets) {
				t.Fatal("caller forwards unexpected secret inputs")
			}
		})
	}
}

func checkORMSecretMap(t *testing.T, payload string, want map[string]string) {
	t.Helper()
	reference := regexp.MustCompile(`\$\{\{ toJSON\(secrets\.([A-Z_]+)\) \}\}`)
	if len(reference.FindAllStringIndex(payload, -1)) != len(want) {
		t.Fatal("each secret map entry must use an explicit secret reference")
	}
	var resolved map[string]string
	if err := json.Unmarshal([]byte(reference.ReplaceAllString(payload, `"$1"`)), &resolved); err != nil {
		t.Fatal("secret map must be valid JSON after expression resolution")
	}
	if !reflect.DeepEqual(resolved, want) {
		t.Fatal("secret map forwards unexpected references")
	}
}

func TestORMAgentDispatchAndCompletion(t *testing.T) {
	var agent, maintenance ormWorkflow
	readORMYAML(t, ".github/workflows/orm-agent.yml", &agent)
	readORMYAML(t, ".github/workflows/maintenance.yml", &maintenance)
	if agent.Name != "ORM agent" || agent.RunName != "ORM agent #${{ inputs.issue }} ${{ inputs.turn }}" {
		t.Fatal("agent runs must carry the issue and turn identity the controller routes")
	}
	dispatch, _ := agent.On["workflow_dispatch"].(map[string]any)
	inputs, _ := dispatch["inputs"].(map[string]any)
	if len(agent.On) != 1 || len(inputs) != 3 {
		t.Fatal("agent must accept only the issue, turn and plan through workflow_dispatch")
	}
	for _, name := range []string{"issue", "turn", "plan"} {
		input, _ := inputs[name].(map[string]any)
		if input["type"] != "string" || input["required"] != true {
			t.Fatalf("%s must be a required string input", name)
		}
	}
	wake := map[string]any{"workflows": []any{agent.Name}, "types": []any{"completed"}}
	if !reflect.DeepEqual(maintenance.On["workflow_run"], wake) {
		t.Fatal("maintenance must wake on agent completion")
	}

	var remoteDev struct {
		AllowedActors []string `yaml:"allowed-actors"`
		Skills        []string
		AutoContinue  *bool `yaml:"auto-continue"`
	}
	readORMYAML(t, ".remote-dev.yml", &remoteDev)
	if !reflect.DeepEqual(remoteDev.AllowedActors, []string{"MarkHuang-AI-Service", "Z-M-Huang"}) ||
		remoteDev.Skills == nil || len(remoteDev.Skills) != 0 || remoteDev.AutoContinue == nil || *remoteDev.AutoContinue {
		t.Fatal("remote development must allow only the controller and probe operator and defer skills and continuation to ORM")
	}
}

func TestORMConfigurationProfilesAndGates(t *testing.T) {
	var configuration struct {
		Version    int
		Github     struct{ Token map[string]string }
		SessionKey map[string]string `yaml:"session_key"`
		Profiles   map[string]map[string]string
		Limits     map[string]int
		Roles      map[string]struct {
			Profile string
			Skills  []string
		}
		Monitor struct {
			Enabled            bool
			RequirementsSource string                           `yaml:"requirements_source"`
			Checks             []string                         `yaml:"required_checks"`
			Reviewers          []struct{ Login, Signal string } `yaml:"automated_reviewers"`
		} `yaml:"pr_monitor"`
	}
	readORMYAML(t, ".github/orm.yml", &configuration)
	if configuration.Version != 2 || configuration.Github.Token["from_secret"] != "ORM_GITHUB_TOKEN" ||
		configuration.SessionKey["from_secret"] != "ORM_SESSION_KEY" {
		t.Fatal("the v2 controller must resolve its PAT and session key from explicit secret references")
	}
	if !reflect.DeepEqual(configuration.Profiles, map[string]map[string]string{"claude": {"sdk": "claude"}}) {
		t.Fatal("the workflow must supply native settings for the sole Claude profile")
	}
	limits := map[string]int{"active_minutes": 60, "agent_runs": 200, "repair_cycles": 3, "infrastructure_retries": 2, "wait_hours": 24}
	if !reflect.DeepEqual(configuration.Limits, limits) {
		t.Fatal("unexpected v2 task limits")
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
	if !configuration.Monitor.Enabled || configuration.Monitor.RequirementsSource != "github" ||
		!reflect.DeepEqual(configuration.Monitor.Checks, []string{"test", "windows-sqlite", "analyze", "CodeQL"}) ||
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
