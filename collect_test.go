package gnoperf

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func fixtureRepo(t *testing.T) (Config, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.25.9\n")
	write("gnovm/cmd/gno/main.go", `package main
import("fmt";"os";"strings")
func main(){
 if strings.Contains(strings.Join(os.Args," "),"-h"){fmt.Println("USAGE\nFLAGS\n  -p int\n  -print-runtime-metrics");os.Exit(1)}
 if os.Getenv("GNOPERF_FIXTURE_FAIL")=="1" {fmt.Println("=== RUN TestOne\n--- FAIL: TestOne (0s)\nFAIL ./gno.land/p/test 0s");os.Exit(1)}
 gas,err:=os.ReadFile(os.Getenv("GNOROOT")+"/examples/gas.txt");if err!=nil{panic(err)}
 fmt.Printf("=== RUN TestOne\n--- PASS: TestOne (0s)\n--- GAS: %s\n---       runtime: cycle=42, allocs=12\nok ./gno.land/p/test 0.01s\n",strings.TrimSpace(string(gas)))
}`)
	for _, name := range []string{"gnoland", "gnokey"} {
		write("gno.land/cmd/"+name+"/main.go", "package main\nfunc main() {}\n")
	}
	write("examples/gas.txt", "42")
	write("examples/gno.land/p/test/one_test.gno", "package test\nfunc TestOne() {}\n")
	write("examples/gno.land/p/test/gnomod.toml", "module = \"gno.land/p/test\"\n")
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-b", "master"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"config", "commit.gpgsign", "false"}, {"add", "."}, {"commit", "-m", "Initial fixture"}} {
		if _, err := git(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	c := Config{GnoCore: repo, CacheDir: filepath.Join(root, "cache"), WorktreeDir: filepath.Join(root, "worktree"), Parallelism: 2, QuickPackages: []string{"gno.land/p/test"}}
	c.Tier2.Count = 10
	c.Tier2.VMBench = "."
	c.Tier2.StoreBench = "."
	sha, err := Resolve(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return c, sha
}

func TestCollectWorktreeCacheAndFailure(t *testing.T) {
	c, sha := fixtureRepo(t)
	ctx := context.Background()
	s, err := OpenStore(filepath.Join(c.CacheDir, "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	collector := Collector{Config: c, Store: s}
	opts := CollectOptions{SkipTx: true, SkipTier2: true}
	got, err := collector.Collect(ctx, sha, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || len(got.Values) != 7 || len(got.Nondeterministic) > 0 {
		t.Fatalf("bad collection: %+v", got)
	}
	for _, m := range got.Values {
		if m.Kind == "unit" && (m.Source != "examples/gno.land/p/test/one_test.gno" || m.InputHash == "") {
			t.Fatalf("missing input metadata: %+v", m)
		}
	}
	cached, err := collector.Collect(ctx, sha, opts)
	if err != nil || cached.ID != got.ID {
		t.Fatalf("cache missed: %+v %v", cached, err)
	}
	logs, err := filepath.Glob(filepath.Join(c.CacheDir, "logs", sha, "*", "t1.log.gz"))
	if err != nil || len(logs) != 1 {
		t.Fatal("raw log missing")
	}
	if err := os.WriteFile(filepath.Join(c.WorktreeDir, "keep.txt"), []byte("local change"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Force = true
	if _, err := collector.Collect(ctx, sha, opts); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("did not protect worktree: %v", err)
	}
	if err := os.Remove(filepath.Join(c.WorktreeDir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNOPERF_FIXTURE_FAIL", "1")
	failed, err := collector.Collect(ctx, sha, opts)
	if err == nil || failed == nil || failed.Complete || len(failed.Errors) == 0 {
		t.Fatalf("failure was lost: %+v %v", failed, err)
	}
	loaded, err := s.Load(sha)
	if err != nil || loaded.ID != failed.ID || len(loaded.Errors) == 0 {
		t.Fatal("failure was not persisted")
	}
}

func TestLockAndToolchain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	unlock, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if other, err := acquireLock(path); err == nil {
		other()
		t.Fatal("lock admitted a second collector")
	}
	for _, tc := range []struct{ mod, want string }{{"module x\ngo 1.25.9\n", "go1.25.9"}, {"go 1.24\n", "go1.24.0"}} {
		got, err := targetGo([]byte(tc.mod))
		if err != nil || got != tc.want {
			t.Fatalf("wrong toolchain %q %v", got, err)
		}
	}
	if _, err := targetGo([]byte("module x\n")); err == nil {
		t.Fatal("missing toolchain accepted")
	}
}

func writeConfig(t *testing.T, c Config) string {
	t.Helper()
	data, err := toml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "gnoperf.toml")
	if err = os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandCancellationKillsChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := output(ctx, "", nil, "sh", "-c", "sleep 30 & wait")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("command group not cancelled promptly: %v", err)
	}
}

func TestFiletestInputInSubdirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "examples/gno.land/p/test/filetests")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "case_filetest.gno"), []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	values := []Measurement{{Kind: "filetest", Benchmark: "gno.land/p/test/case_filetest.gno"}}
	if err := attachInputs(root, values); err != nil {
		t.Fatal(err)
	}
	if values[0].Source != "examples/gno.land/p/test/filetests/case_filetest.gno" || values[0].InputHash == "" {
		t.Fatalf("wrong filetest source: %+v", values[0])
	}
}

func TestWorktreeRejectsSourceAliases(t *testing.T) {
	c, sha := fixtureRepo(t)
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(c.GnoCore, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{alias, filepath.Join(c.GnoCore, "examples")} {
		c.WorktreeDir = target
		collector := Collector{Config: c}
		if err := collector.worktree(context.Background(), sha); err == nil {
			t.Fatalf("accepted source alias %s", target)
		}
	}
	branch, err := git(context.Background(), c.GnoCore, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != "master" {
		t.Fatal("source checkout was detached")
	}
}
