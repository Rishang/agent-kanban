// Package skill syncs the agent skill that teaches the board workflow.
//
// The skill text is not compiled into the binary. It is fetched from the repository so a
// skill edit reaches users without a release.
package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// DefaultURL is the skill on the repository's main branch.
const DefaultURL = "https://raw.githubusercontent.com/Rishang/agent-kanban/main/skills/kanban/SKILL.md"

const maxSize = 256 << 10 // a skill is a short markdown file; refuse anything bigger

// Fetch downloads the skill from rawURL. Only https is accepted, except plain http to a
// loopback host so tests and local mirrors work.
func Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bad skill URL: %w", err)
	}
	if err := checkScheme(u); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// Redirects get the same scheme check, so an https URL cannot bounce to plain http.
	client := &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return checkScheme(r.URL)
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch skill: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch skill: %s returned %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("fetch skill: %w", err)
	}
	if len(body) > maxSize {
		return nil, fmt.Errorf("fetch skill: response is larger than %d bytes", maxSize)
	}
	// A skill starts with YAML front matter; this catches HTML error pages and proxies.
	if !bytes.HasPrefix(body, []byte("---\n")) && !bytes.HasPrefix(body, []byte("---\r\n")) {
		return nil, fmt.Errorf("fetch skill: %s is not a SKILL.md (no front matter)", rawURL)
	}
	return body, nil
}

func checkScheme(u *url.URL) error {
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("skill URL must be https: %s", u)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Install writes content to <root>/kanban/SKILL.md for each root and returns the written paths.
func Install(content []byte, roots ...string) ([]string, error) {
	var out []string
	for _, r := range roots {
		dir := filepath.Join(r, "kanban")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return out, err
		}
		p := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}
