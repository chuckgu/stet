package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/proxy"
)

// Test accounts created by scripts/testdb.sh, one per auth method.
var testUsers = map[string]string{
	"stet_scram": "scram-pw",
	"stet_md5":   "md5-pw",
	"stet_clear": "clear-pw",
	"stet_trust": "",
}

func upstream(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return addr
}

func startProxy(t *testing.T, upstreamAddr string) (string, *proxy.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(upstreamAddr, slog.New(slog.DiscardHandler))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String(), srv
}

func connURL(addr, user, password string, params url.Values) string {
	u := url.URL{Scheme: "postgres", Host: addr, Path: "/stet_test"}
	if password != "" {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	if params == nil {
		params = url.Values{}
	}
	if params.Get("sslmode") == "" {
		params.Set("sslmode", "disable")
	}
	u.RawQuery = params.Encode()
	return u.String()
}

func connect(t *testing.T, addr, user string, params url.Values) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, connURL(addr, user, testUsers[user], params))
	if err != nil {
		t.Fatalf("connect as %s via %s: %v", user, addr, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestAuthMethodsPassThrough(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	for user := range testUsers {
		t.Run(user, func(t *testing.T) {
			conn := connect(t, addr, user, nil)
			var got string
			if err := conn.QueryRow(testCtx(t), "select current_user").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != user {
				t.Errorf("current_user = %q, want %q", got, user)
			}
		})
	}
}

func TestWrongPasswordIsRejectedByUpstream(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	_, err := pgx.Connect(testCtx(t), connURL(addr, "stet_scram", "wrong", nil))
	if code := pgCode(err); code != "28P01" {
		t.Fatalf("err = %v (SQLSTATE %q), want 28P01 invalid_password", err, code)
	}
}

func TestExtendedQueryWithParameters(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	conn := connect(t, addr, "stet_scram", nil)
	var got int
	if err := conn.QueryRow(testCtx(t), "select $1::int + 1", 41).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestWritesAndTransactions(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)
	mustExec(t, conn, "create temp table t (id int primary key, v text)")

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := tx.Exec(ctx, "insert into t values ($1, $2)", i, fmt.Sprint("v", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "insert into t values (3, 'rolled back')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := conn.QueryRow(ctx, "select count(*) from t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestCopyInAndOut(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)
	mustExec(t, conn, "create temp table c (n int, s text)")

	const rows = 50_000
	src := make([][]any, rows)
	for i := range src {
		src[i] = []any{i, "row"}
	}
	n, err := conn.CopyFrom(ctx, pgx.Identifier{"c"}, []string{"n", "s"}, pgx.CopyFromRows(src))
	if err != nil {
		t.Fatal(err)
	}
	if n != rows {
		t.Fatalf("copied in %d rows, want %d", n, rows)
	}

	var out bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &out, "copy c to stdout"); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(out.Bytes(), []byte("\n")); got != rows {
		t.Errorf("copied out %d rows, want %d", got, rows)
	}
}

func TestLargeResultSet(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	conn := connect(t, addr, "stet_scram", nil)
	const n = 200_000
	rows, err := conn.Query(testCtx(t), "select g, repeat('x', 100) from generate_series(1, $1) g", n)
	if err != nil {
		t.Fatal(err)
	}
	var count, sum int64
	for rows.Next() {
		var g int64
		var s string
		if err := rows.Scan(&g, &s); err != nil {
			t.Fatal(err)
		}
		count++
		sum += g
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != n || sum != n*(n+1)/2 {
		t.Errorf("count=%d sum=%d, want %d rows summing to %d", count, sum, n, n*(n+1)/2)
	}
}

func TestCancelRequestReachesUpstream(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	conn := connect(t, addr, "stet_scram", nil)

	errc := make(chan error, 1)
	go func() {
		_, err := conn.Exec(context.Background(), "select pg_sleep(10)")
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := conn.PgConn().CancelRequest(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if code := pgCode(err); code != "57014" {
			t.Fatalf("err = %v (SQLSTATE %q), want 57014 query_canceled", err, code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("query was not canceled through the proxy")
	}
}

func TestNotificationReachesIdleClient(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	listener := connect(t, addr, "stet_scram", nil)
	notifier := connect(t, addr, "stet_scram", nil)
	mustExec(t, listener, "listen stet_test_ch")

	notified := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, err := notifier.Exec(context.Background(), "notify stet_test_ch, 'hello'")
		notified <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := listener.WaitForNotification(ctx)
	if nerr := <-notified; nerr != nil {
		t.Fatal(nerr)
	}
	if err != nil {
		t.Fatalf("no notification through the proxy: %v", err)
	}
	if n.Payload != "hello" {
		t.Errorf("payload = %q, want hello", n.Payload)
	}
}

// A client that sends Flush instead of Sync waits for responses with no
// ReadyForQuery coming. The proxy must not hold those responses back.
func TestPipelineFlushWithoutSync(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))
	conn := connect(t, addr, "stet_scram", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p := conn.PgConn().StartPipeline(ctx)
	p.SendPrepare("stet_s1", "select $1::int", nil)
	p.SendFlushRequest()
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	res, err := p.GetResults()
	if err != nil {
		t.Fatalf("response to Flush did not arrive: %v", err)
	}
	if _, ok := res.(*pgconn.StatementDescription); !ok {
		t.Fatalf("got %T, want *pgconn.StatementDescription", res)
	}
	if err := p.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSSLRequestIsDeclined(t *testing.T) {
	addr, _ := startProxy(t, upstream(t))

	connect(t, addr, "stet_scram", url.Values{"sslmode": {"prefer"}})

	_, err := pgx.Connect(testCtx(t), connURL(addr, "stet_scram", "scram-pw", url.Values{"sslmode": {"require"}}))
	if err == nil {
		t.Fatal("sslmode=require connected, but the proxy does not offer TLS")
	}
}

func TestUnreachableUpstreamReportsError(t *testing.T) {
	addr, _ := startProxy(t, "127.0.0.1:1")
	_, err := pgx.Connect(testCtx(t), connURL(addr, "stet_scram", "scram-pw", nil))
	if code := pgCode(err); code != "08001" {
		t.Fatalf("err = %v (SQLSTATE %q), want 08001", err, code)
	}
}

func TestUpstreamSessionEndsWithClient(t *testing.T) {
	up := upstream(t)
	addr, _ := startProxy(t, up)
	admin := connect(t, up, "stet_scram", nil)

	conn := connect(t, addr, "stet_scram", url.Values{"application_name": {"stet-test-close"}})
	if got := countSessions(t, admin, "stet-test-close"); got != 1 {
		t.Fatalf("sessions before close = %d, want 1", got)
	}
	conn.Close(context.Background())
	waitForSessions(t, admin, "stet-test-close", 0)
}

func TestServerCloseEndsSessions(t *testing.T) {
	up := upstream(t)
	addr, srv := startProxy(t, up)
	admin := connect(t, up, "stet_scram", nil)
	conn := connect(t, addr, "stet_scram", url.Values{"application_name": {"stet-test-shutdown"}})

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(testCtx(t), "select 1"); err == nil {
		t.Error("query succeeded after the proxy closed")
	}
	waitForSessions(t, admin, "stet-test-shutdown", 0)
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(testCtx(t), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func countSessions(t *testing.T, admin *pgx.Conn, app string) int {
	t.Helper()
	var n int
	err := admin.QueryRow(testCtx(t),
		"select count(*) from pg_stat_activity where application_name = $1", app).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func waitForSessions(t *testing.T, admin *pgx.Conn, app string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := countSessions(t, admin, app)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream sessions for %s = %d, want %d", app, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
