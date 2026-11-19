package gnoperf

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestBisectFindsFirstChangedCommit(t *testing.T) {
	c, good := fixtureRepo(t)
	ctx := context.Background()
	commitFiles(t, c.GnoCore, map[string]string{"one.txt": "unchanged metrics"})
	changed := commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "43"})
	bad := commitFiles(t, c.GnoCore, map[string]string{"two.txt": "unchanged metrics"})
	configPath := writeConfig(t, c)
	executable := filepath.Join(t.TempDir(), "gnoperf")
	if _, err := output(ctx, "", []string{"GOTOOLCHAIN=go1.25.9"}, "go", "build", "-o", executable, "./cmd/gnoperf"); err != nil {
		t.Fatal(err)
	}
	data, err := output(ctx, "", nil, executable, "--config", configPath, "bisect", good, bad, "--series", "gno.land/p/test#TestOne:gas", "--quick", "--skip-tx", "--skip-tier2")
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(data)), changed) {
		t.Fatalf("wrong bisect result: %s %v", data, err)
	}
	previous, err := Resolve(ctx, c.GnoCore, changed+"^1")
	if err != nil {
		t.Fatal(err)
	}
	data, err = output(ctx, "", nil, executable, "--config", configPath, "bisect", previous, changed, "--series", "gno.land/p/test#TestOne:gas", "--quick", "--skip-tx", "--skip-tier2")
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(data)), changed) {
		t.Fatalf("adjacent bisect failed: %s %v", data, err)
	}
	checkout, err := Resolve(ctx, c.GnoCore, "HEAD")
	if err != nil || checkout != bad {
		t.Fatal("bisect changed source checkout")
	}
	worktrees, err := git(ctx, c.GnoCore, "worktree", "list", "--porcelain")
	if err != nil || strings.Contains(worktrees, "bisect-") {
		t.Fatalf("temporary bisect worktree left behind: %s %v", worktrees, err)
	}
}
