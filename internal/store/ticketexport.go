package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// exportTickets brings a project's ticket files in line with its board when ticketStorage = files: every
// open ticket has TicketDir/<id>.yml, and generated files of tickets that are done, closed or gone are removed.
// It runs after every write to the project (DB.write) and after a sync pull, rewriting only files whose bytes
// changed, so a re-parented sub-task, a renamed column or a deleted parent all land without per-event cases.
// Failures are logged, never returned: the board write already succeeded.
// ponytail: renders every ticket on each write; fine for hundreds of tickets, track dirty ids if boards get huge.
func (d *DB) exportTickets(pid string) {
	if err := d.syncTicketFiles(pid); err != nil {
		log.Printf("ticket files (%s): %v", pid, err)
	}
}

// exportAllTickets runs exportTickets for every project.
func (d *DB) exportAllTickets() {
	ps, err := d.Projects()
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range ps {
		d.exportTickets(p.ID)
	}
}

func (d *DB) syncTicketFiles(pid string) error {
	var dir string
	if d.db.QueryRow(`SELECT dir FROM projects WHERE id=?`, pid).Scan(&dir) != nil || dir == "" {
		return nil // no folder on disk: nothing to export, as with plan files
	}
	st, err := getSettings(d.db, pid)
	if err != nil || st.TicketStorage != TicketStorageFiles {
		return nil
	}
	cols, err := getColumns(d.db, pid)
	if err != nil {
		return err
	}
	tasks, err := getTasks(d.db, pid, true)
	if err != nil {
		return err
	}
	finished := Finished(cols)
	subs := map[string][]Task{}
	for _, t := range tasks {
		if t.Parent != "" {
			subs[t.Parent] = append(subs[t.Parent], t)
		}
	}
	want := map[string][]byte{}
	for _, t := range tasks {
		if finished[t.Column] {
			continue
		}
		b, err := RenderTicket(ticketFileOf(t, cols, subs[t.ID]))
		if err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
		want[t.ID+".yml"] = b
	}
	tdir, _, err := planFile(dir, TicketDir) // resolved inside the project folder, symlinks included
	if err != nil {
		return err
	}
	if len(want) > 0 {
		if err := os.MkdirAll(tdir, 0o755); err != nil {
			return err
		}
	}
	var errs []error
	for name, b := range want {
		errs = append(errs, writeTicketFile(filepath.Join(tdir, name), b))
	}
	entries, err := os.ReadDir(tdir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if _, keep := want[name]; keep || !e.Type().IsRegular() || !strings.HasSuffix(name, ".yml") {
			continue
		}
		p := filepath.Join(tdir, name)
		if generatedTicket(p) {
			errs = append(errs, os.Remove(p))
		}
	}
	return errors.Join(errs...)
}

// generatedTicket reports whether the file starts with the export's header: other files are never removed.
func generatedTicket(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(ticketHeader))
	n, _ := f.Read(head)
	return string(head[:n]) == ticketHeader
}

// writeTicketFile writes b to path through a temp file and a rename, unless the file already holds b.
// A symlink at path is refused, like plan files.
func writeTicketFile(path string, b []byte) error {
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
			return nil
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ticket-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	os.Chmod(tmp, 0o644)
	return os.Rename(tmp, path)
}
