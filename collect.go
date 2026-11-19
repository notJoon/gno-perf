package gnoperf

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type CollectOptions struct {
	Packages                                  []string
	Quick, SkipTx, SkipTier2, NoRepeat, Force bool
	Parallelism                               int
}

type Scope struct {
	Packages              []string
	SkipTx, SkipTier2     bool
	Parallelism, MaxProcs int
	Tier2                 any
}

type Collector struct {
	Config     Config
	ConfigPath string
	Store      *Store
	Progress   func(string)
}

func output(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		tail := data
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return data, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, tail)
	}
	return data, nil
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	data, err := output(ctx, repo, nil, "git", args...)
	return strings.TrimSpace(string(data)), err
}

func Resolve(ctx context.Context, repo, ref string) (string, error) {
	return git(ctx, repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

func acquireLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another collector holds %s: %w", path, err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (c *Collector) worktree(ctx context.Context, sha string) error {
	w := c.Config.WorktreeDir
	if _, err := os.Stat(w); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(w), 0700); err != nil {
			return err
		}
		_, err = git(ctx, c.Config.GnoCore, "worktree", "add", "--detach", w, sha)
		return err
	} else if err != nil {
		return err
	}
	sourceTop, err := git(ctx, c.Config.GnoCore, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	targetTop, err := git(ctx, w, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	sourceTop, err = filepath.EvalSymlinks(sourceTop)
	if err != nil {
		return err
	}
	targetTop, err = filepath.EvalSymlinks(targetTop)
	if err != nil {
		return err
	}
	targetPath, err := filepath.EvalSymlinks(w)
	if err != nil {
		return err
	}
	if sourceTop == targetTop || targetPath != targetTop {
		return errors.New("worktree_dir must name a separate worktree root, not the source checkout")
	}
	common, err := git(ctx, w, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	original, err := git(ctx, c.Config.GnoCore, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	original, err = filepath.EvalSymlinks(original)
	if err != nil {
		return err
	}
	if common != original {
		return fmt.Errorf("worktree %s belongs to another repository", w)
	}
	status, err := git(ctx, w, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("worktree %s has local changes; refusing to overwrite them", w)
	}
	_, err = git(ctx, w, "checkout", "--detach", sha)
	return err
}

var goDirective = regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)\s*$`)

func targetGo(mod []byte) (string, error) {
	m := goDirective.FindSubmatch(mod)
	if m == nil {
		return "", errors.New("target go.mod has no go directive")
	}
	v := string(m[1])
	if strings.Count(v, ".") == 1 {
		v += ".0"
	}
	return "go" + v, nil
}

func harnessVersion() (string, error) {
	revision := "unknown"
	if b, ok := debug.ReadBuildInfo(); ok {
		for _, s := range b.Settings {
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
		}
	}
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return revision + "+" + hex.EncodeToString(sum[:8]), nil
}

func hostID(ctx context.Context) string {
	cpu := "unknown"
	if runtime.GOOS == "darwin" {
		if data, err := output(ctx, "", nil, "sysctl", "-n", "machdep.cpu.brand_string"); err == nil {
			cpu = strings.TrimSpace(string(data))
		}
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "model name") || strings.HasPrefix(line, "Hardware") {
					cpu = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
					break
				}
			}
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH + "/" + cpu
}

func (c *Collector) Collect(ctx context.Context, ref string, opts CollectOptions) (*Collection, error) {
	unlock, err := acquireLock(c.Config.WorktreeDir + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	sha, err := Resolve(ctx, c.Config.GnoCore, ref)
	if err != nil {
		return nil, err
	}
	mod, err := git(ctx, c.Config.GnoCore, "show", sha+":go.mod")
	if err != nil {
		return nil, err
	}
	version, err := targetGo([]byte(mod))
	if err != nil {
		return nil, err
	}
	if opts.Quick && len(opts.Packages) > 0 {
		return nil, errors.New("--quick and --pkgs cannot be combined")
	}
	pkgs := append([]string{}, opts.Packages...)
	if opts.Quick {
		pkgs = append([]string{}, c.Config.QuickPackages...)
		if len(pkgs) == 0 {
			return nil, errors.New("quick_packages is empty")
		}
	}
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}
	pkgs, err = localPackages(pkgs)
	if err != nil {
		return nil, err
	}
	parallel := opts.Parallelism
	if parallel == 0 {
		parallel = c.Config.Parallelism
	}
	if parallel < 1 {
		return nil, errors.New("parallelism must be positive")
	}
	scope, err := json.Marshal(Scope{pkgs, opts.SkipTx, opts.SkipTier2, parallel, 2, c.Config.Tier2})
	if err != nil {
		return nil, err
	}
	harness, err := harnessVersion()
	if err != nil {
		return nil, err
	}
	host := hostID(ctx)
	cacheScope := string(scope) + fmt.Sprintf(";repeat=%t", !opts.NoRepeat)
	if !opts.Force {
		cached, err := c.Store.Cached(sha, harness, cacheScope, host, version)
		if err != nil {
			return nil, err
		}
		if cached != nil {
			if c.Progress != nil {
				c.Progress("cached " + sha)
			}
			return cached, nil
		}
	}
	if err = c.worktree(ctx, sha); err != nil {
		return nil, err
	}
	result := newCollection()
	start := time.Now()
	result.SHA = sha
	result.HarnessSHA = harness
	result.Host = host
	result.GoVersion = version
	result.Scope = cacheScope
	info, err := git(ctx, c.Config.GnoCore, "show", "-s", "--format=%H%n%P%n%cI%n%s", sha)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(info, "\n", 4)
	if len(parts) != 4 {
		return nil, errors.New("invalid commit metadata")
	}
	if parents := strings.Fields(parts[1]); len(parents) > 0 {
		result.ParentSHA = parents[0]
	}
	result.Date, result.Subject = parts[2], parts[3]
	logDir := filepath.Join(c.Config.CacheDir, "logs", sha, strconv.FormatInt(start.UnixNano(), 10))
	if err = os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	binDir := filepath.Join(c.Config.CacheDir, "bin", sha)
	if err = os.MkdirAll(binDir, 0700); err != nil {
		return nil, err
	}
	env := []string{"GOTOOLCHAIN=" + version, "GNOROOT=" + c.Config.WorktreeDir, "GOMAXPROCS=2", "GOFLAGS=", "SEQ_TS=true", "PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	run := func(step, dir, name string, args ...string) ([]byte, error) {
		if c.Progress != nil {
			c.Progress(sha[:12] + " " + step)
		}
		data, runErr := output(ctx, dir, env, "nice", append([]string{"-n", "10", name}, args...)...)
		var exit *exec.ExitError
		if step == "features" && errors.As(runErr, &exit) && exit.ExitCode() == 1 && bytes.Contains(data, []byte("USAGE")) && bytes.Contains(data, []byte("FLAGS")) {
			runErr = nil
		}
		logErr := writeGzip(filepath.Join(logDir, step+".log.gz"), data)
		err := errors.Join(runErr, logErr)
		if err != nil {
			result.Errors = append(result.Errors, StepError{step, err.Error()})
		}
		return data, err
	}
	parse := func(step string, data []byte, fn func(*bytes.Reader) ([]Measurement, error)) []Measurement {
		values, parseErr := fn(bytes.NewReader(data))
		if parseErr != nil {
			result.Errors = append(result.Errors, StepError{step, parseErr.Error()})
		}
		return values
	}
	_, buildErr := run("build", c.Config.WorktreeDir, "go", "build", "-trimpath", "-o", binDir+string(os.PathSeparator), "./gnovm/cmd/gno", "./gno.land/cmd/gnoland", "./gno.land/cmd/gnokey")
	if buildErr == nil {
		for _, name := range []string{"gno", "gnoland", "gnokey"} {
			if stat, err := os.Stat(filepath.Join(binDir, name)); err == nil {
				result.Values = append(result.Values, Measurement{Tier: 1, Kind: "bin", Benchmark: name, Metric: "bytes", Value: float64(stat.Size()), Precision: 1})
			} else {
				result.Errors = append(result.Errors, StepError{"build", err.Error()})
			}
		}
		help, helpErr := run("features", c.Config.WorktreeDir, filepath.Join(binDir, "gno"), "test", "-h")
		if helpErr == nil {
			source, sourceErr := os.ReadFile(filepath.Join(c.Config.WorktreeDir, "gnovm/pkg/test/test.go"))
			storageSupported := sourceErr == nil && bytes.Contains(source, []byte("func fmtStorageDiffs("))
			args := []string{"test", "-v"}
			if strings.Contains(string(help), "-print-runtime-metrics") {
				args = append(args, "-print-runtime-metrics")
			}
			if regexp.MustCompile(`(?m)^\s*-p(?:\s|$)`).Match(help) {
				args = append(args, "-p", strconv.Itoa(parallel))
			}
			args = append(args, pkgs...)
			data, t1Err := run("t1", filepath.Join(c.Config.WorktreeDir, "examples"), filepath.Join(binDir, "gno"), args...)
			first := parse("t1", data, func(r *bytes.Reader) ([]Measurement, error) { return parseGno(r, storageSupported) })
			if len(first) == 0 {
				result.Errors = append(result.Errors, StepError{"t1", "no measurements found"})
			}
			if !opts.NoRepeat && t1Err == nil {
				data, repeatErr := run("t1-repeat", filepath.Join(c.Config.WorktreeDir, "examples"), filepath.Join(binDir, "gno"), args...)
				second := parse("t1-repeat", data, func(r *bytes.Reader) ([]Measurement, error) { return parseGno(r, storageSupported) })
				result.Nondeterministic = CheckDeterminism(first, second)
				if repeatErr != nil {
					for i := range first {
						if first[i].Tier == 1 {
							first[i].Nondeterministic = true
						}
					}
				}
			}
			result.Values = append(result.Values, first...)
		}
		if !opts.SkipTx {
			data, _ := run("tx", c.Config.WorktreeDir, "go", "test", "-v", "-p", "1", "./gno.land/pkg/integration/", "-run", "^TestTestdata$", "-count=1", "-timeout=30m")
			values := parse("tx", data, func(r *bytes.Reader) ([]Measurement, error) { return ParseTx(r) })
			result.Values = append(result.Values, values...)
		}
		if !opts.SkipTier2 {
			groups := []struct {
				packages []string
				pattern  string
			}{{[]string{"./gnovm/pkg/gnolang"}, c.Config.Tier2.VMBench}, {c.Config.Tier2.StorePackages, c.Config.Tier2.StoreBench}}
			for i, group := range groups {
				args := []string{"test", "-p", "1", "-run", "^$", "-bench", group.pattern, "-count", strconv.Itoa(c.Config.Tier2.Count), "-benchmem", "-timeout=60m"}
				for _, pkg := range group.packages {
					if !strings.HasPrefix(pkg, "./") {
						pkg = "./" + pkg
					}
					args = append(args, pkg)
				}
				step := fmt.Sprintf("gobench-%d", i)
				data, _ := run(step, c.Config.WorktreeDir, "go", args...)
				values := parse(step, data, func(r *bytes.Reader) ([]Measurement, error) { return ParseBench(r) })
				result.Values = append(result.Values, values...)
			}
		}
	}
	if err = attachInputs(c.Config.WorktreeDir, result.Values); err != nil {
		result.Errors = append(result.Errors, StepError{"inputs", err.Error()})
	}
	result.DurationMS = time.Since(start).Milliseconds()
	result.Complete = len(result.Errors) == 0
	if err = c.Store.Save(result); err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return result, err
	}
	if err = os.WriteFile(filepath.Join(logDir, "collection.json"), append(manifest, '\n'), 0600); err != nil {
		return result, err
	}
	if !result.Complete {
		return result, fmt.Errorf("collection %d incomplete: %d errors (logs: %s)", result.ID, len(result.Errors), logDir)
	}
	return result, nil
}

func compactStrings(items []string) []string {
	var out []string
	for _, s := range items {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func localPackages(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == "" || strings.HasPrefix(pattern, "-") || filepath.IsAbs(pattern) {
			return nil, fmt.Errorf("invalid local package %q", pattern)
		}
		clean := filepath.ToSlash(filepath.Clean(pattern))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("package escapes examples: %q", pattern)
		}
		if clean == "." {
			out = append(out, ".")
		} else {
			out = append(out, "./"+clean)
		}
	}
	sort.Strings(out)
	return compactStrings(out), nil
}

func sourcePath(root, relative string) (string, error) {
	name := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(relative) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("input path escapes worktree: %q", relative)
	}
	return name, nil
}

func writeGzip(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	w := gzip.NewWriter(f)
	_, writeErr := w.Write(data)
	return errors.Join(writeErr, w.Close(), f.Close())
}

func attachInputs(root string, values []Measurement) error {
	for i := range values {
		m := &values[i]
		switch m.Kind {
		case "filetest":
			m.Source = "examples/" + m.Benchmark
			nested := filepath.Join(filepath.Dir(m.Source), "filetests", filepath.Base(m.Source))
			if _, err := os.Stat(filepath.Join(root, nested)); err == nil {
				m.Source = filepath.ToSlash(nested)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		case "unit":
			pkg, test, ok := strings.Cut(m.Benchmark, "#")
			if !ok {
				continue
			}
			dir, err := sourcePath(root, "examples/"+pkg)
			if err != nil {
				return err
			}
			files, err := filepath.Glob(filepath.Join(dir, "*_test.gno"))
			if err != nil {
				return err
			}
			pattern := regexp.MustCompile(`(?m)^func\s+` + regexp.QuoteMeta(test) + `\s*\(`)
			for _, path := range files {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if pattern.Match(data) {
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					m.Source = filepath.ToSlash(rel)
					break
				}
			}
		}
		if m.Source != "" {
			name, err := sourcePath(root, m.Source)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			m.InputHash = hex.EncodeToString(sum[:])
		}
	}
	return nil
}
