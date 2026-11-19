package gnoperf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchOpsSummary(t *testing.T) {
	c, _ := fixtureRepo(t)
	bin := t.TempDir()
	data := "pkg: fixture\n" + strings.Repeat("BenchmarkOpTiny-2 1 10 ns/op 0 B/op 0 allocs/op\n", 10)
	script := "#!/bin/sh\nset -eu\ntest \"$GOTOOLCHAIN\" = go1.25.9\nprintf '%s' '" + data + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	collector := Collector{Config: c}
	var out bytes.Buffer
	if err := collector.BenchOps(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fixture#OpTiny-2 ns_op 10 ± 0% (n=10)") {
		t.Fatal(out.String())
	}
}
