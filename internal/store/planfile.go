package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Plan files. A plan sent as a path is read from the project's folder. With the project setting
// planStorage = "files" the task stays linked to that file (tasks.plan_path): edits in the repo come back
// as new versions (RefreshPlans), and board edits and status changes are written to the file, so a spec kept
// in git shows what was approved. The database copy stays either way, for sync and export.

// PlanStorageFiles is the planStorage setting that keeps plans as files in the repo, under PlanDir.
const PlanStorageFiles = "files"

// PlanDir is the repo folder for plan files.
const PlanDir = ".agent-kanban/plans"

var (
	cssLinkRe = regexp.MustCompile(`<link rel="stylesheet" href="_assets/plan\.css">\n?`)
	jsLinkRe  = regexp.MustCompile(`<script src="_assets/plan\.js"( defer)?></script>\n?`)
	backRe    = regexp.MustCompile(`\s*<a class="back" href="index\.html">[^<]*</a>`)
	mdxHeadRe = regexp.MustCompile(`\A(?:[ \t]*(?:import|export)\s[^\n]*\n|[ \t]*\n)*`) // leading MDX import/export lines
	fmStatus  = regexp.MustCompile(`(?m)^status:.*$`)
	slugRe2   = regexp.MustCompile(`[^a-z0-9]+`)
)

// planFile resolves path (relative to dir, or absolute) to a file inside dir and returns it with its
// path relative to dir. Symlinks may not lead outside. The file need not exist yet.
func planFile(dir, path string) (abs, rel string, err error) {
	if dir == "" {
		return "", "", errors.New("this project has no folder on disk; send the plan as body instead")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	outside := fmt.Errorf("path must be inside the project folder %s", dir)
	inside := func(root, p string) bool {
		r, err := filepath.Rel(root, p)
		return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
	}
	if !inside(dir, path) { // as written, then again with symlinks resolved
		return "", "", outside
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	// Resolve the nearest existing ancestor (the file itself if it exists), so a symlinked folder on the
	// way can't lead outside; the parts that don't exist yet are joined back unresolved.
	existing, rest := path, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", "", outside
		}
		rest, existing = filepath.Join(filepath.Base(existing), rest), parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", "", err
	}
	real := filepath.Join(resolved, rest)
	if !inside(root, real) {
		return "", "", outside
	}
	rel, _ = filepath.Rel(root, real)
	return real, filepath.ToSlash(rel), nil
}

func planExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".mdx", ".markdown", ".html":
		return true
	}
	return false
}

// ReadPlanFile reads a plan file inside dir. An html-plan-report page gets its _assets inlined (bundle_plan.py).
func ReadPlanFile(dir, path string) (content, rel string, err error) {
	if !planExt(path) {
		return "", "", errors.New("path must be a .md, .mdx or .html file")
	}
	abs, rel, err := planFile(dir, path)
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxPlan+1))
	if err != nil || !strings.EqualFold(filepath.Ext(abs), ".html") {
		return string(b), rel, err
	}
	page, err := bundlePlan(string(b), dir, filepath.Dir(abs))
	return page, rel, err
}

// bundlePlan makes an html-plan-report page standalone, like the skill's bundle_plan.py:
// it inlines _assets/plan.css and plan.js from beside the page (pageDir; both must resolve inside the project
// folder root, like the page) and drops the "← All plans" link.
func bundlePlan(page, root, pageDir string) (string, error) {
	if !cssLinkRe.MatchString(page) && !jsLinkRe.MatchString(page) {
		return page, nil // already standalone
	}
	asset := func(name string) (string, error) {
		abs, _, err := planFile(root, filepath.Join(pageDir, "_assets", name))
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return "", fmt.Errorf("%w (run the skill's update_index.py, or bundle_plan.py, first)", err)
		}
		return string(b), nil
	}
	css, err := asset("plan.css")
	if err != nil {
		return "", err
	}
	js, err := asset("plan.js")
	if err != nil {
		return "", err
	}
	page = cssLinkRe.ReplaceAllLiteralString(page, "<style>\n"+css+"</style>\n")
	page = jsLinkRe.ReplaceAllLiteralString(page, "<script>\n"+js+"</script>\n")
	page = backRe.ReplaceAllString(page, "")
	return strings.Replace(page, "<head>", "<head>\n<meta name=\"plan-bundle\" content=\"standalone\">", 1), nil
}

// renderPlanFile is the file for a markdown plan: the old file's front matter (status: set) and MDX
// import/export lines, then body. A file without front matter gets one holding the status.
func renderPlanFile(old, body, status string) string {
	front, rest := "", old
	if m := frontRe.FindString(old); m != "" {
		front, rest = m, old[len(m):]
	}
	front = setFileStatus(front, status)
	head := strings.TrimSpace(mdxHeadRe.FindString(rest))
	if head != "" {
		head += "\n"
	}
	return front + head + "\n" + body + "\n"
}

// setFileStatus sets status: in the file's front matter (adding one if needed) and leaves everything else.
func setFileStatus(old, status string) string {
	m := frontRe.FindStringSubmatch(old)
	if m == nil {
		return "---\nstatus: " + status + "\n---\n" + old
	}
	inner := m[1]
	if fmStatus.MatchString(inner) {
		inner = fmStatus.ReplaceAllLiteralString(inner, "status: "+status)
	} else {
		inner = strings.TrimRight(inner, "\n") + "\nstatus: " + status
	}
	return "---\n" + inner + "\n---\n" + old[len(m[0]):]
}

// writePlanFile updates a linked markdown plan file; HTML pages are only read, never rewritten.
// statusOnly changes just the front matter's status, leaving the rest of the file as it is on disk.
func writePlanFile(dir, rel, body, status string, statusOnly bool) error {
	if IsHTMLPlan(body) {
		return nil
	}
	if filepath.IsAbs(rel) || !planExt(rel) || strings.EqualFold(filepath.Ext(rel), ".html") { // plan_path can arrive by sync: only ever write markdown plan files
		return fmt.Errorf("%s is not a markdown plan file", rel)
	}
	abs, _, err := planFile(dir, rel)
	if err != nil {
		return err
	}
	// planFile resolves symlinks: refuse a linked plan path that is a symlink, or resolves to a non-markdown file.
	if fi, err := os.Lstat(filepath.Join(dir, rel)); (err == nil && fi.Mode()&os.ModeSymlink != 0) || !planExt(abs) || strings.EqualFold(filepath.Ext(abs), ".html") {
		return fmt.Errorf("%s is a symlink or not a markdown plan file; plan files must be regular files", rel)
	}
	old, err := os.ReadFile(abs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next := renderPlanFile(string(old), body, status)
	if statusOnly && len(old) > 0 {
		next = setFileStatus(string(old), status)
	}
	if next == string(old) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// A fresh, exclusively created temp file (never a fixed name a symlink could sit on), then an atomic
	// rename: an editor never sees half a file.
	f, err := os.CreateTemp(filepath.Dir(abs), ".plan-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(next); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chmod(tmp, 0o644)
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// newPlanPath is where a plan created on the board goes in files mode: PlanDir/<id>-<title>.mdx.
func newPlanPath(t Task) string {
	slug := strings.Trim(slugRe2.ReplaceAllString(strings.ToLower(t.Title), "-"), "-")
	return PlanDir + "/" + strings.ToLower(t.ID) + "-" + slug[:min(len(slug), 48)] + ".mdx"
}
