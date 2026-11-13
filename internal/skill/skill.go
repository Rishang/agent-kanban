// Package skill installs the agent skill that teaches the board workflow.
package skill

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var Content []byte

// Install writes SKILL.md to <root>/kanban/SKILL.md for each root and returns the written paths.
func Install(roots ...string) ([]string, error) {
	var out []string
	for _, r := range roots {
		dir := filepath.Join(r, "kanban")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return out, err
		}
		p := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(p, Content, 0o644); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}
