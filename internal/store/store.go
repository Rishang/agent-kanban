package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var Priorities = []string{"urgent", "high", "medium", "low"}

// Perms an agent may be granted; keys match Settings.Perms.
var PermNames = map[string]string{
	"create": "create tasks", "edit": "edit tasks", "move": "move tasks between columns",
	"finish": "move tasks to the last column", "comment": "comment", "delete": "delete tasks",
}

type Settings struct {
	Board           string          `json:"board"` // the project name (stored in projects, mirrored here for the UI)
	Prefix          string          `json:"prefix"`
	DefaultCol      string          `json:"defaultCol"`
	DefaultAssignee string          `json:"defaultAssignee"`
	ArchiveDays     int             `json:"archiveDays"`
	Me              string          `json:"me"` // per process (OS user), never stored: synced machines differ
	Perms           map[string]bool `json:"perms"`
}

type Column struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	WIP   int    `json:"wip,omitempty"`
}

type Member struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Agent bool   `json:"agent,omitempty"`
	Icon  string `json:"icon,omitempty"` // UI glyph key (user, bot, claude, codex, …); empty = by name
}

type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Tasks int    `json:"tasks"`
}

type Check struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

type Comment struct {
	ID   int64  `json:"id"`
	Who  string `json:"who"`
	Text string `json:"text"`
	T    int64  `json:"t"`
}

type Task struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Column      string    `json:"column"`
	Priority    string    `json:"priority"`
	Assignee    string    `json:"assignee,omitempty"`
	Due         string    `json:"due,omitempty"`
	Link        string    `json:"link,omitempty"`
	Human       bool      `json:"human,omitempty"`
	Labels      []string  `json:"labels"`
	Checklist   []Check   `json:"checklist"`
	Comments    []Comment `json:"comments"`
	URL         string    `json:"url,omitempty"` // deep link, filled in for MCP responses
	Pos         float64   `json:"pos"`
	Created     int64     `json:"created"`
	Updated     int64     `json:"updated"`
}

type Event struct {
	ID     int64  `json:"id"`
	T      int64  `json:"t"`
	Who    string `json:"who"`
	Via    string `json:"via"`
	Action string `json:"action"`
	TaskID string `json:"taskId,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Board struct {
	Project    Project          `json:"project"`
	Settings   Settings         `json:"settings"`
	Columns    []Column         `json:"columns"`
	Members    []Member         `json:"members"`
	Labels     []Label          `json:"labels"`
	Priorities []string         `json:"priorities"`
	Tasks      []Task           `json:"tasks"`
	Events     []Event          `json:"events"`
	Seen       map[string]int64 `json:"seen"` // agent -> last MCP call (unix ms)
	DB         string           `json:"db"`
	Sync       *SyncStatus      `json:"sync,omitempty"`
}

// Inputs shared by the web API and MCP tools.
type TaskInput struct {
	Title       string   `json:"title" jsonschema:"short task title"`
	Column      string   `json:"column,omitempty" jsonschema:"column id from get_board; defaults to the board's default column"`
	Description string   `json:"description,omitempty" jsonschema:"markdown body"`
	Priority    string   `json:"priority,omitempty" jsonschema:"one of urgent, high, medium, low (default medium)"`
	Assignee    string   `json:"assignee,omitempty" jsonschema:"member name from get_board"`
	Due         string   `json:"due,omitempty" jsonschema:"due date YYYY-MM-DD"`
	Link        string   `json:"link,omitempty" jsonschema:"related http(s) URL: PR, issue, doc"`
	Human       bool     `json:"-"` // humans only: set via the UI's set_human op
	Labels      []string `json:"labels,omitempty" jsonschema:"existing label names from get_board"`
	Checklist   []string `json:"checklist,omitempty" jsonschema:"checklist item texts"`
}

type TaskPatch struct {
	ID          string    `json:"id" jsonschema:"task id, e.g. KB-12"`
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty" jsonschema:"markdown body (replaces the old one)"`
	Priority    *string   `json:"priority,omitempty" jsonschema:"urgent, high, medium or low"`
	Assignee    *string   `json:"assignee,omitempty" jsonschema:"member name, or empty string to unassign"`
	Due         *string   `json:"due,omitempty" jsonschema:"YYYY-MM-DD, or empty string to clear"`
	Link        *string   `json:"link,omitempty" jsonschema:"related http(s) URL, or empty string to clear"`
	Human       *bool     `json:"-"` // humans only: set via the UI's set_human op
	Labels      *[]string `json:"labels,omitempty" jsonschema:"full label list (replaces the old one)"`
	Checklist   *[]Check  `json:"checklist,omitempty" jsonschema:"full checklist (replaces the old one)"`
}

