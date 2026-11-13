// Command local-kanban serves kanban boards for every project from one local Turso
// database, with a web UI and an MCP endpoint for coding agents.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Rishang/local-kanban/internal/api"
	"github.com/Rishang/local-kanban/internal/mcpserver"
	"github.com/Rishang/local-kanban/internal/skill"
	"github.com/Rishang/local-kanban/internal/store"
	"github.com/Rishang/local-kanban/web"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const usage = `local-kanban — kanban boards for humans and coding agents

Usage:
  local-kanban [serve] [--addr 127.0.0.1:7777] [--db FILE]
      Start the server and register the current project (git root, or this folder).
      If a server is already running, just register the project with it.
  local-kanban skill install [--global] [--dir DIR]...
  local-kanban version

Everything lives in the config folder ($LOCAL_KANBAN_CONFIG_PATH, default ~/.config/kanban):
  kanban.db       every project's board
  settings.json   addr, user, syncInterval
  .env            secrets: LOCAL_KANBAN_TURSO_URL, LOCAL_KANBAN_TURSO_TOKEN (optional Turso Cloud sync)

Precedence: flags > environment variables > .env > settings.json.
`

// Settings is settings.json in the config folder.
type Settings struct {
	Addr         string `json:"addr"`
	User         string `json:"user,omitempty"` // defaults to the OS user
	SyncInterval string `json:"syncInterval"`
}

func main() {
	// ponytail: a local single-user board never needs more than 2 cores; halves OS threads (16 → 8).
	// GOMAXPROCS in the environment still wins.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(min(2, runtime.NumCPU()))
	}
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "skill":
		err = installSkill(args)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// configDir returns (and creates) the config folder.
func configDir() (string, error) {
	dir := os.Getenv("LOCAL_KANBAN_CONFIG_PATH")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "kanban")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// loadSettings reads settings.json, writing defaults on first run.
func loadSettings(dir string) (Settings, error) {
	st := Settings{Addr: "127.0.0.1:7777", SyncInterval: "30s"}
	p := filepath.Join(dir, "settings.json")
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		b, _ = json.MarshalIndent(st, "", "  ")
		return st, os.WriteFile(p, append(b, '\n'), 0o600)
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", p, err)
	}
	return st, nil
}

const envTemplate = `# local-kanban secrets. Real environment variables win over this file.
# Optional Turso Cloud sync for every project:
# LOCAL_KANBAN_TURSO_URL=libsql://my-kanban-myorg.turso.io
# LOCAL_KANBAN_TURSO_TOKEN=
`

func serve(args []string) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	envPath := filepath.Join(dir, ".env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		os.WriteFile(envPath, []byte(envTemplate), 0o600)
	}
	if err := loadEnvFile(envPath); err != nil {
		return err
	}
	st, err := loadSettings(dir)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("LOCAL_KANBAN_ADDR", st.Addr), "listen address (loopback only)")
	dbPath := fs.String("db", envOr("LOCAL_KANBAN_DB", filepath.Join(dir, "kanban.db")), "database file")
	fs.Parse(args)

	projDir, projName := currentProject()

	// Listen first: if a server already runs, register this project with it and exit.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		if p, rerr := registerWithRunning(*addr, projName, projDir); rerr == nil {
			fmt.Printf("local-kanban is already running at http://%s\n  project  %s (%s)\n  board    http://%s/#%s\n  mcp      http://%s/mcp?agent=<name>&project=%s\n",
				*addr, p.Name, p.ID, *addr, p.ID, *addr, p.ID)
			return nil
		}
		return err
	}

	sc, err := syncConfig(st)
	if err != nil {
		return err
	}
	me := st.User
	if me == "" {
		me = currentUser()
	}
	d, err := store.Open(*dbPath, me, sc)
	if err != nil {
		if strings.Contains(err.Error(), "locked") {
			return fmt.Errorf("%s is already open by another local-kanban process: %w", *dbPath, err)
		}
		if sc != nil {
			return fmt.Errorf("%w\n\nTurso sync must reach the remote when it first sets up %s. Check the URL, token and network,\nor unset LOCAL_KANBAN_TURSO_URL (env or %s) to run local-only", err, *dbPath, envPath)
		}
		return err
	}
	defer d.Close()
	p, err := d.AddProject(projName, projDir) // returns the existing project for this folder
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base := "http://" + ln.Addr().String()
	srv := &http.Server{
		Handler:           api.Handler(d, web.FS, mcpserver.Handler(d, p.ID, version, base)),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx }, // ends SSE streams on shutdown
	}
	fmt.Printf("local-kanban %s\n  board    %s/#%s\n  project  %s (%s)\n  mcp      %s/mcp?agent=<name>&project=%s\n  config   %s\n",
		version, base, p.ID, p.Name, p.ID, base, p.ID, dir)
	if sc != nil {
		fmt.Printf("  sync     %s every %s\n", d.SyncRemote(), sc.Interval)
		go d.RunSync(ctx)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// currentProject is the git root above the working directory, or the working directory.
func currentProject() (dir, name string) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "project"
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, filepath.Base(d)
		}
		if filepath.Dir(d) == d {
			return wd, filepath.Base(wd)
		}
	}
}

