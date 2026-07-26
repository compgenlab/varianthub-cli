package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Job statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusError   = "error"
)

// Job kinds.
const (
	KindLocus = "locus"
	KindVCF   = "vcf"
)

// Job is one row of the job table (its metadata, without the input/result blobs).
type Job struct {
	ID         string `json:"job_id"`
	Kind       string `json:"kind"`
	Snapshot   string `json:"snapshot"`
	Selection  string `json:"selection"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	NVariants  int64  `json:"n_variants"`
	ClientIP   string `json:"client_ip,omitempty"`
	Session    string `json:"session,omitempty"` // the submitter's session id (browser cookie), for scoping history
	Label      string `json:"label,omitempty"`   // short human label for history lists (the locus, or the VCF filename)
	CreatedAt  int64  `json:"created_at"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

// NewJob is the metadata for enqueuing a job (plus its input body).
type NewJob struct {
	Kind      string
	Snapshot  string
	Selection string
	ClientIP  string
	Session   string
	Label     string
	Body      []byte
}

// jobCols is the SELECT column list backing scanJob (kept in one place so every
// query stays in sync with the scan order).
const jobCols = `id,kind,snapshot,selection,status,error,n_variants,client_ip,session_id,label,created_at,started_at,finished_at`

// prefixCols qualifies each jobCols column with a table alias (for joins), e.g.
// prefixCols("j") → "j.id,j.kind,…". The scan order is unchanged.
func prefixCols(alias string) string {
	parts := strings.Split(jobCols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ",")
}

// Runner annotates one job's input, returning the result JSON and the number of
// variants. An error marks the job failed (its message is stored on the job).
type Runner func(ctx context.Context, job Job, input []byte) (result []byte, nVariants int, err error)

// Queue is the SQLite-backed async job queue: it persists jobs, their inputs, and
// their results, and drives a worker pool that annotates queued jobs. It uses its
// own database, separate from the annotation cache.
type Queue struct {
	db     *sql.DB
	notify chan struct{}
	nowFn  func() int64

	maxJobsPerIP int // per-IP concurrent running-job cap (<=0 = unlimited)

	wg sync.WaitGroup
}

// SetMaxJobsPerIP sets the per-IP concurrent running-job cap enforced by the fair
// scheduler (<=0 = unlimited). Call before starting workers.
func (q *Queue) SetMaxJobsPerIP(n int) { q.maxJobsPerIP = n }

const queueSchema = `
CREATE TABLE IF NOT EXISTS job (
  id          TEXT PRIMARY KEY,
  kind        TEXT    NOT NULL,
  snapshot    TEXT    NOT NULL,
  selection   TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL,
  error       TEXT,
  n_variants  INTEGER,
  client_ip   TEXT    NOT NULL DEFAULT '',
  session_id  TEXT    NOT NULL DEFAULT '',
  label       TEXT    NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  started_at  INTEGER,
  finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS job_status ON job(status, created_at);
CREATE INDEX IF NOT EXISTS job_finished ON job(finished_at);

CREATE TABLE IF NOT EXISTS job_input  (job_id TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS job_result (job_id TEXT PRIMARY KEY, json TEXT NOT NULL);
`

// OpenQueue opens (creating if needed) the job-queue database at path and prepares
// its schema. Any jobs left "running" from a previous process (a crash) are reset
// to "queued" so the worker pool re-processes them.
func OpenQueue(ctx context.Context, path string) (*Queue, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open server db %s: %w", path, err)
	}
	// Single connection keeps the embedded writer simple and, with WAL + a busy
	// timeout, avoids "database is locked" churn between the workers and HTTP handlers.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, queueSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init server db schema: %w", err)
	}
	// Migrate older job DBs: add columns introduced after the table existed. This
	// must precede any index that references those columns (e.g. job_session below).
	for _, col := range []string{
		"client_ip TEXT NOT NULL DEFAULT ''",
		"session_id TEXT NOT NULL DEFAULT ''",
		"label TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE job ADD COLUMN "+col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate job table (%s): %w", col, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS job_session ON job(session_id, created_at)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create job_session index: %w", err)
	}
	// Crash recovery: requeue anything left mid-flight.
	if _, err := db.ExecContext(ctx,
		`UPDATE job SET status=?, started_at=NULL WHERE status=?`, StatusQueued, StatusRunning); err != nil {
		db.Close()
		return nil, fmt.Errorf("requeue running jobs: %w", err)
	}
	return &Queue{db: db, notify: make(chan struct{}, 1), nowFn: func() int64 { return time.Now().Unix() }}, nil
}

// Close closes the queue database.
func (q *Queue) Close() error { return q.db.Close() }