type MoveInput struct {
	ID     string `json:"id" jsonschema:"task id"`
	Column string `json:"column" jsonschema:"target column id"`
	Before string `json:"before,omitempty" jsonschema:"place before this task id; omit to append at the bottom"`
}

type CommentInput struct {
	ID   string `json:"id" jsonschema:"task id"`
	Text string `json:"text" jsonschema:"markdown comment"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"task id"`
}

type RenameInput struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Store is one project's board inside the shared DB.
type Store struct {
	*DB
	pid string
}

func (s *Store) ProjectID() string { return s.pid }

// ---------- reads ----------

type querier interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func getSettings(q querier, pid string) (Settings, error) {
	var raw string
	var st Settings
	if err := q.QueryRow(`SELECT value FROM meta WHERE key=?`, "settings:"+pid).Scan(&raw); err != nil {
		return st, err
	}
	err := json.Unmarshal([]byte(raw), &st)
	if st.Perms == nil {
		st.Perms = map[string]bool{}
	}
	if q.QueryRow(`SELECT name FROM projects WHERE id=?`, pid).Scan(&st.Board) != nil {
		st.Board = pid
	}
	return st, err
}

func getColumns(q querier, pid string) ([]Column, error) {
	rows, err := q.Query(`SELECT id,name,color,wip FROM columns WHERE project=? ORDER BY pos`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &c.WIP); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func getMembers(q querier) ([]Member, error) {
	rows, err := q.Query(`SELECT name,color,agent,icon FROM members ORDER BY pos, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Name, &m.Color, &m.Agent, &m.Icon); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func getLabels(q querier, pid string, tasks []Task) ([]Label, error) {
	rows, err := q.Query(`SELECT name,color FROM labels WHERE project=? ORDER BY pos, name`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.Name, &l.Color); err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if slices.Contains(t.Labels, l.Name) {
				l.Tasks++
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const taskCols = `id,title,descr,col,priority,assignee,due,labels,checklist,pos,created,updated,link,human`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var labels, checklist string
	err := sc.Scan(&t.ID, &t.Title, &t.Description, &t.Column, &t.Priority, &t.Assignee, &t.Due, &labels, &checklist, &t.Pos, &t.Created, &t.Updated, &t.Link, &t.Human)
	if err != nil {
		return t, err
	}
	json.Unmarshal([]byte(labels), &t.Labels)
	json.Unmarshal([]byte(checklist), &t.Checklist)
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Checklist == nil {
		t.Checklist = []Check{}
	}
	t.Comments = []Comment{}
	return t, nil
}

// getTasks loads the project's tasks in position order; Comments stays empty unless comments is set.
func getTasks(q querier, pid string, comments bool) ([]Task, error) {
	rows, err := q.Query(`SELECT `+taskCols+` FROM tasks WHERE project=? ORDER BY pos`, pid)
	if err != nil {
		return nil, err
	}
	out := []Task{} // never null in JSON: the UI maps over it
	idx := map[string]int{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		idx[t.ID] = len(out)
		out = append(out, t)
	}
	rows.Close()
	if !comments {
		return out, nil
	}
	crows, err := q.Query(`SELECT id,task_id,who,text,t FROM comments WHERE project=? ORDER BY id`, pid)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c Comment
		var tid string
		if err := crows.Scan(&c.ID, &tid, &c.Who, &c.Text, &c.T); err != nil {
			return nil, err
		}
		if i, ok := idx[tid]; ok {
			out[i].Comments = append(out[i].Comments, c)
		}
	}
	return out, crows.Err()
}

func getTask(q querier, pid, id string) (Task, error) {
	t, err := scanTask(q.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE project=? AND id=?`, pid, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("task %q not found", id)
	}
	if err != nil {
		return t, err
	}
	rows, err := q.Query(`SELECT id,who,text,t FROM comments WHERE project=? AND task_id=? ORDER BY id`, pid, id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Who, &c.Text, &c.T); err != nil {
			return t, err
		}
		t.Comments = append(t.Comments, c)
	}
	return t, rows.Err()
}

