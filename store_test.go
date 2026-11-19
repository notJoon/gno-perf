package gnoperf

import (
	"math"
	"path/filepath"
	"testing"
)

func TestStoreTransactionsAndCache(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := newCollection()
	c.SHA = "abc"
	c.HarnessSHA = "h"
	c.Scope = "{}"
	c.Host = "host"
	c.GoVersion = "go1.25.9"
	c.Complete = true
	c.Values = []Measurement{{Tier: 1, Kind: "unit", Benchmark: "pkg#Test", Metric: "gas", Value: 42, Precision: 1, InputHash: "hash", Nondeterministic: true}}
	c.Nondeterministic = []string{c.Values[0].Key()}
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Cached("abc", "h", "{}", "host", "go1.25.9")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Values) != 1 || got.Values[0].Value != 42 || !got.Values[0].Nondeterministic {
		t.Fatalf("bad roundtrip: %+v", got)
	}
	if got, err = s.Cached("abc", "h", "different", "host", "go1.25.9"); err != nil || got != nil {
		t.Fatal("scope cache collision")
	}
	c.Complete = false
	c.Errors = []StepError{{"t1", "failed"}}
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load("abc")
	if err != nil || got.Complete || len(got.Errors) != 1 {
		t.Fatalf("latest failed collection missing: %+v %v", got, err)
	}
	c.SHA = "rollback"
	c.Values[0].Value = math.NaN()
	if err = s.Save(c); err == nil {
		t.Fatal("accepted NaN")
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM artifact WHERE sha='rollback'").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed save was not rolled back")
	}
}