// newID returns a random 128-bit hex job id.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Enqueue records a new queued job (metadata + input body) and wakes a worker.
func (q *Queue) Enqueue(ctx context.Context, j NewJob) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job(id,kind,snapshot,selection,status,client_ip,session_id,label,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		id, j.Kind, j.Snapshot, j.Selection, StatusQueued, j.ClientIP, j.Session, j.Label, q.nowFn()); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_input(job_id,body) VALUES(?,?)`, id, j.Body); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	log.Printf("cganno server: job %s queued (kind=%s, ip=%s, session=%s, selection=%q, %d bytes)",
		id, j.Kind, j.ClientIP, j.Session, j.Selection, len(j.Body))
	q.poke()
	return id, nil
}

// poke wakes one waiting worker (non-blocking).
func (q *Queue) poke() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Get returns a job's metadata (ok=false when the id is unknown).
func (q *Queue) Get(ctx context.Context, id string) (Job, bool, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT `+jobCols+` FROM job WHERE id=?`, id)
	j, err := scanJob(row)
	if err == sql.ErrNoRows {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// WaitFor blocks until job id reaches a terminal status (done/error), the timeout
// elapses, or ctx is cancelled — returning the latest job seen. ok=false only when
// the id is unknown. It polls the job row (the annotation runs in the worker pool,
// so this holds only an HTTP goroutine, not a worker). A timeout <= 0 returns the
// current job immediately without waiting.
func (q *Queue) WaitFor(ctx context.Context, id string, timeout time.Duration) (Job, bool, error) {
	job, ok, err := q.Get(ctx, id)
	if err != nil || !ok || timeout <= 0 {
		return job, ok, err
	}
	if job.Status == StatusDone || job.Status == StatusError {
		return job, true, nil
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return job, true, ctx.Err()
		case <-ticker.C:
			job, ok, err = q.Get(ctx, id)
			if err != nil || !ok {
				return job, ok, err
			}
			if job.Status == StatusDone || job.Status == StatusError || !time.Now().Before(deadline) {
				return job, true, nil
			}
		}
	}
}

// Result returns a done job's result JSON (ok=false when the id is unknown or the
// job has no stored result yet).
func (q *Queue) Result(ctx context.Context, id string) ([]byte, bool, error) {
	var js string
	err := q.db.QueryRowContext(ctx, `SELECT json FROM job_result WHERE job_id=?`, id).Scan(&js)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(js), true, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(row rowScanner) (Job, error) {
	var j Job
	var errStr, clientIP, session, label sql.NullString
	var nVar, started, finished sql.NullInt64
	if err := row.Scan(&j.ID, &j.Kind, &j.Snapshot, &j.Selection, &j.Status,
		&errStr, &nVar, &clientIP, &session, &label, &j.CreatedAt, &started, &finished); err != nil {
		return Job{}, err
	}
	j.Error = errStr.String
	j.NVariants = nVar.Int64
	j.ClientIP = clientIP.String
	j.Session = session.String
	j.Label = label.String
	j.StartedAt = started.Int64
	j.FinishedAt = finished.Int64
	return j, nil
}

// JobFilter narrows a List query. Empty fields are not constrained.
type JobFilter struct {
	Status   string // queued|running|done|error
	Session  string // scope to one submitter's session id
	ClientIP string // scope to one client IP
}

// List returns jobs newest-first matching the filter, with limit/offset paging.
func (q *Queue) List(ctx context.Context, f JobFilter, limit, offset int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, f.Status)
	}
	if f.Session != "" {
		where = append(where, "session_id=?")
		args = append(args, f.Session)
	}
	if f.ClientIP != "" {
		where = append(where, "client_ip=?")
		args = append(args, f.ClientIP)
	}
	query := `SELECT ` + jobCols + ` FROM job`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteOlderThan removes terminal jobs (done/error, i.e. finished_at set) whose
// finished_at is before cutoff, along with their input and result blobs. Queued and
// running jobs are never touched. Returns the number of jobs deleted.
func (q *Queue) DeleteOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// No FK cascade in the schema, so delete children explicitly first.
	const where = `SELECT id FROM job WHERE finished_at IS NOT NULL AND finished_at < ?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_result WHERE job_id IN (`+where+`)`, cutoff); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_input WHERE job_id IN (`+where+`)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM job WHERE finished_at IS NOT NULL AND finished_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// StartSweeper launches a goroutine that deletes terminal jobs older than ttl,
