package main

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"text/template"
)

//go:embed dashboard.html dashboard
var dashboardSource embed.FS

var configDashboardHTML, configDashboardError = assembleModelRouterDashboard(dashboardSource)

func assembleModelRouterDashboard(source fs.FS) ([]byte, error) {
	shell, err := fs.ReadFile(source, "dashboard.html")
	if err != nil {
		return nil, fmt.Errorf("read dashboard shell: %w", err)
	}

	page, err := template.New("dashboard.html").Funcs(template.FuncMap{
		"include": func(path string) (string, error) {
			content, err := fs.ReadFile(source, path)
			if err != nil {
				return "", fmt.Errorf("read dashboard asset %q: %w", path, err)
			}
			return string(content), nil
		},
	}).Parse(string(shell))
	if err != nil {
		return nil, fmt.Errorf("parse dashboard shell: %w", err)
	}

	var rendered bytes.Buffer
	if err := page.Execute(&rendered, nil); err != nil {
		return nil, fmt.Errorf("assemble dashboard: %w", err)
	}
	return rendered.Bytes(), nil
}
