package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Rishang/agent-kanban/internal/store"
)

var cols = []store.Column{{ID: "todo", Name: "Todo"}, {ID: "doing", Name: "In Progress", WIP: 1}, {ID: "review", Name: "Review"}, {ID: "done", Name: "Done"}}

func TestResolvers(t *testing.T) {
	for in, want := range map[string]string{"doing": "doing", "In Progress": "doing", "in progress": "doing", "REVIEW": "review"} {
		if got, err := resolveColumn(cols, in); err != nil || got != want {
			t.Errorf("resolveColumn(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := resolveColumn(cols, "wip"); err == nil || !strings.Contains(err.Error(), "doing (In Progress)") {
		t.Errorf("unknown column error should list valid ones: %v", err)
	}
	for role, want := range map[string]string{"todo": "todo", "in_progress": "doing", "review": "review", "done": "done"} {
		if c, ok := colByRole(cols, role); !ok || c.ID != want {
			t.Errorf("colByRole(%s) = %v %v", role, c.ID, ok)
		}
	}
	if got := resolveLabels([]store.Label{{Name: "Bug"}}, []string{"bug", "#BUG", "new"}); strings.Join(got, ",") != "Bug,Bug,new" {
		t.Errorf("resolveLabels = %v", got)
	}
	for in, want := range map[string]string{"12": "KB-12", " kb-12 ": "KB-12", "KB-3": "KB-3"} {
		if got := normID("KB", in); got != want {
			t.Errorf("normID(%q) = %q", in, got)
		}
	}
	cl := []store.Check{{Text: "write tests"}, {Text: "write docs"}, {Text: "ship"}}
	if i, err := findItem(cl, "SHIP"); err != nil || i != 2 {
		t.Errorf("findItem text = %d %v", i, err)
	}
	if i, err := findItem(cl, "1"); err != nil || i != 1 {
		t.Errorf("findItem index = %d %v", i, err)
	}
	if _, err := findItem(cl, "write"); err == nil {
		t.Error("ambiguous text should fail")
	}
	if _, err := findItem(cl, "9"); err == nil {
		t.Error("out of range should fail")
	}
	if _, err := findItem(cl[:1], " "); err == nil {
		t.Error("an empty item should fail, not match everything")
	}
}

func TestPickNext(t *testing.T) {
	tasks := []store.Task{
		{ID: "KB-1", Column: "todo", Priority: "low", Pos: 0},
		{ID: "KB-2", Column: "todo", Priority: "urgent", Pos: 1, Human: true},   // human-only: skipped
		{ID: "KB-3", Column: "todo", Priority: "high", Pos: 2, Assignee: "bob"}, // someone else's
		{ID: "KB-4", Column: "todo", Priority: "high", Pos: 3, Due: "2026-12-01"},
		{ID: "KB-5", Column: "todo", Priority: "high", Pos: 4},
		{ID: "KB-6", Column: "doing", Priority: "low", Assignee: "bob"},
	}
	best, reason, alts := pickNext(cols, tasks, "claude")
	if best == nil || best.ID != "KB-4" || len(alts) != 2 || alts[0].ID != "KB-5" {
		t.Fatalf("pickNext = %+v %q %+v", best, reason, alts)
	}
	if !strings.Contains(reason, "WIP limit") {
		t.Errorf("full in-progress column should be mentioned: %q", reason)
	}
	// an unknown priority (imported board) ranks last, not first
	if best, _, _ := pickNext(cols, append(tasks, store.Task{ID: "KB-7", Column: "todo", Priority: "", Pos: -1}), "claude"); best == nil || best.ID != "KB-4" {
		t.Fatalf("unknown priority picked first: %+v", best)
	}
	// a closed task assigned to you is not "in progress"
	withClosed := append([]store.Column{{ID: "closed", Name: "Closed", Role: store.RoleClosed}}, cols...) // Closed first, as on new boards
	if best, _, _ := pickNext(withClosed, append(tasks, store.Task{ID: "KB-8", Column: "closed", Assignee: "claude"}), "claude"); best == nil || best.ID != "KB-4" {
		t.Fatalf("closed task picked as in progress: %+v", best)
	}
	for i, want := range []string{"closed", "todo", "in_progress", "review", "done"} {
		if got := colRole(withClosed, i); got != want {
			t.Errorf("colRole(%s) = %q, want %q", withClosed[i].ID, got, want)
		}
	}
	plain := []store.Column{{ID: "closed", Role: store.RoleClosed}, {ID: "backlog"}, {ID: "next"}, {ID: "shipped", Role: store.RoleDone}}
	if c, ok := colByRole(plain, "in_progress"); !ok || c.ID != "next" {
		t.Errorf("in_progress fallback = %v %v, want next", c.ID, ok)
	}
	if c, ok := colByRole(plain, "todo"); !ok || c.ID != "backlog" {
		t.Errorf("todo = %v %v, want backlog", c.ID, ok)
	}
	tasks = append(tasks, store.Task{ID: "KB-7", Column: "doing", Assignee: "claude"})
	if best, reason, _ := pickNext(cols, tasks, "claude"); best == nil || best.ID != "KB-7" || !strings.Contains(reason, "already") {
		t.Fatalf("own in-progress task first: %+v %q", best, reason)
	}
}

func TestPickNextSubtasks(t *testing.T) {
	tasks := []store.Task{
		{ID: "KB-1", Column: "todo", Priority: "urgent"},              // parent with an open sub-task: skipped
		{ID: "KB-2", Column: "todo", Priority: "low", Parent: "KB-1"}, // the sub-task is the work
		{ID: "KB-3", Column: "todo", Priority: "high"},                // parent whose sub-tasks are all done
		{ID: "KB-4", Column: "done", Parent: "KB-3"},
	}
	best, _, alts := pickNext(cols, tasks, "claude")
	if best == nil || best.ID != "KB-3" || len(alts) != 1 || alts[0].ID != "KB-2" {
		t.Fatalf("pickNext = %+v %+v", best, alts)
	}
	if got := subProgress(cols, tasks); got["KB-1"] != "0/1" || got["KB-3"] != "1/1" {
		t.Errorf("subProgress = %v", got)
	}
	if got := openSubs(cols, tasks); len(got["KB-1"]) != 1 || len(got["KB-3"]) != 0 {
		t.Errorf("openSubs = %v", got)
	}
}

func TestAgentSchema(t *testing.T) {
	for name, sc := range map[string]any{"input": agentSchema[store.TaskInput](), "patch": agentSchema[store.TaskPatch](), "many": agentSchema[CreateMany]()} {
		b, _ := json.Marshal(sc)
		if strings.Contains(string(b), `"due"`) {
			t.Errorf("%s schema still has due: %s", name, b)
		}
		if !strings.Contains(string(b), `"enum":["","urgent","high","medium","low"`) {
			t.Errorf("%s schema has no priority enum: %s", name, b)
		}
	}
}

func TestListPlans(t *testing.T) {
	tasks := []store.Task{
		{ID: "KB-1", Title: "Offline", Plan: "# Offline task list\n\nQueue edits in SQLite so they survive restarts.\n\n## Steps\nUse a sync worker.", PlanStatus: "awaiting", PlanVersion: 2, Updated: 2},
		{ID: "KB-2", Title: "Push", Plan: "# Android 15 push\n\nCreate a notification channel.\n\n## Risks\nOEM battery savers.", PlanStatus: "approved", PlanVersion: 1, Updated: 3},
		{ID: "KB-3", Title: "Page", PlanFormat: "html", PlanStatus: "draft", PlanVersion: 1, Updated: 1,
			Plan: `<!DOCTYPE html><html><head><style>.x{}</style></head><body><h1>Cache &amp; prompts</h1><p class="lede">Reuse the prompt cache.</p><p>offline mention</p></body></html>`},
		{ID: "KB-4", Title: "No plan"},
	}
	out, err := listPlans(tasks, PlansInput{}, "u/")
	if err != nil || !strings.HasPrefix(out, "3 plan(s)") || !strings.Contains(out, `KB-3 {plan v1 draft} "Cache & prompts"`) ||
		!strings.Contains(out, "Reuse the prompt cache.") || !strings.Contains(out, "Queue edits in SQLite so they survive restarts.") ||
		strings.Index(out, "KB-2") > strings.Index(out, "KB-1") {
		t.Fatalf("list = %s %v", out, err)
	}
	out, _ = listPlans(tasks, PlansInput{Query: "offline sqlite"}, "u/")
	if !strings.HasPrefix(out, "2 plan(s)") || strings.Index(out, "KB-1") > strings.Index(out, "KB-3") || strings.Contains(out, "KB-2 ") {
		t.Fatalf("bm25 = %s", out)
	}
	out, _ = listPlans(tasks, PlansInput{Regex: `(?i)oem|channel`}, "u/")
	if !strings.HasPrefix(out, "1 plan(s)") || !strings.Contains(out, "match: Create a notification channel.") {
		t.Fatalf("regex = %s", out)
	}
	if out, _ = listPlans(tasks, PlansInput{Status: "approved"}, "u/"); !strings.HasPrefix(out, "1 plan(s)") {
		t.Fatalf("status = %s", out)
	}
	if _, err := listPlans(tasks, PlansInput{Regex: "("}, "u/"); err == nil {
		t.Fatal("bad regex accepted")
	}
}

func TestPlanText(t *testing.T) {
	page := "<!DOCTYPE html><html><head><style>x{}</style></head><body><nav>On this page</nav><h1>Fix &amp; ship</h1><script>1</script></body></html>"
	if got := planText(page); got != "Fix & ship" {
		t.Errorf("planText = %q", got)
	}
}
