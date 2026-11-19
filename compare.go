package gnoperf

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/perf/benchmath"
)

type Change struct {
	Kind, Benchmark, Metric, Category, Size string
	Base, Head, Delta, Percent              float64
	Tags                                    []string
	Statistics                              string
	Count                                   int
}

type Report struct {
	Base, Head *Collection
	Warnings   []string
	Changes    []Change
	Title      string
}

type snapshot struct {
	files   map[string][]byte
	deps    map[string][]string
	modules map[string]string
}

func (s *snapshot) packageOf(name string) string {
	dir := path.Dir(name)
	for owner := dir; owner != "." && owner != "examples"; owner = path.Dir(owner) {
		if module := s.modules[owner]; module != "" {
			return module
		}
	}
	return strings.TrimSuffix(strings.TrimPrefix(dir, "examples/"), "/filetests")
}

func readSnapshot(ctx context.Context, repo, sha string) (*snapshot, error) {
	data, err := output(ctx, repo, nil, "git", "archive", sha, "examples")
	if err != nil {
		return nil, err
	}
	s := &snapshot{files: map[string][]byte{}, deps: map[string][]string{}, modules: map[string]string{}}
	r := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		s.files[h.Name] = b
		if path.Base(h.Name) == "gnomod.toml" {
			var m struct {
				Module string `toml:"module"`
			}
			if err := toml.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			if m.Module != "" {
				s.modules[path.Dir(h.Name)] = m.Module
			}
		}
	}
	for name, data := range s.files {
		if !strings.HasSuffix(name, ".gno") {
			continue
		}
		pkg := s.packageOf(name)
		file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("imports in %s: %w", name, err)
		}
		for _, imp := range file.Imports {
			value, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, err
			}
			s.deps[pkg] = append(s.deps[pkg], value)
		}
	}
	return s, nil
}

func (s *snapshot) affected(pkg string, changed map[string]bool) bool {
	// Tests depend on their package's implementation as well as its imports.
	if changed[pkg] {
		return true
	}
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(p string) bool {
		if seen[p] {
			return false
		}
		seen[p] = true
		for _, dep := range s.deps[p] {
			if changed[dep] || visit(dep) {
				return true
			}
		}
		return false
	}
	return visit(pkg)
}

func changedPackages(base, head *snapshot) map[string]bool {
	changed := map[string]bool{}
	files := map[string]bool{}
	for f := range base.files {
		files[f] = true
	}
	for f := range head.files {
		files[f] = true
	}
	for f := range files {
		if strings.HasSuffix(f, "_test.gno") || strings.HasSuffix(f, "_filetest.gno") {
			continue
		}
		if !strings.HasSuffix(f, ".gno") && path.Base(f) != "gnomod.toml" {
			continue
		}
		if !bytes.Equal(base.files[f], head.files[f]) {
			for _, s := range []*snapshot{base, head} {
				changed[s.packageOf(f)] = true
			}
		}
	}
	return changed
}

var gasWantedRE = regexp.MustCompile(`(-gas-wanted\s+|GAS WANTED:\s*)[0-9][0-9_]*`)

func classification(ctx context.Context, repo string, base, head *Collection, changed []string, bs, hs *snapshot, packages map[string]bool, a, b Measurement) ([]string, error) {
	var tags []string
	if a.InputHash != "" && b.InputHash != "" && a.InputHash != b.InputHash {
		gasOnly := false
		if a.Kind == "tx" && a.Source != "" && b.Source != "" {
			before, err := git(ctx, repo, "show", base.SHA+":"+a.Source)
			if err != nil {
				return nil, err
			}
			after, err := git(ctx, repo, "show", head.SHA+":"+b.Source)
			if err != nil {
				return nil, err
			}
			gasOnly = gasWantedRE.ReplaceAllString(before, "${1}<gas>") == gasWantedRE.ReplaceAllString(after, "${1}<gas>")
		}
		if gasOnly {
			tags = append(tags, "gaswanted-only")
		} else {
			tags = append(tags, "test-changed")
		}
	}
	pkg := strings.SplitN(b.Benchmark, "#", 2)[0]
	if b.Kind == "filetest" {
		pkg = path.Dir(pkg)
	}
	if (bs != nil && bs.affected(pkg, packages)) || (hs != nil && hs.affected(pkg, packages)) {
		tags = append(tags, "dep-changed")
	}
	for _, name := range changed {
		if strings.HasPrefix(name, "gnovm/stdlibs/") || strings.HasPrefix(name, "gnovm/tests/stdlibs/") {
			tags = append(tags, "stdlib-changed")
		}
		if name == "gnovm/pkg/gnolang/machine.go" || name == "gnovm/pkg/gnolang/alloc.go" {
			tags = append(tags, "gas-schedule")
		}
	}
	sort.Strings(tags)
	return compactStrings(tags), nil
}

