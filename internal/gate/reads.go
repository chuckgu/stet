package gate

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
	"github.com/stetdb/stet/internal/store"
)

const (
	// readRetention is how long a bucket that cannot be written is kept.
	readRetention = time.Hour
	// readLogQueue bounds the large reads waiting to be logged. Past it they
	// are dropped and counted, so Observe never waits for the store.
	readLogQueue = 1024
	// lateReadReport is how long reads observed after the gate closed are
	// counted before one line reports them.
	lateReadReport = 100 * time.Millisecond
)

// readFlushEvery is how often read buckets of past minutes are written.
var readFlushEvery = time.Minute

// readStats aggregates observed reads per minute, login role and table, and
// hands large reads to the background writer (spec 5).
//
// Nothing here touches the database on the read's path: Observe adds to the
// buckets under mu and queues large reads without waiting. One goroutine
// (record) does all the writing.
type readStats struct {
	logAbove map[string]int // tables whose reads are recorded → log_above; 0 aggregates only

	mu      sync.Mutex
	buckets map[readKey]*readCount
	write   func(context.Context, []store.ReadBucket) error // under mu
	now     func() time.Time                                // under mu
	// closed is set when record takes the buckets for the last time. Reads
	// observed after it are counted in late and reported in one line.
	closed    bool
	late      int64
	lateTimer bool // a report of late is pending

	logs    chan readEvent
	dropped atomic.Int64 // large reads not logged since last reported

	flushes chan chan struct{} // FlushReads' requests
	stop    chan struct{}
	stopped chan struct{} // closed when record has returned
	once    sync.Once
}

type readKey struct {
	minute int64 // Unix seconds at the start of the minute
	role   string
	table  string
}

type readCount struct {
	statements, rows, maxRows int64
	failing                   time.Time // when a write of it first failed; zero until then
}

// readEvent is a large read waiting to be logged.
type readEvent struct {
	session proxy.SessionInfo
	sql     string
	tables  []string
	at      time.Time
	sent    int64
	err     string
}

// newReadStats returns nil when no table records reads.
func newReadStats(tables map[string]int, write func(context.Context, []store.ReadBucket) error) *readStats {
	if len(tables) == 0 {
		return nil
	}
	return &readStats{
		logAbove: tables,
		buckets:  map[readKey]*readCount{},
		write:    write,
		now:      time.Now,
		logs:     make(chan readEvent, readLogQueue),
		flushes:  make(chan chan struct{}),
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// recordsAny reports whether reads of any of the gated tables in keys are
// recorded.
func (g *Gate) recordsAny(keys []string) bool {
	if g.reads == nil {
		return false
	}
	for _, k := range keys {
		if _, ok := g.reads.logAbove[k]; ok {
			return true
		}
	}
	return false
}

// Observe counts a read in the bucket of the minute it arrived, for each table
// it read that records reads: one statement and the rows sent. A read that
// sent more than any of those tables' log_above is also logged, once, naming
// every gated table it read. The proxy calls it before forwarding the read's
// ReadyForQuery, so it does no I/O and never waits for the store.
func (g *Gate) Observe(s proxy.SessionInfo, st proxy.Statement, o proxy.Outcome) {
	r, ok := st.Data.(sqlparse.Result)
	a := g.reads
	if !ok || a == nil || len(r.Reads) == 0 {
		return
	}
	at := o.Arrived
	if at.IsZero() { // the proxy always sets it
		at = time.Now()
	}
	minute := at.Truncate(time.Minute).Unix()

	// Under mu throughout, so that once record has taken the buckets for the
	// last time, every read is either in them or counted as late, and every
	// large read it counted is already queued for the last drain.
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		a.lateRead(g.log)
		return
	}
	large := false
	for _, t := range r.Reads {
		logAbove, ok := a.logAbove[t]
		if !ok {
			continue
		}
		k := readKey{minute, s.User, t}
		c := a.buckets[k]
		if c == nil {
			c = &readCount{}
			a.buckets[k] = c
		}
		c.statements++
		c.rows += o.Sent
		c.maxRows = max(c.maxRows, o.Sent)
		if logAbove > 0 && o.Sent > int64(logAbove) {
			large = true
		}
	}
	if !large {
		return
	}
	select {
	case a.logs <- readEvent{session: s, sql: st.SQL, tables: r.Reads, at: at, sent: o.Sent, err: o.Error}:
	default:
		a.dropped.Add(1)
	}
}

// lateRead counts a read observed after the gate closed. The first of a
// burst sets a timer, and one line reports them all. It runs with a.mu held.
func (a *readStats) lateRead(log *slog.Logger) {
	a.late++
	if a.lateTimer {
		return
	}
	a.lateTimer = true
	time.AfterFunc(lateReadReport, func() {
		a.mu.Lock()
		n := a.late
		a.late, a.lateTimer = 0, false
		a.mu.Unlock()
		log.Warn("reads observed after the gate closed were not recorded", "count", n)
	})
}

// record runs while the gate records reads. It logs large reads as they
// come, writes the buckets of past minutes every interval, and writes what
// is left when the gate closes.
func (g *Gate) record(every time.Duration) {
	a := g.reads
	defer close(a.stopped)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case ev := <-a.logs:
			g.logRead(context.Background(), ev)
		case <-tick.C:
			g.flushReads(false, false)
			g.reportDropped()
		case done := <-a.flushes:
			g.drainReadLogs(context.Background())
			g.flushReads(true, false)
			g.reportDropped()
			close(done)
		case <-a.stop:
			g.flushReads(true, true)
			ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
			g.drainReadLogs(ctx)
			cancel()
			g.reportDropped()
			return
		}
	}
}

