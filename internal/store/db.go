package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	turso "turso.tech/database/tursogo"
)

// DB is the one database file holding every project. Use Project(id) for a board.
type DB struct {
	db   *sql.DB
	path string
	me   string
	mu   sync.Mutex // serialises writes (and sync) so validation + write are atomic

	sdb      *turso.TursoSyncDb // nil unless Turso Cloud sync is on
	syncCfg  *SyncConfig
	syncSt   SyncStatus
	pushSoon chan struct{}

	// Notify receives every committed event; project "" means the project list changed.
	Notify func(project string, e Event)

	stMu sync.Mutex // guards seen and syncSt
	seen map[string]int64
}

type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Dir     string `json:"dir,omitempty"` // folder it was registered from (may differ per machine)
	Created int64  `json:"created"`
}

// SyncConfig turns on optional Turso Cloud sync.
type SyncConfig struct {
	URL      string // libsql://… or https://…
	Token    string
	Interval time.Duration
}

type SyncStatus struct {
	Remote   string `json:"remote"`
	LastPull int64  `json:"lastPull,omitempty"`
	LastPush int64  `json:"lastPush,omitempty"`
	Error    string `json:"error,omitempty"`
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, dir TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS columns (project TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, color TEXT NOT NULL,
		wip INTEGER NOT NULL DEFAULT 0, pos INTEGER NOT NULL, role TEXT NOT NULL DEFAULT '', PRIMARY KEY (project, id))`,
	`CREATE TABLE IF NOT EXISTS members (name TEXT PRIMARY KEY, color TEXT NOT NULL, agent INTEGER NOT NULL DEFAULT 0,
		pos INTEGER NOT NULL DEFAULT 0, icon TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS labels (project TEXT NOT NULL, name TEXT NOT NULL, color TEXT NOT NULL, pos INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (project, name))`,
	`CREATE TABLE IF NOT EXISTS tasks (project TEXT NOT NULL, id TEXT NOT NULL, title TEXT NOT NULL, descr TEXT NOT NULL DEFAULT '',
		col TEXT NOT NULL, priority TEXT NOT NULL, assignee TEXT NOT NULL DEFAULT '', due TEXT NOT NULL DEFAULT '',
		labels TEXT NOT NULL DEFAULT '[]', checklist TEXT NOT NULL DEFAULT '[]', pos REAL NOT NULL, created INTEGER NOT NULL,
		updated INTEGER NOT NULL, link TEXT NOT NULL DEFAULT '', human INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (project, id))`,
	`CREATE TABLE IF NOT EXISTS comments (id INTEGER PRIMARY KEY, project TEXT NOT NULL, task_id TEXT NOT NULL, who TEXT NOT NULL,
		text TEXT NOT NULL, t INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS comments_task ON comments(project, task_id)`,
	`CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, project TEXT NOT NULL, t INTEGER NOT NULL, who TEXT NOT NULL,
		via TEXT NOT NULL, action TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS events_project ON events(project, id)`,
}

// Open opens (or creates) the database. sc is nil for a purely local file.
func Open(path, me string, sc *SyncConfig) (*DB, error) {
	d := &DB{path: path, me: me, seen: map[string]int64{}}
	var err error
	if sc == nil {
		d.db, err = sql.Open("turso", path)
	} else {
		d.syncCfg, d.pushSoon = sc, make(chan struct{}, 1)
		d.syncSt.Remote = redactURL(sc.URL)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		d.sdb, err = turso.NewTursoSyncDb(ctx, turso.TursoSyncDbConfig{Path: path, RemoteUrl: sc.URL, AuthToken: sc.Token, ClientName: "agent-kanban"})
		if err == nil {
			d.db, err = d.sdb.Connect(ctx)
		}
		if err != nil {
			err = fmt.Errorf("turso sync with %s: %w", d.syncSt.Remote, err)
		}
	}
	if err != nil {
		return nil, err
	}
	for _, q := range schema {
		if _, err := d.db.Exec(q); err != nil {
			d.db.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	if err := d.migrate(); err != nil {
		d.db.Close()
		return nil, fmt.Errorf("upgrade %s: %w", path, err)
	}
	if err := d.ensureMembers(me); err != nil {
		d.db.Close()
		return nil, err
	}
	return d, nil
}

// migrate upgrades databases created by older versions. Each step is idempotent.
func (d *DB) migrate() error {
	// columns.role: databases from before column roles have no such column.
	if rows, err := d.db.Query(`SELECT role FROM columns LIMIT 0`); err == nil {
		rows.Close()
	} else if _, err := d.db.Exec(`ALTER TABLE columns ADD COLUMN role TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return d.addColumnRoles()
}

// addColumnRoles runs once per database: on every board without roles, the last column becomes the done
// column (what it already meant) and a Closed column is appended for tasks that won't be done.
func (d *DB) addColumnRoles() error {
	const key = "migrated:column-roles"
	var n int
	if err := d.db.QueryRow(`SELECT count(*) FROM meta WHERE key=?`, key).Scan(&n); err != nil || n > 0 {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ps, err := tx.Query(`SELECT id FROM projects`)
	if err != nil {
		return err
	}
	var pids []string
	for ps.Next() {
		var id string
		if err := ps.Scan(&id); err != nil {
			ps.Close()
			return err
		}
		pids = append(pids, id)
	}
	ps.Close()
	for _, pid := range pids {
		cols, err := getColumns(tx, pid)
		if err != nil {
			return err
		}
		if len(cols) == 0 || slices.ContainsFunc(cols, func(c Column) bool { return c.Role != "" }) {
			continue
		}
		if _, err := tx.Exec(`UPDATE columns SET role=? WHERE project=? AND id=?`, RoleDone, pid, cols[len(cols)-1].ID); err != nil {
			return err
		}
		id := RoleClosed
		for i := 2; slices.ContainsFunc(cols, func(c Column) bool { return c.ID == id }); i++ {
			id = fmt.Sprintf("%s-%d", RoleClosed, i)
		}
		if err := insertColumn(tx, pid, closedColumn(id), len(cols)); err != nil {
			return err
		}
	}
	if err := putMeta(tx, key, 1); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.nudgeSync()
	return nil
}

func closedColumn(id string) Column {
	return Column{ID: id, Name: "Closed", Color: "#7f848e", Role: RoleClosed}
}

func (d *DB) Close() error {
	if d.sdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d.sdb.Push(ctx) // best effort: don't lose the last edits on shutdown
	}
	return d.db.Close()
}

func (d *DB) Path() string { return d.path }

// Me is the human the web UI acts as (the OS user who started the server).
func (d *DB) Me() string { return d.me }

// ensureMembers seeds the default agents once and adds the local user
// (who may be new to a database synced from another machine).
func (d *DB) ensureMembers(me string) error {
	var n int
	if err := d.db.QueryRow(`SELECT count(*) FROM members`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		for i, m := range []Member{{Name: "claude", Color: "#d97757", Agent: true}, {Name: "codex", Color: "#10a37f", Agent: true}} {
			if _, err := d.db.Exec(`INSERT INTO members(name,color,agent,pos) VALUES (?,?,?,?)`, m.Name, m.Color, m.Agent, i+1); err != nil {
				return err
			}
		}
	}
	if err := d.db.QueryRow(`SELECT count(*) FROM members WHERE name=?`, me).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := d.db.Exec(`INSERT INTO members(name,color,agent,pos) VALUES (?,?,0,0)`, me, "#c678dd")
	return err
}

// redactURL keeps only scheme and host, so tokens or query strings never reach logs or the UI.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host
}

// SyncRemote is the redacted remote (scheme://host), or "" when sync is off.
func (d *DB) SyncRemote() string { return d.syncSt.Remote }

func (d *DB) syncStatus() *SyncStatus {
	if d.sdb == nil {
		return nil
	}
	d.stMu.Lock()
	defer d.stMu.Unlock()
	st := d.syncSt
	return &st
}

// RunSync pushes local changes and pulls remote ones every interval (and shortly
// after local writes) until ctx ends. Remote changes are broadcast so UIs refresh.
func (d *DB) RunSync(ctx context.Context) {
	if d.sdb == nil {
		return
	}
	tick := time.NewTicker(d.syncCfg.Interval)
	defer tick.Stop()
	for {
		d.syncOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-d.pushSoon:
			time.Sleep(time.Second) // batch bursts of edits
		}
	}
}

