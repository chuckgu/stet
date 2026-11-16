package sqlparse

import (
	"fmt"
	"strings"
	"testing"
)

// Every statement of every session is classified, so each case runs with
// reads recorded and without: without, the classifier must not walk the tree
// for reads (it costs every statement and records nothing).
func BenchmarkClassify(b *testing.B) {
	var rows []string
	for i := range 5000 {
		rows = append(rows, fmt.Sprintf("(%d, 'note', 'body %d')", i, i))
	}
	big := "insert into activities (contact_id, kind, body) values " + strings.Join(rows, ", ")
	for _, bc := range []struct{ name, sql string }{
		{"select 1", "select 1"},
		{"read", "select id, name, email from contacts where company_id = $1 order by updated_at desc limit 20"},
		{"join", "select c.name, d.stage from contacts c join deals d on d.contact_id = c.id where c.company_id = $1"},
		{"gated update", "update contacts set title = $1, status = $2 where id = $3"},
		{"other table insert", "insert into activities (contact_id, kind, body) values ($1, $2, $3)"},
		{"5000-row other table insert", big},
	} {
		for _, tc := range []struct {
			name   string
			tables Tables
		}{{"reads on", gated}, {"reads off", Tables{PK: gated.PK}}} {
			b.Run(bc.name+"/"+tc.name, func(b *testing.B) {
				for b.Loop() {
					if _, err := Classify(bc.sql, tc.tables); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
