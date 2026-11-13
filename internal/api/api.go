// Package api serves the web UI, its JSON API and the live event stream.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Rishang/local-kanban/internal/store"
)

type opFunc func(s *store.Store, who string, body []byte) (any, error)

// op adapts a typed store call to the generic POST /api/p/{project}/{op} handler.
func op[T any](fn func(s *store.Store, who string, in T) (any, error)) opFunc {
	return func(s *store.Store, who string, body []byte) (any, error) {
		var in T
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("bad request body: %w", err)
		}
		return fn(s, who, in)
	}
}

func ok(err error) (any, error) { return map[string]bool{"ok": err == nil}, err }

var ops = map[string]opFunc{
	"create_task": op(func(s *store.Store, who string, in store.TaskInput) (any, error) { return s.CreateTask(who, "ui", in) }),
	"update_task": op(func(s *store.Store, who string, in store.TaskPatch) (any, error) { return s.UpdateTask(who, "ui", in) }),
	"move_task":   op(func(s *store.Store, who string, in store.MoveInput) (any, error) { return s.MoveTask(who, "ui", in) }),
	"delete_task": op(func(s *store.Store, who string, in store.IDInput) (any, error) {
		return ok(s.DeleteTask(who, "ui", in.ID))
	}),
	"add_comment": op(func(s *store.Store, who string, in store.CommentInput) (any, error) {
		return s.AddComment(who, "ui", in)
	}),
	"set_human": op(func(s *store.Store, who string, in struct {
		ID    string `json:"id"`
		Human bool   `json:"human"`
	}) (any, error) {
		return s.UpdateTask(who, "ui", store.TaskPatch{ID: in.ID, Human: &in.Human})
	}),

	"save_settings":    op(func(s *store.Store, who string, in store.Settings) (any, error) { return ok(s.SaveSettings(who, in)) }),
	"save_columns":     op(func(s *store.Store, who string, in []store.Column) (any, error) { return ok(s.SaveColumns(who, in)) }),
	"save_members":     op(func(s *store.Store, who string, in []store.Member) (any, error) { return ok(s.SaveMembers(who, in)) }),
	"save_labels":      op(func(s *store.Store, who string, in []store.Label) (any, error) { return ok(s.SaveLabels(who, in)) }),
	"rename_label":     op(func(s *store.Store, who string, in store.RenameInput) (any, error) { return ok(s.RenameLabel(who, in)) }),
	"delete_all_tasks": op(func(s *store.Store, who string, _ struct{}) (any, error) { return ok(s.DeleteAllTasks(who)) }),
	"import":           op(func(s *store.Store, who string, in store.Board) (any, error) { return ok(s.Import(who, in)) }),
}

// Msg is one SSE message: an event tagged with its project ("" = project list or whole-db change).
type Msg struct {
	Project string `json:"project"`
	store.Event
}

type hub struct {
	mu   sync.Mutex
	subs map[chan Msg]struct{}
}

func (h *hub) publish(project string, e store.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- Msg{project, e}:
		default: // slow client: it resyncs on the next message
		}
	}
}

// ProjectSummary is a project with its open/total task counts.
type ProjectSummary struct {
	store.Project
	Open  int `json:"open"`
	Total int `json:"total"`
}

// Handler wires routes. mcp is mounted at /mcp.
func Handler(d *store.DB, web fs.FS, mcp http.Handler) http.Handler {
	h := &hub{subs: map[chan Msg]struct{}{}}
	d.Notify = h.publish

	mux := http.NewServeMux()
	static := http.FileServerFS(web)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // embedded files have no mtime; always revalidate after upgrades
		static.ServeHTTP(w, r)
	}))
	mux.Handle("/mcp", mcp)

	// ---- projects
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		ps, err := d.Projects()
		if err != nil {
			reply(w, nil, err)
			return
		}
		out := []ProjectSummary{}
		for _, p := range ps {
			sum := ProjectSummary{Project: p}
			if s, err := d.Project(p.ID); err == nil {
				if open, total, err := s.Counts(); err == nil {
					sum.Open, sum.Total = open, total
				}
			}
			out = append(out, sum)
		}
		reply(w, map[string]any{"projects": out, "me": d.Me(), "db": d.Path()}, nil)
	})
	mux.HandleFunc("POST /api/projects/add", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Dir string }
		if err := decode(w, r, &in); err != nil {
			reply(w, nil, err)
			return
		}
		p, err := d.AddProject(in.Name, in.Dir)
		reply(w, p, err)
	})
	mux.HandleFunc("POST /api/projects/rename", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID, Name string }
		if err := decode(w, r, &in); err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, nil, d.RenameProject(in.ID, in.Name))
	})
	mux.HandleFunc("POST /api/projects/remove", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID string }
		if err := decode(w, r, &in); err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, nil, d.RemoveProject(in.ID))
	})

	// ---- search across every project
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		hits, err := d.Search(r.URL.Query().Get("q"), 40)
		reply(w, hits, err)
	})

	// ---- one board
	withStore := func(fn func(w http.ResponseWriter, r *http.Request, s *store.Store)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s, err := d.Project(r.PathValue("pid"))
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			fn(w, r, s)
		}
	}
	mux.HandleFunc("GET /api/p/{pid}/board", withStore(func(w http.ResponseWriter, r *http.Request, s *store.Store) {
		b, err := s.Board()
		reply(w, b, err)
	}))
	mux.HandleFunc("GET /api/p/{pid}/export", withStore(func(w http.ResponseWriter, r *http.Request, s *store.Store) {
		b, err := s.Board()
		b.Events, b.Seen, b.Sync = nil, nil, nil
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kanban-%s.json"`, s.ProjectID()))
		reply(w, b, err)
	}))
	mux.HandleFunc("POST /api/p/{pid}/{op}", withStore(func(w http.ResponseWriter, r *http.Request, s *store.Store) {
		fn, found := ops[r.PathValue("op")]
		if !found {
			http.Error(w, "unknown op", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
		if err != nil {
			reply(w, nil, err)
			return
		}
		out, err := fn(s, d.Me(), body)
		reply(w, out, err)
	}))

	// ---- server-sent events for every project; the UI refetches what changed
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, canFlush := w.(http.Flusher)
		if !canFlush {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan Msg, 64)
		h.mu.Lock()
		h.subs[ch] = struct{}{}
		h.mu.Unlock()
		defer func() { h.mu.Lock(); delete(h.subs, ch); h.mu.Unlock() }()
		fmt.Fprint(w, "retry: 2000\n\n")
		fl.Flush()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case m := <-ch:
				b, _ := json.Marshal(m)
				fmt.Fprintf(w, "data: %s\n\n", b)
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
			}
			fl.Flush()
		}
	})

	return localOnly(mux)
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if v == nil {
		v = map[string]bool{"ok": true}
	}
	json.NewEncoder(w).Encode(v)
}

// localOnly rejects non-loopback Host headers (DNS rebinding) and non-JSON
// API posts (cross-site form posts can't set application/json without CORS).
// ponytail: loopback only; add an --allow-host flag if LAN access is ever wanted.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch strings.Trim(host, "[]") {
		case "localhost", "127.0.0.1", "::1":
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/") &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}
