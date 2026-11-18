package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// PlanStatuses is a plan's lifecycle, from the html-plan-report skill:
// draft → awaiting → approved → implemented (or implemented-deviations); superseded when re-planned.
var PlanStatuses = []string{"draft", "awaiting", "approved", "implemented", "implemented-deviations", "superseded"}

// PlanApproved is the status only a human may set (from the board UI).
const PlanApproved = "approved"

type PlanInput struct {
	ID     string  `json:"id" jsonschema:"task id"`
	Path   string  `json:"path,omitempty" jsonschema:"plan file inside the project folder (relative or absolute): .md/.mdx, or an html-plan-report page (its _assets are inlined); cheaper than body for long plans"`
	Body   *string `json:"body,omitempty" jsonschema:"the plan as markdown, MDX or a standalone HTML page instead of path (replaces the old one; empty string removes the plan)"`
	Status string  `json:"status,omitempty" jsonschema:"draft, awaiting, implemented, implemented-deviations or superseded (approved is set by a human on the board)"`
}

var (
	frontRe   = regexp.MustCompile(`(?s)\A\s*---\r?\n(.*?)\r?\n---[ \t]*(\r?\n|\z)`)
	statusRe  = regexp.MustCompile(`(?m)^status:\s*["']?([a-z-]+)["']?\s*$`)
	mdxLineRe = regexp.MustCompile(`(?m)^(import|export)\s.*$\n?`) // ponytail: drops top-level MDX import/export lines; JSX tags are left to the sanitizer
	htmlStRe  = regexp.MustCompile(`data-status="([a-z-]+)"`)      // html-plan-report's status badge
)

// MaxPlan is the largest plan body accepted. A standalone html-plan-report page is about 70 KB.
const MaxPlan = 1 << 20

// IsHTMLPlan reports whether a plan is a full HTML page (html-plan-report) rather than markdown.
func IsHTMLPlan(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s[:min(len(s), 64)]))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html")
}

// ParsePlan turns a markdown or MDX plan into the stored body: front matter and MDX import/export lines
// are removed, and front matter's `status:` is returned.
func ParsePlan(src string) (body, status string) {
	if IsHTMLPlan(src) {
		if m := htmlStRe.FindStringSubmatch(src); m != nil {
			status = m[1]
		}
		return strings.TrimSpace(src), status
	}
	if m := frontRe.FindStringSubmatch(src); m != nil {
		if sm := statusRe.FindStringSubmatch(m[1]); sm != nil {
			status = sm[1]
		}
		src = src[len(m[0]):]
	}
	return strings.TrimSpace(mdxLineRe.ReplaceAllString(src, "")), status
}

