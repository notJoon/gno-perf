package gnoperf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	data, err := os.ReadFile("gnoperf.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gnoperf.toml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.GnoCore) || !filepath.IsAbs(c.CacheDir) || c.Parallelism != 2 {
		t.Fatalf("wrong config: %+v", c)
	}
	for _, bad := range []string{strings.Replace(string(data), "parallelism = 2", "parallelism = 0", 1), string(data) + "\nunknown = true\n", strings.Replace(string(data), "count = 10", "count = 1", 1)} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
}
