package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
)

const (
	staleQueue          = 1024                   // lookups waiting; past it a read is dropped and counted
	staleWorkers        = 2                      // lookups running at once
	staleLookupTimeout  = 500 * time.Millisecond // one lookup (spec-v0.2-stale 5); past it the read is not remembered
	staleCatalogTimeout = 5 * time.Second        // one reading of the catalog, every table with stale
)

var (
	// staleCatalogEvery is how often the catalog is read again: a var so
	// tests can shorten it.
	staleCatalogEvery = time.Minute
	// staleReportEvery is how often what was not remembered is logged
	// (spec-v0.2-stale 8).
	staleReportEvery = time.Minute
)

// stale is the read side of stale-write detection (spec-v0.2-stale 4, 5, 8):
// which tables are tracked, as the catalog says; the reads Observe hands the
// memory; and the queue and workers that look up the versions of the rows
// those reads sent, on Stet's own connections, outside any transaction, and
// feed them to the memory. Nothing on a client's path waits for a lookup.
type stale struct {
	mem   *readMemory
	pks   map[string]string // config key → key column, of every table with stale
	pool  *pgxpool.Pool
	query func(ctx context.Context, sql string, args []*string) ([][]*string, error) // Gate.queryText
	log   *slog.Logger

	// cat is the catalog last read, swapped whole: Tracked and Observe read
	// it without a lock.
	cat atomic.Pointer[staleCatalog]
	// lookup reads the versions of rows (lookupVersions): a field so tests
	// can hold it back or make it fail. lookupTimeout bounds each call:
	// staleLookupTimeout, a field so tests can lengthen it.
	lookup        func(ctx context.Context, table string, keys []string) (map[string]string, error)
	lookupTimeout time.Duration

	jobs chan lookupJob
	// mu orders queueing against Close: a lookup is queued under its read
	// lock, and Close sets closed under its write lock, so every lookup
	// queued before Close is on the queue when Close drains it, and none is
	// queued after.
	mu     sync.RWMutex
	closed bool
	idle   idleCount

	// What was not remembered since it was last logged (report).
	dropped atomic.Int64 // lookups the full queue turned away
	failed  atomic.Int64 // lookups that failed or ran out of time
	// lastFailure is why the last failed lookup failed (failure); nil when
	// none failed.
	lastFailure atomic.Pointer[string]

	// writes is what the write side keeps of marks between calls
	// (stale_write.go).
	writes ownWrites

	ctx    context.Context // done once the gate closes: running lookups and catalog reads end
	cancel context.CancelFunc
	stop   chan struct{}
	wg     sync.WaitGroup // the workers and watch
	once   sync.Once

	// What the catalog said of each table at the last reading, and which
	// tables that reading could not read. Touched by newStale, then by
	// watch alone.
	known   map[string]catalogEntry
	failing map[string]bool
}

// lookupJob is a lookup waiting or running: a read's, which learned or
// dropped ends on its ticket, or a refresh's, whose then is called.
type lookupJob struct {
	table string
	keys  []string
	t     *ticket
	then  func(serverKey, version string, ok bool)
}

// newStale reads the catalog once and starts the workers. A table the
// catalog does not track is logged and left out; the gate starts anyway.
func newStale(g *Gate, remember map[string]time.Duration) *stale {
	ctx, cancel := context.WithCancel(context.Background())
	s := &stale{
		mem:     newReadMemory(remember, g.cfg.StaleMaxKeys, time.Now),
		pks:     map[string]string{},
		pool:    g.pool,
		query:   g.queryText,
		log:     g.log,
		jobs:    make(chan lookupJob, staleQueue),
		ctx:     ctx,
		cancel:  cancel,
		stop:    make(chan struct{}),
		known:   map[string]catalogEntry{},
		failing: map[string]bool{},
	}
	for key := range remember {
		s.pks[key] = g.tables[key]
	}
	s.lookup, s.lookupTimeout = s.lookupVersions, staleLookupTimeout
	s.readCatalog()
	s.wg.Add(staleWorkers + 1)
	for range staleWorkers {
		go s.work()
	}
	go s.watch(staleCatalogEvery, staleReportEvery)
	return s
}