// sweeping once immediately and then every interval until ctx is cancelled. A
// ttl <= 0 disables GC (the goroutine is not started).
func (q *Queue) StartSweeper(ctx context.Context, ttl, interval time.Duration) {
	if ttl <= 0 {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		sweep := func() {
			cutoff := q.nowFn() - int64(ttl.Seconds())
			if n, err := q.DeleteOlderThan(ctx, cutoff); err != nil {
				log.Printf("cganno server: job GC: %v", err)
			} else if n > 0 {
				log.Printf("cganno server: job GC removed %d job(s) older than %s", n, ttl)
			}
		}
		sweep()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

// StartWorkers launches n worker goroutines that claim and process queued jobs
// until ctx is cancelled. Call Wait to block for their shutdown.
func (q *Queue) StartWorkers(ctx context.Context, n int, runner Runner) {
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		q.wg.Add(1)
		go q.worker(ctx, runner)
	}
}

// Wait blocks until all workers have stopped (after ctx cancellation).
func (q *Queue) Wait() { q.wg.Wait() }

func (q *Queue) worker(ctx context.Context, runner Runner) {
	defer q.wg.Done()
	ticker := time.NewTicker(time.Second) // fallback poll (also covers missed pokes)
	defer ticker.Stop()
	for {
		// Drain all currently-queued jobs before sleeping.
		for {
			if ctx.Err() != nil {
				return
			}
			job, input, ok, err := q.claimNext(ctx)
			if err != nil {
				log.Printf("cganno server: claim job: %v", err)
				break
			}
			if !ok {
				break
			}
			q.process(ctx, job, input, runner)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.notify:
		case <-ticker.C:
		}
	}
}

// claimNext atomically claims the next queued job, marking it running. ok=false
// when there is nothing claimable. Scheduling is fair across client IPs: among
// queued jobs it prefers the IP with the fewest jobs already running (round-robin),
// and skips any IP already at the per-IP concurrency cap. This keeps one client
// from starving the pool. Ties break by oldest created_at (FIFO within an IP).
func (q *Queue) claimNext(ctx context.Context) (Job, []byte, bool, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, nil, false, err
	}
	defer tx.Rollback()

	// maxPerIP as an upper bound in SQL: <=0 means unlimited, expressed as a large
	// sentinel so the WHERE clause never filters.
	maxPerIP := q.maxJobsPerIP
	if maxPerIP <= 0 {
		maxPerIP = 1 << 30
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+prefixCols("j")+`
		 FROM job j
		 LEFT JOIN (SELECT client_ip, COUNT(*) c FROM job WHERE status=? GROUP BY client_ip) r
		   ON r.client_ip = j.client_ip
		 WHERE j.status=? AND COALESCE(r.c,0) < ?
		 ORDER BY COALESCE(r.c,0) ASC, j.created_at ASC, j.id ASC
		 LIMIT 1`, StatusRunning, StatusQueued, maxPerIP)
	job, err := scanJob(row)
	if err == sql.ErrNoRows {
		return Job{}, nil, false, nil
	}
	if err != nil {
		return Job{}, nil, false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE job SET status=?, started_at=? WHERE id=? AND status=?`,
		StatusRunning, q.nowFn(), job.ID, StatusQueued)
	if err != nil {
		return Job{}, nil, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, nil, false, nil // lost the claim to another worker
	}
	var body []byte
	if err := tx.QueryRowContext(ctx, `SELECT body FROM job_input WHERE job_id=?`, job.ID).Scan(&body); err != nil {
		return Job{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, nil, false, err
	}
	job.Status = StatusRunning
	return job, body, true, nil
}

// process runs the job's runner and records its outcome.
func (q *Queue) process(ctx context.Context, job Job, input []byte, runner Runner) {
	start := time.Now()
	log.Printf("cganno server: job %s running (kind=%s, ip=%s)", job.ID, job.Kind, job.ClientIP)
	result, nVar, err := runner(ctx, job, input)
	if err != nil {
		log.Printf("cganno server: job %s failed after %s: %v", job.ID, time.Since(start).Round(time.Millisecond), err)
		if _, uerr := q.db.ExecContext(ctx,
			`UPDATE job SET status=?, error=?, finished_at=? WHERE id=?`,
			StatusError, err.Error(), q.nowFn(), job.ID); uerr != nil {
			log.Printf("cganno server: mark job %s errored: %v", job.ID, uerr)
		}
		return
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("cganno server: store job %s result: %v", job.ID, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_result(job_id,json) VALUES(?,?)
		 ON CONFLICT(job_id) DO UPDATE SET json=excluded.json`, job.ID, string(result)); err != nil {
		log.Printf("cganno server: store job %s result: %v", job.ID, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE job SET status=?, n_variants=?, finished_at=? WHERE id=?`,
		StatusDone, nVar, q.nowFn(), job.ID); err != nil {
		log.Printf("cganno server: finish job %s: %v", job.ID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("cganno server: commit job %s: %v", job.ID, err)
		return
	}
	log.Printf("cganno server: job %s done (%d variant(s) in %s)", job.ID, nVar, time.Since(start).Round(time.Millisecond))
}