// taskLabels loads only each task's id and labels (for label counts and renames).
func taskLabels(q querier, pid string) ([]Task, error) {
	rows, err := q.Query(`SELECT id,labels FROM tasks WHERE project=? ORDER BY pos`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var labels string
		if err := rows.Scan(&t.ID, &labels); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(labels), &t.Labels)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Board is everything the UI shows for this project.
func (s *Store) Board() (Board, error) { return s.board(true) }

// Meta is Board without tasks, events, seen and sync (label counts are still filled):
// what MCP needs to describe the board and check permissions, without reading every task and comment.
func (s *Store) Meta() (Board, error) { return s.board(false) }

func (s *Store) board(full bool) (Board, error) {
	var b Board
	var err error
	if err = s.db.QueryRow(`SELECT id,name,dir,created FROM projects WHERE id=?`, s.pid).Scan(&b.Project.ID, &b.Project.Name, &b.Project.Dir, &b.Project.Created); err != nil {
		return b, fmt.Errorf("project %q: %w", s.pid, err)
	}
	if b.Settings, err = getSettings(s.db, s.pid); err != nil {
		return b, err
	}
	b.Settings.Me = s.me
	if b.Columns, err = getColumns(s.db, s.pid); err != nil {
		return b, err
	}
	if b.Members, err = getMembers(s.db); err != nil {
		return b, err
	}
	b.Priorities, b.DB = Priorities, s.path
	if !full {
		ts, err := taskLabels(s.db, s.pid)
		if err != nil {
			return b, err
		}
		b.Labels, err = getLabels(s.db, s.pid, ts)
		return b, err
	}
	if b.Tasks, err = getTasks(s.db, s.pid, true); err != nil {
		return b, err
	}
	if b.Labels, err = getLabels(s.db, s.pid, b.Tasks); err != nil {
		return b, err
	}
	if b.Events, err = s.Events(60); err != nil {
		return b, err
	}
	b.Seen, b.Sync = s.seenCopy(), s.syncStatus()
	return b, nil
}

// Tasks returns the tasks in board order without comments (Comments is empty); see CommentCounts.
func (s *Store) Tasks() ([]Task, error) { return getTasks(s.db, s.pid, false) }

// CommentCounts maps task id to its number of comments.
func (s *Store) CommentCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT task_id,count(*) FROM comments WHERE project=? GROUP BY task_id`, s.pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Counts returns how many tasks are open (not in the last column) and in total; 0, 0 without columns.
func (s *Store) Counts() (open, total int, err error) {
	cols, err := getColumns(s.db, s.pid)
	if err != nil || len(cols) == 0 {
		return 0, 0, err
	}
	err = s.db.QueryRow(`SELECT count(*),coalesce(sum(col<>?),0) FROM tasks WHERE project=?`, cols[len(cols)-1].ID, s.pid).Scan(&total, &open)
	return open, total, err
}

func (s *Store) Task(id string) (Task, error) { return getTask(s.db, s.pid, id) }

func (s *Store) Events(limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id,t,who,via,action,task_id,detail FROM events WHERE project=? ORDER BY id DESC LIMIT ?`, s.pid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.T, &e.Who, &e.Via, &e.Action, &e.TaskID, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Archived reports whether a task is hidden by the auto-archive setting.
func Archived(t Task, st Settings, lastCol string) bool {
	return st.ArchiveDays > 0 && t.Column == lastCol && time.Since(time.UnixMilli(t.Updated)) > time.Duration(st.ArchiveDays)*24*time.Hour
}

// ---------- writes ----------

// write runs fn in a transaction for this project, records its event and broadcasts it.
func (s *Store) write(who, via string, fn func(tx *sql.Tx) (Event, error)) error {
	return s.DB.write(func(tx *sql.Tx) (string, Event, error) {
		e, err := fn(tx)
		e.Who, e.Via = who, via
		return s.pid, e, err
	})
}

// LogBlocked records a refused agent action so humans see it in Activity.
func (s *Store) LogBlocked(who, perm, taskID string) {
	s.write(who, "mcp", func(*sql.Tx) (Event, error) {
		return Event{Action: "was blocked from", Detail: PermNames[perm], TaskID: taskID}, nil
	})
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type domain struct {
	st      Settings
	cols    []Column
	members []Member
	labels  map[string]bool
}

func (s *Store) loadDomain(q querier) (d domain, err error) {
	if d.st, err = getSettings(q, s.pid); err != nil {
		return
	}
	if d.cols, err = getColumns(q, s.pid); err != nil {
		return
	}
	if d.members, err = getMembers(q); err != nil {
		return
	}
	ls, err := getLabels(q, s.pid, nil)
	d.labels = map[string]bool{}
	for _, l := range ls {
		d.labels[l.Name] = true
	}
	return
}

func (d domain) colIDs() []string {
	var ids []string
	for _, c := range d.cols {
		ids = append(ids, c.ID)
	}
	return ids
}

// Validation errors list the valid values so agents can self-correct.
func (d domain) checkCol(id string) error {
	if !slices.Contains(d.colIDs(), id) {
		return fmt.Errorf("unknown column %q; valid: %s", id, strings.Join(d.colIDs(), ", "))
	}
	return nil
}

func (d domain) checkAssignee(name string) error {
	if name == "" {
		return nil
	}
	var names []string
	for _, m := range d.members {
		if m.Name == name {
			return nil
		}
		names = append(names, m.Name)
	}
	return fmt.Errorf("unknown assignee %q; valid: %s", name, strings.Join(names, ", "))
}

func (d domain) isAgent(name string) bool {
	return slices.ContainsFunc(d.members, func(m Member) bool { return m.Name == name && m.Agent })
}

func (d domain) checkLabels(ls []string) error {
	for _, l := range ls {
		if !d.labels[l] {
			var names []string
			for n := range d.labels {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("unknown label %q; valid: %s (ask a human to add new labels in Settings)", l, strings.Join(names, ", "))
		}
	}
	return nil
}

func checkPriority(p string) error {
	if !slices.Contains(Priorities, p) {
		return fmt.Errorf("unknown priority %q; valid: %s", p, strings.Join(Priorities, ", "))
	}
	return nil
}

// checkLink only allows http(s) so a link can never become javascript: in the UI.
func checkLink(link string) error {
	if link == "" {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("link must be an http(s) URL, got %q", link)
	}
	return nil
}

func checkDue(due string) error {
	if due != "" && !dateRe.MatchString(due) {
		return fmt.Errorf("due must be YYYY-MM-DD, got %q", due)
	}
	return nil
}

// newPos returns a sort position in col placed before task `before` (or at the end).
func (s *Store) newPos(q querier, col, before, self string) (float64, error) {
	rows, err := q.Query(`SELECT id,pos FROM tasks WHERE project=? AND col=? AND id<>? ORDER BY pos`, s.pid, col, self)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []string
	var ps []float64
	for rows.Next() {
		var id string
		var p float64
		if err := rows.Scan(&id, &p); err != nil {
			return 0, err
		}
		ids, ps = append(ids, id), append(ps, p)
	}
	if before == "" {
		if len(ps) == 0 {
			return 0, nil
		}
		return ps[len(ps)-1] + 1, nil
	}
	i := slices.Index(ids, before)
	if i < 0 {
		return 0, fmt.Errorf("task %q is not in column %q", before, col)
	}
	prev := ps[i] - 1
	if i > 0 {
		prev = ps[i-1]
	}
	// ponytail: float midpoints; ~50 reorders at one spot before precision runs out — renumber the column if that ever bites
	return (prev + ps[i]) / 2, nil
}

func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *Store) insertTask(tx *sql.Tx, t Task) error {
	_, err := tx.Exec(`INSERT INTO tasks(project,`+taskCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.pid, t.ID, t.Title, t.Description, t.Column,
		t.Priority, t.Assignee, t.Due, jsonStr(t.Labels), jsonStr(t.Checklist), t.Pos, t.Created, t.Updated, t.Link, t.Human)
	return err
}

func (s *Store) CreateTask(who, via string, in TaskInput) (t Task, err error) {
	s.pullBeforeID()
	err = s.write(who, via, func(tx *sql.Tx) (Event, error) {
		d, err := s.loadDomain(tx)
		if err != nil {
			return Event{}, err
		}
		in.Title = strings.TrimSpace(in.Title)
		if in.Title == "" {
			return Event{}, errors.New("title is required")
		}
		if in.Column == "" {
			in.Column = d.st.DefaultCol
			if d.checkCol(in.Column) != nil {
				in.Column = d.cols[0].ID
			}
		}
		if in.Priority == "" {
			in.Priority = "medium"
		}
		if in.Assignee == "" && via == "ui" {
			in.Assignee = d.st.DefaultAssignee
		}
		if in.Human && d.isAgent(in.Assignee) {
			in.Assignee = "" // human-only tasks never default to an agent
		}
		if in.Labels == nil {
			in.Labels = []string{}
		}
		for _, e := range []error{d.checkCol(in.Column), checkPriority(in.Priority), d.checkAssignee(in.Assignee), d.checkLabels(in.Labels), checkDue(in.Due), checkLink(in.Link)} {
			if e != nil {
				return Event{}, e
			}
		}
		var seq int
		var raw string
		if tx.QueryRow(`SELECT value FROM meta WHERE key=?`, "seq:"+s.pid).Scan(&raw) == nil {
			json.Unmarshal([]byte(raw), &seq)
		}
		seq++
		if err := putMeta(tx, "seq:"+s.pid, seq); err != nil {
			return Event{}, err
		}
		pos, err := s.newPos(tx, in.Column, "", "")
		if err != nil {
			return Event{}, err
		}
		checks := []Check{}
		for _, c := range in.Checklist {
			if c = strings.TrimSpace(c); c != "" {
				checks = append(checks, Check{Text: c})
			}
		}
		now := time.Now().UnixMilli()
		t = Task{ID: fmt.Sprintf("%s-%d", d.st.Prefix, seq), Title: in.Title, Description: in.Description, Column: in.Column,
			Priority: in.Priority, Assignee: in.Assignee, Due: in.Due, Link: in.Link, Human: in.Human, Labels: in.Labels, Checklist: checks, Comments: []Comment{},
			Pos: pos, Created: now, Updated: now}
		return Event{Action: "created", TaskID: t.ID, Detail: t.Title}, s.insertTask(tx, t)
	})
	return
}

func (s *Store) UpdateTask(who, via string, p TaskPatch) (t Task, err error) {
	err = s.write(who, via, func(tx *sql.Tx) (Event, error) {
		d, err := s.loadDomain(tx)
		if err != nil {
			return Event{}, err
		}
		if t, err = getTask(tx, s.pid, p.ID); err != nil {
			return Event{}, err
		}
		var changed []string
		if p.Title != nil {
			if strings.TrimSpace(*p.Title) == "" {
				return Event{}, errors.New("title cannot be empty")
			}
			t.Title, changed = *p.Title, append(changed, "title")
		}
		if p.Description != nil {
			t.Description, changed = *p.Description, append(changed, "description")
		}
		if p.Priority != nil {
			if err := checkPriority(*p.Priority); err != nil {
				return Event{}, err
			}
			t.Priority, changed = *p.Priority, append(changed, "priority")
		}
		if p.Assignee != nil {
			if err := d.checkAssignee(*p.Assignee); err != nil {
				return Event{}, err
			}
			t.Assignee, changed = *p.Assignee, append(changed, "assignee")
		}
		if p.Due != nil {
			if err := checkDue(*p.Due); err != nil {
				return Event{}, err
			}
			t.Due, changed = *p.Due, append(changed, "due date")
		}
		if p.Link != nil {
			if err := checkLink(*p.Link); err != nil {
				return Event{}, err
			}
			t.Link, changed = *p.Link, append(changed, "link")
		}
		if p.Human != nil {
			t.Human = *p.Human
			changed = append(changed, map[bool]string{true: "marked human-only", false: "opened to agents"}[t.Human])
			if t.Human && d.isAgent(t.Assignee) && p.Assignee == nil {
				t.Assignee, changed = "", append(changed, "unassigned agent")
			}
		}
		if t.Human && d.isAgent(t.Assignee) {
			return Event{}, fmt.Errorf("%s is human-only; it can't be assigned to agent %q", t.ID, t.Assignee)
		}
		if p.Labels != nil {
			if err := d.checkLabels(*p.Labels); err != nil {
				return Event{}, err
			}
			t.Labels, changed = *p.Labels, append(changed, "labels")
		}
		if p.Checklist != nil {
			t.Checklist, changed = *p.Checklist, append(changed, "checklist")
		}
		if len(changed) == 0 {
			return Event{}, errors.New("nothing to update")
		}
		t.Updated = time.Now().UnixMilli()
		_, err = tx.Exec(`UPDATE tasks SET title=?,descr=?,priority=?,assignee=?,due=?,link=?,human=?,labels=?,checklist=?,updated=? WHERE project=? AND id=?`,
			t.Title, t.Description, t.Priority, t.Assignee, t.Due, t.Link, t.Human, jsonStr(t.Labels), jsonStr(t.Checklist), t.Updated, s.pid, t.ID)
		return Event{Action: "updated", TaskID: t.ID, Detail: strings.Join(changed, ", ")}, err
	})
	return
}

func (s *Store) MoveTask(who, via string, in MoveInput) (t Task, err error) {
	err = s.write(who, via, func(tx *sql.Tx) (Event, error) {
		d, err := s.loadDomain(tx)
		if err != nil {
			return Event{}, err
		}
		if err := d.checkCol(in.Column); err != nil {
			return Event{}, err
		}
		if t, err = getTask(tx, s.pid, in.ID); err != nil {
			return Event{}, err
		}
		from := t.Column
		if t.Pos, err = s.newPos(tx, in.Column, in.Before, t.ID); err != nil {
			return Event{}, err
		}
		t.Column, t.Updated = in.Column, time.Now().UnixMilli()
		if _, err = tx.Exec(`UPDATE tasks SET col=?,pos=?,updated=? WHERE project=? AND id=?`, t.Column, t.Pos, t.Updated, s.pid, t.ID); err != nil {
			return Event{}, err
		}
		if from == in.Column {
			return Event{Action: "reordered", TaskID: t.ID}, nil
		}
		name := in.Column
		for _, c := range d.cols {
			if c.ID == in.Column {
				name = c.Name
			}
		}
		return Event{Action: "moved", TaskID: t.ID, Detail: "→ " + name}, nil
	})
	return
}

func (s *Store) DeleteTask(who, via, id string) error {
	return s.write(who, via, func(tx *sql.Tx) (Event, error) {
		t, err := getTask(tx, s.pid, id)
		if err != nil {
			return Event{}, err
		}
		if _, err := tx.Exec(`DELETE FROM comments WHERE project=? AND task_id=?`, s.pid, id); err != nil {
			return Event{}, err
		}
		_, err = tx.Exec(`DELETE FROM tasks WHERE project=? AND id=?`, s.pid, id)
		return Event{Action: "deleted", TaskID: id, Detail: t.Title}, err
	})
}

func (s *Store) AddComment(who, via string, in CommentInput) (c Comment, err error) {
	err = s.write(who, via, func(tx *sql.Tx) (Event, error) {
		if strings.TrimSpace(in.Text) == "" {
			return Event{}, errors.New("comment text is required")
		}
		if _, err := getTask(tx, s.pid, in.ID); err != nil {
			return Event{}, err
		}
		c = Comment{Who: who, Text: in.Text, T: time.Now().UnixMilli()}
		res, err := tx.Exec(`INSERT INTO comments(project,task_id,who,text,t) VALUES (?,?,?,?,?)`, s.pid, in.ID, c.Who, c.Text, c.T)
		if err != nil {
			return Event{}, err
		}
		c.ID, _ = res.LastInsertId()
		_, err = tx.Exec(`UPDATE tasks SET updated=? WHERE project=? AND id=?`, c.T, s.pid, in.ID)
		return Event{Action: "commented on", TaskID: in.ID}, err
	})
	return
}

// ---------- board configuration (UI only) ----------

func (s *Store) SaveSettings(who string, st Settings) error {
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		st.Prefix = strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(st.Prefix, ""))
		if st.Prefix == "" {
			st.Prefix = "KB"
		}
		if st.Perms == nil {
			st.Perms = map[string]bool{}
		}
		if name := strings.TrimSpace(st.Board); name != "" {
			if _, err := tx.Exec(`UPDATE projects SET name=? WHERE id=?`, name, s.pid); err != nil {
				return Event{}, err
			}
		}
		st.Me, st.Board = "", "" // per process / lives in projects
		return Event{Action: "updated settings"}, putMeta(tx, "settings:"+s.pid, st)
	})
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// SaveColumns replaces the column list. Removed columns must be empty.
func (s *Store) SaveColumns(who string, cols []Column) error {
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		if len(cols) == 0 {
			return Event{}, errors.New("a board needs at least one column")
		}
		keep := map[string]bool{}
		for _, c := range cols {
			if !idRe.MatchString(c.ID) || keep[c.ID] {
				return Event{}, fmt.Errorf("bad or duplicate column id %q", c.ID)
			}
			if strings.TrimSpace(c.Name) == "" {
				return Event{}, errors.New("column name cannot be empty")
			}
			keep[c.ID] = true
		}
		old, err := getColumns(tx, s.pid)
		if err != nil {
			return Event{}, err
		}
		for _, c := range old {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM tasks WHERE project=? AND col=?`, s.pid, c.ID).Scan(&n); err != nil {
				return Event{}, err
			}
			if !keep[c.ID] && n > 0 {
				return Event{}, fmt.Errorf("column %q still has %d tasks", c.Name, n)
			}
		}
		if _, err := tx.Exec(`DELETE FROM columns WHERE project=?`, s.pid); err != nil {
			return Event{}, err
		}
		for i, c := range cols {
			if _, err := tx.Exec(`INSERT INTO columns VALUES (?,?,?,?,?,?)`, s.pid, c.ID, c.Name, c.Color, max(c.WIP, 0), i); err != nil {
				return Event{}, err
			}
		}
		return Event{Action: "updated columns"}, nil
	})
}

// SaveMembers replaces the member list (shared by every project); removed members' tasks become unassigned.
func (s *Store) SaveMembers(who string, ms []Member) error {
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		keep := map[string]bool{}
		for _, m := range ms {
			if !idRe.MatchString(m.Name) || keep[m.Name] {
				return Event{}, fmt.Errorf("bad or duplicate member name %q (lowercase letters, digits, - _)", m.Name)
			}
			if m.Icon != "" && !idRe.MatchString(m.Icon) {
				return Event{}, fmt.Errorf("bad icon %q", m.Icon)
			}
			keep[m.Name] = true
		}
		if !keep[s.me] {
			return Event{}, fmt.Errorf("you (%s) can't remove yourself", s.me)
		}
		old, err := getMembers(tx)
		if err != nil {
			return Event{}, err
		}
		for _, m := range old {
			if !keep[m.Name] {
				if _, err := tx.Exec(`UPDATE tasks SET assignee='' WHERE assignee=?`, m.Name); err != nil {
					return Event{}, err
				}
			}
		}
		if _, err := tx.Exec(`DELETE FROM members`); err != nil {
			return Event{}, err
		}
		for i, m := range ms {
			if _, err := tx.Exec(`INSERT INTO members(name,color,agent,pos,icon) VALUES (?,?,?,?,?)`, m.Name, m.Color, m.Agent, i, m.Icon); err != nil {
				return Event{}, err
			}
		}
		return Event{Action: "updated members"}, nil
	})
}

// JoinAgent adds an unknown MCP client as an agent member (no-op if it exists).
func (s *Store) JoinAgent(name string) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM members WHERE name=?`, name).Scan(&n); err != nil || n > 0 {
		return err
	}
	return s.write(name, "mcp", func(tx *sql.Tx) (Event, error) {
		_, err := tx.Exec(`INSERT INTO members(name,color,agent,pos) VALUES (?,?,1,(SELECT coalesce(max(pos),0)+1 FROM members)) ON CONFLICT(name) DO NOTHING`,
			name, "#56b6c2")
		return Event{Action: "joined as an agent"}, err
	})
}

