package gnoperf

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

var prNumberRE = regexp.MustCompile(`\(#(\d+)\)\s*$`)

func commitLink(subject, sha string) string {
	link := fmt.Sprintf("[%s](https://github.com/gnolang/gno/commit/%s)", sha[:7], sha)
	if m := prNumberRE.FindStringSubmatch(subject); m != nil {
		subject = strings.TrimSpace(prNumberRE.ReplaceAllString(subject, ""))
		link = fmt.Sprintf("[#%s](https://github.com/gnolang/gno/pull/%s) · %s", m[1], m[1], link)
	}
	return escapeCell(subject) + " " + link
}

func primaryChanges(r *Report) []Change {
	var out []Change
	for _, c := range r.Changes {
		if primary(c) {
			out = append(out, c)
		}
	}
	return out
}

// Triage writes a weekly triage draft from stored collections, like rustc-perf's /perf/triage.
// It never collects; commits without both collections are listed for sync or backfill.
func (c *Collector) Triage(ctx context.Context, commitRange string, now time.Time, out io.Writer) error {
	from, to, ok := strings.Cut(commitRange, "..")
	if !ok || from == "" || to == "" || strings.Contains(to, "..") {
		return errors.New("triage requires a from..to range")
	}
	base, err := Resolve(ctx, c.Config.GnoCore, from)
	if err != nil {
		return err
	}
	head, err := Resolve(ctx, c.Config.GnoCore, to)
	if err != nil {
		return err
	}
	list, err := git(ctx, c.Config.GnoCore, "log", "--first-parent", "--reverse", "--format=%H%x00%s", base+".."+head)
	if err != nil {
		return err
	}
	load := func(sha string) (*Collection, error) {
		col, err := c.Store.Load(sha)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return col, err
	}
	var review, quiet, missing []string
	var body strings.Builder
	harness := map[string]bool{}
	nondet := map[string]string{}
	var first, last *Collection
	lines := strings.Split(strings.TrimSpace(list), "\n")
	if lines[0] == "" {
		lines = nil
	}
	for _, line := range lines {
		sha, subject, _ := strings.Cut(line, "\x00")
		entry := commitLink(subject, sha)
		parent, err := Resolve(ctx, c.Config.GnoCore, sha+"^1")
		if err != nil {
			return err
		}
		a, err := load(parent)
		if err != nil {
			return err
		}
		b, err := load(sha)
		if err != nil {
			return err
		}
		if a == nil || b == nil {
			missing = append(missing, entry)
			continue
		}
		if first == nil {
			first = a
		}
		last = b
		harness[b.HarnessSHA] = true
		for _, key := range b.Nondeterministic {
			if _, ok := nondet[key]; !ok {
				nondet[key] = sha[:7]
			}
		}
		r, err := Compare(ctx, c.Config.GnoCore, a, b, false)
		if err != nil {
			return err
		}
		primaries := primaryChanges(r)
		v := verdict(r.Changes)
		if !strings.HasSuffix(v, "review needed") {
			if len(r.Warnings) > 0 {
				entry += " — WARNING: " + escapeCell(strings.Join(r.Warnings, "; "))
			}
			quiet = append(quiet, entry)
			continue
		}
		review = append(review, entry)
		fmt.Fprintf(&body, "\n#### %s\n\n%s", entry, v)
		if len(r.Context) > 0 {
			fmt.Fprintf(&body, " · context: %s", strings.Join(r.Context, ", "))
		}
		body.WriteString("\n\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&body, "WARNING: %s\n", escapeCell(w))
		}
		if len(r.Warnings) > 0 {
			body.WriteString("\n")
		}
		writeSummary(&body, "md", primaries)
		for _, category := range tier1Categories[:2] {
			r.writeSection(&body, "md", "#####", category, r.listed(primaries, category))
		}
		body.WriteString("\nVerdict: TODO (intended change / actual regression / classifier error / unclear range)\n")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# gno-perf triage %s\n\nTODO: Summary\n\nTriage done by **@???**.\n", now.Format("2006-01-02"))
	fmt.Fprintf(&b, "Revision range: [%s..%s](https://github.com/gnolang/gno/compare/%s...%s) (%d commits)\n", base[:7], head[:7], base, head, len(lines))
	var harnesses []string
	for h := range harness {
		harnesses = append(harnesses, h)
	}
	sort.Strings(harnesses)
	fmt.Fprintf(&b, "Harness: %s\n", strings.Join(harnesses, ", "))
	b.WriteString("Security check: TODO (SECURITY.md first for anything that could be a gas or storage bypass)\n")
	if first != nil && first != last {
		r, err := Compare(ctx, c.Config.GnoCore, first, last, false)
		if err != nil {
			return err
		}
		if primaries := primaryChanges(r); len(primaries) > 0 {
			fmt.Fprintf(&b, "\n**Summary** (primary, %s → %s):\n\n", first.SHA[:7], last.SHA[:7])
			writeSummary(&b, "md", primaries)
		}
	}
	fmt.Fprintf(&b, "\n%d review needed, %d no action needed, %d not collected\n", len(review), len(quiet), len(missing))
	b.WriteString("\n### Review needed\n")
	if len(review) == 0 {
		b.WriteString("\nNone.\n")
	}
	b.WriteString(body.String())
	for _, section := range []struct {
		title   string
		entries []string
	}{{"No action needed", quiet}, {"Not collected (run sync or backfill)", missing}} {
		if len(section.entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n", section.title)
		for _, e := range section.entries {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	if len(nondet) > 0 {
		b.WriteString("\n### Nondeterministic series\n\n| Series (kind:benchmark:metric) | First seen at commit | Status |\n|---|---|---|\n")
		var keys []string
		for k := range nondet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "| %s | %s | TODO |\n", escapeCell(strings.ReplaceAll(k, "\x00", ":")), nondet[k])
		}
	}
	b.WriteString("\n### Notes\n\n- \n")
	_, err = io.WriteString(out, b.String())
	return err
}
