package gnoperf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(*strings.Reader) ([]Measurement, error)
	}{
		{"gno", func(r *strings.Reader) ([]Measurement, error) { return ParseGno(r) }},
		{"tx", func(r *strings.Reader) ([]Measurement, error) { return ParseTx(r) }},
		{"bench", func(r *strings.Reader) ([]Measurement, error) { return ParseBench(r) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.name+".log"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.parse(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				t.Fatal("no measurements")
			}
			encoded, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tc.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, append(encoded, '\n'), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(want)) != string(encoded) {
				t.Fatalf("golden mismatch for %s", tc.name)
			}
		})
	}
}

func TestPrettySize(t *testing.T) {
	for _, tc := range []struct {
		s                string
		value, precision float64
	}{{"19.9M", 19900000, 100000}, {"482.1k", 482100, 100}, {"999", 999, 1}, {"0", 0, 1}, {"1.0G", 1e9, 1e8}} {
		v, p, err := PrettySize(tc.s)
		if err != nil || v != tc.value || p != tc.precision {
			t.Errorf("%s: %v %v %v", tc.s, v, p, err)
		}
	}
	if _, _, err := PrettySize("bad"); err == nil {
		t.Fatal("accepted invalid size")
	}
}

func TestParseGnoBoundaries(t *testing.T) {
	input := "=== RUN   TestA\n=== RUN   TestA/sub\n--- PASS: TestA/sub (0.00s)\n--- PASS: TestA (0.00s)\n--- GAS:  42\n---       runtime: cycle=42, allocs=n/a\nok ./gno.land/p/a 0.01s\n=== RUN   TestB\n--- GAS: 9\n"
	got, err := ParseGno(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("missing truncation error: %v", err)
	}
	for _, m := range got {
		if m.Kind == "unit" && m.Benchmark != "gno.land/p/a#TestA" {
			t.Fatalf("wrong attribution: %+v", m)
		}
		if m.Metric == "allocs" {
			t.Fatal("n/a should remain absent")
		}
	}
	if len(got) != 4 {
		t.Fatalf("got %d values, want gas, cycles, storage, wall", len(got))
	}
	if _, err := ParseGno(strings.NewReader("=== RUN TestA\n--- GAS: broken\nok ./p 0s\n")); err == nil {
		t.Fatal("accepted malformed gas")
	}
	if _, err := ParseGno(strings.NewReader("=== RUN TestA\n--- FAIL: TestA (0s)\nFAIL ./p 0s\n")); err == nil {
		t.Fatal("accepted failed test")
	}
}

func TestParseTxCommandBoundaries(t *testing.T) {
	input := "=== RUN TestTestdata/a\n> gnokey failed\nGAS WANTED: 9\n> gnokey success\nGAS USED: 7\n> stdout 'GAS USED: 123'\n=== RUN TestTestdata/b\n> gnokey next\nGAS USED: 2\n"
	got, err := ParseTx(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Benchmark != "a#2 gnokey success" || got[1].Benchmark != "b#1 gnokey next" {
		t.Fatalf("wrong pairing: %+v", got)
	}
}

func TestCheckDeterminism(t *testing.T) {
	first := []Measurement{{Tier: 1, Kind: "unit", Benchmark: "a", Metric: "gas", Value: 1}, {Tier: 1, Kind: "unit", Benchmark: "b", Metric: "gas", Value: 1}, {Tier: 2, Kind: "wall", Value: 4}}
	second := []Measurement{{Tier: 1, Kind: "unit", Benchmark: "a", Metric: "gas", Value: 2}, {Tier: 2, Kind: "wall", Value: 9}}
	changed := CheckDeterminism(first, second)
	if !reflect.DeepEqual(changed, []string{first[0].Key(), first[1].Key()}) || !first[0].Nondeterministic || !first[1].Nondeterministic || first[2].Nondeterministic {
		t.Fatalf("wrong determinism result: %v %+v", changed, first)
	}
}