// rewriteLabels applies fn to every task's label list in this project.
func (s *Store) rewriteLabels(tx *sql.Tx, fn func([]string) []string) error {
	tasks, err := taskLabels(tx, s.pid)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		nl := fn(slices.Clone(t.Labels))
		if !slices.Equal(nl, t.Labels) {
			if _, err := tx.Exec(`UPDATE tasks SET labels=? WHERE project=? AND id=?`, jsonStr(nl), s.pid, t.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// SaveLabels replaces the label list (names + colors); removed labels are stripped from tasks.
func (s *Store) SaveLabels(who string, ls []Label) error {
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		keep := map[string]bool{}
		for _, l := range ls {
			l.Name = strings.TrimSpace(l.Name)
			if l.Name == "" || len(l.Name) > 40 || keep[l.Name] {
				return Event{}, fmt.Errorf("bad or duplicate label %q", l.Name)
			}
			keep[l.Name] = true
		}
		if err := s.rewriteLabels(tx, func(in []string) []string {
			return slices.DeleteFunc(in, func(l string) bool { return !keep[l] })
		}); err != nil {
			return Event{}, err
		}
		if _, err := tx.Exec(`DELETE FROM labels WHERE project=?`, s.pid); err != nil {
			return Event{}, err
		}
		for i, l := range ls {
			if _, err := tx.Exec(`INSERT INTO labels VALUES (?,?,?,?)`, s.pid, strings.TrimSpace(l.Name), l.Color, i); err != nil {
				return Event{}, err
			}
		}
		return Event{Action: "updated labels"}, nil
	})
}

func (s *Store) RenameLabel(who string, in RenameInput) error {
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		in.To = strings.TrimSpace(in.To)
		if in.To == "" || len(in.To) > 40 {
			return Event{}, errors.New("bad label name")
		}
		var n int
		tx.QueryRow(`SELECT count(*) FROM labels WHERE project=? AND name=?`, s.pid, in.To).Scan(&n)
		if n > 0 {
			return Event{}, fmt.Errorf("label %q already exists", in.To)
		}
		if _, err := tx.Exec(`UPDATE labels SET name=? WHERE project=? AND name=?`, in.To, s.pid, in.From); err != nil {
			return Event{}, err
		}
		err := s.rewriteLabels(tx, func(ls []string) []string {
			for i := range ls {
				if ls[i] == in.From {
					ls[i] = in.To
				}
			}
			return ls
		})
		return Event{Action: "renamed label", Detail: in.From + " → " + in.To}, err
	})
}