func (d *DB) syncOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d.mu.Lock()
	err := d.sdb.Push(ctx)
	pulled := false
	if err == nil {
		pulled, err = d.sdb.Pull(ctx)
	}
	d.mu.Unlock()
	now := time.Now().UnixMilli()
	d.stMu.Lock()
	if err != nil {
		d.syncSt.Error = err.Error() // offline is fine: local writes keep working and push later
	} else {
		d.syncSt.Error, d.syncSt.LastPush, d.syncSt.LastPull = "", now, now
	}
	d.stMu.Unlock()
	if pulled {
		d.publish("", Event{T: now, Who: "sync", Via: "sync", Action: "pulled remote changes"})
	}
}

// pullBeforeID narrows the window in which two synced machines pick the same task number.
// ponytail: still racy if both create within one sync round trip; per-machine id ranges if that ever bites.
func (d *DB) pullBeforeID() {
	if d.sdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d.mu.Lock()
	d.sdb.Pull(ctx) // offline: carry on with the local counter
	d.mu.Unlock()
}

func (d *DB) publish(project string, e Event) {
	if d.Notify != nil {
		d.Notify(project, e)
	}
}

func (d *DB) nudgeSync() {
	if d.pushSoon != nil {
		select {
		case d.pushSoon <- struct{}{}:
		default:
		}
	}
}

