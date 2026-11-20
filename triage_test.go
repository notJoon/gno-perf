package gnoperf

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTriageDraft(t *testing.T) {
	c, root := fixtureRepo(t)
	ctx := context.Background()
	commit := func(file, subject string) string {
		commitFiles(t, c.GnoCore, map[string]string{file: subject})
		if _, err := git(ctx, c.GnoCore, "commit", "--amend", "-m", subject); err != nil {
			t.Fatal(err)
		}
		sha, err := Resolve(ctx, c.GnoCore, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	regressed := commit("gnovm/a.go", "feat: change gas (#42)")
	tip := commit("docs/a.md", "docs: note (#43)")
	s, err := OpenStore(filepath.Join(c.CacheDir, "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for sha, gas := range map[string]float64{root: 100, regressed: 110} {
		col, _ := collections(sha, sha)
		col.Values = []Measurement{{Tier: 1, Kind: "unit", Benchmark: "gno.land/p/test#TestOne", Metric: "gas", Value: gas}}
		if err = s.Save(col); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	collector := Collector{Config: c, Store: s}
	if err = collector.Triage(ctx, root+".."+tip, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	review, rest, _ := strings.Cut(got, "### Not collected")
	for _, want := range []string{"# gno-perf triage 2026-11-20", "(2 commits)", "1 review needed, 0 no action needed, 1 not collected", "[#42](https://github.com/gnolang/gno/pull/42)", "##### Regressions", "+10%", "Verdict: TODO"} {
		if !strings.Contains(review, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(rest, "#43") {
		t.Fatalf("uncollected commit not listed:\n%s", got)
	}
}