// remembersAny reports whether any of the config keys tables has stale,
// whether or not the catalog tracks it now.
func (s *stale) remembersAny(tables []string) bool {
	if s == nil {
		return false
	}
	return slices.ContainsFunc(tables, func(t string) bool { _, ok := s.pks[t]; return ok })
}

// staleRead is what one read hands the memory of one table: the keys of the
// rows it sent, as the server writes them, each once; or, when the result
// gave none, the one key its WHERE clause selected by (spec 4, correction
// S5), as the agent wrote it, which the read is noted under as some row of
// the table and whose lookup supplies the server's text.
type staleRead struct {
	table string
	keys  []string
	where *string
}

// count is how many keys r gives the memory: one for a WHERE clause's key.
func (r staleRead) count() int64 {
	if r.where != nil {
		return 1
	}
	return int64(len(r.keys))
}

// reads is what the read r, which ended with o, gives the memory, table by
// table in the order of r.Reads. It gives nothing when the session's login
// role is not known, since the memory is a login role's, nor for a read that
// failed or whose connection ended before its answer (spec 4): what it showed
// the client is not known. It does no I/O and does not change the memory.
func (s *stale) reads(sess proxy.SessionInfo, r sqlparse.Result, o proxy.Outcome) []staleRead {
	if s == nil || sess.User == "" || !o.Executed || o.Error != "" || o.Lost || o.Read == nil {
		return nil
	}
	c := s.cat.Load()
	byTable := map[string][]string{}
	seen := map[proxy.RowKey]bool{}
	for _, k := range o.Read.Keys {
		// A key of a table the statement does not name (one renamed since
		// the catalog was read, say) is left out, as the read's counts are
		// of the tables it names.
		table, ok := c.byOID[k.Table]
		if !ok || seen[k] || !slices.Contains(r.Reads, table) {
			continue
		}
		seen[k] = true
		byTable[table] = append(byTable[table], k.Text)
	}
	var out []staleRead
	for _, table := range r.Reads {
		if keys := byTable[table]; len(keys) > 0 {
			out = append(out, staleRead{table: table, keys: keys})
		}
	}
	// r.ReadKey is set only when r reads one table.
	if r.ReadKey != nil && len(r.Reads) == 1 && len(byTable[r.Reads[0]]) == 0 && o.Sent >= 1 {
		if k, ok := whereKey(c, r.Reads[0], *r.ReadKey, o.Read); ok {
			out = append(out, staleRead{table: r.Reads[0], where: &k})
		}
	}
	return out
}

// whereKey is the key that the value v of a read's WHERE clause `key = v`
// gives, as text a lookup reads as table's key column: a literal's text, or
// the bind value the proxy kept, read as the type it was sent as: the one
// the client gave it, else the one its cast names, else the key column's.
// ok is false when the table is not tracked; when the result had no column
// of it (the classifier matches tables by name, and a table of the same name
// in another schema would name another row: correction S5); when v does not
// read as a value of the key column (keyUsable), or the client gave the
// value a type the key column may compare otherwise (paramType); and for a
// value that is NULL or KeyOutput does not read.
func whereKey(c *staleCatalog, table string, v sqlparse.Value, rk *proxy.ReadKeys) (string, bool) {
	t, ok := c.byKey[table]
	if !ok || !slices.Contains(rk.Tables, t.oid) || !keyUsable(t, v) {
		return "", false
	}
	switch v.Kind {
	case sqlparse.Literal:
		return proxy.KeyOutput(t.typ, 0, []byte(v.Text))
	case sqlparse.Param:
		p := rk.Param
		if p == nil {
			return "", false
		}
		oid := p.OID
		if oid == 0 {
			oid = castOIDs[v.Cast] // 0 for none
		}
		typ, ok := paramType(oid, t)
		if !ok {
			return "", false
		}
		return proxy.KeyOutput(typ, p.Format, p.Value)
	}
	return "", false
}

