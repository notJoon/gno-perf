package gnoperf

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const usage = `Usage: gnoperf [--config path] <command> [options]

Commands:
  collect [sha]       Collect metrics for a commit (default HEAD)
  compare base head   Compare stored collections
  pr number           Collect and compare a GitHub pull request
  sync                Collect new first-parent master commits
  backfill from..to   Collect a historical range
  bisect good bad     Find a change in a deterministic series
  bench ops           Run opcode benchmarks manually
`

func parseFlags(fs *flag.FlagSet, args []string) error {
	var options, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		name, _, hasValue := strings.Cut(name, "=")
		f := fs.Lookup(name)
		if f == nil {
			return fmt.Errorf("unknown option %s", arg)
		}
		options = append(options, arg)
		if !hasValue {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				i++
				if i >= len(args) {
					return fmt.Errorf("option %s needs a value", arg)
				}
				options = append(options, args[i])
			}
		}
	}
	return fs.Parse(append(options, positionals...))
}

func collectFlags(fs *flag.FlagSet, opts *CollectOptions, pkgs *string) {
	fs.StringVar(pkgs, "pkgs", "", "Comma-separated Gno package patterns")
	fs.BoolVar(&opts.Quick, "quick", false, "Use the configured quick package set")
	fs.BoolVar(&opts.SkipTx, "skip-tx", false, "Skip transaction tests")
	fs.BoolVar(&opts.SkipTier2, "skip-tier2", false, "Skip Go benchmarks")
	fs.BoolVar(&opts.NoRepeat, "no-repeat", false, "Skip the determinism check")
	fs.BoolVar(&opts.Force, "force", false, "Bypass cached collections")
	fs.IntVar(&opts.Parallelism, "p", 0, "Gno test parallelism (default from config)")
}

func Run(ctx context.Context, args []string, out, errout io.Writer) error {
	configPath := "gnoperf.toml"
	var filtered []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			i++
			if i >= len(args) {
				return errors.New("--config needs a path")
			}
			configPath = args[i]
		} else if strings.HasPrefix(args[i], "--config=") {
			configPath = strings.TrimPrefix(args[i], "--config=")
		} else {
			filtered = append(filtered, args[i])
		}
	}
	args = filtered
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}
	c, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	store, err := OpenStore(filepath.Join(c.CacheDir, "perf.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	collector := &Collector{Config: c, ConfigPath: configPath, Store: store, Progress: func(message string) { fmt.Fprintln(errout, message) }}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(errout)
	var opts CollectOptions
	var pkgs string
	if args[0] != "compare" && args[0] != "bench" {
		collectFlags(fs, &opts, &pkgs)
	}
	switch args[0] {
	case "sync":
		maxCount := fs.Int("max", 0, "Maximum commits per run; first run starts at the current tip")
		if err = parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("sync takes no positional arguments")
		}
		if pkgs != "" {
			opts.Packages = strings.Split(pkgs, ",")
		}
		return collector.Sync(ctx, *maxCount, opts, out)
	case "backfill":
		every := fs.Int("every", 1, "Collect every Nth first-parent commit")
		if err = parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("backfill requires one commit range")
		}
		if pkgs != "" {
			opts.Packages = strings.Split(pkgs, ",")
		}
		return collector.Backfill(ctx, fs.Arg(0), *every, opts, out)
	case "bench":
		if err = parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || fs.Arg(0) != "ops" {
			return errors.New("bench requires ops")
		}
		return collector.BenchOps(ctx, out)
	case "pr":
		base := fs.String("base", "", "Override the base commit")
		head := fs.String("head", "", "Override the head commit")
		rebase := fs.Bool("rebase-base", false, "Compare a temporary merge onto current master")
		all := fs.Bool("all", false, "Show every changed series")
		if err = parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("pr requires one PR number")
		}
		number, err := strconv.Atoi(fs.Arg(0))
		if err != nil {
			return err
		}
		if pkgs != "" {
			opts.Packages = strings.Split(pkgs, ",")
		}
		r, err := collector.PullRequest(ctx, number, PROptions{CollectOptions: opts, Base: *base, Head: *head, RebaseBase: *rebase, All: *all})
		if err != nil {
			return err
		}
		return SaveReport(c, r, fmt.Sprintf("pr-%d-%s.md", number, r.Head.SHA[:7]), out)
	case "compare":
		format := fs.String("format", "md", "Report format: md or text")
		all := fs.Bool("all", false, "Show every changed series")
		if err = parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return errors.New("compare requires base and head commits")
		}
		baseSHA, err := Resolve(ctx, c.GnoCore, fs.Arg(0))
		if err != nil {
			return err
		}
		headSHA, err := Resolve(ctx, c.GnoCore, fs.Arg(1))
		if err != nil {
			return err
		}
		base, err := store.Load(baseSHA)
		if err != nil {
			return err
		}
		head, err := store.Load(headSHA)
		if err != nil {
			return err
		}
		report, err := Compare(ctx, c.GnoCore, base, head, *all)
		if err != nil {
			return err
		}
		return report.Write(out, *format)
	case "collect":
		if err = parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() > 1 {
			return errors.New("collect accepts at most one commit")
		}
		if pkgs != "" {
			opts.Packages = strings.Split(pkgs, ",")
		}
		ref := "HEAD"
		if fs.NArg() == 1 {
			ref = fs.Arg(0)
		}
		result, err := collector.Collect(ctx, ref, opts)
		if result != nil {
			fmt.Fprintf(out, "collection %d: %s, %d measurements, %d nondeterministic series, %.2fs\n", result.ID, result.SHA, len(result.Values), len(result.Nondeterministic), float64(result.DurationMS)/1000)
		}
		return err
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}
