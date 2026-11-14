package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubtasks(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("Sub", "")
	s, _ := db.Project(p.ID)

	parent, _ := s.CreateTask("me", "ui", TaskInput{Title: "parent"})
	other, _ := s.CreateTask("me", "ui", TaskInput{Title: "other"})
	child, err := s.CreateTask("me", "ui", TaskInput{Title: "child", Parent: parent.ID})
	if err != nil || child.Parent != parent.ID {
		t.Fatalf("create sub-task: %+v %v", child, err)
	}
	if got, _ := s.Task(child.ID); got.Parent != parent.ID {
		t.Fatalf("parent not stored: %+v", got)
	}

	// one level: no grandchildren, no parent becoming a child, no self, parent must exist
	if _, err := s.CreateTask("me", "ui", TaskInput{Title: "grandchild", Parent: child.ID}); err == nil || !strings.Contains(err.Error(), "one level") {
		t.Fatalf("grandchild allowed: %v", err)
	}
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: parent.ID, Parent: ptr(other.ID)}); err == nil || !strings.Contains(err.Error(), "sub-task(s)") {
		t.Fatalf("parent with children re-parented: %v", err)
	}
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: other.ID, Parent: ptr(other.ID)}); err == nil {
		t.Fatal("task became its own parent")
	}
	if _, err := s.CreateTask("me", "ui", TaskInput{Title: "x", Parent: "KB-999"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing parent accepted: %v", err)
	}

	// re-parent and un-parent
	if got, err := s.UpdateTask("me", "ui", TaskPatch{ID: child.ID, Parent: ptr(other.ID)}); err != nil || got.Parent != other.ID {
		t.Fatalf("re-parent: %+v %v", got, err)
	}
	if got, err := s.UpdateTask("me", "ui", TaskPatch{ID: child.ID, Parent: ptr("")}); err != nil || got.Parent != "" {
		t.Fatalf("un-parent: %+v %v", got, err)
	}
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: child.ID, Parent: ptr(parent.ID)}); err != nil {
		t.Fatal(err)
	}

	// deleting a parent deletes its sub-tasks and their comments; other tasks stay
	if _, err := s.AddComment("me", "ui", CommentInput{ID: child.ID, Text: "note"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask("me", "ui", parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(child.ID); err == nil {
		t.Fatal("sub-task survived its parent")
	}
	if n, _ := s.CommentCounts(); n[child.ID] != 0 {
		t.Fatal("sub-task comments survived")
	}
	if _, err := s.Task(other.ID); err != nil {
		t.Fatalf("unrelated task deleted: %v", err)
	}

	// import keeps parents and applies the same rules
	kid, _ := s.CreateTask("me", "ui", TaskInput{Title: "kid", Parent: other.ID})
	b, _ := s.Board()
	if err := s.Import("me", b); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(kid.ID); got.Parent != other.ID {
		t.Fatalf("import dropped parent: %+v", got)
	}
	bad, _ := s.Board()
	for i := range bad.Tasks {
		if bad.Tasks[i].ID == other.ID {
			bad.Tasks[i].Parent = kid.ID // two levels
		}
	}
	if err := s.Import("me", bad); err == nil || !strings.Contains(err.Error(), "one level") {
		t.Fatalf("import accepted two levels: %v", err)
	}
	if got, _ := s.Task(kid.ID); got.Parent != other.ID {
		t.Fatal("rejected import changed the board")
	}
}

// A database from before sub-tasks: tasks has no parent column.
func TestMigrateTaskParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE tasks (project TEXT NOT NULL, id TEXT NOT NULL, title TEXT NOT NULL, descr TEXT NOT NULL DEFAULT '',
			col TEXT NOT NULL, priority TEXT NOT NULL, assignee TEXT NOT NULL DEFAULT '', due TEXT NOT NULL DEFAULT '',
			labels TEXT NOT NULL DEFAULT '[]', checklist TEXT NOT NULL DEFAULT '[]', pos REAL NOT NULL, created INTEGER NOT NULL,
			updated INTEGER NOT NULL, link TEXT NOT NULL DEFAULT '', human INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (project, id))`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db, err := Open(path, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("Old", "")
	s, _ := db.Project(p.ID)
	a, _ := s.CreateTask("me", "ui", TaskInput{Title: "a"})
	if c, err := s.CreateTask("me", "ui", TaskInput{Title: "b", Parent: a.ID}); err != nil || c.Parent != a.ID {
		t.Fatalf("sub-task after upgrade: %+v %v", c, err)
	}
}
