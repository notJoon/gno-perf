package gnoperf

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type StepError struct{ Step, Message string }

type Collection struct {
	ID                                 int64
	SHA, ParentSHA, Date, Subject      string
	HarnessSHA, GoVersion, Host, Scope string
	StartedAt                          string
	DurationMS                         int64
	Complete                           bool
	Nondeterministic                   []string
	Errors                             []StepError
	Values                             []Measurement
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS artifact (
 id INTEGER PRIMARY KEY, sha TEXT NOT NULL UNIQUE, parent_sha TEXT, date TEXT NOT NULL, subject TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS collection (
 id INTEGER PRIMARY KEY, artifact_id INTEGER NOT NULL REFERENCES artifact(id), harness_sha TEXT NOT NULL,
 go_version TEXT NOT NULL, host TEXT NOT NULL, scope TEXT NOT NULL, started_at TEXT NOT NULL,
 duration_ms INTEGER, complete INTEGER NOT NULL, nondeterministic TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS series (
 id INTEGER PRIMARY KEY, tier INTEGER NOT NULL, kind TEXT NOT NULL, benchmark TEXT NOT NULL, metric TEXT NOT NULL,
 UNIQUE(kind, benchmark, metric)
);
CREATE TABLE IF NOT EXISTS value (
 collection_id INTEGER NOT NULL REFERENCES collection(id), series_id INTEGER NOT NULL REFERENCES series(id),
 value REAL, sample INTEGER NOT NULL DEFAULT 0, input_hash TEXT, source TEXT, precision REAL NOT NULL,
 nondeterministic INTEGER NOT NULL, PRIMARY KEY(collection_id, series_id, sample)
);
CREATE TABLE IF NOT EXISTS error (
 collection_id INTEGER NOT NULL REFERENCES collection(id), step TEXT NOT NULL, message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS collection_artifact ON collection(artifact_id, id);
`

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;" + schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Save(c *Collection) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO artifact(sha,parent_sha,date,subject) VALUES(?,?,?,?) ON CONFLICT(sha) DO NOTHING`, c.SHA, c.ParentSHA, c.Date, c.Subject); err != nil {
		return err
	}
	var artifact int64
	if err = tx.QueryRow("SELECT id FROM artifact WHERE sha=?", c.SHA).Scan(&artifact); err != nil {
		return err
	}
	nondet, err := json.Marshal(c.Nondeterministic)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO collection(artifact_id,harness_sha,go_version,host,scope,started_at,duration_ms,complete,nondeterministic) VALUES(?,?,?,?,?,?,?,?,?)`, artifact, c.HarnessSHA, c.GoVersion, c.Host, c.Scope, c.StartedAt, c.DurationMS, c.Complete, string(nondet))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, m := range c.Values {
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			return fmt.Errorf("nonfinite measurement %s", m.Key())
		}
		if _, err = tx.Exec(`INSERT INTO series(tier,kind,benchmark,metric) VALUES(?,?,?,?) ON CONFLICT(kind,benchmark,metric) DO NOTHING`, m.Tier, m.Kind, m.Benchmark, m.Metric); err != nil {
			return err
		}
		var series int64
		if err = tx.QueryRow("SELECT id FROM series WHERE kind=? AND benchmark=? AND metric=?", m.Kind, m.Benchmark, m.Metric).Scan(&series); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO value(collection_id,series_id,value,sample,input_hash,source,precision,nondeterministic) VALUES(?,?,?,?,?,?,?,?)`, id, series, m.Value, m.Sample, m.InputHash, m.Source, m.Precision, m.Nondeterministic); err != nil {
			return err
		}
	}
	for _, e := range c.Errors {
		if _, err = tx.Exec("INSERT INTO error(collection_id,step,message) VALUES(?,?,?)", id, e.Step, e.Message); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	c.ID = id
	return nil
}

func (s *Store) Load(sha string) (*Collection, error) {
	var id int64
	err := s.db.QueryRow(`SELECT c.id FROM collection c JOIN artifact a ON a.id=c.artifact_id WHERE a.sha=? ORDER BY c.id DESC LIMIT 1`, sha).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("collection for %s: %w", sha, err)
	}
	return s.loadID(id)
}

func (s *Store) Cached(sha, harness, scope, host, goVersion string) (*Collection, error) {
	var id int64
	err := s.db.QueryRow(`SELECT c.id FROM collection c JOIN artifact a ON a.id=c.artifact_id WHERE a.sha=? AND c.harness_sha=? AND c.scope=? AND c.host=? AND c.go_version=? AND c.complete=1 ORDER BY c.id DESC LIMIT 1`, sha, harness, scope, host, goVersion).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.loadID(id)
}

func (s *Store) loadID(id int64) (*Collection, error) {
	c := &Collection{ID: id}
	var nondet string
	err := s.db.QueryRow(`SELECT a.sha,COALESCE(a.parent_sha,''),a.date,a.subject,c.harness_sha,c.go_version,c.host,c.scope,c.started_at,c.duration_ms,c.complete,c.nondeterministic FROM collection c JOIN artifact a ON a.id=c.artifact_id WHERE c.id=?`, id).Scan(&c.SHA, &c.ParentSHA, &c.Date, &c.Subject, &c.HarnessSHA, &c.GoVersion, &c.Host, &c.Scope, &c.StartedAt, &c.DurationMS, &c.Complete, &nondet)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(nondet), &c.Nondeterministic); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT s.tier,s.kind,s.benchmark,s.metric,v.value,v.sample,COALESCE(v.input_hash,''),COALESCE(v.source,''),v.precision,v.nondeterministic FROM value v JOIN series s ON s.id=v.series_id WHERE v.collection_id=? ORDER BY s.kind,s.benchmark,s.metric,v.sample`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m Measurement
		var value sql.NullFloat64
		if err = rows.Scan(&m.Tier, &m.Kind, &m.Benchmark, &m.Metric, &value, &m.Sample, &m.InputHash, &m.Source, &m.Precision, &m.Nondeterministic); err != nil {
			rows.Close()
			return nil, err
		}
		if value.Valid {
			m.Value = value.Float64
			c.Values = append(c.Values, m)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.Query("SELECT step,message FROM error WHERE collection_id=?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e StepError
		if err = rows.Scan(&e.Step, &e.Message); err != nil {
			return nil, err
		}
		c.Errors = append(c.Errors, e)
	}
	return c, rows.Err()
}

func newCollection() *Collection {
	return &Collection{StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}
