package main

import (
	"strings"
	"testing"
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
