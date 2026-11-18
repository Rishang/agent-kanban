package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPlanFile(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "p.mdx"), []byte("# plan"), 0o600)
	os.WriteFile(filepath.Join(out, "x.md"), []byte("secret"), 0o600)
	os.Symlink(filepath.Join(out, "x.md"), filepath.Join(dir, "link.md"))
	if got, rel, err := ReadPlanFile(dir, "p.mdx"); err != nil || got != "# plan" || rel != "p.mdx" {
		t.Fatalf("ReadPlanFile = %q %q %v", got, rel, err)
	}
	for _, bad := range []string{"../" + filepath.Base(out) + "/x.md", filepath.Join(out, "x.md"), "link.md", "p.txt"} {
		if _, _, err := ReadPlanFile(dir, bad); err == nil {
			t.Errorf("ReadPlanFile(%q) should fail", bad)
		}
	}
	// a new file under a symlinked folder that leads outside is refused
	os.Symlink(out, filepath.Join(dir, "away"))
	if _, _, err := planFile(dir, "away/new/plan.mdx"); err == nil {
		t.Error("new file through a symlinked folder should fail")
	}
	if _, _, err := ReadPlanFile("", "p.md"); err == nil {
		t.Error("project without a folder should fail")
	}
	// an html-plan-report page gets its _assets inlined
	os.MkdirAll(filepath.Join(dir, ".plan", "_assets"), 0o755)
	os.WriteFile(filepath.Join(dir, ".plan", "_assets", "plan.css"), []byte("body{}"), 0o600)
	os.WriteFile(filepath.Join(dir, ".plan", "_assets", "plan.js"), []byte("var x=1"), 0o600)
	page := "<!DOCTYPE html>\n<html><head>\n<link rel=\"stylesheet\" href=\"_assets/plan.css\">\n<script src=\"_assets/plan.js\"></script>\n</head>" +
		"<body><a class=\"back\" href=\"index.html\">← All plans</a><h1>Fix</h1></body></html>"
	os.WriteFile(filepath.Join(dir, ".plan", "x.plan.html"), []byte(page), 0o600)
	got, _, err := ReadPlanFile(dir, ".plan/x.plan.html")
	if err != nil || !strings.Contains(got, "<style>\nbody{}") || !strings.Contains(got, "var x=1") || strings.Contains(got, "All plans") || !strings.Contains(got, "plan-bundle") {
		t.Fatalf("bundled plan = %q %v", got, err)
	}
	// a symlinked asset that leads outside is never inlined
	os.Remove(filepath.Join(dir, ".plan", "_assets", "plan.css"))
	os.Symlink(filepath.Join(out, "x.md"), filepath.Join(dir, ".plan", "_assets", "plan.css"))
	if got, _, err := ReadPlanFile(dir, ".plan/x.plan.html"); err == nil || strings.Contains(got, "secret") {
		t.Fatalf("inlined an asset from outside the project: %v", err)
	}
}