// remember notes each of reads as a read of sess that ended now, and queues
// its lookup. A lookup the full queue turns away is dropped at once and
// counted. After Close it does nothing. It does no I/O and never waits.
func (s *stale) remember(sess proxy.SessionInfo, reads []staleRead) {
	if s == nil || len(reads) == 0 {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	p := party{role: sess.User, task: sess.OnBehalfOf}
	at := s.mem.now()
	// Counted at once for all the read's tables, so that StaleIdle cannot
	// return between two of them.
	s.idle.add(len(reads))
	for _, r := range reads {
		keys, ask := r.keys, r.keys
		if r.where != nil {
			keys, ask = nil, []string{*r.where}
		}
		// The ticket exists before its lookup is queued (see reading).
		t := s.mem.reading(p, r.table, keys, at)
		select {
		case s.jobs <- lookupJob{table: r.table, keys: ask, t: t}:
		default:
			s.dropped.Add(1)
			s.mem.dropped(t)
			s.idle.done()
		}
	}
}

// refresh queues a lookup of one row's version for a write that ran; then is
// called with the row's key as the server writes it and its xmin, with ok
// false when the row is not there, the lookup failed or the queue was full,
// or the catalog does not track table. then runs on a worker, or, when the
// queue is full, the gate closed or table is not tracked, before refresh
// returns.
func (s *stale) refresh(table, key string, then func(serverKey, version string, ok bool)) {
	queued := false
	s.mu.RLock()
	if _, ok := s.table(table); ok && !s.closed {
		s.idle.add(1)
		select {
		case s.jobs <- lookupJob{table: table, keys: []string{key}, then: then}:
			queued = true
		default:
			s.dropped.Add(1)
			defer s.idle.done() // after then, below
		}
	}
	s.mu.RUnlock()
	if !queued {
		then("", "", false)
	}
}

// lookupVersions reads, in one statement outside any transaction, the key
// text and xmin of the rows of table whose key is one of keys. A key may be
// written as an agent wrote it ('042'): the map's keys are the server's text.
//
// The keys go as one text parameter holding an array literal, its type left
// to the server, which reads it as an array of the key column's type (its
// base type, for a domain). Each element is double-quoted, with \ and "
// escaped, so that no key reads as NULL, as two elements, or as another key.
// A key that comes back from two rows, which only a key column the config
// wrongly calls a key can give, is left out: which row a write of it means
// is not known.
func (s *stale) lookupVersions(ctx context.Context, table string, keys []string) (map[string]string, error) {
	// A table the catalog does not track now may be another table than the
	// one its reads were remembered of, or have another key type.
	if _, ok := s.table(table); !ok {
		return nil, errUntracked
	}
	pk := s.pks[table]
	col := pgx.Identifier{pk}.Sanitize()
	q := fmt.Sprintf("select t.%s, t.xmin::text from %s as t where t.%s = any($1)", col, ident(table), col)
	rows, err := s.query(ctx, q, []*string{ptr(arrayLiteral(keys))})
	if err != nil {
		return nil, err
	}
	versions := make(map[string]string, len(rows))
	var twice []string
	for _, r := range rows {
		if r[0] == nil || r[1] == nil {
			continue
		}
		if _, ok := versions[*r[0]]; ok {
			twice = append(twice, *r[0])
		}
		versions[*r[0]] = *r[1]
	}
	for _, k := range twice {
		delete(versions, k)
	}
	return versions, nil
}

// arrayLiteral writes keys as the text of an array, every element quoted.
func arrayLiteral(keys []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for j := 0; j < len(k); j++ {
			if k[j] == '"' || k[j] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(k[j])
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// work runs lookups off the queue until the gate closes.
func (s *stale) work() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case j := <-s.jobs:
			s.run(j)
		}
	}
}

