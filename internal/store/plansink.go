package store

import (
	"cmp"
	"log"
	"time"
)

// A PlanSink is a one-way mirror of plans: the board's database stays the source of truth and the sink
// gets a copy each time a plan changes. Repo files (planfile.go) read edits back, so they are not a sink.
// To add a mirror (Notion, D1, …): implement PlanSink and add its planStorage value to sinkFor and SaveSettings.
type PlanSink interface {
	Save(recs []PlanRecord) error // upsert, keyed by Project + TaskID
	Remove(rec PlanRecord) error  // the plan is gone
}

// PlanRecord is one task's latest plan, in terms no backend owns.
type PlanRecord struct {
	Project, ProjectName, TaskID, Title string
	Plan, Format, Status                string
	Version, Approved                   int
	Updated                             time.Time
}

// sinkFor is nil for modes without a mirror; an error when the mirror isn't configured on this machine.
func sinkFor(mode string) (PlanSink, error) {
	switch mode {
	case PlanStorageSupabase:
		return newSupabaseSink()
	}
	return nil, nil
}

func (s *Store) planRecord(t Task) PlanRecord {
	var name string
	s.db.QueryRow(`SELECT name FROM projects WHERE id=?`, s.pid).Scan(&name)
	return PlanRecord{s.pid, name, t.ID, t.Title, t.Plan, t.PlanFormat, t.PlanStatus, t.PlanVersion, t.PlanApproved, time.UnixMilli(cmp.Or(t.PlanUpdated, t.Updated))}
}

// mirrorPlan copies one task's plan to the sink of mode (or removes it once the plan is gone), in the background.
// Failures are logged, never returned: the board is the source of truth and the next change retries.
func (s *Store) mirrorPlan(mode string, t Task) {
	sink, err := sinkFor(mode)
	if err != nil {
		log.Printf("plan storage %s: %v; %s not mirrored", mode, err, t.ID)
	}
	if sink == nil {
		return
	}
	rec := s.planRecord(t)
	go func() {
		var err error
		if t.Plan == "" {
			err = sink.Remove(rec)
		} else {
			err = sink.Save([]PlanRecord{rec})
		}
		if err != nil {
			log.Printf("mirror plan %s (%s): %v", t.ID, mode, err)
		}
	}()
}

// MirrorAllPlans copies every plan of this project, for when a mirror is switched on.
func (s *Store) MirrorAllPlans(mode string) {
	sink, err := sinkFor(mode)
	if err != nil {
		log.Printf("plan storage %s: %v", mode, err)
	}
	if sink == nil {
		return
	}
	tasks, err := s.Tasks()
	if err != nil {
		return
	}
	var recs []PlanRecord
	for _, t := range tasks {
		if t.Plan != "" {
			recs = append(recs, s.planRecord(t))
		}
	}
	go func() {
		if err := sink.Save(recs); err != nil {
			log.Printf("mirror plans (%s): %v", mode, err)
		}
	}()
}