// Touch records that an agent just used MCP.
func (d *DB) Touch(agent string) {
	d.stMu.Lock()
	d.seen[agent] = time.Now().UnixMilli()
	d.stMu.Unlock()
}

func (d *DB) seenCopy() map[string]int64 {
	d.stMu.Lock()
	defer d.stMu.Unlock()
	out := make(map[string]int64, len(d.seen))
	for k, v := range d.seen {
		out[k] = v
	}
	return out
}

// ---------- projects ----------

func (d *DB) Projects() ([]Project, error) {
	rows, err := d.db.Query(`SELECT id,name,dir,created FROM projects ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Dir, &p.Created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Project returns the board for id.
func (d *DB) Project(id string) (*Store, error) {
	var n int
	if err := d.db.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		ps, _ := d.Projects()
		ids := make([]string, len(ps))
		for i, p := range ps {
			ids[i] = p.ID
		}
		return nil, fmt.Errorf("unknown project %q; valid: %s", id, strings.Join(ids, ", "))
	}
	return &Store{DB: d, pid: id}, nil
}

// ProjectByDir finds the project registered from dir.
func (d *DB) ProjectByDir(dir string) (Project, bool) {
	var p Project
	err := d.db.QueryRow(`SELECT id,name,dir,created FROM projects WHERE dir=?`, dir).Scan(&p.ID, &p.Name, &p.Dir, &p.Created)
	return p, err == nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// AddProject creates a project with a default board. dir may be empty.
func (d *DB) AddProject(name, dir string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, errors.New("project name is required")
	}
	if dir != "" {
		if p, ok := d.ProjectByDir(dir); ok {
			return p, nil
		}
	}
	base := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "project"
	}
	base = base[:min(len(base), 32)]
	p := Project{Name: name, Dir: dir, Created: time.Now().UnixMilli()}
	err := d.write(func(tx *sql.Tx) (string, Event, error) {
		p.ID = base
		for n := 2; ; n++ {
			var c int
			if err := tx.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, p.ID).Scan(&c); err != nil {
				return "", Event{}, err
			}
			if c == 0 {
				break
			}
			p.ID = fmt.Sprintf("%s-%d", base, n)
		}
		if _, err := tx.Exec(`INSERT INTO projects VALUES (?,?,?,?)`, p.ID, p.Name, p.Dir, p.Created); err != nil {
			return "", Event{}, err
		}
		return p.ID, Event{Who: d.me, Via: "ui", Action: "created project", Detail: p.Name}, seedProject(tx, p.ID)
	})
	if err == nil {
		d.publish("", Event{T: time.Now().UnixMilli(), Who: d.me, Via: "ui", Action: "projects changed"})
	}
	return p, err
}

func seedProject(tx *sql.Tx, pid string) error {
	cols := []Column{{ID: "todo", Name: "Todo", Color: "#61afef"}, {ID: "doing", Name: "In Progress", Color: "#d19a66", WIP: 3},
		{ID: "review", Name: "Review", Color: "#e5c07b"}, {ID: "done", Name: "Done", Color: "#98c379", Role: RoleDone}, closedColumn(RoleClosed)}
	for i, c := range cols {
		if err := insertColumn(tx, pid, c, i); err != nil {
			return err
		}
	}
	for i, l := range []Label{{Name: "bug", Color: "#e06c75"}, {Name: "feature", Color: "#61afef"}, {Name: "docs", Color: "#7f848e"}, {Name: "chore", Color: "#56b6c2"}} {
		if _, err := tx.Exec(`INSERT INTO labels VALUES (?,?,?,?)`, pid, l.Name, l.Color, i); err != nil {
			return err
		}
	}
	st := Settings{Prefix: "KB", DefaultCol: "todo",
		Perms: map[string]bool{"create": true, "edit": true, "move": true, "finish": false, "close": false, "comment": true, "delete": false}}
	return putMeta(tx, "settings:"+pid, st)
}

// RenameProject changes a project's display name. The id stays, so board URLs and MCP ?project= links keep working.
func (d *DB) RenameProject(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return errors.New("project name must be 1-64 characters")
	}
	err := d.write(func(tx *sql.Tx) (string, Event, error) {
		var old string
		if err := tx.QueryRow(`SELECT name FROM projects WHERE id=?`, id).Scan(&old); err != nil {
			return "", Event{}, fmt.Errorf("unknown project %q", id)
		}
		_, err := tx.Exec(`UPDATE projects SET name=? WHERE id=?`, name, id)
		return id, Event{Who: d.me, Via: "ui", Action: "renamed project", Detail: old + " → " + name}, err
	})
	if err == nil {
		d.publish("", Event{T: time.Now().UnixMilli(), Who: d.me, Via: "ui", Action: "projects changed"})
	}
	return err
}

// RemoveProject deletes a project and everything in it.
func (d *DB) RemoveProject(id string) error {
	err := d.write(func(tx *sql.Tx) (string, Event, error) {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
			return "", Event{}, err
		}
		if n <= 1 {
			return "", Event{}, errors.New("can't remove the last project")
		}
		for _, q := range []string{`DELETE FROM comments WHERE project=?`, `DELETE FROM tasks WHERE project=?`, `DELETE FROM columns WHERE project=?`,
			`DELETE FROM labels WHERE project=?`, `DELETE FROM events WHERE project=?`, `DELETE FROM projects WHERE id=?`} {
			if _, err := tx.Exec(q, id); err != nil {
				return "", Event{}, err
			}
		}
		_, err := tx.Exec(`DELETE FROM meta WHERE key IN (?,?)`, "settings:"+id, "seq:"+id)
		return "", Event{}, err // no event row: the project is gone
	})
	if err == nil {
		d.publish("", Event{T: time.Now().UnixMilli(), Who: d.me, Via: "ui", Action: "projects changed"})
	}
	return err
}

// write runs fn in a transaction; when fn returns a project and an event with an Action,
// the event is recorded for that project and broadcast after commit.
func (d *DB) write(fn func(tx *sql.Tx) (string, Event, error)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pid, e, err := fn(tx)
	if err != nil {
		return err
	}
	if e.Action != "" {
		e.T = time.Now().UnixMilli()
		res, err := tx.Exec(`INSERT INTO events(project,t,who,via,action,task_id,detail) VALUES (?,?,?,?,?,?,?)`, pid, e.T, e.Who, e.Via, e.Action, e.TaskID, e.Detail)
		if err != nil {
			return err
		}
		e.ID, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if e.Action != "" {
		d.publish(pid, e)
	}
	d.nudgeSync()
	return nil
}

// ---------- search across every project ----------

type Hit struct {
	Project     string   `json:"project"`
	ProjectName string   `json:"projectName"`
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Column      string   `json:"column"`
	ColumnName  string   `json:"columnName"`
	Priority    string   `json:"priority"`
	Assignee    string   `json:"assignee,omitempty"`
	Labels      []string `json:"labels"`
	Human       bool     `json:"human,omitempty"`
	Updated     int64    `json:"updated"`
	Where       string   `json:"where,omitempty"`   // field the best text match was in
	Snippet     string   `json:"snippet,omitempty"` // context around a description/comment match
	score       int
}

// query is a parsed search: free text terms plus field filters.
type query struct {
	terms                                  []string
	label, who, col, prio, project, status []string
}

// parseQuery understands label:x  #x  @who  assignee:x  col:x  prio:x  p:x  project:x  is:open|done|closed|human|agent|unassigned|overdue
func parseQuery(q string) query {
	var out query
	for _, f := range strings.Fields(strings.ToLower(q)) {
		k, v, ok := strings.Cut(f, ":")
		switch {
		case strings.HasPrefix(f, "@") && len(f) > 1:
			out.who = append(out.who, f[1:])
		case strings.HasPrefix(f, "#") && len(f) > 1:
			out.label = append(out.label, f[1:])
		case ok && v != "" && (k == "label" || k == "l"):
			out.label = append(out.label, v)
		case ok && v != "" && (k == "assignee" || k == "a"):
			out.who = append(out.who, v)
		case ok && v != "" && (k == "col" || k == "column" || k == "status" || k == "in"):
			out.col = append(out.col, v)
		case ok && v != "" && (k == "prio" || k == "priority" || k == "p"):
			out.prio = append(out.prio, v)
		case ok && v != "" && (k == "project" || k == "proj"):
			out.project = append(out.project, v)
		case ok && v != "" && k == "is":
			out.status = append(out.status, v)
		default:
			out.terms = append(out.terms, f)
		}
	}
	return out
}

func anyPrefix(vals []string, fn func(string) bool) bool {
	if len(vals) == 0 {
		return true
	}
	return slices.ContainsFunc(vals, fn)
}

func snippet(text, term string) string {
	lt := strings.ToLower(text)
	i := strings.Index(lt, term)
	if i < 0 {
		return ""
	}
	start, end := max(0, i-50), min(len(text), i+len(term)+70)
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}

// Search ranks tasks in every project. An empty query returns recently updated tasks.
// ponytail: scans all tasks in memory; fine for thousands, add FTS if boards get huge.
func (d *DB) Search(raw string, limit int) ([]Hit, error) {
	q := parseQuery(raw)
	projs, err := d.Projects()
	if err != nil {
		return nil, err
	}
	members, err := getMembers(d.db)
	if err != nil {
		return nil, err
	}
	hits := []Hit{}
	for _, p := range projs {
		if !anyPrefix(q.project, func(v string) bool {
			return strings.HasPrefix(p.ID, v) || strings.HasPrefix(strings.ToLower(p.Name), v)
		}) {
			continue
		}
		cols, err := getColumns(d.db, p.ID)
		if err != nil {
			return nil, err
		}
		tasks, err := searchTasks(d.db, p.ID, len(q.terms) > 0)
		if err != nil {
			return nil, err
		}
		fin := Finished(cols)
		done, _ := DoneColumn(cols)
		closed, _ := ClosedColumn(cols)
		colName := map[string]string{}
		for _, c := range cols {
			colName[c.ID] = c.Name
		}
		for _, t := range tasks {
			cname := strings.ToLower(colName[t.Column])
			if !anyPrefix(q.label, func(v string) bool {
				return slices.ContainsFunc(t.Labels, func(l string) bool { return strings.HasPrefix(strings.ToLower(l), v) })
			}) || !anyPrefix(q.who, func(v string) bool {
				return v == "none" && t.Assignee == "" || t.Assignee != "" && strings.HasPrefix(t.Assignee, v)
			}) ||
				!anyPrefix(q.col, func(v string) bool { return strings.HasPrefix(t.Column, v) || strings.HasPrefix(cname, v) }) ||
				!anyPrefix(q.prio, func(v string) bool { return strings.HasPrefix(t.Priority, v) }) ||
				!anyPrefix(q.status, func(v string) bool {
					switch v {
					case "open":
						return !fin[t.Column]
					case "done":
						return t.Column == done.ID
					case "closed":
						return closed.ID != "" && t.Column == closed.ID
					case "human":
						return t.Human
					case "agent":
						return slices.ContainsFunc(members, func(m Member) bool { return m.Name == t.Assignee && m.Agent })
					case "unassigned":
						return t.Assignee == ""
					case "overdue":
						return t.Due != "" && !fin[t.Column] && t.Due < time.Now().Format("2006-01-02")
					}
					return false
				}) {
				continue
			}
			h := Hit{Project: p.ID, ProjectName: p.Name, ID: t.ID, Title: t.Title, Column: t.Column, ColumnName: colName[t.Column],
				Priority: t.Priority, Assignee: t.Assignee, Labels: t.Labels, Human: t.Human, Updated: t.Updated}
			matched := true
			for _, term := range q.terms {
				id, title, desc := strings.ToLower(t.ID), strings.ToLower(t.Title), strings.ToLower(t.Description)
				best := 0
				switch {
				case id == term:
					best, h.Where = 100, "id"
				case strings.HasPrefix(id, term):
					best = 40
				}
				if strings.Contains(title, term) {
					sc := 12
					if strings.HasPrefix(title, term) || strings.Contains(title, " "+term) {
						sc = 18 // word start
					}
					best = max(best, sc)
				}
				if slices.ContainsFunc(t.Labels, func(l string) bool { return strings.Contains(strings.ToLower(l), term) }) {
					best = max(best, 6)
				}
				if best == 0 && strings.Contains(desc, term) {
					best, h.Where, h.Snippet = 4, "description", snippet(t.Description, term)
				}
				if best == 0 {
					for _, c := range t.Comments {
						if strings.Contains(strings.ToLower(c.Text), term) {
							best, h.Where, h.Snippet = 2, "comment by "+c.Who, snippet(c.Text, term)
							break
						}
					}
				}
				if best == 0 {
					matched = false
					break
				}
				h.score += best
			}
			if !matched {
				continue
			}
			if !fin[t.Column] {
				h.score++ // open work first on ties
			}
			hits = append(hits, h)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].Updated > hits[j].Updated
	})
	return hits[:min(len(hits), limit)], nil
}

// searchTasks loads only the task fields Search reads, in board order.
// Description and comments are only needed (and loaded) when there are text terms.
func searchTasks(q querier, pid string, text bool) ([]Task, error) {
	descr := "''"
	if text {
		descr = "descr"
	}
	rows, err := q.Query(`SELECT id,title,`+descr+`,col,priority,assignee,due,labels,updated,human FROM tasks WHERE project=? ORDER BY pos`, pid)
	if err != nil {
		return nil, err
	}
	var out []Task
	idx := map[string]int{}
	for rows.Next() {
		var t Task
		var labels string
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.Column, &t.Priority, &t.Assignee, &t.Due, &labels, &t.Updated, &t.Human); err != nil {
			rows.Close()
			return nil, err
		}
		json.Unmarshal([]byte(labels), &t.Labels)
		if t.Labels == nil {
			t.Labels = []string{}
		}
		idx[t.ID] = len(out)
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil || !text {
		return out, err
	}
	crows, err := q.Query(`SELECT task_id,who,text FROM comments WHERE project=? ORDER BY id`, pid)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c Comment
		var tid string
		if err := crows.Scan(&tid, &c.Who, &c.Text); err != nil {
			return nil, err
		}
		if i, ok := idx[tid]; ok {
			out[i].Comments = append(out[i].Comments, c)
		}
	}
	return out, crows.Err()
}

func putMeta(q querier, key string, v any) error {
	b, _ := json.Marshal(v)
	_, err := q.Exec(`INSERT INTO meta(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return err
}
