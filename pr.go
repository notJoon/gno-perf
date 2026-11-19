package gnoperf

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type PRInfo struct {
	State, HeadRefOid, BaseRefName string
	MergeCommit                    *struct{ Oid string }
}

type PROptions struct {
	CollectOptions
	Base, Head      string
	RebaseBase, All bool
}

func (c *Collector) fetch(ctx context.Context, ref string) (sha string, resultErr error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	temporary := "refs/gnoperf/fetch/" + hex.EncodeToString(id[:])
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := git(cleanup, c.Config.GnoCore, "update-ref", "-d", temporary)
		resultErr = errors.Join(resultErr, err)
	}()
	if _, err := git(ctx, c.Config.GnoCore, "fetch", "--no-write-fetch-head", "https://github.com/gnolang/gno.git", ref+":"+temporary); err != nil {
		return "", err
	}
	return Resolve(ctx, c.Config.GnoCore, temporary)
}

func (c *Collector) PullRequest(ctx context.Context, number int, opts PROptions) (*Report, error) {
	if number < 1 {
		return nil, errors.New("PR number must be positive")
	}
	data, err := output(ctx, c.Config.GnoCore, []string{"GH_PROMPT_DISABLED=1"}, "gh", "pr", "view", strconv.Itoa(number), "--repo", "gnolang/gno", "--json", "state,headRefOid,baseRefName,mergeCommit")
	if err != nil {
		return nil, err
	}
	var info PRInfo
	if err = json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	if info.State != "OPEN" && info.State != "MERGED" {
		return nil, fmt.Errorf("PR is %s, expected OPEN or MERGED", info.State)
	}
	if _, err = c.fetch(ctx, "pull/"+strconv.Itoa(number)+"/head"); err != nil {
		return nil, err
	}
	base, head, err := c.prRefs(ctx, info, opts)
	if err != nil {
		return nil, err
	}
	baseOpts := opts.CollectOptions
	baseOpts.NoRepeat = true
	a, err := c.Collect(ctx, base, baseOpts)
	if err != nil {
		return nil, err
	}
	b, err := c.Collect(ctx, head, opts.CollectOptions)
	if err != nil {
		return nil, err
	}
	r, err := Compare(ctx, c.Config.GnoCore, a, b, opts.All)
	if err != nil {
		return nil, err
	}
	r.Title = fmt.Sprintf("PR #%d (%s)", number, strings.ToLower(info.State))
	return r, nil
}

func (c *Collector) prRefs(ctx context.Context, info PRInfo, opts PROptions) (string, string, error) {
	head := info.HeadRefOid
	if info.State == "MERGED" {
		if info.MergeCommit == nil || info.MergeCommit.Oid == "" {
			return "", "", errors.New("merged PR has no merge commit")
		}
		head = info.MergeCommit.Oid
	}
	if opts.Head != "" {
		head = opts.Head
	}
	if _, err := Resolve(ctx, c.Config.GnoCore, head); err != nil {
		if _, err = c.fetch(ctx, head); err != nil {
			return "", "", err
		}
	}
	head, err := Resolve(ctx, c.Config.GnoCore, head)
	if err != nil {
		return "", "", err
	}
	var base string
	if opts.RebaseBase {
		if opts.Base != "" {
			return "", "", errors.New("--base cannot be combined with --rebase-base")
		}
		base, err = c.fetch(ctx, "refs/heads/master")
		if err != nil {
			return "", "", err
		}
		head, err = c.mergeCommit(ctx, base, head)
		return base, head, err
	}
	if opts.Base != "" {
		base = opts.Base
	} else if info.State == "MERGED" {
		base = head + "^1"
	} else {
		if info.BaseRefName == "" || strings.HasPrefix(info.BaseRefName, "-") {
			return "", "", errors.New("invalid PR base branch")
		}
		tip, err := c.fetch(ctx, "refs/heads/"+info.BaseRefName)
		if err != nil {
			return "", "", err
		}
		base, err = git(ctx, c.Config.GnoCore, "merge-base", head, tip)
		if err != nil {
			return "", "", err
		}
	}
	base, err = Resolve(ctx, c.Config.GnoCore, base)
	return base, head, err
}

func (c *Collector) mergeCommit(ctx context.Context, base, head string) (string, error) {
	tree, err := git(ctx, c.Config.GnoCore, "merge-tree", "--write-tree", base, head)
	if err != nil {
		return "", fmt.Errorf("temporary merge failed (possible conflicts): %w", err)
	}
	tree = strings.SplitN(tree, "\n", 2)[0]
	env := []string{"GIT_AUTHOR_NAME=gno-perf", "GIT_AUTHOR_EMAIL=gnoperf@localhost", "GIT_COMMITTER_NAME=gno-perf", "GIT_COMMITTER_EMAIL=gnoperf@localhost", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
	data, err := output(ctx, c.Config.GnoCore, env, "git", "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", base, "-p", head, "-m", "Temporary performance comparison")
	return strings.TrimSpace(string(data)), err
}

func SaveReport(c Config, r *Report, name string, out io.Writer) error {
	var b bytes.Buffer
	if err := r.Write(&b, "md"); err != nil {
		return err
	}
	dir := filepath.Join(c.CacheDir, "reports")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if filepath.Base(name) != name {
		return errors.New("invalid report name")
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	_, err = out.Write(b.Bytes())
	return err
}
