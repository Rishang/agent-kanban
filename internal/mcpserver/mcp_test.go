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
	tasks = append(tasks, store.Task{ID: "KB-7", Column: "doing", Assignee: "claude"})
	if best, reason, _ := pickNext(cols, tasks, "claude"); best == nil || best.ID != "KB-7" || !strings.Contains(reason, "already") {
		t.Fatalf("own in-progress task first: %+v %q", best, reason)
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
