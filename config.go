package gnoperf

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	GnoCore       string   `toml:"gno_core"`
	CacheDir      string   `toml:"cache_dir"`
	WorktreeDir   string   `toml:"worktree_dir"`
	Parallelism   int      `toml:"parallelism"`
	QuickPackages []string `toml:"quick_packages"`
	Tier2         struct {
		Count         int      `toml:"count"`
		VMBench       string   `toml:"vm_bench"`
		StorePackages []string `toml:"store_packages"`
		StoreBench    string   `toml:"store_bench"`
	} `toml:"tier2"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := toml.NewDecoder(strings.NewReader(string(data))).DisallowUnknownFields().Decode(&c); err != nil {
		return c, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	for _, p := range []*string{&c.GnoCore, &c.CacheDir, &c.WorktreeDir} {
		if *p == "" {
			return c, fmt.Errorf("gno_core, cache_dir and worktree_dir are required")
		}
		if strings.HasPrefix(*p, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return c, err
			}
			*p = filepath.Join(home, (*p)[2:])
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
		*p = filepath.Clean(*p)
	}
	if c.GnoCore == c.WorktreeDir {
		return c, fmt.Errorf("worktree_dir must differ from gno_core")
	}
	if c.Parallelism < 1 {
		return c, fmt.Errorf("parallelism must be positive")
	}
	if c.Tier2.Count < 10 {
		return c, fmt.Errorf("tier2.count must be at least 10")
	}
	for _, pattern := range []string{c.Tier2.VMBench, c.Tier2.StoreBench} {
		if _, err := regexp.Compile(pattern); err != nil {
			return c, err
		}
	}
	for _, pkg := range append(append([]string{}, c.QuickPackages...), c.Tier2.StorePackages...) {
		if pkg == "" || strings.HasPrefix(pkg, "-") {
			return c, fmt.Errorf("invalid package pattern %q", pkg)
		}
	}
	return c, nil
}