func (s *Store) DeleteAllTasks(who string) error {
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		if _, err := tx.Exec(`DELETE FROM comments WHERE project=?`, s.pid); err != nil {
			return Event{}, err
		}
		_, err := tx.Exec(`DELETE FROM tasks WHERE project=?`, s.pid)
		return Event{Action: "deleted all tasks"}, err
	})
}

// Import replaces this project's board with an export. Members are merged (they are shared).
func (s *Store) Import(who string, b Board) error {
	if len(b.Columns) == 0 {
		return errors.New("import has no columns")
	}
	return s.write(who, "ui", func(tx *sql.Tx) (Event, error) {
		for _, t := range []string{"comments", "tasks", "columns", "labels"} {
			if _, err := tx.Exec(`DELETE FROM `+t+` WHERE project=?`, s.pid); err != nil {
				return Event{}, err
			}
		}
		for i, c := range b.Columns {
			if _, err := tx.Exec(`INSERT INTO columns VALUES (?,?,?,?,?,?)`, s.pid, c.ID, c.Name, c.Color, c.WIP, i); err != nil {
				return Event{}, err
			}
		}
		for _, m := range b.Members {
			if _, err := tx.Exec(`INSERT INTO members(name,color,agent,pos,icon) VALUES (?,?,?,(SELECT coalesce(max(pos),0)+1 FROM members),?)
				ON CONFLICT(name) DO NOTHING`, m.Name, m.Color, m.Agent, m.Icon); err != nil {
				return Event{}, err
			}
		}
		for i, l := range b.Labels {
			if _, err := tx.Exec(`INSERT INTO labels VALUES (?,?,?,?)`, s.pid, l.Name, l.Color, i); err != nil {
				return Event{}, err
			}
		}
		maxSeq := 0
		for _, t := range b.Tasks {
			if t.Labels == nil {
				t.Labels = []string{}
			}
			if t.Checklist == nil {
				t.Checklist = []Check{}
			}
			if err := checkLink(t.Link); err != nil { // imports are untrusted files: links must be http(s), like CreateTask
				return Event{}, fmt.Errorf("task %s: %w", t.ID, err)
			}
			if err := s.insertTask(tx, t); err != nil {
				return Event{}, fmt.Errorf("task %s: %w", t.ID, err)
			}
			for _, c := range t.Comments {
				if _, err := tx.Exec(`INSERT INTO comments(project,task_id,who,text,t) VALUES (?,?,?,?,?)`, s.pid, t.ID, c.Who, c.Text, c.T); err != nil {
					return Event{}, err
				}
			}
			var n int
			if i := strings.LastIndex(t.ID, "-"); i >= 0 {
				fmt.Sscan(t.ID[i+1:], &n)
			}
			maxSeq = max(maxSeq, n)
		}
		if err := putMeta(tx, "seq:"+s.pid, maxSeq); err != nil {
			return Event{}, err
		}
		b.Settings.Me, b.Settings.Board = "", ""
		if b.Settings.Prefix == "" {
			b.Settings.Prefix = "KB"
		}
		return Event{Action: "imported", Detail: fmt.Sprintf("%d tasks", len(b.Tasks))}, putMeta(tx, "settings:"+s.pid, b.Settings)
	})
}