// closeReads stops record once it has written everything. It may be called
// more than once.
func (g *Gate) closeReads() {
	a := g.reads
	if a == nil {
		return
	}
	a.once.Do(func() { close(a.stop) })
	<-a.stopped
}

// flushReads writes the buckets of minutes before the current one, or all of
// them. Buckets are taken out while they are written, so Observe never waits
// on the write. Ones that fail go back to be written next time, until they
// have failed for readRetention; the hour counts from the first failure, so a
// long read that lands in an old minute still gets its retries.
//
// A read is bucketed by when it arrived, so a long one can add to a minute
// already written; it is written as it appears, and stet.reads sums it in.
//
// last is the write at Close: it takes every bucket and closes the gate to
// later reads, and what fails is dropped, since nothing will retry it.
func (g *Gate) flushReads(all, last bool) {
	a := g.reads
	a.mu.Lock()
	a.closed = a.closed || last
	now, write := a.now(), a.write
	current := now.Truncate(time.Minute).Unix()
	var keys []readKey
	var counts []*readCount
	var buckets []store.ReadBucket
	for k, c := range a.buckets {
		if !all && k.minute >= current {
			continue
		}
		delete(a.buckets, k)
		keys, counts = append(keys, k), append(counts, c)
		buckets = append(buckets, store.ReadBucket{
			Bucket: time.Unix(k.minute, 0), LoginRole: k.role, Table: k.table,
			Statements: c.statements, Rows: c.rows, MaxRows: c.maxRows,
		})
	}
	a.mu.Unlock()
	if len(buckets) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	err := write(ctx, buckets)
	if err == nil {
		return
	}
	if last {
		g.log.Error("reads dropped: not written when the gate closed", "buckets", len(buckets), "err", err)
		return
	}
	kept, dropped := 0, 0
	a.mu.Lock()
	for i, k := range keys {
		c := counts[i]
		if c.failing.IsZero() {
			c.failing = now
		}
		if now.Sub(c.failing) >= readRetention {
			dropped++
			continue
		}
		kept++
		if newer := a.buckets[k]; newer != nil { // reads that came in meanwhile
			c.statements += newer.statements
			c.rows += newer.rows
			c.maxRows = max(c.maxRows, newer.maxRows)
		}
		a.buckets[k] = c
	}
	a.mu.Unlock()
	if kept > 0 {
		g.log.Error("reads not written, kept to retry", "buckets", kept, "err", err)
	}
	if dropped > 0 {
		g.log.Error("reads dropped after failing to be written for an hour", "buckets", dropped, "err", err)
	}
}

// drainReadLogs logs the large reads already queued. Once ctx is done the
// rest are dropped and counted.
func (g *Gate) drainReadLogs(ctx context.Context) {
	for {
		select {
		case ev := <-g.reads.logs:
			if ctx.Err() != nil {
				g.reads.dropped.Add(1)
				continue
			}
			g.logRead(ctx, ev)
		default:
			return
		}
	}
}

func (g *Gate) reportDropped() {
	if n := g.reads.dropped.Swap(0); n > 0 {
		g.log.Warn("large reads not logged: the queue was full or the gate was closing", "count", n)
	}
}

// logRead writes one line to stet.read_log, filled as judge fills a decision.
// A failure is logged only (spec 7).
func (g *Gate) logRead(ctx context.Context, ev readEvent) {
	l := &store.ReadLog{
		ID:          store.NewID(),
		CreatedAt:   ev.at,
		AgentID:     ev.session.ApplicationName,
		SessionID:   ev.session.ID,
		Statement:   ev.sql,
		StatementFP: sqlparse.Fingerprint(ev.sql),
		Tables:      ev.tables,
		Rows:        ev.sent,
	}
	if ev.session.User != "" {
		l.LoginRole = ptr(ev.session.User)
	}
	if ev.session.OnBehalfOf != "" {
		l.OnBehalfOf = ptr(ev.session.OnBehalfOf)
	}
	if ev.err != "" {
		l.ExecError = ptr(ev.err)
	}
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	if err := g.store.InsertReadLog(ctx, l); err != nil {
		g.log.Error("large read not recorded", "tables", ev.tables, "rows", ev.sent, "err", err)
	}
}
