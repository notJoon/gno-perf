package gnoperf

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/perf/benchmath"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (c *Collector) BenchOps(ctx context.Context, out io.Writer) error {
	unlock, err := acquireLock(c.Config.WorktreeDir + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	sha, err := Resolve(ctx, c.Config.GnoCore, "HEAD")
	if err != nil {
		return err
	}
	if err = c.worktree(ctx, sha); err != nil {
		return err
	}
	mod, err := os.ReadFile(filepath.Join(c.Config.WorktreeDir, "go.mod"))
	if err != nil {
		return err
	}
	version, err := targetGo(mod)
	if err != nil {
		return err
	}
	env := []string{"GOTOOLCHAIN=" + version, "GOFLAGS=", "GNOROOT=" + c.Config.WorktreeDir, "GOMAXPROCS=2"}
	data, err := output(ctx, c.Config.WorktreeDir, env, "nice", "-n", "10", "go", "test", "./gnovm/pkg/gnolang", "-run", "^$", "-bench", "^BenchmarkOp", "-count=3", "-benchtime=2s", "-benchmem", "-timeout=0")
	if err != nil {
		return err
	}
	var summary strings.Builder
	values, err := ParseBench(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return errors.New("no opcode benchmarks found")
	}
	group := map[string][]float64{}
	for _, m := range values {
		group[m.Key()] = append(group[m.Key()], m.Value)
	}
	for _, m := range values {
		if m.Sample != 0 {
			continue
		}
		sample := benchmath.NewSample(group[m.Key()], &benchmath.DefaultThresholds)
		stats := benchmath.AssumeNothing.Summary(sample, .95)
		fmt.Fprintf(&summary, "%s %s %.8g ± %s (n=%d)\n", m.Benchmark, m.Metric, stats.Center, stats.PctRangeString(), len(sample.Values))
	}
	_, err = io.WriteString(out, string(data)+"\n"+summary.String())
	return err
}