// planStorage = files: the plan is a file in the repo, synced both ways.
func TestPlanFiles(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	p, _ := db.AddProject("Spec", repo)
	s, _ := db.Project(p.ID)
	st, _ := getSettings(s.db, s.pid)
	st.PlanStorage = PlanStorageFiles
	if err := s.SaveSettings("me", st); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(repo, rel)); return string(b) }

	// an agent attaches a spec file: linked, front matter and imports kept in the file
	spec := PlanDir + "/kb-1-search.mdx"
	os.MkdirAll(filepath.Join(repo, PlanDir), 0o755)
	os.WriteFile(filepath.Join(repo, spec), []byte("---\ntitle: Search\n---\nimport X from './x'\n\n# Search\n\nv1 text\n"), 0o644)
	a, _ := s.CreateTask("me", "ui", TaskInput{Title: "Search"})
	got, err := s.SetPlan("bot", "mcp", PlanInput{ID: a.ID, Path: spec})
	if err != nil || got.PlanPath != spec || got.PlanVersion != 1 || got.PlanStatus != "awaiting" {
		t.Fatalf("link: %+v %v", got, err)
	}
	if f := read(spec); !strings.Contains(f, "status: awaiting") || !strings.Contains(f, "title: Search") || !strings.Contains(f, "import X from './x'") {
		t.Fatalf("status not written to the file:\n%s", f)
	}
	// approving on the board writes the status into the spec, without a new version
	if got, _ = s.SetPlan("me", "ui", PlanInput{ID: a.ID, Status: "approved", Version: 1}); got.PlanVersion != 1 || !strings.Contains(read(spec), "status: approved") {
		t.Fatalf("approve: %+v\n%s", got, read(spec))
	}
	if ev, _ := s.Events(1); ev[0].Action != "marked the plan of" {
		t.Fatalf("approve logged as %q", ev[0].Action)
	}
	// an edit in the repo comes back as v2, awaiting again (the file can't keep "approved" for new text)
	f := strings.Replace(read(spec), "v1 text", "v2 text", 1)
	os.WriteFile(filepath.Join(repo, spec), []byte(f), 0o644)
	s.RefreshPlans()
	if got, _ = s.Task(a.ID); got.PlanVersion != 2 || got.PlanStatus != "awaiting" || !strings.Contains(got.Plan, "v2 text") {
		t.Fatalf("refresh: %+v", got)
	}
	s.RefreshPlans() // unchanged: no new version
	if got, _ = s.Task(a.ID); got.PlanVersion != 2 {
		t.Fatalf("refresh twice: %+v", got)
	}
	// a board edit is written to the file
	body := "# Search\n\nedited on the board"
	if _, err := s.SetPlan("me", "ui", PlanInput{ID: a.ID, Body: &body, Status: "awaiting"}); err != nil || !strings.Contains(read(spec), "edited on the board") || !strings.Contains(read(spec), "import X") {
		t.Fatalf("write back: %v\n%s", err, read(spec))
	}
	// (a) a board edit based on a stale copy is refused instead of overwriting the repo's newer text
	os.WriteFile(filepath.Join(repo, spec), []byte(strings.Replace(read(spec), "edited on the board", "agent edit not yet synced", 1)), 0o644)
	stale := "# Search\n\nboard edit on the old text"
	if _, err := s.SetPlan("me", "ui", PlanInput{ID: a.ID, Body: &stale}); err == nil || !strings.Contains(err.Error(), "changed in the repo") {
		t.Fatalf("stale board edit allowed: %v", err)
	}
	// (b) a status change touches only the status line, even with unsynced text on disk
	if _, err := s.SetPlan("me", "ui", PlanInput{ID: a.ID, Status: "draft"}); err != nil || !strings.Contains(read(spec), "agent edit not yet synced") || !strings.Contains(read(spec), "status: draft") {
		t.Fatalf("status write clobbered the file: %v\n%s", err, read(spec))
	}
	// (c) an agent can't mark a never-approved plan implemented
	n, _ := s.CreateTask("me", "ui", TaskInput{Title: "Never approved"})
	nb := "# draft plan"
	s.SetPlan("bot", "mcp", PlanInput{ID: n.ID, Body: &nb})
	if _, err := s.SetPlan("bot", "mcp", PlanInput{ID: n.ID, Status: "implemented"}); err == nil || !strings.Contains(err.Error(), "never approved") {
		t.Fatalf("implemented without approval: %v", err)
	}
	// (e) after approval, a deviation-log edit picked up from the repo first (awaiting) can still be marked
	// implemented-deviations by the agent's own call, without a second approval
	cur, _ := s.Task(a.ID)
	s.SetPlan("me", "ui", PlanInput{ID: a.ID, Status: "approved", Version: cur.PlanVersion})
	os.WriteFile(filepath.Join(repo, spec), []byte(read(spec)+"\n## Deviation log\n| S1 | changed |\n"), 0o644)
	s.RefreshPlans()
	got, err = s.SetPlan("bot", "mcp", PlanInput{ID: a.ID, Path: spec, Status: "implemented-deviations"})
	if err != nil || got.PlanStatus != "implemented-deviations" || !strings.Contains(read(spec), "status: implemented-deviations") {
		t.Fatalf("build status after approval: %+v %v", got, err)
	}
	// writing never follows a symlinked plan file
	other := filepath.Join(t.TempDir(), "victim.mdx")
	os.WriteFile(other, []byte("keep"), 0o644)
	os.Symlink(other, filepath.Join(repo, PlanDir, "sym.mdx"))
	if err := writePlanFile(repo, PlanDir+"/sym.mdx", "# x", "draft", false); err == nil {
		t.Error("write through a symlinked plan file should fail")
	}
	if b, _ := os.ReadFile(other); string(b) != "keep" {
		t.Errorf("file outside the repo was changed: %q", b)
	}
	// a plan written on the board gets its own file
	b, _ := s.CreateTask("me", "ui", TaskInput{Title: "Dark mode!"})
	body = "# Dark mode"
	if got, _ = s.SetPlan("me", "ui", PlanInput{ID: b.ID, Body: &body}); got.PlanPath != PlanDir+"/"+strings.ToLower(b.ID)+"-dark-mode.mdx" || !strings.Contains(read(got.PlanPath), "# Dark mode") {
		t.Fatalf("new file: %+v", got)
	}
	// in the default mode a path is copied once, not linked
	st.PlanStorage = ""
	s.SaveSettings("me", st)
	c, _ := s.CreateTask("me", "ui", TaskInput{Title: "Copy"})
	if got, _ = s.SetPlan("bot", "mcp", PlanInput{ID: c.ID, Path: spec}); got.PlanPath != "" || got.Plan == "" {
		t.Fatalf("board mode linked a file: %+v", got)
	}
}
