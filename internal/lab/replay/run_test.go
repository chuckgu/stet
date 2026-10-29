package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
)

type flaky struct {
	mu    sync.Mutex
	fails map[string]int // remaining failures by state
	seen  []string
}

func (f *flaky) Decide(_ context.Context, state json.RawMessage, qs []decide.Question) (decide.Answers, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, string(state))
	if f.fails[string(state)] > 0 {
		f.fails[string(state)]--
		return decide.Answers{}, errors.New("503 from the model")
	}
	out := map[string]decide.Answer{}
	for _, q := range qs {
		out[q.ID] = decide.Answer{Choice: "none", Confidence: 0.9}
	}
	return decide.Answers{ByQuestion: out, CostUSD: 0.00002}, nil
}
func (f *flaky) ModelID() string      { return "flaky" }
func (f *flaky) ModelVersion() string { return "flaky-1" }

func TestPendingIsWhatTheModelWouldBeAsked(t *testing.T) {
	q := []decide.Question{{ID: "policy_1"}}
	recs := []Record{
		{ID: "a", Agent: "salesops", Questions: q, RawState: json.RawMessage(`{}`)},
		{ID: "done", Agent: "salesops", Questions: q, RawState: json.RawMessage(`{}`)},
		{ID: "unasked", Agent: "salesops", RawState: json.RawMessage(`{}`)},
		{ID: "setup", Agent: "crm-sync", Questions: q, RawState: json.RawMessage(`{}`)},
		{ID: "unsupported", Agent: "salesops", Questions: q, Verdict: decide.Unsupported},
	}
	got := Pending(recs, map[string]Judgment{"done": {}})
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("pending = %v, want only a", got)
	}
}

func TestAskRetriesAndSkipsWhatStillFails(t *testing.T) {
	q := []decide.Question{{ID: "duplicate"}}
	recs := []Record{
		{ID: "ok", Questions: q, RawState: json.RawMessage(`{"table":"a"}`)},
		{ID: "flaky", Questions: q, RawState: json.RawMessage(`{"table":"b"}`)},
		{ID: "down", Questions: q, RawState: json.RawMessage(`{"table":"c"}`)},
	}
	d := &flaky{fails: map[string]int{`{"table":"b"}`: 1, `{"table":"c"}`: 99}}
	var saved []Judgment
	n, failed, err := Ask(context.Background(), d, recs, AskOptions{Parallel: 2, Timeout: time.Second, Retries: 2},
		func(j Judgment) error { saved = append(saved, j); return nil })
	if err != nil || n != 2 || failed != 1 {
		t.Fatalf("saved %d, failed %d, err %v; want 2, 1, nil", n, failed, err)
	}
	ids := []string{saved[0].DecisionID, saved[1].DecisionID}
	slices.Sort(ids)
	if !reflect.DeepEqual(ids, []string{"flaky", "ok"}) {
		t.Errorf("saved %v", ids)
	}
	for _, j := range saved {
		if j.Model != "flaky" || j.ModelVersion != "flaky-1" || j.Answers["duplicate"].Choice != "none" || j.CostUSD != 0.00002 {
			t.Errorf("judgment %+v", j)
		}
	}
	if !slices.Contains(d.seen, `{"table":"a"}`) {
		t.Errorf("the model was not sent the rebuilt state: %v", d.seen)
	}
}

func TestStoreKeepsTheFirstAnswer(t *testing.T) {
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://stet_scram:scram-pw@"+addr+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	s := Store{Schema: fmt.Sprintf("lab_replay_%d", time.Now().UnixNano())}
	t.Cleanup(func() {
		conn.Exec(ctx, "drop schema if exists "+s.Schema+" cascade")
		conn.Close(ctx)
	})
	if err := s.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	id := "6f1c9a52-0d4e-4c39-9d8e-2b1f9c0a7e11"
	first := Judgment{DecisionID: id, Model: "jev", ModelVersion: "jev-1.14.0", LatencyMS: 170, CostUSD: 0.00003,
		Answers: map[string]decide.Answer{"policy_1": {Choice: "false", Probability: p(0.05)}}}
	second := first
	second.Answers = map[string]decide.Answer{"policy_1": {Choice: "true", Probability: p(0.95)}}
	for _, j := range []Judgment{first, second} {
		if err := s.Save(ctx, conn, "C", j); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load(ctx, conn, "C")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]Judgment{id: first}) {
		t.Errorf("got %+v; a stored answer must never be replaced (rule 4)", got)
	}
	if other, _ := s.Load(ctx, conn, "B"); len(other) != 0 {
		t.Errorf("arm B sees arm C's answers: %v", other)
	}
}
