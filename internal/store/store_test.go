package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// One end-to-end pass over the write paths that carry logic.
func TestStore(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.AddProject("Demo App", "/tmp/demo")
	if err != nil || p.ID != "demo-app" {
		t.Fatalf("add project: %+v %v", p, err)
	}
	if again, _ := db.AddProject("other name", "/tmp/demo"); again.ID != p.ID {
		t.Fatalf("same dir registered twice: %+v", again)
	}
	if err := db.RenameProject(p.ID, "  Renamed App "); err != nil {
		t.Fatal(err)
	}
	if ps, _ := db.Projects(); ps[0].ID != p.ID || ps[0].Name != "Renamed App" {
		t.Fatalf("rename kept the id and trimmed the name? %+v", ps[0])
	}
	if db.RenameProject(p.ID, " ") == nil || db.RenameProject("nope", "x") == nil {
		t.Fatal("accepted an empty name or an unknown project")
	}
	s, _ := db.Project(p.ID)
	other, _ := db.AddProject("Other", "")
	os2, _ := db.Project(other.ID)

	a, err := s.CreateTask("me", "ui", TaskInput{Title: "A", Labels: []string{"bug"}, Checklist: []string{"x", " "}})
	if err != nil || a.ID != "KB-1" || a.Column != "todo" || a.Priority != "medium" || len(a.Checklist) != 1 {
		t.Fatalf("create: %+v %v", a, err)
	}
	b, _ := s.CreateTask("me", "ui", TaskInput{Title: "B"})

	// validation errors name the valid values so agents can self-correct
	_, err = s.CreateTask("claude", "mcp", TaskInput{Title: "C", Labels: []string{"nope"}})
	if err == nil || !strings.Contains(err.Error(), "valid: bug") {
		t.Fatalf("label validation: %v", err)
	}
	if _, err = s.MoveTask("me", "ui", MoveInput{ID: a.ID, Column: "zzz"}); err == nil || !strings.Contains(err.Error(), "todo") {
		t.Fatalf("column validation: %v", err)
	}

	// reorder: move B before A
	if _, err := s.MoveTask("me", "ui", MoveInput{ID: b.ID, Column: "todo", Before: a.ID}); err != nil {
		t.Fatal(err)
	}
	board, _ := s.Board()
	if board.Tasks[0].ID != b.ID {
		t.Fatalf("order: %s first, want %s", board.Tasks[0].ID, b.ID)
	}

	// rename label rewrites tasks; deleting a label strips it
	if err := s.RenameLabel("me", RenameInput{From: "bug", To: "defect"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(a.ID); got.Labels[0] != "defect" {
		t.Fatalf("rename: %v", got.Labels)
	}
	if err := s.SaveLabels("me", []Label{{Name: "docs", Color: "#fff"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(a.ID); len(got.Labels) != 0 {
		t.Fatalf("label delete: %v", got.Labels)
	}

	// non-empty column can't be removed
	if err := s.SaveColumns("me", []Column{{ID: "done", Name: "Done"}}); err == nil {
		t.Fatal("removed a column that still has tasks")
	}

	// ids never repeat after delete
	if err := s.DeleteTask("me", "ui", b.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.CreateTask("me", "ui", TaskInput{Title: "C"}); c.ID != "KB-3" {
		t.Fatalf("id reuse: %s", c.ID)
	}

	// human-only: turning it on unassigns an agent; agents can't be assigned afterwards
	agentT, _ := s.CreateTask("me", "ui", TaskInput{Title: "H", Assignee: "claude"})
	on := true
	if got, err := s.UpdateTask("me", "ui", TaskPatch{ID: agentT.ID, Human: &on}); err != nil || !got.Human || got.Assignee != "" {
		t.Fatalf("human on: %+v %v", got, err)
	}
	claude := "claude"
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: agentT.ID, Assignee: &claude}); err == nil {
		t.Fatal("assigned an agent to a human-only task")
	}
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: agentT.ID, Link: ptr("javascript:alert(1)")}); err == nil {
		t.Fatal("accepted a javascript: link")
	}
	if b, err := s.Board(); err != nil {
		t.Fatal(err)
	} else {
		b.Tasks[0].Link = "javascript:alert(1)"
		if err := s.Import("me", b); err == nil {
			t.Fatal("import accepted a javascript: link")
		}
		if after, err := s.Board(); err != nil || len(after.Tasks) != len(b.Tasks) || after.Tasks[0].Link == b.Tasks[0].Link {
			t.Fatalf("rejected import was not rolled back: %v", err)
		}
	}

	// projects are isolated: same ids, separate boards
	if o1, err := os2.CreateTask("me", "ui", TaskInput{Title: "other 1"}); err != nil || o1.ID != "KB-1" {
		t.Fatalf("other project: %+v %v", o1, err)
	}
	if got, _ := s.Task("KB-1"); got.Title != "A" {
		t.Fatalf("project leak: %+v", got)
	}

	// search spans projects, supports filters
	if hits, _ := db.Search("other", 10); len(hits) != 1 || hits[0].Project != other.ID {
		t.Fatalf("search: %+v", hits)
	}
	if hits, _ := db.Search("label:defect", 10); len(hits) != 0 {
		t.Fatalf("label filter after delete: %+v", hits)
	}
	if hits, _ := db.Search("is:human", 10); len(hits) != 1 || !hits[0].Human {
		t.Fatalf("is:human: %+v", hits)
	}

	if _, err := s.AddComment("claude", "mcp", CommentInput{ID: a.ID, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.Events(1); ev[0].Action != "commented on" || ev[0].Who != "claude" {
		t.Fatalf("event: %+v", ev)
	}

	// the cheap readers (project counts, MCP Meta/Tasks, search) agree with Board
	docs := []string{"docs"}
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: a.ID, Labels: &docs}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTask("me", "ui", MoveInput{ID: "KB-3", Column: "done"}); err != nil {
		t.Fatal(err)
	}
	full, _ := s.Board()
	meta, err := s.Meta()
	if err != nil || len(meta.Tasks) != 0 || !reflect.DeepEqual(meta.Labels, full.Labels) || meta.Labels[0].Tasks != 1 ||
		!reflect.DeepEqual(meta.Columns, full.Columns) || !reflect.DeepEqual(meta.Settings, full.Settings) || !reflect.DeepEqual(meta.Members, full.Members) {
		t.Fatalf("meta: %+v %v", meta, err)
	}
	if open, total, err := s.Counts(); err != nil || open != 2 || total != 3 {
		t.Fatalf("counts: %d/%d %v", open, total, err)
	}
	tasks, _ := s.Tasks()
	counts, _ := s.CommentCounts()
	if len(tasks) != len(full.Tasks) || tasks[0].ID != full.Tasks[0].ID || len(tasks[0].Comments) != 0 || counts[a.ID] != 1 || len(counts) != 1 {
		t.Fatalf("tasks: %+v %v", tasks, counts)
	}
	if hits, _ := db.Search("is:open hi", 10); len(hits) != 1 || hits[0].ID != a.ID || hits[0].Where != "comment by claude" || hits[0].Snippet != "hi" {
		t.Fatalf("comment search: %+v", hits)
	}
	if hits, _ := db.Search("project:demo is:done", 10); len(hits) != 1 || hits[0].ID != "KB-3" || hits[0].ColumnName != "Done" || hits[0].Labels == nil {
		t.Fatalf("is:done: %+v", hits)
	}
	if hits, _ := db.Search("label:docs", 10); len(hits) != 1 || hits[0].ID != a.ID {
		t.Fatalf("label filter: %+v", hits)
	}
}

func ptr[T any](v T) *T { return &v }
