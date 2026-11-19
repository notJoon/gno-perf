package gnoperf

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commitFiles(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "Change fixture"}} {
		if _, err := git(context.Background(), repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	sha, err := Resolve(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func collections(base, head string) (*Collection, *Collection) {
	a := &Collection{SHA: base, GoVersion: "go1.25.9", Host: "darwin/arm64/cpu", HarnessSHA: "h", Scope: "full;repeat=true", Complete: true}
	b := *a
	b.SHA = head
	return a, &b
}

func TestCompareThresholdsAndStatistics(t *testing.T) {
	c, baseSHA := fixtureRepo(t)
	headSHA := commitFiles(t, c.GnoCore, map[string]string{"gnovm/engine.go": "package engine\n"})
	base, head := collections(baseSHA, headSHA)
	for _, tc := range []struct {
		kind, name, metric string
		tier               int
		a, b, p            float64
	}{{"unit", "pkg#TestA", "gas", 1, 100, 110, 1}, {"unit", "pkg#TestZero", "gas", 1, 0, 1, 1}, {"unit", "pkg#TestRounded", "cycles", 1, 100000, 100010, 100}, {"unit", "pkg#TestNoisy", "gas", 1, 100, 900, 1}, {"bin", "gno", "bytes", 1, 10000, 10020, 1}} {
		m := Measurement{Tier: tc.tier, Kind: tc.kind, Benchmark: tc.name, Metric: tc.metric, Value: tc.a, Precision: tc.p}
		base.Values = append(base.Values, m)
		m.Value = tc.b
		if tc.name == "pkg#TestNoisy" {
			m.Nondeterministic = true
		}
		head.Values = append(head.Values, m)
	}
	for i := 0; i < 10; i++ {
		m := Measurement{Tier: 2, Kind: "gobench", Benchmark: "pkg#BenchmarkA", Metric: "ns_op", Sample: i, Value: 100 + float64(i)}
		base.Values = append(base.Values, m)
		m.Value += 100
		head.Values = append(head.Values, m)
	}
	r, err := Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 3 {
		t.Fatalf("unexpected changes: %+v", r.Changes)
	}
	for _, change := range r.Changes {
		if change.Benchmark == "pkg#TestZero" && !math.IsInf(change.Percent, 1) {
			t.Fatal("zero base must not divide by zero")
		}
		if change.Kind == "gobench" && !strings.Contains(change.Statistics, "n=10") {
			t.Fatal("missing benchstat comparison")
		}
	}
	for _, format := range []string{"md", "text"} {
		var out bytes.Buffer
		if err = r.Write(&out, format); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Regressions") || !strings.Contains(out.String(), "informational") {
			t.Fatal(out.String())
		}
	}
	head.Host = "linux/arm64/other"
	r, err = Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range r.Changes {
		if change.Kind == "gobench" {
			t.Fatal("compared wall-clock across hosts")
		}
	}
}

func TestCompareExplainsTestAndTransitiveDependencyChanges(t *testing.T) {
	c, _ := fixtureRepo(t)
	baseSHA := commitFiles(t, c.GnoCore, map[string]string{
		"examples/gno.land/p/test/one_test.gno":  "package test\nimport \"gno.land/p/middle\"\nfunc TestOne() {}\n",
		"examples/gno.land/p/middle/gnomod.toml": "module = \"gno.land/p/middle\"\n",
		"examples/gno.land/p/middle/middle.gno":  "package middle\nimport \"gno.land/p/leaf\"\n",
		"examples/gno.land/p/leaf/gnomod.toml":   "module = \"gno.land/p/leaf\"\n",
		"examples/gno.land/p/leaf/leaf.gno":      "package leaf\nconst X = 1\n",
	})
	headSHA := commitFiles(t, c.GnoCore, map[string]string{"examples/gno.land/p/leaf/leaf.gno": "package leaf\nconst X = 2\n"})
	base, head := collections(baseSHA, headSHA)
	m := Measurement{Tier: 1, Kind: "unit", Benchmark: "gno.land/p/test#TestOne", Metric: "gas", Value: 100, Source: "examples/gno.land/p/test/one_test.gno", InputHash: "same"}
	base.Values = []Measurement{m}
	m.Value = 110
	head.Values = []Measurement{m}
	r, err := Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || strings.Join(r.Changes[0].Tags, ",") != "dep-changed" {
		t.Fatalf("missing dependency tag: %+v", r.Changes)
	}
	headSHA = commitFiles(t, c.GnoCore, map[string]string{"examples/gno.land/p/test/one_test.gno": "package test\nimport \"gno.land/p/middle\"\nfunc TestOne() { println(1) }\n"})
	head.SHA = headSHA
	head.Values[0].InputHash = "different"
	r, err = Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.Changes[0].Tags, ","), "test-changed") {
		t.Fatal("missing test tag")
	}
}