// run runs one lookup, under lookupTimeout, and ends it. A failure is
// counted with its cause, unless the gate is closing or the catalog stopped
// tracking the table, which it logged.
func (s *stale) run(j lookupJob) {
	defer s.idle.done()
	ctx, cancel := context.WithTimeout(s.ctx, s.lookupTimeout)
	versions, err := s.lookup(ctx, j.table, j.keys)
	cancel()
	if err != nil && s.ctx.Err() == nil && !errors.Is(err, errUntracked) {
		s.failed.Add(1)
		s.lastFailure.Store(ptr(failure(err)))
	}
	s.end(j, versions, err)
}

// failure is what err, a lookup's, says of why it failed, quoting nothing an
// agent wrote: the SQLSTATE the server gave (22P02 for a key the key
// column's type does not read), timeout, or error.
func failure(err error) string {
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		return pe.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "error"
}

var (
	// errClosed ends the lookups still queued when the gate closes.
	errClosed = errors.New("the gate closed")
	// errUntracked refuses a lookup in a table the catalog does not track.
	errUntracked = errors.New("the table is not tracked")
)

// end ends j with what its lookup found; err is not nil when it found nothing.
func (s *stale) end(j lookupJob, versions map[string]string, err error) {
	if j.t != nil {
		if err != nil {
			s.mem.dropped(j.t)
		} else {
			s.mem.learned(j.t, versions)
		}
		return
	}
	if err == nil && len(versions) == 1 {
		for k, v := range versions {
			j.then(k, v, true)
		}
		return
	}
	j.then("", "", false)
}

// watch reads the catalog again every catalogEvery and logs what was not
// remembered every reportEvery, until the gate closes.
func (s *stale) watch(catalogEvery, reportEvery time.Duration) {
	defer s.wg.Done()
	cat := time.NewTicker(catalogEvery)
	defer cat.Stop()
	rep := time.NewTicker(reportEvery)
	defer rep.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-cat.C:
			s.readCatalog()
		case <-rep.C:
			s.report()
		}
	}
}

// report logs, in one line, the reads the full queue dropped, the lookups
// that failed and why the last of them did, and the remembered versions the
// limits threw away since the last report; nothing when all are 0. No row
// data is logged.
func (s *stale) report() {
	dropped, failed, evicted := s.dropped.Swap(0), s.failed.Swap(0), s.mem.evicted()
	last := ""
	if p := s.lastFailure.Swap(nil); p != nil {
		last = *p
	}
	if dropped == 0 && failed == 0 && evicted == 0 {
		return
	}
	s.log.Warn("row versions not remembered: some reads will not be checked",
		"queue_full", dropped, "lookups_failed", failed, "last_failure", last, "evicted", evicted)
}

// close stops the workers, ends what is left on the queue with nothing
// learned, ends the marks of lost writes still waiting out their grace, and
// logs what was not remembered. A lookup still running ends at once: its
// context is cancelled. Observe and refresh do nothing to the memory
// afterwards. It may be called more than once.
func (s *stale) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		close(s.stop)
		s.wg.Wait()
		for drained := false; !drained; {
			select {
			case j := <-s.jobs:
				s.end(j, nil, errClosed)
				s.idle.done()
			default:
				drained = true
			}
		}
		s.closeWrites()
		s.report()
	})
}

// idleCount counts the lookups queued or running. Its zero value counts none.
type idleCount struct {
	mu   sync.Mutex
	n    int
	none chan struct{} // while n is not 0: closed when it gets back to 0
}

// add counts n more lookups.
func (c *idleCount) add(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		c.none = make(chan struct{})
	}
	c.n += n
}

func (c *idleCount) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n--; c.n == 0 {
		close(c.none)
	}
}

// wait returns once the count is 0, or with ctx's error once ctx is done.
func (c *idleCount) wait(ctx context.Context) error {
	c.mu.Lock()
	n, none := c.n, c.none
	c.mu.Unlock()
	if n == 0 {
		return nil
	}
	select {
	case <-none:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StaleIdle waits until no lookup of row versions is queued or running, or
// ctx is done. Tests and the lab use it; nothing on a client's path does.
func (g *Gate) StaleIdle(ctx context.Context) error {
	if g.stale == nil {
		return nil
	}
	return g.stale.idle.wait(ctx)
}
