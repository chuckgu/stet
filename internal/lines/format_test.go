package lines

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/store"
)

func i64(n int64) *int64   { return &n }
func str(s string) *string { return &s }

var t0 = time.Date(2026, 11, 22, 14, 2, 11, 0, time.UTC)

func dec(mod func(d *store.Decision)) *store.Decision {
	d := &store.Decision{CreatedAt: t0, Mode: "shadow", Verdict: "commit", Op: "update", TargetTable: "contacts", Executed: true,
		Statement: "update contacts set x = 1"}
	mod(d)
	return d
}

func TestFormatLines(t *testing.T) {
	stmt := func(s string) func(d *store.Decision) { return func(d *store.Decision) { d.Statement = s } }
	_ = stmt
	cases := []struct {
		name string
		d    *store.Decision
		want string
	}{
		{"shadow commit", dec(func(d *store.Decision) {
			d.RowsAffected = i64(1)
			d.ReadCheck = &decide.ReadCheck{Result: decide.ReadFresh}
			d.Statement = "update contacts set status='customer' where id = 7"
		}), "14:02:11  ok            UPDATE contacts  1 row, fresh read  update contacts set status='customer' where id = 7"},
		{"shadow hold share", dec(func(d *store.Decision) {
			d.Verdict = "hold"
			d.RowsAffected = i64(300)
			d.Impact = &decide.Impact{TableRows: i64(1000)}
			d.Reasons = []string{"impact:rows"}
			d.Statement = "update contacts set status='churned' where company_id < 3"
		}), "14:02:11  would hold    UPDATE contacts  300 rows, ~30% of table  [impact:rows]  update contacts set status='churned' where company_id < 3"},
		{"cascade", dec(func(d *store.Decision) {
			d.Verdict = "hold"
			d.Op = "delete"
			d.RowsAffected = i64(200)
			d.Impact = &decide.Impact{TableRows: i64(1000), Cascades: []decide.Cascade{{Table: "deals"}, {Table: "deals"}}}
			d.Reasons = []string{"impact:rows"}
			d.Statement = "delete from contacts where id between 1 and 200"
		}), "14:02:11  would hold    DELETE contacts  200 rows, ~20% of table, cascades into deals  [impact:rows]  delete from contacts where id between 1 and 200"},
		{"two cascades", dec(func(d *store.Decision) {
			d.Impact = &decide.Impact{Cascades: []decide.Cascade{{Table: "deals"}, {Table: "notes"}}}
		}), "14:02:11  ok            UPDATE contacts  cascades into deals, notes  update contacts set x = 1"},
		{"capped estimate refused", dec(func(d *store.Decision) {
			d.Mode = "enforce"
			d.Verdict = "hold"
			d.Executed = false
			d.Refused = str("ST001")
			d.Impact = &decide.Impact{Method: "count", Rows: i64(101), Capped: true, Cap: 101, Estimate: i64(257), TableRows: i64(1000)}
		}), "14:02:11  hold          UPDATE contacts  ~257 rows, ~26% of table, refused ST001  update contacts set x = 1"},
		{"capped no estimate", dec(func(d *store.Decision) {
			d.Mode = "enforce"
			d.Verdict = "hold"
			d.Executed = false
			d.Refused = str("ST001")
			d.Impact = &decide.Impact{Method: "count", Rows: i64(101), Capped: true, Cap: 101, TableRows: i64(1000)}
		}), "14:02:11  hold          UPDATE contacts  101+ rows, refused ST001  update contacts set x = 1"},
		{"estimate method", dec(func(d *store.Decision) {
			d.Executed = false
			d.Refused = str("ST002")
			d.Impact = &decide.Impact{Method: "estimate", Rows: i64(40), TableRows: i64(100000)}
		}), "14:02:11  ok            UPDATE contacts  ~40 rows, <1% of table, refused ST002  update contacts set x = 1"},
		{"count", dec(func(d *store.Decision) {
			d.Executed = false
			d.Refused = str("ST001")
			d.Impact = &decide.Impact{Method: "count", Rows: i64(12)}
		}), "14:02:11  ok            UPDATE contacts  12 rows, refused ST001  update contacts set x = 1"},
		{"estimate one", dec(func(d *store.Decision) {
			d.Impact = &decide.Impact{Method: "estimate", Rows: i64(1)}
		}), "14:02:11  ok            UPDATE contacts  ~1 row  update contacts set x = 1"},
		{"estimate only", dec(func(d *store.Decision) {
			d.Impact = &decide.Impact{Estimate: i64(5)}
		}), "14:02:11  ok            UPDATE contacts  ~5 rows  update contacts set x = 1"},
		{"reject stale", dec(func(d *store.Decision) {
			d.Verdict = "reject"
			d.RowsAffected = i64(1)
			d.ReadCheck = decide.ReadCheck{Result: decide.ReadStale}
			d.Reasons = []string{"stale_read"}
		}), "14:02:11  would reject  UPDATE contacts  1 row, stale read  [stale_read]  update contacts set x = 1"},
		{"escalate", dec(func(d *store.Decision) {
			d.Verdict = "escalate"
			d.RowsAffected = i64(1)
		}), "14:02:11  would escalate  UPDATE contacts  1 row  update contacts set x = 1"},
		{"sent but failed", dec(func(d *store.Decision) { d.Executed = false }),
			"14:02:11  ok            UPDATE contacts  not executed  update contacts set x = 1"},
		{"unsupported", dec(func(d *store.Decision) {
			d.Verdict = "unsupported"
			d.Op, d.TargetTable = "", ""
			d.Reasons = []string{"multi_statement"}
			d.Statement = "select 1; select 2"
		}), "14:02:11  unsupported   [multi_statement]  select 1; select 2"},
		{"nothing", dec(func(d *store.Decision) {
			d.Op, d.TargetTable = "", ""
		}), "14:02:11  ok            update contacts set x = 1"},
		{"over table", dec(func(d *store.Decision) {
			d.RowsAffected = i64(2000)
			d.Impact = &decide.Impact{TableRows: i64(1000)}
		}), "14:02:11  ok            UPDATE contacts  2000 rows, ~100% of table  update contacts set x = 1"},
		{"zero table rows", dec(func(d *store.Decision) {
			d.RowsAffected = i64(5)
			d.Impact = &decide.Impact{TableRows: i64(0)}
		}), "14:02:11  ok            UPDATE contacts  5 rows  update contacts set x = 1"},
		{"enforce unknown verdict", dec(func(d *store.Decision) {
			d.Mode = "enforce"
			d.Verdict = "weird"
		}), "14:02:11  weird         UPDATE contacts  update contacts set x = 1"},
	}
	for _, c := range cases {
		if got := Format(c.d, time.UTC); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestFormatUsesLoc(t *testing.T) {
	loc := time.FixedZone("x", 9*3600)
	if got := Format(dec(func(*store.Decision) {}), loc); !strings.HasPrefix(got, "23:02:11  ") {
		t.Fatalf("got %q", got)
	}
}

func TestFormatCutsTheStatement(t *testing.T) {
	get := func(s string) string {
		line := Format(dec(func(d *store.Decision) { d.Op, d.TargetTable = "", ""; d.Statement = s }), time.UTC)
		return strings.TrimPrefix(line, "14:02:11  ok            ")
	}
	if got := get("update\n\tcontacts   set\r\nx = 1  "); got != "update contacts set x = 1" {
		t.Errorf("got %q", got)
	}
	s80 := strings.Repeat("a", 80)
	if got := get(s80); got != s80 {
		t.Errorf("80 runes cut: %q", got)
	}
	if got := get(s80 + "a"); got != strings.Repeat("a", 79)+"…" {
		t.Errorf("81 runes: %q", got)
	}
	k := strings.Repeat("한", 80)
	if got := get(k); got != k {
		t.Errorf("80 hangul cut")
	}
	if got := get(k + "한"); got != strings.Repeat("한", 79)+"…" || utf8.RuneCountInString(got) != 80 {
		t.Errorf("81 hangul: %q", got)
	}
}

func clean(t *testing.T, line string) {
	t.Helper()
	for _, r := range line {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			t.Errorf("line %q holds %U", line, r)
			return
		}
	}
}

func TestLineHasNoControlCharacters(t *testing.T) {
	nasty := "a\x1b[2J b\r c\b d\n14:00:00  ok  fake \u202e e\u2028f\u2029g\u2066h\u2069i\x7f"
	d := dec(func(d *store.Decision) {
		d.Statement = nasty
		d.TargetTable = nasty
		d.Reasons = []string{nasty}
		d.Impact = &decide.Impact{Cascades: []decide.Cascade{{Table: nasty}}}
	})
	line := Format(d, time.UTC)
	clean(t, line)
	if strings.ContainsAny(line, "\n\r") {
		t.Error("newline in line")
	}
	line = Format(dec(func(d *store.Decision) { d.Statement = nasty }), time.UTC)
	clean(t, line)
	if strings.Contains(line, "\n") {
		t.Error("newline")
	}
}

func TestFormatIgnoresWhatItCannotRead(t *testing.T) {
	d := dec(func(d *store.Decision) {
		d.Impact = map[string]any{"rows": 3}
		d.ReadCheck = "fresh"
	})
	if got := Format(d, time.UTC); got != "14:02:11  ok            UPDATE contacts  update contacts set x = 1" {
		t.Errorf("got %q", got)
	}
	var typedNil *decide.Impact
	d = dec(func(d *store.Decision) { d.Impact = typedNil; d.ReadCheck = (*decide.ReadCheck)(nil) })
	Format(d, time.UTC)
	Format(&store.Decision{}, time.UTC)
	Format(&store.Decision{Impact: &decide.Impact{Cascades: []decide.Cascade{{}}}}, nil)
}
