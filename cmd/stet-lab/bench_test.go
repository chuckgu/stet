package main

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/lab/bench"
)

func TestCloneRefusesNumbersOutOfRange(t *testing.T) {
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"-x", "-1"}, "-x"},
		{[]string{"-rerun", "0"}, "-rerun"},
		{[]string{"-rerun", "-2"}, "-rerun"},
	} {
		// Nothing listens on the address: without the check the command
		// fails to connect instead of cloning anything.
		args := append([]string{"-pg", "127.0.0.1:1", "-arm", "hoop"}, c.args...)
		err := benchClone(args)
		if err == nil || !strings.HasPrefix(err.Error(), "bench clone: "+c.flag+" ") {
			t.Errorf("bench clone %s: err = %v, want a usage error about %s", strings.Join(c.args, " "), err, c.flag)
		}
	}
}

func TestSendKeepsTheXFlagsToTheXSet(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-sets", "X,V"}, "bench send: -sets: X is sent alone"},
		{[]string{"-sets", "V", "-x", "2"}, "bench send: -x and -from go with -sets X"},
		{[]string{"-from", "3"}, "bench send: -x and -from go with -sets X"},
		{[]string{"-sets", "X", "-x", "-1"}, "bench send: -x -1:"},
		{[]string{"-sets", "X", "-from", "-2"}, "bench send: -from -2:"},
		{[]string{"-arm", "hoop-cache", "-sets", "X"}, `bench send: -sets: arm hoop-cache does not send set "X"`},
	} {
		// Nothing listens on the addresses: without the check the command
		// fails to connect instead.
		args := append([]string{"-pg", "127.0.0.1:1", "-arm", "hoop", "-addr", "127.0.0.1:2"}, c.args...)
		err := benchSend(args)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("bench send %s: err = %v, want one starting %q", strings.Join(c.args, " "), err, c.want)
		}
	}
}

func TestSendGoesThroughTheArmsProxy(t *testing.T) {
	for _, c := range []struct {
		args    []string
		refused bool
	}{
		{[]string{"-arm", "hoop", "-addr", "127.0.0.1:1"}, true}, // the database server itself
		{[]string{"-arm", "hoop", "-addr", "localhost:1"}, true},
		{[]string{"-arm", "stet", "-sets", "X", "-addr", "127.0.0.1:1"}, true},
		{[]string{"-arm", "stet-bind", "-sets", "V", "-addr", "localhost:1"}, true},
		{[]string{"-practice", "-arm", "hoop-a", "-addr", "127.0.0.1:1"}, true},
		{[]string{"-arm", "none", "-addr", "127.0.0.1:2"}, true}, // none goes to the database server
		{[]string{"-arm", "none", "-sets", "X", "-addr", "127.0.0.1:2"}, true},
		{[]string{"-arm", "hoop", "-addr", "127.0.0.1:2"}, false},
		{[]string{"-arm", "stet", "-sets", "X", "-addr", "localhost:2"}, false},
		{[]string{"-arm", "none", "-addr", "localhost:1"}, false},
		{[]string{"-arm", "none", "-sets", "X", "-addr", "127.0.0.1:1"}, false},
	} {
		// Nothing listens on either address: an address that passes the check
		// fails to connect instead.
		err := benchSend(append([]string{"-pg", "127.0.0.1:1"}, c.args...))
		refused := err != nil && strings.HasPrefix(err.Error(), "bench send: -addr ")
		if err == nil || refused != c.refused {
			t.Errorf("bench send %s: err = %v, want the -addr refused: %v", strings.Join(c.args, " "), err, c.refused)
		}
	}
}

func TestPracticeSendsXIntoThePracticeDatabases(t *testing.T) {
	// Nothing listens on the addresses: the command gets as far as connecting to the arm's own database.
	for _, arm := range []string{"none", "stet", "hoop-a"} {
		addr := "127.0.0.1:2"
		if arm == "none" {
			addr = "127.0.0.1:1"
		}
		err := benchSend([]string{"-pg", "127.0.0.1:1", "-practice", "-arm", arm, "-sets", "X", "-addr", addr})
		if want := bench.DBName(arm, true, 0, 1); err == nil || !strings.Contains(err.Error(), "database="+want+"`") {
			t.Errorf("bench send -practice -arm %s -sets X: err = %v, want it to connect to %s", arm, err, want)
		}
	}
}

