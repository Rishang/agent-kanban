package mcpserver

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/Rishang/agent-kanban/internal/store"
)

type PlansInput struct {
	Query  string `json:"q,omitempty" jsonschema:"search words; plans are ranked by BM25 over title, description and body"`
	Regex  string `json:"regex,omitempty" jsonschema:"RE2 regular expression matched against each plan's text, e.g. (?i)offline|sqlite; shows the first matching line"`
	Status string `json:"status,omitempty" jsonschema:"only plans with this status: draft, awaiting, approved, implemented, implemented-deviations, superseded"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max plans to return (default 20)"`
}

var (
	mdH1Re   = regexp.MustCompile(`(?m)^#\s+(.+)$`)
	htmlH1Re = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	ledeRe   = regexp.MustCompile(`(?is)<p[^>]*class="[^"]*\blede\b[^"]*"[^>]*>(.*?)</p>`)
	tokenRe  = regexp.MustCompile(`[\p{L}\p{N}_]+`)
)

// planDoc is one plan as list_plans sees it: plain text plus the title and one-line description a reviewer would read first.
type planDoc struct {
	task        store.Task
	title, desc string
	text        string
}

func plainLine(s string) string {
	return strings.Join(strings.Fields(planText(s)), " ")
}

func newPlanDoc(t store.Task) planDoc {
	d := planDoc{task: t, title: t.Title}
	if t.PlanFormat == "html" {
		d.text = planText(t.Plan)
		if m := htmlH1Re.FindStringSubmatch(t.Plan); m != nil {
			d.title = plainLine(m[1])
		}
		if m := ledeRe.FindStringSubmatch(t.Plan); m != nil {
			d.desc = plainLine(m[1])
		}
	} else {
		d.text = t.Plan
		rest := t.Plan
		if loc := mdH1Re.FindStringSubmatchIndex(t.Plan); loc != nil {
			d.title, rest = strings.TrimSpace(t.Plan[loc[2]:loc[3]]), t.Plan[loc[1]:]
		}
		// description: the first prose paragraph after the title (skips headings, components, tables, fences, comments)
		for _, para := range strings.Split(rest, "\n\n") {
			p := strings.TrimSpace(para)
			if p != "" && !strings.ContainsAny(p[:1], "#<|`>-") {
				d.desc = plainLine(p)
				break
			}
		}
	}
	if len(d.desc) > 220 {
		d.desc = d.desc[:strings.LastIndex(d.desc[:220], " ")] + "…"
	}
	return d
}

func tokens(s string) []string { return tokenRe.FindAllString(strings.ToLower(s), -1) }

// bm25 scores each doc against the query terms (k1=1.2, b=0.75). The title counts twice.
// ponytail: scored on every call; fine for a board's plans, index them if a board grows to thousands.
func bm25(docs []planDoc, query string) []float64 {
	q := tokens(query)
	tf := make([]map[string]int, len(docs))
	lens := make([]float64, len(docs))
	df := map[string]int{}
	avg := 0.0
	for i, d := range docs {
		tf[i] = map[string]int{}
		ts := tokens(d.title + " " + d.title + " " + d.desc + " " + d.text)
		for _, t := range ts {
			tf[i][t]++
		}
		for t := range tf[i] {
			df[t]++
		}
		lens[i] = float64(len(ts))
		avg += lens[i]
	}
	if len(docs) > 0 {
		avg /= float64(len(docs))
	}
	scores := make([]float64, len(docs))
	n := float64(len(docs))
	for i := range docs {
		for _, t := range q {
			f := float64(tf[i][t])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
			scores[i] += idf * f * 2.2 / (f + 1.2*(0.25+0.75*lens[i]/max(avg, 1)))
		}
	}
	return scores
}

// listPlans renders the board's plans, one line each plus their description, filtered and ranked as asked.
func listPlans(tasks []store.Task, in PlansInput, link string) (string, error) {
	var re *regexp.Regexp
	if in.Regex != "" {
		var err error
		if re, err = regexp.Compile(in.Regex); err != nil {
			return "", fmt.Errorf("bad regex: %w", err)
		}
	}
	var docs []planDoc
	for _, t := range tasks {
		if t.Plan != "" && !t.Human && (in.Status == "" || strings.EqualFold(t.PlanStatus, in.Status)) {
			docs = append(docs, newPlanDoc(t))
		}
	}
	type hit struct {
		d     planDoc
		score float64
		line  string
	}
	var hits []hit
	var scores []float64
	if strings.TrimSpace(in.Query) != "" {
		scores = bm25(docs, in.Query)
	}
	for i, d := range docs {
		h := hit{d: d}
		if scores != nil {
			if h.score = scores[i]; h.score == 0 {
				continue
			}
		}
		if re != nil {
			all := d.title + "\n" + d.desc + "\n" + d.text
			loc := re.FindStringIndex(all)
			if loc == nil {
				continue
			}
			start := strings.LastIndex(all[:loc[0]], "\n") + 1
			end := strings.Index(all[loc[1]:], "\n")
			if end < 0 {
				end = len(all) - loc[1]
			}
			h.line = strings.TrimSpace(all[start : loc[1]+end])
			if len(h.line) > 160 {
				h.line = h.line[:160] + "…"
			}
		}
		hits = append(hits, h)
	}
	if scores != nil {
		slices.SortStableFunc(hits, func(a, b hit) int { return cmpDesc(a.score, b.score) })
	} else {
		slices.SortStableFunc(hits, func(a, b hit) int { return cmpDesc(float64(a.d.task.Updated), float64(b.d.task.Updated)) })
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	total := len(hits)
	hits = hits[:min(len(hits), limit)]
	var o strings.Builder
	fmt.Fprintf(&o, "%d plan(s)", total)
	if total > len(hits) {
		fmt.Fprintf(&o, ", showing %d", len(hits))
	}
	o.WriteString("; get_plan id for the full plan; task url = " + link + "<id>")
	for _, h := range hits {
		t := h.d.task
		fmt.Fprintf(&o, "\n%s {plan v%d %s} %q", t.ID, t.PlanVersion, t.PlanStatus, h.d.title)
		if h.d.title != t.Title {
			fmt.Fprintf(&o, " (task: %q)", t.Title)
		}
		if scores != nil {
			fmt.Fprintf(&o, " score %.2f", h.score)
		}
		if h.d.desc != "" {
			o.WriteString("\n  " + h.d.desc)
		}
		if h.line != "" {
			o.WriteString("\n  match: " + h.line)
		}
	}
	return o.String(), nil
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}
