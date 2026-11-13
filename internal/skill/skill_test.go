package skill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serve(t *testing.T, status int, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func redirect(t *testing.T, to string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, http.StatusFound)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestFetch(t *testing.T) {
	const good = "---\nname: kanban\n---\n# hi\n"
	got, err := Fetch(context.Background(), serve(t, 200, good))
	if err != nil || string(got) != good {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchFollowsSafeRedirect(t *testing.T) {
	const good = "---\nname: kanban\n---\n"
	got, err := Fetch(context.Background(), redirect(t, serve(t, 200, good)))
	if err != nil || string(got) != good {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchRejects(t *testing.T) {
	cases := map[string]string{
		"not found":        serve(t, 404, "nope"),
		"html page":        serve(t, 200, "<html>captive portal</html>"),
		"too big":          serve(t, 200, "---\n"+strings.Repeat("x", maxSize)),
		"plain http":       "http://example.com/SKILL.md",
		"ftp scheme":       "ftp://example.com/SKILL.md",
		"redirect to http": redirect(t, "http://example.com/SKILL.md"),
	}
	for name, u := range cases {
		if _, err := Fetch(context.Background(), u); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInstall(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	paths, err := Install([]byte("---\nx\n"), a, b)
	if err != nil || len(paths) != 2 {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	if got, _ := os.ReadFile(filepath.Join(a, "kanban", "SKILL.md")); string(got) != "---\nx\n" {
		t.Fatalf("content %q", got)
	}
}
