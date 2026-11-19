package gnoperf

import (
	"bytes"
	"context"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestInterspersedFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	quick := fs.Bool("quick", false, "")
	pkgs := fs.String("pkgs", "", "")
	p := fs.Int("p", 0, "")
	if err := parseFlags(fs, []string{"HEAD", "--quick", "--pkgs=p/...", "-p", "2"}); err != nil {
		t.Fatal(err)
	}
	if !*quick || *pkgs != "p/..." || *p != 2 || fs.NArg() != 1 || fs.Arg(0) != "HEAD" {
		t.Fatal("interspersed flags were lost")
	}
	if err := parseFlags(fs, []string{"--unknown"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := parseFlags(fs, []string{"--pkgs"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestCLICollectAndCompare(t *testing.T) {
	c, base := fixtureRepo(t)
	head := commitFiles(t, c.GnoCore, map[string]string{"examples/gas.txt": "43"})
	config := writeConfig(t, c)
	var out, logs bytes.Buffer
	for _, sha := range []string{base, head} {
		if err := Run(context.Background(), []string{"--config", config, "collect", sha, "--quick", "--skip-tx", "--skip-tier2"}, &out, &logs); err != nil {
			t.Fatalf("%s: %v", logs.String(), err)
		}
	}
	out.Reset()
	if err := Run(context.Background(), []string{"compare", base, head, "--format", "text", "--config", config}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Regressions") {
		t.Fatal(out.String())
	}
	if err := Run(context.Background(), []string{"--help"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
