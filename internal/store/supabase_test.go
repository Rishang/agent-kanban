package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSupabaseMirror(t *testing.T) {
	type req struct{ method, path, key, prefer, body string }
	got := make(chan req, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- req{r.Method, r.URL.RequestURI(), r.Header.Get("apikey"), r.Header.Get("Prefer"), string(b)}
	}))
	defer srv.Close()
	t.Setenv("AGENT_KANBAN_SUPABASE_URL", srv.URL+"/")
	t.Setenv("AGENT_KANBAN_SUPABASE_KEY", "secret")

	db, err := Open(filepath.Join(t.TempDir(), "k.db"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("Mirror", "")
	s, _ := db.Project(p.ID)
	task, _ := s.CreateTask("me", "ui", TaskInput{Title: "feature"})
	next := func() req {
		select {
		case r := <-got:
			return r
		case <-time.After(3 * time.Second):
			t.Fatal("no request to Supabase")
			return req{}
		}
	}

	s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# Plan")}) // storage is the board: nothing is pushed
	st, _ := getSettings(db.db, s.pid)
	st.PlanStorage = PlanStorageSupabase
	if err := s.SaveSettings("me", st); err != nil {
		t.Fatal(err)
	}
	r := next() // switching on pushes the existing plan
	if r.method != "POST" || r.path != "/rest/v1/kanban_plans?on_conflict=project,task_id" || r.key != "secret" || r.prefer == "" {
		t.Fatalf("backfill request: %+v", r)
	}
	var rows []planRow
	if json.Unmarshal([]byte(r.body), &rows) != nil || len(rows) != 1 || rows[0].TaskID != task.ID || rows[0].Plan != "# Plan" || rows[0].ProjectName != "Mirror" {
		t.Fatalf("backfill rows: %s", r.body)
	}

	s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("# Plan v2")})
	if r = next(); json.Unmarshal([]byte(r.body), &rows) != nil || rows[0].PlanVersion != 2 || rows[0].PlanStatus != "awaiting" {
		t.Fatalf("revision: %s", r.body)
	}
	s.SetPlan("bot", "mcp", PlanInput{ID: task.ID, Body: ptr("")})
	if r = next(); r.method != "DELETE" {
		t.Fatalf("removing the plan: %+v", r)
	}
}
