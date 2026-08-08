package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowYAML(t *testing.T) {
	for _, path := range []string{".github/workflows/test.yml", ".github/workflows/release.yml"} {
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
