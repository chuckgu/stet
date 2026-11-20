package gate

import (
	"testing"

	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
)

// A key value is used only when it reads as a value of the key column: no
// cast, or a cast to a type that compares as the key column's does; no type
// modifier or schema on the cast; a parameter named nowhere else; and no
// backslash in a literal for a key of a string type.
func TestKeyUsable(t *testing.T) {
	keys := map[string]tracked{
		"int2":    {typ: proxy.KeyInt2, base: 21},
		"int4":    {typ: proxy.KeyInt4, base: 23},
		"int8":    {typ: proxy.KeyInt8, base: 20},
		"uuid":    {typ: proxy.KeyUUID, base: 2950},
		"text":    {typ: proxy.KeyText, base: 25},
		"varchar": {typ: proxy.KeyText, base: 1043},
		"bpchar":  {typ: proxy.KeyText, base: 1042},
		"name":    {typ: proxy.KeyText, base: 19},
	}
	lit := func(text, cast string) sqlparse.Value {
		return sqlparse.Value{Kind: sqlparse.Literal, Text: text, Cast: cast}
	}
	param := func(cast string) sqlparse.Value { return sqlparse.Value{Kind: sqlparse.Param, Param: 1, Cast: cast} }
	usable := map[string][]string{ // key type → the casts a parameter may carry
		"int2":    {"", "int2", "int4", "int8"},
		"int4":    {"", "int2", "int4", "int8"},
		"int8":    {"", "int2", "int4", "int8"},
		"uuid":    {"", "uuid"},
		"text":    {"", "text", "varchar"},
		"varchar": {"", "text", "varchar"},
		"bpchar":  {""},
		"name":    {""},
	}
	casts := []string{"", "int2", "int4", "int8", "uuid", "text", "varchar", "bpchar", "name", "numeric", "float8", "t"}
	for key, tk := range keys {
		for _, c := range casts {
			want := false
			for _, u := range usable[key] {
				want = want || u == c
			}
			if got := keyUsable(tk, param(c)); got != want {
				t.Errorf("%s key, $1::%q: usable %v, want %v", key, c, got, want)
			}
			if got := keyUsable(tk, lit("7", c)); got != want {
				t.Errorf("%s key, '7'::%q: usable %v, want %v", key, c, got, want)
			}
		}
		for _, v := range []sqlparse.Value{
			{Kind: sqlparse.Param, Param: 1, Shared: true},
			{Kind: sqlparse.Param, Param: 1, Cast: usable[key][len(usable[key])-1], CastMod: true},
			{Kind: sqlparse.Literal, Text: "7", Cast: usable[key][len(usable[key])-1], CastMod: true},
			{Kind: sqlparse.Expr, Text: "7 + 0"},
			{Kind: sqlparse.Null},
			{Kind: sqlparse.Default},
		} {
			if keyUsable(tk, v) {
				t.Errorf("%s key: %+v is usable", key, v)
			}
		}
		// A backslash in a string key's literal: standard_conforming_strings
		// off reads it otherwise. A parameter's value is not SQL text.
		backslash := lit(`a\b`, "")
		if got, want := keyUsable(tk, backslash), tk.typ != proxy.KeyText; got != want {
			t.Errorf("%s key, literal %q: usable %v, want %v", key, backslash.Text, got, want)
		}
	}
}
