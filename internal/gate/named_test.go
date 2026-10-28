package gate

import (
	"reflect"
	"testing"
)

func TestCompileNamed(t *testing.T) {
	cases := []struct {
		in, out string
		names   []string
	}{
		{"select 1 where a = :email", "select 1 where a = $1", []string{"email"}},
		{"where a = :x or b = :y or c = :x", "where a = $1 or b = $2 or c = $1", []string{"x", "y"}},
		{"select :a::int", "select $1::int", []string{"a"}},
		{"select ':not_param', \":nor_this\"", "select ':not_param', \":nor_this\"", nil},
		{"select 'it''s :x'", "select 'it''s :x'", nil},
		{"select $$ :x $$, $q$ :y $q$", "select $$ :x $$, $q$ :y $q$", nil},
		{"select 1 -- :x\nwhere a = :y", "select 1 -- :x\nwhere a = $1", []string{"y"}},
		{"select /* :x */ :y", "select /* :x */ $1", []string{"y"}},
		{"select arr[1:2]", "select arr[1:2]", nil},
	}
	for _, tc := range cases {
		out, names := compileNamed(tc.in)
		if out != tc.out || !reflect.DeepEqual(names, tc.names) {
			t.Errorf("compileNamed(%q) = %q %v, want %q %v", tc.in, out, names, tc.out, tc.names)
		}
	}
}
