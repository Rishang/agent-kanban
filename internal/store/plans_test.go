package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPlans(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("Plans", "")
	s, _ := db.Project(p.ID)
	task, _ := s.CreateTask("me", "ui", TaskInput{Title: "feature"})

	if _, err := s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Status: "draft"}); err == nil {
		t.Fatal("status without a plan accepted")
	}
	mdx := "---\ntitle: X\nstatus: draft\n---\nimport Foo from './foo'\n\n# Plan\n\n<Foo />\n"
	got, err := s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr(mdx)})
	if err != nil || got.PlanStatus != "draft" || got.PlanVersion != 1 || got.Plan != "# Plan\n\n<Foo />" {
		t.Fatalf("mdx plan: %+v %v", got, err)
	}
	if _, err := s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Status: "approved"}); err == nil || !strings.Contains(err.Error(), "human") {
		t.Fatalf("agent approved its own plan: %v", err)
	}
	if _, err := s.SetPlan("me", "ui", PlanInput{ID: task.ID, Status: "approved", Version: 0}); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("approved a version the reviewer didn't see: %v", err)
	}
	if got, _ = s.SetPlan("me", "ui", PlanInput{ID: task.ID, Status: "approved", Version: 1}); got.PlanStatus != "approved" || got.PlanVersion != 1 {
		t.Fatalf("human approve: %+v", got)
	}
	// resending the same text with a stale status keeps the approval
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr(mdx)}); got.PlanStatus != "approved" || got.PlanVersion != 1 {
		t.Fatalf("resend reset approval: %+v", got)
	}
	// an agent can't rewrite an approved plan and call it implemented, by status or by front matter
	if _, err := s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# Other plan"), Status: "implemented"}); err == nil {
		t.Fatal("unreviewed version marked implemented")
	}
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("---\nstatus: implemented\n---\n# Other plan")}); got.PlanStatus != "awaiting" {
		t.Fatalf("front matter moved a plan past review: %+v", got)
	}
	s.SetPlan("me", "ui", PlanInput{ID: task.ID, Body: ptr(mdx)})
	s.SetPlan("me", "ui", PlanInput{ID: task.ID, Status: "approved", Version: 3})
	// a revision after approval goes back to review, as a new version
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# Plan v2")}); got.PlanStatus != "awaiting" || got.PlanVersion != 4 {
		t.Fatalf("revision: %+v", got)
	}
	// an html-plan-report page: status from its badge, but a page can't approve itself
	page := `<!DOCTYPE html><html><body><span class="badge status ok" data-status="approved">Approved</span></body></html>`
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr(page)}); got.PlanStatus != "awaiting" || got.PlanFormat != "html" || got.PlanVersion != 5 {
		t.Fatalf("html plan: %+v", got)
	}
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr(strings.Replace(page, "approved", "draft", 1))}); got.PlanStatus != "draft" {
		t.Fatalf("html status: %+v", got)
	}
	if _, err := s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Status: "bogus"}); err == nil {
		t.Fatal("bad status accepted")
	}
	if got, _ = s.Task(task.ID); !strings.Contains(got.Plan, `data-status="draft"`) {
		t.Fatalf("not stored: %+v", got)
	}
	if got, _ = s.SetPlan("me", "ui", PlanInput{ID: task.ID, Body: ptr("")}); got.Plan != "" || got.PlanStatus != "" {
		t.Fatalf("remove: %+v", got)
	}
}

func TestPlanCodeImports(t *testing.T) {
	body, _ := ParsePlan("import X from './x'\n\n# Plan\n\n```python\nimport os\n```\n\n```js\nexport default 1\n```")
	if body != "# Plan\n\n```python\nimport os\n```\n\n```js\nexport default 1\n```" {
		t.Fatalf("body = %q", body)
	}
}

func TestPlanStaleEdit(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("Plans", "")
	s, _ := db.Project(p.ID)
	task, _ := s.CreateTask("me", "ui", TaskInput{Title: "feature"})
	s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# v1")})
	s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# v2 from the agent")})
	if _, err := s.SetPlan("me", "ui", PlanInput{ID: task.ID, Version: 1, Body: ptr("# v1 edited"), Status: "awaiting"}); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale edit accepted: %v", err)
	}
	if got, _ := s.Task(task.ID); got.Plan != "# v2 from the agent" {
		t.Fatalf("plan = %q", got.Plan)
	}
	if got, err := s.SetPlan("me", "ui", PlanInput{ID: task.ID, Version: 2, Body: ptr("# v2 edited"), Status: "awaiting"}); err != nil || got.PlanVersion != 3 {
		t.Fatalf("current edit: %+v %v", got, err)
	}
}