func Size(percent float64) string {
	for _, s := range []struct {
		bound float64
		name  string
	}{{.2, "very small"}, {1, "small"}, {2, "moderate"}, {5, "large"}} {
		if math.Abs(percent) < s.bound {
			return s.name
		}
	}
	return "very large"
}

func comparisonKey(m Measurement) string {
	if m.Kind == "tx" {
		return gasWantedRE.ReplaceAllString(m.Key(), "${1}<gas>")
	}
	return m.Key()
}

func platform(host string) string {
	parts := strings.SplitN(host, "/", 3)
	if len(parts) < 2 {
		return host
	}
	return parts[0] + "/" + parts[1]
}

func Compare(ctx context.Context, repo string, base, head *Collection, all bool) (*Report, error) {
	r := &Report{Base: base, Head: head}
	if !base.Complete || !head.Complete {
		r.Warnings = append(r.Warnings, "one or both collections are incomplete; inspect collection errors")
	}
	if strings.Split(base.Scope, ";repeat=")[0] != strings.Split(head.Scope, ";repeat=")[0] {
		r.Warnings = append(r.Warnings, "collection scopes differ; only overlapping series are compared")
	}
	if base.HarnessSHA != head.HarnessSHA {
		r.Warnings = append(r.Warnings, "collector versions differ")
	}
	if strings.HasSuffix(base.Scope, ";repeat=false") || strings.HasSuffix(head.Scope, ";repeat=false") {
		r.Warnings = append(r.Warnings, "determinism check was skipped for at least one collection")
	}
	if base.Host != head.Host {
		r.Warnings = append(r.Warnings, "hosts differ; wall-clock series are excluded")
	}
	if base.GoVersion != head.GoVersion {
		r.Warnings = append(r.Warnings, "Go toolchains differ; binary-size series are excluded")
	}
	for _, c := range []*Collection{base, head} {
		for _, e := range c.Errors {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s %s: %s", short(c.SHA), e.Step, e.Message))
		}
		if len(c.Nondeterministic) > 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s: %d nondeterministic series excluded", short(c.SHA), len(c.Nondeterministic)))
		}
	}
	baseValues, headValues := map[string][]Measurement{}, map[string][]Measurement{}
	nondet := map[string]bool{}
	for _, c := range []*Collection{base, head} {
		for _, key := range c.Nondeterministic {
			nondet[key] = true
		}
	}
	for _, m := range base.Values {
		key := comparisonKey(m)
		baseValues[key] = append(baseValues[key], m)
		nondet[key] = nondet[key] || m.Nondeterministic
	}
	for _, m := range head.Values {
		key := comparisonKey(m)
		headValues[key] = append(headValues[key], m)
		nondet[key] = nondet[key] || m.Nondeterministic
	}
	changedText, err := git(ctx, repo, "diff", "--name-only", base.SHA, head.SHA, "--")
	if err != nil {
		return nil, err
	}
	changed := strings.Fields(changedText)
	var bs, hs *snapshot
	packages := map[string]bool{}
	for _, f := range changed {
		if strings.HasPrefix(f, "examples/") {
			bs, err = readSnapshot(ctx, repo, base.SHA)
			if err != nil {
				return nil, err
			}
			hs, err = readSnapshot(ctx, repo, head.SHA)
			if err != nil {
				return nil, err
			}
			packages = changedPackages(bs, hs)
			break
		}
	}
	missing := 0
	for key, heads := range headValues {
		bases, ok := baseValues[key]
		if !ok {
			missing++
			continue
		}
		a, b := bases[0], heads[0]
		if nondet[key] {
			continue
		}
		change := Change{Kind: b.Kind, Benchmark: b.Benchmark, Metric: b.Metric, Base: a.Value, Head: b.Value, Count: 1}
		if b.Tier == 2 {
			if base.Host != head.Host || len(bases) < 10 || len(heads) < 10 {
				continue
			}
			bv, hv := make([]float64, len(bases)), make([]float64, len(heads))
			for i, m := range bases {
				bv[i] = m.Value
			}
			for i, m := range heads {
				hv[i] = m.Value
			}
			ba := benchmath.NewSample(bv, &benchmath.DefaultThresholds)
			ha := benchmath.NewSample(hv, &benchmath.DefaultThresholds)
			stats := benchmath.AssumeNothing.Compare(ba, ha)
			if stats.P >= .05 {
				continue
			}
			change.Base = benchmath.AssumeNothing.Summary(ba, .95).Center
			change.Head = benchmath.AssumeNothing.Summary(ha, .95).Center
			change.Statistics = stats.String()
			change.Category = "Tier 2 (informational)"
		}
		change.Delta = change.Head - change.Base
		if change.Base == 0 {
			if change.Delta == 0 {
				change.Percent = 0
			} else {
				change.Percent = math.Copysign(math.Inf(1), change.Delta)
			}
		} else {
			change.Percent = 100 * change.Delta / math.Abs(change.Base)
		}
		if change.Delta == 0 {
			continue
		}
		if b.Tier == 2 && math.Abs(change.Percent) < 5 {
			continue
		}
		if b.Kind == "bin" {
			if base.GoVersion != head.GoVersion || platform(base.Host) != platform(head.Host) || math.Abs(change.Percent) < .5 {
				continue
			}
		}
		if (b.Metric == "cycles" || b.Metric == "allocs") && math.Abs(change.Delta) <= math.Max(a.Precision, b.Precision)/2 {
			continue
		}
		change.Size = Size(change.Percent)
		if b.Tier == 1 {
			change.Tags, err = classification(ctx, repo, base, head, changed, bs, hs, packages, a, b)
			if err != nil {
				return nil, err
			}
			change.Category = "Regressions"
			if change.Delta < 0 {
				change.Category = "Improvements"
			}
			if len(change.Tags) > 0 {
				change.Category = "Explained changes"
			}
		}
		r.Changes = append(r.Changes, change)
	}
	for key := range baseValues {
		if _, ok := headValues[key]; !ok {
			missing++
		}
	}
	if missing > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%d added or removed series have no counterpart", missing))
	}
	if !all {
		r.Changes = groupChanges(r.Changes)
	}
	sort.Slice(r.Changes, func(i, j int) bool {
		a, b := r.Changes[i], r.Changes[j]
		if math.Abs(a.Percent) != math.Abs(b.Percent) {
			return math.Abs(a.Percent) > math.Abs(b.Percent)
		}
		return a.Benchmark+"/"+a.Metric < b.Benchmark+"/"+b.Metric
	})
	return r, nil
}

