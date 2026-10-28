package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	ds := []time.Duration{50, 10, 40, 20, 30}
	if got := percentile(ds, 0.5); got != 30 {
		t.Errorf("p50 = %v, want 30", got)
	}
	if got := percentile(ds, 1); got != 50 {
		t.Errorf("max = %v, want 50", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

func TestBenchCasesAreRealistic(t *testing.T) {
	for _, c := range benchCases() {
		var s map[string]any
		if err := json.Unmarshal(c.state, &s); err != nil {
			t.Fatalf("%s: state is not JSON: %v", c.name, err)
		}
		if s["operation"] == nil || s["proposed"] == nil || s["provenance"] == nil {
			t.Errorf("%s: state lacks the gate's fields: %v", c.name, s)
		}
		if len(c.questions) < 2 {
			t.Errorf("%s: %d questions, want the gate's full set", c.name, len(c.questions))
		}
	}
}

// The bench once sent maps, whose keys Go sorts, and Jev answered
// differently than it does for the gate's real states.
func TestBenchStatesUseTheGateFieldOrder(t *testing.T) {
	want := []string{"table", "entity", "operation", "proposed", "current", "candidates", "recent_changes", "policy", "provenance"}
	for _, c := range benchCases() {
		dec := json.NewDecoder(bytes.NewReader(c.state))
		dec.Token() // {
		var keys []string
		for dec.More() {
			k, _ := dec.Token()
			keys = append(keys, k.(string))
			var skip json.RawMessage
			dec.Decode(&skip)
		}
		if !reflect.DeepEqual(keys, want) {
			t.Errorf("%s: keys %v, want %v", c.name, keys, want)
		}
	}
}
