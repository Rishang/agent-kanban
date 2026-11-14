package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A database from before column roles: no columns.role, last column means done.
func TestMigrateColumnRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, dir TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL)`,
		`CREATE TABLE columns (project TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, color TEXT NOT NULL,
			wip INTEGER NOT NULL DEFAULT 0, pos INTEGER NOT NULL, PRIMARY KEY (project, id))`,
		`INSERT INTO projects VALUES ('app','App','',1), ('odd','Odd','',2)`,
		`INSERT INTO columns VALUES ('app','todo','Todo','#fff',0,0), ('app','shipped','Shipped','#fff',0,1)`,
		// a board that already uses the id "closed" for something else
		`INSERT INTO columns VALUES ('odd','todo','Todo','#fff',0,0), ('odd','closed','Closed sale','#fff',0,1), ('odd','won','Won','#fff',0,2)`,
		`INSERT INTO meta VALUES ('settings:app','{"prefix":"KB"}'), ('settings:odd','{"prefix":"KB"}')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	for range 2 { // second open must not add another Closed column
		db, err := Open(path, "me", nil)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := db.Project("app")
		b, err := s.Meta()
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, c := range b.Columns {
			got = append(got, c.ID+":"+c.Role)
		}
		if strings.Join(got, ",") != "todo:,shipped:done,closed:closed" {
			t.Fatalf("app columns after upgrade: %v", got)
		}
		o, _ := db.Project("odd")
		ob, _ := o.Meta()
		if n := len(ob.Columns); n != 4 || ob.Columns[1].Role != "" || ob.Columns[2].Role != RoleDone || ob.Columns[3].ID != "closed-2" {
			t.Fatalf("odd columns after upgrade: %+v", ob.Columns)
		}
		db.Close()
	}
}

func TestColumnRoles(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("Roles", "")
	s, _ := db.Project(p.ID)
	b, _ := s.Meta()
	if d, _ := DoneColumn(b.Columns); d.ID != "done" {
		t.Fatalf("new board done column: %+v", b.Columns)
	}
	if c, ok := ClosedColumn(b.Columns); !ok || c.ID != "closed" || b.Columns[len(b.Columns)-1].ID != "closed" {
		t.Fatalf("new board closed column: %+v", b.Columns)
	}
	if b.Settings.Perms["close"] {
		t.Fatal("close permission should be off by default")
	}

	a, _ := s.CreateTask("me", "ui", TaskInput{Title: "open"})
	d, _ := s.CreateTask("me", "ui", TaskInput{Title: "shipped"})
	c, _ := s.CreateTask("me", "ui", TaskInput{Title: "wontfix"})
	s.MoveTask("me", "ui", MoveInput{ID: d.ID, Column: "done"})
	s.MoveTask("me", "ui", MoveInput{ID: c.ID, Column: "closed"})
	if open, total, err := s.Counts(); err != nil || open != 1 || total != 3 {
		t.Fatalf("counts = %d/%d %v, want 1/3", open, total, err)
	}
	for q, want := range map[string]string{"is:open": a.ID, "is:done": d.ID, "is:closed": c.ID} {
		hits, err := db.Search(q, 10)
		if err != nil || len(hits) != 1 || hits[0].ID != want {
			t.Errorf("Search(%q) = %+v %v, want %s", q, hits, err, want)
		}
	}

	// roles: one of each, known values only; reordering keeps them
	cols := b.Columns
	bad := append([]Column{}, cols...)
	bad[0].Role = RoleDone
	if err := s.SaveColumns("me", bad); err == nil || !strings.Contains(err.Error(), "only one done") {
		t.Fatalf("two done columns: %v", err)
	}
	bad = append([]Column{}, cols...)
	bad[0].Role = "archived"
	if err := s.SaveColumns("me", bad); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("unknown role: %v", err)
	}
	moved := append([]Column{cols[len(cols)-1]}, cols[:len(cols)-1]...) // Closed first
	if err := s.SaveColumns("me", moved); err != nil {
		t.Fatal(err)
	}
	if b2, _ := s.Meta(); b2.Columns[0].Role != RoleClosed || b2.Columns[len(b2.Columns)-1].Role != RoleDone {
		t.Fatalf("roles lost on reorder: %+v", b2.Columns)
	}
	if open, _, _ := s.Counts(); open != 1 {
		t.Fatalf("open after reorder = %d", open)
	}

	// export/import keeps roles
	full, _ := s.Board()
	if err := s.Import("me", full); err != nil {
		t.Fatal(err)
	}
	if c, ok := ClosedColumn(func() []Column { b, _ := s.Meta(); return b.Columns }()); !ok || c.ID != "closed" {
		t.Fatal("import dropped the closed role")
	}
}

func TestDoneColumnFallback(t *testing.T) {
	// boards without roles: the last column that isn't closed is done
	cols := []Column{{ID: "a"}, {ID: "b"}, {ID: "x", Role: RoleClosed}}
	if d, _ := DoneColumn(cols); d.ID != "b" {
		t.Fatalf("fallback done = %s", d.ID)
	}
	if f := Finished(cols); !f["b"] || !f["x"] || f["a"] {
		t.Fatalf("finished = %v", f)
	}
}
