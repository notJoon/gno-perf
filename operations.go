package gnoperf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func (c *Collector) compareRefs(ctx context.Context, base, head string, opts CollectOptions) (*Report, error) {
	a, err := c.Collect(ctx, base, opts)
	if err != nil {
		return nil, err
	}
	b, err := c.Collect(ctx, head, opts)
	if err != nil {
		return nil, err
	}
	return Compare(ctx, c.Config.GnoCore, a, b, false)
}

// Sync starts at the current tip; backfill handles history before the first run.
func (c *Collector) Sync(ctx context.Context, maxCount int, opts CollectOptions, out io.Writer) error {
	if maxCount < 0 {
		return errors.New("--max must not be negative")
	}
	unlock, err := acquireLock(filepath.Join(c.Config.CacheDir, "sync.lock"))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		_, err = fmt.Fprintln(out, "sync already running; skipped")
		return err
	}
	if err != nil {
		return err
	}
	defer unlock()
	tip, err := c.fetch(ctx, "refs/heads/master")
	if err != nil {
		return err
	}
	cursorPath := filepath.Join(c.Config.CacheDir, "sync-cursor")
	data, err := os.ReadFile(cursorPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cursor := strings.TrimSpace(string(data))
	args := []string{"rev-list", "--first-parent", "--reverse"}
	if cursor != "" {
		if _, err = git(ctx, c.Config.GnoCore, "merge-base", "--is-ancestor", cursor, tip); err != nil {
			return fmt.Errorf("sync cursor is not an ancestor of master: %w", err)
		}
		args = append(args, cursor+".."+tip)
	} else {
		limit := maxCount
		if limit == 0 {
			limit = 1
		}
		args = append(args, "--max-count="+strconv.Itoa(limit), tip)
	}
	list, err := git(ctx, c.Config.GnoCore, args...)
	if err != nil {
		return err
	}
	commits := strings.Fields(list)
	if maxCount > 0 && len(commits) > maxCount {
		commits = commits[:maxCount]
	}
	for _, sha := range commits {
		parent, err := Resolve(ctx, c.Config.GnoCore, sha+"^1")
		if err != nil {
			return err
		}
		r, err := c.compareRefs(ctx, parent, sha, opts)
		if err != nil {
			return err
		}
		if err = SaveReport(c.Config, r, sha+".md", out); err != nil {
			return err
		}
		if err = writeAtomic(cursorPath, []byte(sha+"\n")); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (c *Collector) Backfill(ctx context.Context, commitRange string, every int, opts CollectOptions, out io.Writer) error {
	if every < 1 {
		return errors.New("--every must be positive")
	}
	from, to, ok := strings.Cut(commitRange, "..")
	if !ok || from == "" || to == "" || strings.Contains(to, "..") {
		return errors.New("backfill requires a from..to range")
	}
	base, err := Resolve(ctx, c.Config.GnoCore, from)
	if err != nil {
		return err
	}
	head, err := Resolve(ctx, c.Config.GnoCore, to)
	if err != nil {
		return err
	}
	list, err := git(ctx, c.Config.GnoCore, "rev-list", "--first-parent", "--reverse", base+".."+head)
	if err != nil {
		return err
	}
	for i, sha := range strings.Fields(list) {
		if i%every != 0 {
			continue
		}
		parent, err := Resolve(ctx, c.Config.GnoCore, sha+"^1")
		if err != nil {
			return err
		}
		r, err := c.compareRefs(ctx, parent, sha, opts)
		if err != nil {
			return err
		}
		if err = SaveReport(c.Config, r, sha+".md", out); err != nil {
			return err
		}
	}
	return nil
}
