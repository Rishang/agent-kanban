package store

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderTicketRoundTrip(t *testing.T) {
	desc := "Intro: with colons, # not a comment\n\n---\n\n```go\nfunc x() {}\n```\n\ttabbed line   \nlast line  "
	f := TicketFile{
		ID: "KB-1", Title: "Fix: the \"thing\" #1", Status: TicketOpen, Column: "QA", Priority: "high",
		Labels: []string{"a", "b"}, Created: "2026-11-28T10:00:00Z", Description: fileText(desc),
		Subtasks:  []TicketSubtask{{"KB-2", "child", TicketDone}},
		Plan:      &TicketPlan{"approved", 3},
		Checklist: []TicketCheck{{"one: two", true}},
		Comments:  []TicketComment{{"claude", "2026-11-28T10:05:00Z", "line one\nline two"}},
	}
	b, err := RenderTicket(f)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.HasPrefix(out, ticketHeader+"\n") {
		t.Fatalf("no header:\n%s", out)
	}
	if !strings.Contains(out, "description: |") || !strings.Contains(out, "    text: |") || !strings.Contains(out, "labels: [a, b]") {
		t.Fatalf("multi-line text should be literal blocks, labels on one line:\n%s", out)
	}
	got, err := ParseTicket(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip:\n got %+v\nwant %+v\n%s", got, f, out)
	}
	if got.Description != "Intro: with colons, # not a comment\n\n---\n\n```go\nfunc x() {}\n```\n\ttabbed line\nlast line" {
		t.Fatalf("description: %q", got.Description)
	}
	b2, _ := RenderTicket(f)
	if !bytes.Equal(b, b2) {
		t.Fatal("same ticket rendered differently twice")
	}
	// key order is fixed: id first, comments last
	if i, j := strings.Index(out, "\nid:"), strings.Index(out, "\ncomments:"); i < 0 || j < i || !strings.Contains(out, "column: QA") {
		t.Fatalf("key order:\n%s", out)
	}
}

// ticketStorage = files: open tickets are YAML files kept in step with the board; off writes nothing.
func TestTicketFiles(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	p, _ := db.AddProject("Tix", repo)
	s, _ := db.Project(p.ID)
	tdir := filepath.Join(repo, TicketDir)
	read := func(id string) (TicketFile, bool) {
		b, err := os.ReadFile(filepath.Join(tdir, id+".yml"))
		if err != nil {
			return TicketFile{}, false
		}
		f, err := ParseTicket(b)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return f, true
	}

	// off by default: no folder at all
	a, _ := s.CreateTask("me", "ui", TaskInput{Title: "Parent", Checklist: []string{"step"}})
	s.AddComment("me", "ui", CommentInput{ID: a.ID, Text: "hi"})
	if _, err := os.Stat(tdir); !os.IsNotExist(err) {
		t.Fatalf("ticket folder written with the setting off: %v", err)
	}

	// switching on backfills existing tickets
	st, _ := getSettings(s.db, s.pid)
	st.TicketStorage = TicketStorageFiles
	if err := s.SaveSettings("me", st); err != nil {
		t.Fatal(err)
	}
	f, ok := read(a.ID)
	if !ok || f.Title != "Parent" || f.Status != TicketOpen || len(f.Comments) != 1 || len(f.Checklist) != 1 {
		t.Fatalf("backfill: %v %+v", ok, f)
	}

	// a sub-task gets its own file and shows in the parent's list
	c, _ := s.CreateTask("me", "ui", TaskInput{Title: "Child", Parent: a.ID})
	if f, _ = read(a.ID); len(f.Subtasks) != 1 || f.Subtasks[0].ID != c.ID {
		t.Fatalf("parent list: %+v", f.Subtasks)
	}
	if cf, ok := read(c.ID); !ok || cf.Parent != a.ID {
		t.Fatalf("child file: %v %+v", ok, cf)
	}

	// finishing the sub-task removes its file; the parent keeps it as done
	b, _ := s.Meta()
	done, _ := DoneColumn(b.Columns)
	if _, err := s.MoveTask("me", "ui", MoveInput{ID: c.ID, Column: done.ID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := read(c.ID); ok {
		t.Fatal("done sub-task still has a file")
	}
	if f, _ = read(a.ID); len(f.Subtasks) != 1 || f.Subtasks[0].Status != TicketDone {
		t.Fatalf("parent list after done: %+v", f.Subtasks)
	}

	// re-parenting moves the sub-task between lists
	o, _ := s.CreateTask("me", "ui", TaskInput{Title: "Other"})
	d, _ := s.CreateTask("me", "ui", TaskInput{Title: "Moving", Parent: a.ID})
	np := o.ID
	if _, err := s.UpdateTask("me", "ui", TaskPatch{ID: d.ID, Parent: &np}); err != nil {
		t.Fatal(err)
	}
	if f, _ = read(a.ID); len(f.Subtasks) != 1 {
		t.Fatalf("old parent still lists the moved sub-task: %+v", f.Subtasks)
	}
	if of, _ := read(o.ID); len(of.Subtasks) != 1 || of.Subtasks[0].ID != d.ID {
		t.Fatalf("new parent list: %+v", of.Subtasks)
	}

	// files that aren't generated are never removed; deleting a parent removes its sub-task files
	os.WriteFile(filepath.Join(tdir, "notes.yml"), []byte("mine: true\n"), 0o644)
	if err := s.DeleteTask("me", "ui", o.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := read(o.ID); ok {
		t.Fatal("deleted ticket still has a file")
	}
	if _, ok := read(d.ID); ok {
		t.Fatal("sub-task of a deleted parent still has a file")
	}
	if _, err := os.Stat(filepath.Join(tdir, "notes.yml")); err != nil {
		t.Fatal("a hand-written file was removed")
	}

	// a hand edit is overwritten on the next change (read-only export)
	os.WriteFile(filepath.Join(tdir, a.ID+".yml"), []byte(ticketHeader+"\ntitle: edited\n"), 0o644)
	s.AddComment("me", "ui", CommentInput{ID: a.ID, Text: "again"})
	if f, _ = read(a.ID); f.Title != "Parent" || len(f.Comments) != 2 {
		t.Fatalf("not re-exported: %+v", f)
	}

	// switching off stops the export and leaves the files
	st.TicketStorage = ""
	s.SaveSettings("me", st)
	s.AddComment("me", "ui", CommentInput{ID: a.ID, Text: "third"})
	if f, ok = read(a.ID); !ok || len(f.Comments) != 2 {
		t.Fatalf("off: file should stay as it was: %v %+v", ok, f)
	}
}
