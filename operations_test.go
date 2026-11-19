package gnoperf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncCursorBackfillAndLock(t *testing.T) {
	c, root := fixtureRepo(t)
	localRemote(t, c.GnoCore)
	ctx := context.Background()
	commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "43"})
	commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "44"})
	head := commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "45"})
	s, err := OpenStore(filepath.Join(c.CacheDir, "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	collector := Collector{Config: c, Store: s}
	opts := CollectOptions{SkipTx: true, SkipTier2: true}
	var out bytes.Buffer
	if err = collector.Sync(ctx, 2, opts, &out); err != nil {
		t.Fatal(err)
	}
	cursor, err := os.ReadFile(filepath.Join(c.CacheDir, "sync-cursor"))
	if err != nil || strings.TrimSpace(string(cursor)) != head {
		t.Fatalf("bad cursor: %s %v", cursor, err)
	}
	out.Reset()
	if err = collector.Sync(ctx, 2, opts, &out); err != nil || out.Len() != 0 {
		t.Fatalf("already collected commits were rerun: %s %v", out.String(), err)
	}
	next := commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "46"})
	if err = collector.Sync(ctx, 1, opts, &out); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(next); err != nil {
		t.Fatal(err)
	}
	if err = collector.Backfill(ctx, root+".."+head, 2, opts, &out); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLock(filepath.Join(c.CacheDir, "sync.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out.Reset()
	if err = collector.Sync(ctx, 1, opts, &out); err != nil || !strings.Contains(out.String(), "skipped") {
		t.Fatalf("sync lock: %s %v", out.String(), err)
	}
}