// registerWithRunning adds this project to a local-kanban already listening on addr.
func registerWithRunning(addr, name, dir string) (store.Project, error) {
	var p store.Project
	body, _ := json.Marshal(map[string]string{"name": name, "dir": dir})
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Post("http://"+addr+"/api/projects/add", "application/json", bytes.NewReader(body))
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p, fmt.Errorf("register project: %s", resp.Status)
	}
	return p, json.NewDecoder(resp.Body).Decode(&p)
}

// syncConfig returns nil (local only) unless LOCAL_KANBAN_TURSO_URL is set.
func syncConfig(st Settings) (*store.SyncConfig, error) {
	u := os.Getenv("LOCAL_KANBAN_TURSO_URL")
	if u == "" {
		return nil, nil
	}
	tok := os.Getenv("LOCAL_KANBAN_TURSO_TOKEN")
	if tok == "" {
		return nil, errors.New("LOCAL_KANBAN_TURSO_URL is set but LOCAL_KANBAN_TURSO_TOKEN is empty")
	}
	iv := envOr("LOCAL_KANBAN_SYNC_INTERVAL", st.SyncInterval)
	every, err := time.ParseDuration(iv)
	if err != nil || every < time.Second {
		return nil, fmt.Errorf("sync interval must be a duration of at least 1s (e.g. 30s), got %q", iv)
	}
	return &store.SyncConfig{URL: u, Token: tok, Interval: every}, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func installSkill(args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return errors.New("usage: local-kanban skill install [--global] [--dir DIR]")
	}
	fs := flag.NewFlagSet("skill install", flag.ExitOnError)
	var dirs multiFlag
	global := fs.Bool("global", false, "install for your user (~/.claude/skills, ~/.codex/skills) instead of the current project")
	fs.Var(&dirs, "dir", "skills root to install into (repeatable); overrides the default")
	fs.Parse(args[1:])
	if len(dirs) == 0 {
		base, err := os.Getwd() // ponytail: cwd is the project root; no git-root lookup
		if *global {
			base, err = os.UserHomeDir()
		}
		if err != nil {
			return err
		}
		dirs = append(dirs, filepath.Join(base, ".claude", "skills"))
		if _, err := os.Stat(filepath.Join(base, ".codex")); err == nil {
			dirs = append(dirs, filepath.Join(base, ".codex", "skills"))
		}
	}
	paths, err := skill.Install(dirs...)
	for _, p := range paths {
		fmt.Println("installed", p)
	}
	return err
}

// currentUser is the member name the web UI acts as.
func currentUser() string {
	name := "me"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	name = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(strings.ToLower(name), "-")
	name = strings.Trim(name, "-_")
	if name == "" {
		return "me"
	}
	return name[:min(len(name), 32)]
}
