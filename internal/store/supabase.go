package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// PlanStorageSupabase is the planStorage setting that mirrors plans into a Supabase table through its REST
// API, one row per task (the latest version). One-way: edits made in Supabase don't come back.
// Credentials are per machine, from the environment (see README).
const PlanStorageSupabase = "supabase"

type supabaseSink struct{ url, key, table string }

func newSupabaseSink() (PlanSink, error) {
	u, k := strings.TrimRight(os.Getenv("AGENT_KANBAN_SUPABASE_URL"), "/"), os.Getenv("AGENT_KANBAN_SUPABASE_KEY")
	if u == "" || k == "" {
		return nil, errors.New("AGENT_KANBAN_SUPABASE_URL and AGENT_KANBAN_SUPABASE_KEY are not set")
	}
	t := os.Getenv("AGENT_KANBAN_SUPABASE_TABLE")
	if t == "" {
		t = "kanban_plans"
	}
	return &supabaseSink{u, k, t}, nil
}

// planRow is the table's column set.
type planRow struct {
	Project      string `json:"project"`
	TaskID       string `json:"task_id"`
	ProjectName  string `json:"project_name"`
	Title        string `json:"title"`
	Plan         string `json:"plan"`
	PlanFormat   string `json:"plan_format"`
	PlanStatus   string `json:"plan_status"`
	PlanVersion  int    `json:"plan_version"`
	PlanApproved int    `json:"plan_approved"`
	Updated      string `json:"updated"`
}

func (c *supabaseSink) Save(recs []PlanRecord) error {
	if len(recs) == 0 {
		return nil
	}
	rows := make([]planRow, len(recs))
	for i, r := range recs {
		rows[i] = planRow{r.Project, r.TaskID, r.ProjectName, r.Title, r.Plan, r.Format, r.Status, r.Version, r.Approved, r.Updated.UTC().Format(time.RFC3339)}
	}
	body, _ := json.Marshal(rows)
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/rest/v1/%s?on_conflict=project,task_id", c.url, c.table), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=minimal")
	return c.do(req)
}

func (c *supabaseSink) Remove(r PlanRecord) error {
	req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/rest/v1/%s?project=eq.%s&task_id=eq.%s", c.url, c.table, url.QueryEscape(r.Project), url.QueryEscape(r.TaskID)), nil)
	if err != nil {
		return err
	}
	return c.do(req)
}

var supabaseHTTP = &http.Client{Timeout: 15 * time.Second}

func (c *supabaseSink) do(req *http.Request) error {
	req.Header.Set("apikey", c.key)
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := supabaseHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("supabase %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
