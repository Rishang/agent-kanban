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
	if got, _ = s.SetPlan("me", "ui", PlanInput{ID: task.ID, Status: "approved"}); got.PlanStatus != "approved" || got.PlanVersion != 1 {
		t.Fatalf("human approve: %+v", got)
	}
	// resending the same text with a stale status keeps the approval
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr(mdx)}); got.PlanStatus != "approved" || got.PlanVersion != 1 {
		t.Fatalf("resend reset approval: %+v", got)
	}
	// a revision after approval goes back to review, as a new version
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# Plan v2")}); got.PlanStatus != "awaiting" || got.PlanVersion != 2 {
		t.Fatalf("revision: %+v", got)
	}
	// an html-plan-report page: status from its badge, but a page can't approve itself
	page := `<!DOCTYPE html><html><body><span class="badge status ok" data-status="approved">Approved</span></body></html>`
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr(page)}); got.PlanStatus != "awaiting" || got.PlanFormat != "html" || got.PlanVersion != 3 {
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