// SetPlan writes a task's plan and/or its status. A new body bumps the version and, unless a status is given,
// asks for review again (awaiting). Only the UI may approve: an agent can't sign off its own plan.
// With planStorage = files the plan is also a file in the repo (see planfile.go), written in the same transaction.
func (s *Store) SetPlan(who, via string, in PlanInput) (t Task, err error) {
	err = s.write(who, via, func(tx *sql.Tx) (Event, error) {
		if t, err = getTask(tx, s.pid, in.ID); err != nil {
			return Event{}, err
		}
		st, err := getSettings(tx, s.pid)
		if err != nil {
			return Event{}, err
		}
		var dir string
		if err := tx.QueryRow(`SELECT dir FROM projects WHERE id=?`, s.pid).Scan(&dir); err != nil {
			return Event{}, err
		}
		files := st.PlanStorage == PlanStorageFiles
		if in.Path != "" {
			if in.Body != nil {
				return Event{}, errors.New("give path or body, not both")
			}
			content, rel, err := ReadPlanFile(dir, in.Path)
			if err != nil {
				return Event{}, err
			}
			in.Body = &content
			if files {
				t.PlanPath = rel // linked: later edits to the file come back as new versions
			}
		}
		status := strings.ToLower(strings.TrimSpace(in.Status))
		if in.Body == nil && status == "" {
			return Event{}, errors.New("give body, status or both")
		}
		changed := false
		if in.Body != nil {
			body, fm := ParsePlan(*in.Body)
			if len(body) > MaxPlan {
				return Event{}, errors.New("plan is over 1 MB; trim it or split the task")
			}
			if fm == PlanApproved && via != "ui" {
				fm = "" // a document can't approve itself; the board decides
			}
			if body == "" { // remove; a linked file is left alone, only unlinked
				_, err := tx.Exec(`UPDATE tasks SET plan='',plan_status='',plan_version=0,plan_path='',plan_approved=0,updated=? WHERE project=? AND id=?`, time.Now().UnixMilli(), s.pid, t.ID)
				t.Plan, t.PlanStatus, t.PlanVersion, t.PlanPath, t.PlanApproved = "", "", 0, "", 0
				return Event{Action: "removed the plan of", TaskID: t.ID}, err
			}
			old := t.Plan
			if status == "" && body != old {
				status = fm // a document's own status counts only when its content changed: relinking doesn't reset approval
			}
			if changed = body != t.Plan; changed && files && t.PlanPath != "" && in.Path == "" && via != "file" {
				// The new text is based on the stored copy; if the file moved on since, writing would lose that edit.
				if cur, _, err := ReadPlanFile(dir, t.PlanPath); err == nil {
					if fb, _ := ParsePlan(cur); fb != old {
						return Event{}, fmt.Errorf("%s changed in the repo since the board last read it; reload to see it, then edit again", t.PlanPath)
					}
				}
			}
			if changed {
				t.Plan, t.PlanVersion, t.PlanFormat = body, t.PlanVersion+1, ""
				if IsHTMLPlan(body) {
					t.PlanFormat = "html"
				}
				if status == "" {
					status = "awaiting"
				}
			}
		}
		if t.Plan == "" {
			return Event{}, fmt.Errorf("%s has no plan yet; send its body first", t.ID)
		}
		if status == "" {
			status = t.PlanStatus
		}
		if !slices.Contains(PlanStatuses, status) {
			return Event{}, fmt.Errorf("plan status %q is not one of: %s", status, strings.Join(PlanStatuses, ", "))
		}
		if status == PlanApproved && via != "ui" && (changed || t.PlanStatus != PlanApproved) {
			return Event{}, errors.New("only a human can approve a plan, on the board; set status awaiting and ask the user to review it")
		}
		if strings.HasPrefix(status, "implemented") && via != "ui" && t.PlanApproved == 0 && status != t.PlanStatus {
			return Event{}, errors.New("this plan was never approved; it can't be implemented yet. Ask the user to approve it on the board")
		}
		if status == PlanApproved {
			t.PlanApproved = t.PlanVersion
		}
		if files && t.PlanPath == "" && dir != "" && !IsHTMLPlan(t.Plan) {
			t.PlanPath = newPlanPath(t) // files mode: a plan without a file gets one
		}
		statusChanged := status != t.PlanStatus
		if !changed && !statusChanged && in.Path == "" {
			return Event{}, nil // nothing to do: no event
		}
		t.PlanStatus, t.Updated = status, time.Now().UnixMilli()
		if _, err := tx.Exec(`UPDATE tasks SET plan=?,plan_status=?,plan_version=?,plan_path=?,plan_approved=?,updated=? WHERE project=? AND id=?`,
			t.Plan, t.PlanStatus, t.PlanVersion, t.PlanPath, t.PlanApproved, t.Updated, s.pid, t.ID); err != nil {
			return Event{}, err
		}
		if files && t.PlanPath != "" {
			if err := writePlanFile(dir, t.PlanPath, t.Plan, t.PlanStatus, !changed); err != nil {
				return Event{}, fmt.Errorf("write %s: %w", t.PlanPath, err)
			}
		}
		if !changed && !statusChanged {
			return Event{Action: "linked the plan of", TaskID: t.ID, Detail: t.PlanPath}, nil
		}
		detail := fmt.Sprintf("v%d · %s", t.PlanVersion, status)
		if changed {
			return Event{Action: "updated the plan of", TaskID: t.ID, Detail: detail}, nil
		}
		return Event{Action: "marked the plan of", TaskID: t.ID, Detail: detail}, nil
	})
	return
}

// RefreshPlans reads every linked plan file of this project and records the ones edited in the repo as a
// new version (needing approval again). Files that are missing or unreadable keep the stored copy.
// ponytail: reads each linked file on every call; a board has a handful, add an mtime check if it has hundreds.
func (s *Store) RefreshPlans() {
	var dir string
	if s.db.QueryRow(`SELECT dir FROM projects WHERE id=?`, s.pid).Scan(&dir) != nil || dir == "" {
		return
	}
	if st, err := getSettings(s.db, s.pid); err != nil || st.PlanStorage != PlanStorageFiles {
		return
	}
	rows, err := s.db.Query(`SELECT id,plan_path,plan FROM tasks WHERE project=? AND plan_path<>''`, s.pid)
	if err != nil {
		return
	}
	type linked struct{ id, path, plan string }
	var ls []linked
	for rows.Next() {
		var l linked
		if rows.Scan(&l.id, &l.path, &l.plan) == nil {
			ls = append(ls, l)
		}
	}
	rows.Close()
	for _, l := range ls {
		content, _, err := ReadPlanFile(dir, l.path)
		if err != nil {
			continue
		}
		if body, _ := ParsePlan(content); body != l.plan && body != "" {
			s.SetPlan(s.me, "file", PlanInput{ID: l.id, Body: &content})
		}
	}
}
