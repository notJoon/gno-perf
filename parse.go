package gnoperf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/perf/benchfmt"
)

type Measurement struct {
	Tier                    int
	Kind, Benchmark, Metric string
	Value, Precision        float64
	Sample                  int
	InputHash, Source       string
	Nondeterministic        bool
}

func (m Measurement) Key() string { return m.Kind + "\x00" + m.Benchmark + "\x00" + m.Metric }

var (
	summaryRE = regexp.MustCompile(`^(ok|FAIL|\?)\s+(\S+)\s+(.*)$`)
	resultRE  = regexp.MustCompile(`^--- (PASS|FAIL|SKIP):\s+(\S+)`)
	gasRE     = regexp.MustCompile(`^--- GAS:\s+([0-9]+)\s*$`)
	fileGasRE = regexp.MustCompile(`\bgas: ([0-9]+)`)
	runtimeRE = regexp.MustCompile(`cycle=([^, ]+), allocs=([^,( ]+)`)
	storageRE = regexp.MustCompile(`([^\s:]+):([+-][0-9]+)b`)
	sizeRE    = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kMGTPE]?)$`)
)

func PrettySize(s string) (value, precision float64, err error) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("invalid size %q", s)
	}
	value, err = strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, 0, err
	}
	factor := 1.0
	if m[2] != "" {
		factor = math.Pow(1000, float64(strings.Index("kMGTPE", m[2])+1))
	}
	precision = 1
	if m[2] != "" {
		digits := 0
		if dot := strings.IndexByte(m[1], '.'); dot >= 0 {
			digits = len(m[1]) - dot - 1
		}
		precision = factor / math.Pow10(digits)
	}
	return value * factor, precision, nil
}

func scanLines(r io.Reader, fn func(string) error) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for s.Scan() {
		if err := fn(s.Text()); err != nil {
			return err
		}
	}
	return s.Err()
}

func packageName(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "./")
	return strings.TrimPrefix(s, "examples/")
}

// ParseGno waits for each package trailer before assigning buffered test output.
func ParseGno(r io.Reader) ([]Measurement, error) {
	return parseGno(r, false)
}

func parseGno(r io.Reader, storageSupported bool) ([]Measurement, error) {
	var out, pending []Measurement
	var test string
	var parseErrors []error
	add := func(kind, name, metric string, value, precision float64) {
		pending = append(pending, Measurement{Tier: 1, Kind: kind, Benchmark: name, Metric: metric, Value: value, Precision: precision})
	}
	storage := func(line string) (float64, bool) {
		var total float64
		matches := storageRE.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			n, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				parseErrors = append(parseErrors, err)
			}
			total += n
		}
		return total, strings.Contains(line, "storage:")
	}
	err := scanLines(r, func(line string) error {
		line = strings.TrimSpace(line)
		if m := summaryRE.FindStringSubmatch(line); m != nil {
			pkg := packageName(m[2])
			last := map[string]Measurement{}
			for _, v := range pending {
				if v.Kind == "unit" {
					v.Benchmark = pkg + "#" + v.Benchmark
				}
				if v.Kind == "pkg" {
					v.Benchmark = pkg
				}
				last[v.Key()] = v
			}
			for _, v := range last {
				out = append(out, v)
			}
			if m[1] == "ok" {
				if d, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(m[3]), "s"), 64); err == nil {
					out = append(out, Measurement{Tier: 2, Kind: "wall", Benchmark: pkg, Metric: "seconds", Value: d})
				}
			}
			if m[1] == "FAIL" {
				parseErrors = append(parseErrors, fmt.Errorf("package %s failed", pkg))
			}
			pending, test = nil, ""
			return nil
		}
		if strings.HasPrefix(line, "=== RUN") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "=== RUN"))
			if !strings.Contains(name, "/") || strings.HasSuffix(name, ".gno") {
				test = name
			}
		}
		if m := resultRE.FindStringSubmatch(line); m != nil {
			if m[1] == "FAIL" {
				parseErrors = append(parseErrors, fmt.Errorf("test %s failed", m[2]))
			}
			if strings.HasSuffix(m[2], ".gno") {
				name := packageName(m[2])
				if g := fileGasRE.FindStringSubmatch(line); g != nil {
					n, err := strconv.ParseFloat(g[1], 64)
					if err != nil {
						return err
					}
					add("filetest", name, "gas", n, 1)
				}
				n, present := storage(line)
				if fileGasRE.MatchString(line) && (present || storageSupported) {
					add("filetest", name, "storage", n, 1)
				}
			} else if !strings.Contains(m[2], "/") {
				test = m[2]
			}
		}
		if m := gasRE.FindStringSubmatch(line); m != nil && test != "" {
			n, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return err
			}
			add("unit", test, "gas", n, 1)
		} else if strings.HasPrefix(line, "--- GAS:") {
			return fmt.Errorf("malformed gas line: %s", line)
		}
		if strings.Contains(line, "runtime:") && test != "" {
			if m := runtimeRE.FindStringSubmatch(line); m != nil {
				for i, metric := range []string{"cycles", "allocs"} {
					if m[i+1] == "n/a" {
						continue
					}
					n, p, err := PrettySize(m[i+1])
					if err != nil {
						return err
					}
					add("unit", test, metric, n, p)
				}
			}
			n, present := storage(line)
			if present || storageSupported {
				add("pkg", "", "storage", n, 1)
			}
		}
		return nil
	})
	if len(pending) > 0 || test != "" {
		parseErrors = append(parseErrors, errors.New("truncated package output: missing package trailer"))
	}
	if err != nil {
		parseErrors = append(parseErrors, err)
	}
	sortMeasurements(out)
	return out, errors.Join(parseErrors...)
}

var txRunRE = regexp.MustCompile(`=== RUN\s+TestTestdata/(\S+)`)
var txCommandRE = regexp.MustCompile(`^>\s+!?\s*gnokey\b`)
var txGasRE = regexp.MustCompile(`^GAS USED:\s+([0-9]+)\s*$`)

func ParseTx(r io.Reader) ([]Measurement, error) {
	var out []Measurement
	var file, command string
	index := 0
	seen := false
	err := scanLines(r, func(line string) error {
		line = strings.TrimSpace(line)
		if m := txRunRE.FindStringSubmatch(line); m != nil {
			file, command, index = m[1], "", 0
			return nil
		}
		if strings.HasPrefix(line, "=== NAME") {
			return errors.New("interleaved txtar output; set SEQ_TS=true")
		}
		if strings.HasPrefix(line, "--- PASS: TestTestdata/") || strings.HasPrefix(line, "--- FAIL: TestTestdata/") {
			command = ""
		}
		if file == "" {
			return nil
		}
		if txCommandRE.MatchString(line) {
			index++
			command = strings.TrimSpace(strings.TrimPrefix(line, ">"))
			seen = false
			return nil
		}
		if strings.HasPrefix(line, ">") {
			command = ""
			return nil
		}
		if m := txGasRE.FindStringSubmatch(line); m != nil && command != "" {
			if seen {
				return errors.New("multiple GAS USED lines for a single command")
			}
			n, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return err
			}
			out = append(out, Measurement{Tier: 1, Kind: "tx", Benchmark: fmt.Sprintf("%s#%d %s", file, index, command), Metric: "gas_used", Value: n, Precision: 1, Source: "gno.land/pkg/integration/testdata/" + file + ".txtar"})
			seen = true
		}
		return nil
	})
	return out, err
}

func ParseBench(r io.Reader) ([]Measurement, error) {
	reader := benchfmt.NewReader(r, "benchmark")
	var out []Measurement
	samples := map[string]int{}
	for reader.Scan() {
		switch rec := reader.Result().(type) {
		case *benchfmt.SyntaxError:
			return out, rec
		case *benchfmt.Result:
			for _, v := range rec.Values {
				metric, value := "", v.Value
				switch v.Unit {
				case "sec/op":
					metric, value = "ns_op", value*1e9
				case "ns/op":
					metric = "ns_op"
				case "B/op":
					metric = "b_op"
				case "allocs/op":
					metric = "allocs_op"
				}
				if metric == "" {
					continue
				}
				m := Measurement{Tier: 2, Kind: "gobench", Benchmark: rec.GetConfig("pkg") + "#" + string(rec.Name), Metric: metric, Value: value}
				m.Sample = samples[m.Key()]
				samples[m.Key()]++
				out = append(out, m)
			}
		}
	}
	return out, reader.Err()
}

func sortMeasurements(ms []Measurement) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Key() == ms[j].Key() {
			return ms[i].Sample < ms[j].Sample
		}
		return ms[i].Key() < ms[j].Key()
	})
}

func CheckDeterminism(first, second []Measurement) []string {
	index := map[string]Measurement{}
	for _, m := range second {
		if m.Tier == 1 {
			index[m.Key()] = m
		}
	}
	var changed []string
	for i, m := range first {
		if m.Tier != 1 {
			continue
		}
		other, ok := index[m.Key()]
		if !ok || m.Value != other.Value {
			first[i].Nondeterministic = true
			changed = append(changed, m.Key())
		}
		delete(index, m.Key())
	}
	for key := range index {
		changed = append(changed, key)
	}
	sort.Strings(changed)
	return changed
}