func TestCompareGasWantedOnly(t *testing.T) {
	c, _ := fixtureRepo(t)
	source := "gno.land/pkg/integration/testdata/a.txtar"
	baseSHA := commitFiles(t, c.GnoCore, map[string]string{source: "gnokey call -gas-wanted 100\nstdout 'GAS WANTED: 100'\n"})
	headSHA := commitFiles(t, c.GnoCore, map[string]string{source: "gnokey call -gas-wanted 200\nstdout 'GAS WANTED: 200'\n"})
	base, head := collections(baseSHA, headSHA)
	m := Measurement{Tier: 1, Kind: "tx", Benchmark: "a#1 gnokey call -gas-wanted 100", Metric: "gas_used", Value: 100, InputHash: "before", Source: source}
	base.Values = []Measurement{m}
	m.Value = 110
	m.InputHash = "after"
	m.Benchmark = "a#1 gnokey call -gas-wanted 200"
	head.Values = []Measurement{m}
	r, err := Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || strings.Join(r.Changes[0].Tags, ",") != "gaswanted-only" {
		t.Fatalf("wrong classification: %+v", r.Changes)
	}
}

func TestGroupingAndSize(t *testing.T) {
	changes := []Change{{Kind: "unit", Benchmark: "pkg#A", Metric: "gas", Category: "Regressions", Base: 100, Head: 110, Delta: 10, Percent: 10, Count: 1}, {Kind: "unit", Benchmark: "pkg#B", Metric: "gas", Category: "Regressions", Base: 200, Head: 220, Delta: 20, Percent: 10, Count: 1}}
	got := groupChanges(changes)
	if len(got) != 1 || got[0].Count != 2 || got[0].Delta != 30 || got[0].Percent != 10 {
		t.Fatalf("wrong grouping: %+v", got)
	}
	for _, tc := range []struct {
		value float64
		want  string
	}{{.1, "very small"}, {.2, "small"}, {1, "moderate"}, {2, "large"}, {5, "very large"}} {
		if Size(tc.value) != tc.want {
			t.Errorf("size %v", tc.value)
		}
	}
}

func TestRoundedMetricBoundary(t *testing.T) {
	c, sha := fixtureRepo(t)
	base, head := collections(sha, sha)
	base.Values = []Measurement{{Tier: 1, Kind: "unit", Benchmark: "pkg#Test", Metric: "cycles", Value: 1000, Precision: 100}}
	head.Values = append([]Measurement{}, base.Values...)
	head.Values[0].Value = 1050
	r, err := Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil || len(r.Changes) != 0 {
		t.Fatalf("rounding noise reported: %+v %v", r, err)
	}
	head.Values[0].Value = 1100
	r, err = Compare(context.Background(), c.GnoCore, base, head, true)
	if err != nil || len(r.Changes) != 1 {
		t.Fatalf("adjacent rounded value suppressed: %+v %v", r, err)
	}
}
