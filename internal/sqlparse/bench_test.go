package sqlparse

import "testing"

func BenchmarkClassify(b *testing.B) {
	for _, bc := range []struct{ name, sql string }{
		{"read", "select id, name, email from contacts where company_id = $1 order by updated_at desc limit 20"},
		{"gated update", "update contacts set title = $1, status = $2 where id = $3"},
		{"other table insert", "insert into activities (contact_id, kind, body) values ($1, $2, $3)"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := Classify(bc.sql, gated); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
