package gnoperf

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localRemote(t *testing.T, repo string) {
	t.Helper()
	if _, err := git(context.Background(), repo, "config", "url."+repo+".insteadOf", "https://github.com/gnolang/gno.git"); err != nil {
		t.Fatal(err)
	}
}

func TestPRBaseSelectionAndTemporaryMerge(t *testing.T) {
	c, root := fixtureRepo(t)
	ctx := context.Background()
	localRemote(t, c.GnoCore)
	if _, err := git(ctx, c.GnoCore, "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	head := commitFiles(t, c.GnoCore, map[string]string{"feature.txt": "feature"})
	if _, err := git(ctx, c.GnoCore, "checkout", "master"); err != nil {
		t.Fatal(err)
	}
	tip := commitFiles(t, c.GnoCore, map[string]string{"master.txt": "master"})
	collector := Collector{Config: c}
	base, got, err := collector.prRefs(ctx, PRInfo{State: "OPEN", HeadRefOid: head, BaseRefName: "master"}, PROptions{})
	if err != nil || base != root || got != head {
		t.Fatalf("open PR selection: %s %s %v", base, got, err)
	}
	base, got, err = collector.prRefs(ctx, PRInfo{State: "OPEN", HeadRefOid: head, BaseRefName: "master"}, PROptions{RebaseBase: true})
	if err != nil || base != tip || got == head {
		t.Fatalf("temporary merge: %s %s %v", base, got, err)
	}
	parents, err := git(ctx, c.GnoCore, "show", "-s", "--format=%P", got)
	if err != nil || parents != tip+" "+head {
		t.Fatalf("wrong merge parents: %q %v", parents, err)
	}
	if checkout, err := Resolve(ctx, c.GnoCore, "HEAD"); err != nil || checkout != tip {
		t.Fatal("temporary merge changed source checkout")
	}
	mergeInfo := PRInfo{State: "MERGED", MergeCommit: &struct{ Oid string }{tip}}
	base, got, err = collector.prRefs(ctx, mergeInfo, PROptions{})
	if err != nil || base != root || got != tip {
		t.Fatalf("merged PR selection: %s %s %v", base, got, err)
	}
	if _, err := git(ctx, c.GnoCore, "checkout", "feature"); err != nil {
		t.Fatal(err)
	}
	conflict := commitFiles(t, c.GnoCore, map[string]string{"master.txt": "conflict"})
	if _, err := collector.mergeCommit(ctx, tip, conflict); err == nil {
		t.Fatal("merge conflict was accepted")
	}
}

func TestPullRequestCache(t *testing.T) {
	c, _ := fixtureRepo(t)
	ctx := context.Background()
	localRemote(t, c.GnoCore)
	head := commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "43"})
	if _, err := git(ctx, c.GnoCore, "update-ref", "refs/pull/1/head", head); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf '%s\\n' '{\"state\":\"MERGED\",\"mergeCommit\":{\"oid\":\""+head+"\"}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s, err := OpenStore(filepath.Join(c.CacheDir, "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	collector := Collector{Config: c, Store: s}
	opts := PROptions{CollectOptions: CollectOptions{SkipTx: true, SkipTier2: true}}
	r, err := collector.PullRequest(ctx, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Changes[0].Category != "Regressions" {
		t.Fatalf("missing real gas change: %+v", r.Changes)
	}
	again, err := collector.PullRequest(ctx, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Base.ID != r.Base.ID || again.Head.ID != r.Head.ID {
		t.Fatal("second PR run did not use cache")
	}
	var report strings.Builder
	if err = SaveReport(c, r, "pr-1.md", &report); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(c.CacheDir, "reports", "pr-1.md")); err != nil {
		t.Fatal(err)
	}
}