// noDatabases starts a server that answers every connection's startup as PostgreSQL does for a database that
// does not exist, and returns its address.
func noDatabases(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				be := pgproto3.NewBackend(c, c)
				msg, err := be.ReceiveStartupMessage()
				if err != nil {
					return
				}
				if sm, ok := msg.(*pgproto3.StartupMessage); ok {
					be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "3D000",
						Message: fmt.Sprintf("database %q does not exist", sm.Parameters["database"])})
					be.Flush()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestScoreNamesTheArmWhoseDatabaseIsMissing(t *testing.T) {
	addr := noDatabases(t)
	for _, c := range []struct{ arms, arm, db string }{
		{"hoop-opus", "hoop-opus", "crm_hoop_opus"},
		{"hoop:2", "hoop", "crm_hoop_r2"},
	} {
		err := benchScore([]string{"-pg", addr, "-arms", c.arms})
		if err == nil || !strings.Contains(err.Error(), "arm "+c.arm+" ") || !strings.Contains(err.Error(), c.db+" does not exist") {
			t.Errorf("bench score -arms %s: err = %v, want it to name arm %s and say %s does not exist", c.arms, err, c.arm, c.db)
		}
	}
}

func TestLLMRecorderOutMustBeAJSONLFile(t *testing.T) {
	// Only *.jsonl files in the directory count against the cap: any other
	// name would let a restart forget what it spent.
	for _, out := range []string{"", "run.log", "calls.json", "calls.jsonl.bak"} {
		args := []string{"-listen", "127.0.0.1:0"}
		if out != "" {
			args = append(args, "-out", filepath.Join(t.TempDir(), out))
		}
		err := benchLLMRecorder(args)
		if err == nil || !strings.Contains(err.Error(), "bench llm-recorder: -out") {
			t.Errorf("-out %q: err = %v", out, err)
		}
		if out != "" && !strings.Contains(err.Error(), ".jsonl") {
			t.Errorf("-out %q: err = %v, want it to name .jsonl", out, err)
		}
	}
}

func TestCollectTakesOnlyTheArmsOwnDatabases(t *testing.T) {
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"-arm", "stet", "-db", "crm_gate"}, true},
		{[]string{"-arm", "stet", "-db", "crm_gate_x"}, true},
		{[]string{"-arm", "stet", "-db", "crm_gate_x", "-db", "crm_gate_x3"}, true},
		{[]string{"-arm", "stet", "-rerun", "2", "-db", "crm_gate_r2_x2"}, true},
		{[]string{"-arm", "stet-bind", "-db", "crm_gate_bind_x"}, true},
		{[]string{"-arm", "stet", "-practice", "-db", "crm_practice_gate_x"}, true},
		{[]string{"-arm", "stet", "-rerun", "2", "-db", "crm_gate_x"}, false}, // run 1's X clone
		{[]string{"-arm", "stet", "-db", "crm_gate_r2_x"}, false},             // rerun 2's
		{[]string{"-arm", "stet", "-db", "crm_gate_bind_x"}, false},           // another arm's
		{[]string{"-arm", "stet", "-db", "crm_hoop_x"}, false},
		{[]string{"-arm", "stet", "-db", "crm_gate_x1"}, false}, // the first X clone is crm_gate_x
		{[]string{"-arm", "stet", "-db", "crm_gate_x0"}, false},
		{[]string{"-arm", "stet", "-db", "crm_gate_xx"}, false},
		{[]string{"-arm", "stet", "-db", "crm_gate_x", "-db", "crm_other"}, false},
		{[]string{"-arm", "stet", "-practice", "-db", "crm_gate_x"}, false},
	} {
		// Nothing listens on the address: a database that passes the check
		// fails to connect instead.
		err := benchCollect(append([]string{"-pg", "127.0.0.1:1"}, c.args...))
		refused := err != nil && strings.HasPrefix(err.Error(), "bench collect: -db ")
		if refused == c.ok {
			t.Errorf("bench collect %s: err = %v, want the -db refused: %v", strings.Join(c.args, " "), err, !c.ok)
		}
	}
}
