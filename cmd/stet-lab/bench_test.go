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
		// Nothing listens on the address: without the check the command
		// fails to connect instead.
		args := append([]string{"-pg", "127.0.0.1:1", "-arm", "hoop", "-addr", "127.0.0.1:1"}, c.args...)
		err := benchSend(args)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("bench send %s: err = %v, want one starting %q", strings.Join(c.args, " "), err, c.want)
		}
	}
}
