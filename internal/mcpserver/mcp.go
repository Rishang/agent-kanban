package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Rishang/agent-kanban/internal/store"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ListInput struct {
	Column          string `json:"column,omitempty" jsonschema:"column id or name, e.g. doing or In Progress"`
	Assignee        string `json:"assignee,omitempty" jsonschema:"member name; me = you; none = unassigned"`
	Label           string `json:"label,omitempty" jsonschema:"label name (any case)"`
	Query           string `json:"q,omitempty" jsonschema:"case-insensitive text search in id, title and description"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"include auto-archived done and closed tasks"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"task id, e.g. KB-12 (or just 12)"`
}

type CheckInput struct {
	ID    string `json:"id" jsonschema:"task id"`
	Item  string `json:"item,omitempty" jsonschema:"checklist item: 0-based index, or (part of) its text"`
	Done  *bool  `json:"done,omitempty" jsonschema:"default true"`
	Index *int   `json:"index,omitempty" jsonschema:"deprecated: use item"`
}

type StartInput struct {
	ID   string `json:"id" jsonschema:"task id"`
	Note string `json:"note,omitempty" jsonschema:"optional markdown: your plan or first observations"`
}

type ProgressInput struct {
	ID   string   `json:"id" jsonschema:"task id"`
	Note string   `json:"note,omitempty" jsonschema:"markdown progress note (posted as a comment)"`
	Done []string `json:"done,omitempty" jsonschema:"checklist items now done: indexes or (part of) their text"`
	Add  []string `json:"add,omitempty" jsonschema:"new checklist items to append"`
}

type FinishInput struct {
	ID      string `json:"id" jsonschema:"task id"`
	Summary string `json:"summary" jsonschema:"markdown: what changed and how it was verified"`
	Link    string `json:"link,omitempty" jsonschema:"PR or commit URL (http/https)"`
	Done    bool   `json:"done,omitempty" jsonschema:"move straight to the done column instead of review (needs the finish permission)"`
}

type CloseInput struct {
	ID     string `json:"id" jsonschema:"task id"`
	Reason string `json:"reason" jsonschema:"markdown: why this won't be done (duplicate of KB-3, obsolete, out of scope…)"`
}

type CreateMany struct {
	Tasks []store.TaskInput `json:"tasks" jsonschema:"tasks to create, in order"`
}

// agentSchema is the MCP input schema for T as agents see it:
//   - no "due" property: due dates are set by humans in the UI (sending one is rejected by schema validation);
//   - "priority" is an enum of store.Priorities, so clients can check it before calling.
func agentSchema[T any]() *jsonschema.Schema {
	sc, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	var strip func(*jsonschema.Schema)
	strip = func(s *jsonschema.Schema) {
		if s == nil {
			return
		}
		delete(s.Properties, "due")
		if pr := s.Properties["priority"]; pr != nil {
			pr.Enum = []any{""} // "" = the default (medium), as the store treats it
			for _, v := range store.Priorities {
				pr.Enum = append(pr.Enum, v)
			}
			if slices.Contains(pr.Types, "null") { // optional *string in patches
				pr.Enum = append(pr.Enum, nil)
			}
		}
		for _, p := range s.Properties {
			strip(p)
		}
		strip(s.Items)
		for _, d := range s.Defs {
			strip(d)
		}
	}
	strip(sc)
	return sc
}

var agentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Handler serves MCP over streamable HTTP at /mcp?agent=<name>&project=<id>.
// The agent name attributes every change; unknown agents are added as members.
// project defaults to defProject (the project the server was started in).
// baseURL (e.g. http://127.0.0.1:7777) is used to build task links.
func Handler(d *store.DB, defProject, version, baseURL string) http.Handler {
	var mu sync.Mutex
	servers := map[string]*mcp.Server{}
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		agent := strings.ToLower(r.URL.Query().Get("agent"))
		if !agentRe.MatchString(agent) {
			agent = "agent"
		}
		pid := r.URL.Query().Get("project")
		if pid == "" {
			pid = defProject
		}
		s, err := d.Project(pid)
		if err != nil {
			return nil // the SDK answers 400; the error is in the server log
		}
		// Any request (initialize, tools/list, a call) counts as connected, so a client shows up as soon as it connects.
		s.Touch(agent)
		if err := s.JoinAgent(agent); err != nil {
			log.Printf("mcp: add agent %s: %v", agent, err)
		}
		mu.Lock()
		defer mu.Unlock()
		key := pid + "|" + agent
		if srv, ok := servers[key]; ok {
			return srv
		}
		srv := newServer(s, agent, version, baseURL)
		servers[key] = srv
		return srv
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// ---------- forgiving input resolution (pure, tested) ----------

// colRole is a column's place in the workflow: its explicit role (done, closed), else a guess from its id/name and position.
func colRole(cols []store.Column, i int) string {
	if cols[i].Role != "" {
		return cols[i].Role
	}
	if d, ok := store.DoneColumn(cols); ok && d.ID == cols[i].ID {
		return store.RoleDone
	}
	start, _ := store.StartColumn(cols)
	n := strings.ToLower(cols[i].ID + " " + cols[i].Name)
	switch {
	case strings.Contains(n, "review") || strings.Contains(n, "qa") || strings.Contains(n, "test"):
		return "review"
	case strings.Contains(n, "progress") || strings.Contains(n, "doing") || strings.Contains(n, "wip") || strings.Contains(n, "active"):
		return "in_progress"
	case cols[i].ID == start.ID: // the first open column, even when Closed sits before it
		return "todo"
	}
	return ""
}

// colByRole returns the first column with role, falling back to the open column after todo for in_progress.
func colByRole(cols []store.Column, role string) (store.Column, bool) {
	for i := range cols {
		if colRole(cols, i) == role {
			return cols[i], true
		}
	}
	if role == "in_progress" {
		fin := store.Finished(cols)
		if i := slices.IndexFunc(cols, func(c store.Column) bool { return !fin[c.ID] }); i >= 0 && i+1 < len(cols) && !fin[cols[i+1].ID] {
			return cols[i+1], true
		}
	}
	return store.Column{}, false
}

// resolveColumn accepts a column id or name in any case ("In Progress", "doing").
func resolveColumn(cols []store.Column, in string) (string, error) {
	in = strings.TrimSpace(in)
	var valid []string
	for _, c := range cols {
		if c.ID == in {
			return c.ID, nil
		}
		valid = append(valid, fmt.Sprintf("%s (%s)", c.ID, c.Name))
	}
	for _, c := range cols {
		if strings.EqualFold(c.ID, in) || strings.EqualFold(c.Name, in) {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("unknown column %q; valid: %s", in, strings.Join(valid, ", "))
}

// resolveLabels maps labels to their existing spelling (any case); unknown ones pass through for the store to reject.
func resolveLabels(ls []store.Label, in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.TrimSpace(strings.TrimPrefix(l, "#"))
		for _, have := range ls {
			if strings.EqualFold(have.Name, l) {
				l = have.Name
				break
			}
		}
		out = append(out, l)
	}
	return out
}

// normID turns "12", "kb-12" or " KB-12 " into "KB-12".
func normID(prefix, id string) string {
	id = strings.TrimSpace(id)
	if _, err := strconv.Atoi(id); err == nil {
		return prefix + "-" + id
	}
	return strings.ToUpper(id)
}

// findItem resolves a checklist item by index or unique (case-insensitive) text match.
func findItem(cl []store.Check, item string) (int, error) {
	item = strings.TrimSpace(item)
	if i, err := strconv.Atoi(item); err == nil {
		if i < 0 || i >= len(cl) {
			return 0, fmt.Errorf("item %d out of range; checklist has %d items", i, len(cl))
		}
		return i, nil
	}
	low, match := strings.ToLower(item), -1
	for i, c := range cl {
		t := strings.ToLower(c.Text)
		if t == low {
			return i, nil
		}
		if strings.Contains(t, low) {
			if match >= 0 {
				return 0, fmt.Errorf("item %q matches more than one checklist entry; use its index", item)
			}
			match = i
		}
	}
	if match < 0 {
		texts := make([]string, len(cl))
		for i, c := range cl {
			texts[i] = fmt.Sprintf("%d: %s", i, c.Text)
		}
		return 0, fmt.Errorf("no checklist item matches %q; items: %s", item, strings.Join(texts, "; "))
	}
	return match, nil
}

var prioRank = map[string]int{"urgent": 0, "high": 1, "medium": 2, "low": 3}

// pickNext chooses what an agent should work on: its own in-progress task first, else the
// most urgent unassigned (or self-assigned) task in the todo column. Human-only, done and closed work is skipped.
func pickNext(cols []store.Column, tasks []store.Task, agent string) (best *store.Task, reason string, alts []store.Task) {
	if len(cols) == 0 {
		return nil, "board has no columns", nil
	}
	todo, _ := colByRole(cols, "todo")
	doing, hasDoing := colByRole(cols, "in_progress")
	fin := store.Finished(cols)
	for i := range tasks {
		t := &tasks[i]
		if !t.Human && t.Assignee == agent && !fin[t.Column] && t.Column != todo.ID && (!hasDoing || t.Column == doing.ID) {
			return t, "you already have this in progress; finish it before starting something new", nil
		}
	}
	var cand []store.Task
	for _, t := range tasks {
		if !t.Human && t.Column == todo.ID && (t.Assignee == "" || t.Assignee == agent) {
			cand = append(cand, t)
		}
	}
	if len(cand) == 0 {
		return nil, "nothing to pick up: no unassigned, non-human tasks in " + todo.Name, nil
	}
	slices.SortStableFunc(cand, func(a, b store.Task) int {
		if d := prioRank[a.Priority] - prioRank[b.Priority]; d != 0 {
			return d
		}
		if (a.Due != "") != (b.Due != "") {
			if a.Due != "" {
				return -1
			}
			return 1
		}
		if a.Due != b.Due {
			return strings.Compare(a.Due, b.Due)
		}
		switch {
		case a.Pos < b.Pos:
			return -1
		case a.Pos > b.Pos:
			return 1
		}
		return 0
	})
	reason = fmt.Sprintf("highest priority (%s) open task in %s", cand[0].Priority, todo.Name)
	if cand[0].Due != "" {
		reason += ", due " + cand[0].Due
	}
	if hasDoing && doing.WIP > 0 {
		n := 0
		for _, t := range tasks {
			if t.Column == doing.ID {
				n++
			}
		}
		if n >= doing.WIP {
			reason += fmt.Sprintf("; note: %s is at its WIP limit (%d/%d), consider finishing something first", doing.Name, n, doing.WIP)
		}
	}
	return &cand[0], reason, cand[1:min(len(cand), 4)]
}

func checkDone(cl []store.Check) string {
	if len(cl) == 0 {
		return ""
	}
	n := 0
	for _, c := range cl {
		if c.Done {
			n++
		}
	}
	return fmt.Sprintf("%d/%d", n, len(cl))
}

// ---------- server ----------

func newServer(s *store.Store, agent, version, baseURL string) *mcp.Server {
	link := baseURL + "/#" + s.ProjectID() + "/"
	srv := mcp.NewServer(&mcp.Implementation{Name: "agent-kanban", Version: version}, &mcp.ServerOptions{
		Instructions: "Kanban board for project " + s.ProjectID() + ", shared live with the humans on this project. " +
			"Use it proactively for any non-trivial coding work:\n" +
			"1. next_task (or my_tasks) to see what to work on; create_task / create_tasks when the user asks for work that has no task yet.\n" +
			"2. start_task when you begin: it assigns you, moves the task to in progress and posts your plan.\n" +
			"3. update_progress as you go: tick checklist items, post short notes about decisions and blockers.\n" +
			"4. finish_task when done: summary of what changed and how it was verified, plus the PR link; it goes to review.\n" +
			"A task that should not be done at all (duplicate, obsolete, won't fix) is closed with close_task and a reason, not finished.\n" +
			"Every result includes the task url; share it with the user. Columns and labels accept names in any case; ids accept 12 or KB-12. " +
			"Tasks marked human-only are not yours: they are hidden from list_tasks and read-only. If a call returns permission denied, don't retry; tell the user.",
	})

	// Replies are one compact text block: no structured output, no output schema, and mutating
	// tools return a one-line status only (the agent just sent the data, it does not need it echoed).
	text := func(st string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: st}}}
	}
	line := func(t store.Task, comments int) string {
		st := fmt.Sprintf("%s [%s] %s %q", t.ID, t.Column, t.Priority, t.Title)
		if t.Assignee != "" {
			st += " @" + t.Assignee
		}
		if t.Due != "" {
			st += " due " + t.Due
		}
		if len(t.Labels) > 0 {
			st += " #" + strings.Join(t.Labels, " #")
		}
		if c := checkDone(t.Checklist); c != "" {
			st += " [" + c + "]"
		}
		if comments > 0 {
			st += fmt.Sprintf(" (%d comments)", comments)
		}
		return st
	}
	urlNote := "task url = " + link + "<id>"
	taskStatus := func(verb string, t store.Task, b store.Board) string {
		col := t.Column
		for _, c := range b.Columns {
			if c.ID == t.Column {
				col = c.Name
			}
		}
		st := fmt.Sprintf("%s %s %q → %s", verb, t.ID, t.Title, col)
		if t.Assignee != "" {
			st += " · assigned " + t.Assignee
		}
		if c := checkDone(t.Checklist); c != "" {
			st += " · checklist " + c
		}
		return st + " · " + link + t.ID
	}

	meta := func() (store.Board, error) { return s.Meta() }
	id := func(b store.Board, raw string) string { return normID(b.Settings.Prefix, raw) }

	// guard checks every perm, rejects human-only tasks and makes sure the agent is a member.
	guard := func(b store.Board, taskID string, perms ...string) error {
		s.Touch(agent)
		if !slices.ContainsFunc(b.Members, func(m store.Member) bool { return m.Name == agent }) {
			if err := s.JoinAgent(agent); err != nil {
				return err
			}
		}
		if taskID != "" {
			if t, err := s.Task(taskID); err == nil && t.Human {
				return fmt.Errorf("%s is a human-only task; agents skip it. Pick another task (next_task) or ask the user", taskID)
			}
		}
		for _, perm := range perms {
			if !b.Settings.Perms[perm] {
				s.LogBlocked(agent, perm, taskID)
				return fmt.Errorf("permission denied: agents may not %s on this board (a human can change this in Settings → MCP & skill)", store.PermNames[perm])
			}
		}
		return nil
	}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	// ---- read

	mcp.AddTool(srv, &mcp.Tool{Name: "get_board", Annotations: ro,
		Description: "Board metadata: columns in order (task counts, WIP limits, workflow role), labels, members, priorities and your permissions. " +
			"Call this once at the start of a session."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			if err := guard(b, ""); err != nil {
				return nil, nil, err
			}
			tasks, err := s.Tasks()
			if err != nil {
				return nil, nil, err
			}
			fin := store.Finished(b.Columns)
			open := 0
			count := map[string]int{}
			for _, t := range tasks {
				count[t.Column]++
				if t.Assignee == agent && !fin[t.Column] {
					open++
				}
			}
			var o strings.Builder
			fmt.Fprintf(&o, "project %s (%s); you are %s, %d open task(s) assigned to you; %s\ncolumns:", s.ProjectID(), b.Settings.Board, agent, open, urlNote)
			for i, c := range b.Columns {
				fmt.Fprintf(&o, "\n- %s %q role=%s tasks=%d", c.ID, c.Name, colRole(b.Columns, i), count[c.ID])
				if c.WIP > 0 {
					fmt.Fprintf(&o, " wip=%d", c.WIP)
				}
			}
			o.WriteString("\nlabels:")
			for _, l := range b.Labels {
				fmt.Fprintf(&o, " %s(%d)", l.Name, l.Tasks)
			}
			o.WriteString("\nmembers:")
			for _, m := range b.Members {
				o.WriteString(" " + m.Name)
				if m.Agent {
					o.WriteString("(agent)")
				}
			}
			o.WriteString("\npriorities: " + strings.Join(b.Priorities, ">") + "\npermissions:")
			for _, k := range slices.Sorted(maps.Keys(store.PermNames)) {
				mark := "+"
				if !b.Settings.Perms[k] {
					mark = "-"
				}
				o.WriteString(" " + mark + k)
			}
			return text(o.String()), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_labels", Annotations: ro, Description: "All labels with task counts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			s.Touch(agent)
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			names := make([]string, len(b.Labels))
			for i, l := range b.Labels {
				names[i] = fmt.Sprintf("%s(%d)", l.Name, l.Tasks)
			}
			return text(strings.Join(names, " ")), nil, nil
		})

	// listTasks renders matching tasks as one line each, in board order.
	listTasks := func(in ListInput, mine bool) (string, error) {
		b, err := meta()
		if err != nil {
			return "", err
		}
		if in.Column != "" {
			if in.Column, err = resolveColumn(b.Columns, in.Column); err != nil {
				return "", err
			}
		}
		if mine {
			in.Assignee = agent
		}
		if strings.EqualFold(in.Assignee, "me") {
			in.Assignee = agent
		}
		if in.Label != "" {
			in.Label = resolveLabels(b.Labels, []string{in.Label})[0]
		}
		tasks, err := s.Tasks()
		if err != nil {
			return "", err
		}
		ncomments, err := s.CommentCounts()
		if err != nil {
			return "", err
		}
		fin := store.Finished(b.Columns)
		q := strings.ToLower(in.Query)
		var lines []string
		hidden := 0
		for _, c := range b.Columns { // column order, then position
			for _, t := range tasks {
				if t.Column != c.ID || (in.Column != "" && t.Column != in.Column) ||
					(mine && fin[t.Column]) ||
					(in.Assignee != "" && !(t.Assignee == in.Assignee || strings.EqualFold(in.Assignee, "none") && t.Assignee == "")) ||
					(in.Label != "" && !slices.Contains(t.Labels, in.Label)) ||
					(q != "" && !strings.Contains(strings.ToLower(t.ID+" "+t.Title+" "+t.Description), q)) ||
					(!in.IncludeArchived && store.Archived(t, b.Settings, fin)) {
					continue
				}
				if t.Human {
					hidden++
					continue
				}
				lines = append(lines, line(t, ncomments[t.ID]))
			}
		}
		out := fmt.Sprintf("%d task(s); %s\n%s", len(lines), urlNote, strings.Join(lines, "\n"))
		if hidden > 0 {
			out += fmt.Sprintf("\n(%d human-only task(s) hidden)", hidden)
		}
		return out, nil
	}

	mcp.AddTool(srv, &mcp.Tool{Name: "list_tasks", Annotations: ro,
		Description: "List tasks, one line each, in board order. Filters: column (id or name), assignee (me / none / name), label, q (text). Use get_task for the description and comments."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, any, error) {
			s.Touch(agent)
			out, err := listTasks(in, false)
			if err != nil {
				return nil, nil, err
			}
			return text(out), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "my_tasks", Annotations: ro,
		Description: "Your open tasks (assigned to you, not done or closed). Check this when resuming work."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			s.Touch(agent)
			out, err := listTasks(ListInput{}, true)
			if err != nil {
				return nil, nil, err
			}
			return text(out), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "next_task", Annotations: ro,
		Description: "Suggest what to work on next: your own in-progress task if you have one, otherwise the most urgent unassigned task in the todo column " +
			"(priority, then due date, then board order). Returns the reason and a few alternatives. Follow with start_task."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			s.Touch(agent)
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			tasks, err := s.Tasks()
			if err != nil {
				return nil, nil, err
			}
			best, reason, alts := pickNext(b.Columns, tasks, agent)
			if best == nil {
				return text("Nothing to pick up: " + reason), nil, nil
			}
			out := fmt.Sprintf("Next: %s (%s)\n%s", line(*best, 0), reason, link+best.ID)
			for _, a := range alts {
				out += "\nalt: " + line(a, 0)
			}
			return text(out), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_task", Annotations: ro, Description: "Full task: description (markdown), checklist and comments. id accepts 12 or KB-12."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
			s.Touch(agent)
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			t, err := s.Task(id(b, in.ID))
			if err != nil {
				return nil, nil, err
			}
			var o strings.Builder
			o.WriteString(line(t, 0) + "\n" + link + t.ID)
			if t.Link != "" {
				o.WriteString("\nlink: " + t.Link)
			}
			for i, c := range t.Checklist {
				mark := " "
				if c.Done {
					mark = "x"
				}
				fmt.Fprintf(&o, "\n%d. [%s] %s", i, mark, c.Text)
			}
			if t.Description != "" {
				o.WriteString("\n\n" + t.Description)
			}
			for _, c := range t.Comments {
				fmt.Fprintf(&o, "\n\n-- %s:\n%s", c.Who, c.Text)
			}
			return text(o.String()), nil, nil
		})

	// ---- write

	createOne := func(b store.Board, in store.TaskInput) (store.Task, error) {
		var err error
		if in.Column != "" {
			if in.Column, err = resolveColumn(b.Columns, in.Column); err != nil {
				return store.Task{}, err
			}
		}
		in.Labels = resolveLabels(b.Labels, in.Labels)
		in.Priority = strings.ToLower(strings.TrimSpace(in.Priority))
		if strings.EqualFold(in.Assignee, "me") {
			in.Assignee = agent
		}
		return s.CreateTask(agent, "mcp", in)
	}

	mcp.AddTool(srv, &mcp.Tool{Name: "create_task", InputSchema: agentSchema[store.TaskInput](),
		Description: "Create a task. Search first (list_tasks q=…) to avoid duplicates. Column and labels accept names in any case; assignee accepts me. " +
			"Write the description in markdown: goal, approach, acceptance criteria; add a checklist for multi-step work."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in store.TaskInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			if err := guard(b, "", "create"); err != nil {
				return nil, nil, err
			}
			t, err := createOne(b, in)
			if err != nil {
				return nil, nil, err
			}
			t.URL = link + t.ID
			return text(taskStatus("Created", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "create_tasks", InputSchema: agentSchema[CreateMany](),
		Description: "Create several tasks at once, e.g. when breaking a feature into steps. Each item takes the same fields as create_task. " +
			"Valid items are created even if others fail; errors are reported per item."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CreateMany) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			if err := guard(b, "", "create"); err != nil {
				return nil, nil, err
			}
			if len(in.Tasks) == 0 || len(in.Tasks) > 50 {
				return nil, nil, errors.New("pass between 1 and 50 tasks")
			}
			var made, errs []string
			for i, ti := range in.Tasks {
				t, err := createOne(b, ti)
				if err != nil {
					errs = append(errs, fmt.Sprintf("task %d (%q): %v", i, ti.Title, err))
					continue
				}
				made = append(made, fmt.Sprintf("%s %q", t.ID, t.Title))
			}
			st := fmt.Sprintf("Created %d task(s): %s · %s", len(made), strings.Join(made, ", "), urlNote)
			if len(errs) > 0 {
				st += fmt.Sprintf("\n%d failed:\n%s", len(errs), strings.Join(errs, "\n"))
			}
			return text(st), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "update_task", InputSchema: agentSchema[store.TaskPatch](),
		Description: "Change task fields. Only the fields you pass change; labels and checklist replace the whole list (use update_progress to tick or add items). " +
			"Use start_task / finish_task / move_task to change column."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in store.TaskPatch) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if err := guard(b, in.ID, "edit"); err != nil {
				return nil, nil, err
			}
			if in.Labels != nil {
				l := resolveLabels(b.Labels, *in.Labels)
				in.Labels = &l
			}
			if in.Priority != nil {
				p := strings.ToLower(strings.TrimSpace(*in.Priority))
				in.Priority = &p
			}
			if in.Assignee != nil && strings.EqualFold(*in.Assignee, "me") {
				in.Assignee = &agent
			}
			t, err := s.UpdateTask(agent, "mcp", in)
			if err != nil {
				return nil, nil, err
			}
			t.URL = link + t.ID
			return text(taskStatus("Updated", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "check_item", Description: "Mark one checklist item done (default) or not done. item is its index or (part of) its text."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CheckInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if err := guard(b, in.ID, "edit"); err != nil {
				return nil, nil, err
			}
			t, err := s.Task(in.ID)
			if err != nil {
				return nil, nil, err
			}
			if in.Item == "" && in.Index != nil {
				in.Item = strconv.Itoa(*in.Index)
			}
			i, err := findItem(t.Checklist, in.Item)
			if err != nil {
				return nil, nil, err
			}
			t.Checklist[i].Done = in.Done == nil || *in.Done
			if t, err = s.UpdateTask(agent, "mcp", store.TaskPatch{ID: in.ID, Checklist: &t.Checklist}); err != nil {
				return nil, nil, err
			}
			t.URL = link + t.ID
			return text(taskStatus("Checked", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "move_task", Description: "Move a task to another column (id or name) or reorder it within one (before = task id)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in store.MoveInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if in.Before != "" {
				in.Before = id(b, in.Before)
			}
			if in.Column, err = resolveColumn(b.Columns, in.Column); err != nil {
				return nil, nil, err
			}
			perm := "move"
			switch colRole(b.Columns, slices.IndexFunc(b.Columns, func(c store.Column) bool { return c.ID == in.Column })) {
			case store.RoleDone:
				perm = "finish"
			case store.RoleClosed:
				perm = "close"
			}
			if err := guard(b, in.ID, perm); err != nil {
				return nil, nil, err
			}
			t, err := s.MoveTask(agent, "mcp", in)
			if err != nil {
				return nil, nil, err
			}
			t.URL = link + t.ID
			return text(taskStatus("Moved", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "start_task",
		Description: "Start working on a task: assigns it to you, moves it to the in-progress column and posts your plan as a comment (note). " +
			"Refuses tasks assigned to someone else. Call this before you begin coding."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in StartInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if err := guard(b, in.ID, "edit", "move"); err != nil {
				return nil, nil, err
			}
			t, err := s.Task(in.ID)
			if err != nil {
				return nil, nil, err
			}
			if t.Assignee != "" && t.Assignee != agent {
				return nil, nil, fmt.Errorf("%s is assigned to %s; ask the user before taking it over", t.ID, t.Assignee)
			}
			doing, ok := colByRole(b.Columns, "in_progress")
			if !ok {
				return nil, nil, errors.New("this board has no in-progress column; use move_task")
			}
			if t.Assignee != agent {
				if t, err = s.UpdateTask(agent, "mcp", store.TaskPatch{ID: t.ID, Assignee: &agent}); err != nil {
					return nil, nil, err
				}
			}
			if t.Column != doing.ID {
				if t, err = s.MoveTask(agent, "mcp", store.MoveInput{ID: t.ID, Column: doing.ID}); err != nil {
					return nil, nil, err
				}
			}
			if strings.TrimSpace(in.Note) != "" && b.Settings.Perms["comment"] {
				if _, err := s.AddComment(agent, "mcp", store.CommentInput{ID: t.ID, Text: in.Note}); err != nil {
					return nil, nil, err
				}
			}
			if t, err = s.Task(t.ID); err != nil {
				return nil, nil, err
			}
			t.URL = link + t.ID
			return text(taskStatus("Started", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "update_progress",
		Description: "Report progress on a task in one call: tick checklist items (done = indexes or text), append new items (add), and post a markdown note as a comment."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ProgressInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			perms := []string{}
			if len(in.Done)+len(in.Add) > 0 {
				perms = append(perms, "edit")
			}
			if strings.TrimSpace(in.Note) != "" {
				perms = append(perms, "comment")
			}
			if len(perms) == 0 {
				return nil, nil, errors.New("pass a note, done items or new items")
			}
			if err := guard(b, in.ID, perms...); err != nil {
				return nil, nil, err
			}
			t, err := s.Task(in.ID)
			if err != nil {
				return nil, nil, err
			}
			if len(in.Done)+len(in.Add) > 0 {
				cl := t.Checklist
				for _, it := range in.Done {
					i, err := findItem(cl, it)
					if err != nil {
						return nil, nil, err
					}
					cl[i].Done = true
				}
				for _, a := range in.Add {
					if a = strings.TrimSpace(a); a != "" {
						cl = append(cl, store.Check{Text: a})
					}
				}
				if t, err = s.UpdateTask(agent, "mcp", store.TaskPatch{ID: t.ID, Checklist: &cl}); err != nil {
					return nil, nil, err
				}
			}
			if strings.TrimSpace(in.Note) != "" {
				if _, err := s.AddComment(agent, "mcp", store.CommentInput{ID: t.ID, Text: in.Note}); err != nil {
					return nil, nil, err
				}
			}
			t.URL = link + t.ID
			return text(taskStatus("Progress on", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "finish_task",
		Description: "Hand finished work back: posts your summary (what changed, how it was verified), sets the PR link, and moves the task to review " +
			"(or to the done column when done=true and you have the finish permission)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in FinishInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if strings.TrimSpace(in.Summary) == "" {
				return nil, nil, errors.New("summary is required: what changed and how it was verified")
			}
			target, ok := colByRole(b.Columns, "review")
			perms := []string{"move", "comment"}
			if in.Done || !ok {
				target, _ = store.DoneColumn(b.Columns)
				perms = []string{"finish", "comment"}
			}
			if in.Link != "" {
				perms = append(perms, "edit")
			}
			if err := guard(b, in.ID, perms...); err != nil {
				if !ok && !in.Done {
					return nil, nil, fmt.Errorf("%w; this board has no review column, so finishing means moving to %s", err, target.Name)
				}
				return nil, nil, err
			}
			t, err := s.Task(in.ID)
			if err != nil {
				return nil, nil, err
			}
			if in.Link != "" {
				if t, err = s.UpdateTask(agent, "mcp", store.TaskPatch{ID: t.ID, Link: &in.Link}); err != nil {
					return nil, nil, err
				}
			}
			if _, err := s.AddComment(agent, "mcp", store.CommentInput{ID: t.ID, Text: in.Summary}); err != nil {
				return nil, nil, err
			}
			if t.Column != target.ID {
				if t, err = s.MoveTask(agent, "mcp", store.MoveInput{ID: t.ID, Column: target.ID}); err != nil {
					return nil, nil, err
				}
			}
			t.URL = link + t.ID
			return text(taskStatus("Finished", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "close_task",
		Description: "Close a task that won't be done (duplicate, obsolete, out of scope, won't fix): posts your reason as a comment and moves it " +
			"to the closed column. Not for finished work; use finish_task for that. Needs the close permission."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CloseInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if strings.TrimSpace(in.Reason) == "" {
				return nil, nil, errors.New("reason is required: why this task won't be done")
			}
			target, ok := store.ClosedColumn(b.Columns)
			if !ok {
				return nil, nil, errors.New("this board has no closed column; a human can set a column's role to Closed in Settings → Columns")
			}
			if err := guard(b, in.ID, "close", "comment"); err != nil {
				return nil, nil, err
			}
			t, err := s.Task(in.ID)
			if err != nil {
				return nil, nil, err
			}
			if _, err := s.AddComment(agent, "mcp", store.CommentInput{ID: t.ID, Text: "Closed: " + in.Reason}); err != nil {
				return nil, nil, err
			}
			if t.Column != target.ID {
				if t, err = s.MoveTask(agent, "mcp", store.MoveInput{ID: t.ID, Column: target.ID}); err != nil {
					return nil, nil, err
				}
			}
			t.URL = link + t.ID
			return text(taskStatus("Closed", t, b)), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "add_comment", Description: "Add a markdown comment to a task: progress notes, questions for the user, findings. Code blocks, mermaid and svg render in the UI."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in store.CommentInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			in.ID = id(b, in.ID)
			if err := guard(b, in.ID, "comment"); err != nil {
				return nil, nil, err
			}
			_, err = s.AddComment(agent, "mcp", in)
			if err != nil {
				return nil, nil, err
			}
			return text("Commented on " + in.ID + " · " + link + in.ID), nil, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "delete_task", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
		Description: "Permanently delete a task and its comments. Only when the user explicitly asks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
			b, err := meta()
			if err != nil {
				return nil, nil, err
			}
			tid := id(b, in.ID)
			if err := guard(b, tid, "delete"); err != nil {
				return nil, nil, err
			}
			if err := s.DeleteTask(agent, "mcp", tid); err != nil {
				return nil, nil, err
			}
			return text("Deleted " + tid), nil, nil
		})

	// ---- prompts

	srv.AddPrompt(&mcp.Prompt{Name: "plan", Description: "Break a goal into kanban tasks on this board",
		Arguments: []*mcp.PromptArgument{{Name: "goal", Description: "what you want built or fixed", Required: true}}},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Plan this on the kanban board: " +
				req.Params.Arguments["goal"] + "\n\nCall get_board, search list_tasks for related work, then create_tasks with 2–8 small, independently " +
				"shippable tasks (imperative titles, markdown descriptions with acceptance criteria, checklists, existing labels, sensible priorities). " +
				"Reply with the task ids and urls."}}}}, nil
		})
	srv.AddPrompt(&mcp.Prompt{Name: "standup", Description: "Summarise board status: in progress, in review, blocked, next up"},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Give me a short standup for this " +
				"kanban board: call get_board and list_tasks, then summarise what is in progress (and by whom), what is waiting in review, anything " +
				"overdue or over its WIP limit, and what next_task suggests. Include task urls."}}}}, nil
		})
	return srv
}

func ptr[T any](v T) *T { return &v }
