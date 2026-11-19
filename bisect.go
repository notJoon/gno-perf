package gnoperf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ErrBisectBad = errors.New("selected series differs from baseline")

func seriesValue(c *Collection, series string) (Measurement, error) {
	benchmark, metric, ok := strings.Cut(series, ":")
	if !ok || benchmark == "" || metric == "" {
		return Measurement{}, errors.New("--series requires benchmark:metric")
	}
	var found *Measurement
	for _, m := range c.Values {
		if m.Benchmark != benchmark || m.Metric != metric {
			continue
		}
		if m.Tier != 1 {
			return m, errors.New("bisect supports only deterministic metrics")
		}
		if m.Nondeterministic {
			return m, errors.New("selected series is nondeterministic")
		}
		if found != nil {
			return m, errors.New("series is ambiguous across metric kinds")
		}
		v := m
		found = &v
	}
	if found == nil {
		return Measurement{}, fmt.Errorf("series absent at %s", c.SHA)
	}
	for _, key := range c.Nondeterministic {
		if key == found.Key() {
			return *found, errors.New("selected series is nondeterministic")
		}
	}
	return *found, nil
}

func (c *Collector) Bisect(ctx context.Context, good, bad, series string, opts CollectOptions) (commit string, resultErr error) {
	opts.NoRepeat = false
	a, err := c.Collect(ctx, good, opts)
	if err != nil {
		return "", err
	}
	b, err := c.Collect(ctx, bad, opts)
	if err != nil {
		return "", err
	}
	first, err := seriesValue(a, series)
	if err != nil {
		return "", err
	}
	last, err := seriesValue(b, series)
	if err != nil {
		return "", err
	}
	if first.Value == last.Value {
		return "", errors.New("good and bad commits have the same series value")
	}
	chain, err := git(ctx, c.Config.GnoCore, "rev-list", "--first-parent", b.SHA)
	if err != nil {
		return "", err
	}
	found := false
	for _, sha := range strings.Fields(chain) {
		if sha == a.SHA {
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("good must lie on the bad commit's first-parent chain")
	}
	if c.ConfigPath == "" {
		return "", errors.New("bisect requires a config file path")
	}
	unlock, err := acquireLock(filepath.Join(c.Config.CacheDir, "bisect.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	dir, err := os.MkdirTemp(c.Config.CacheDir, "bisect-")
	if err != nil {
		return "", err
	}
	if err = os.Remove(dir); err != nil {
		return "", err
	}
	if _, err = git(ctx, c.Config.GnoCore, "worktree", "add", "--detach", dir, b.SHA); err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		git(cleanup, dir, "bisect", "reset")
		_, cleanupErr := git(cleanup, c.Config.GnoCore, "worktree", "remove", "--force", dir)
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	if _, err = git(ctx, dir, "bisect", "start", "--first-parent", "--no-checkout", b.SHA, a.SHA); err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	args := []string{"bisect", "run", executable, "--config", c.ConfigPath, "_bisect-check", "--bisect-dir", dir, "--series", series, "--baseline", strconv.FormatFloat(first.Value, 'g', -1, 64)}
	if opts.Quick {
		args = append(args, "--quick")
	}
	if opts.SkipTx {
		args = append(args, "--skip-tx")
	}
	if opts.SkipTier2 {
		args = append(args, "--skip-tier2")
	}
	if opts.Force {
		args = append(args, "--force")
	}
	if len(opts.Packages) > 0 {
		args = append(args, "--pkgs", strings.Join(opts.Packages, ","))
	}
	if opts.Parallelism > 0 {
		args = append(args, "-p", strconv.Itoa(opts.Parallelism))
	}
	if _, err = git(ctx, dir, args...); err != nil {
		return "", err
	}
	return Resolve(ctx, dir, "refs/bisect/bad")
}
