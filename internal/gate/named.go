package gate

import (
	"strconv"
	"strings"
)

// compileNamed rewrites :name parameters in candidate SQL to $n, reusing the
// number for repeated names. Quoted text, dollar-quoted strings, comments,
// and :: casts are left alone.
func compileNamed(sql string) (string, []string) {
	var out strings.Builder
	var names []string
	index := map[string]int{}
	isStart := func(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	isPart := func(c byte) bool { return isStart(c) || c >= '0' && c <= '9' }

	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(sql) {
				if sql[j] == c {
					if j+1 < len(sql) && sql[j+1] == c { // doubled quote
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(sql))
			out.WriteString(sql[i:end])
			i = end

		case c == '$':
			j := i + 1
			for j < len(sql) && isPart(sql[j]) && !(j == i+1 && sql[j] >= '0' && sql[j] <= '9') {
				j++
			}
			if j < len(sql) && sql[j] == '$' { // $tag$ ... $tag$
				tag := sql[i : j+1]
				end := strings.Index(sql[j+1:], tag)
				stop := len(sql)
				if end >= 0 {
					stop = j + 1 + end + len(tag)
				}
				out.WriteString(sql[i:stop])
				i = stop
				continue
			}
			out.WriteByte(c)
			i++

		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			end := strings.IndexByte(sql[i:], '\n')
			stop := len(sql)
			if end >= 0 {
				stop = i + end
			}
			out.WriteString(sql[i:stop])
			i = stop

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			stop := len(sql)
			if end >= 0 {
				stop = i + 2 + end + 2
			}
			out.WriteString(sql[i:stop])
			i = stop

		case c == ':' && i+1 < len(sql) && sql[i+1] == ':':
			out.WriteString("::")
			i += 2

		case c == ':' && i+1 < len(sql) && isStart(sql[i+1]):
			j := i + 1
			for j < len(sql) && isPart(sql[j]) {
				j++
			}
			name := sql[i+1 : j]
			n, ok := index[name]
			if !ok {
				names = append(names, name)
				n = len(names)
				index[name] = n
			}
			out.WriteString("$" + strconv.Itoa(n))
			i = j

		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), names
}

// compile prepares a configured query: trailing semicolon dropped, :name
// parameters turned into $n.
func compile(sql string) named {
	q, names := compileNamed(strings.TrimRight(strings.TrimSpace(sql), ";"))
	return named{q, names}
}

// bind orders a row's values as the query's parameters.
func (n named) bind(row map[string]*string) []*string {
	args := make([]*string, len(n.names))
	for i, name := range n.names {
		args[i] = row[name]
	}
	return args
}