func groupChanges(changes []Change) []Change {
	var out []Change
	index := map[string]int{}
	for _, c := range changes {
		pkg, _, ok := strings.Cut(c.Benchmark, "#")
		if !ok || c.Kind != "unit" {
			out = append(out, c)
			continue
		}
		direction := "+"
		if c.Delta < 0 {
			direction = "-"
		}
		key := pkg + "/" + c.Metric + "/" + c.Category + "/" + direction + strings.Join(c.Tags, ",")
		if i, ok := index[key]; ok {
			g := &out[i]
			g.Base += c.Base
			g.Head += c.Head
			g.Delta += c.Delta
			g.Count++
			g.Benchmark = pkg + fmt.Sprintf(" (%d tests)", g.Count)
			if g.Base != 0 {
				g.Percent = 100 * g.Delta / math.Abs(g.Base)
			}
			g.Size = Size(g.Percent)
		} else {
			index[key] = len(out)
			out = append(out, c)
		}
	}
	return out
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.NewReplacer("\n", " ", "\r", " ", "\x00", "#").Replace(s)
}

func displayValue(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
func displayDelta(value float64) string {
	if value >= 0 {
		return "+" + displayValue(value)
	}
	return displayValue(value)
}

func (r *Report) Write(w io.Writer, format string) error {
	if format != "md" && format != "text" {
		return fmt.Errorf("unknown report format %q", format)
	}
	var b strings.Builder
	title := "gno-perf"
	if r.Title != "" {
		title += " — " + r.Title
	}
	if format == "md" {
		fmt.Fprintf(&b, "## %s\n\n", escapeCell(title))
	} else {
		fmt.Fprintf(&b, "%s\n", title)
	}
	fmt.Fprintf(&b, "head %s vs base %s · harness %s\n\n", short(r.Head.SHA), short(r.Base.SHA), r.Head.HarnessSHA)
	for _, warning := range r.Warnings {
		fmt.Fprintf(&b, "WARNING: %s\n", escapeCell(warning))
	}
	for _, category := range []string{"Regressions", "Improvements", "Explained changes", "Tier 2 (informational)"} {
		var rows []Change
		for _, c := range r.Changes {
			if c.Category == category {
				rows = append(rows, c)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if format == "md" {
			fmt.Fprintf(&b, "\n### %s\n\n| benchmark | metric | base | head | Δ | Δ%% | size | notes |\n|---|---|---:|---:|---:|---:|---|---|\n", category)
		} else {
			fmt.Fprintf(&b, "\n%s\n", category)
		}
		for _, c := range rows {
			notes := strings.Join(c.Tags, ", ")
			if c.Statistics != "" {
				notes = c.Statistics
			}
			if format == "md" {
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %+.3g%% | %s | %s |\n", escapeCell(c.Benchmark), c.Metric, displayValue(c.Base), displayValue(c.Head), displayDelta(c.Delta), c.Percent, c.Size, escapeCell(notes))
			} else {
				fmt.Fprintf(&b, "%s %s: %s → %s (%s, %+.3g%%) %s %s\n", c.Benchmark, c.Metric, displayValue(c.Base), displayValue(c.Head), displayDelta(c.Delta), c.Percent, c.Size, notes)
			}
		}
	}
	if len(r.Changes) == 0 {
		b.WriteString("\nNo reportable changes.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
