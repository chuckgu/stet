package lines

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// gate is an io.Writer that blocks every Write until released.
type gate struct {
	mu      sync.Mutex
	buf     strings.Builder
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{release: make(chan struct{}), entered: make(chan struct{})} }

func (g *gate) Write(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gate) lines() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := strings.TrimSuffix(g.buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestWriterWritesInOrder(t *testing.T) {
	var b safeBuf
	w := NewWriter(&b)
	w.Offer("one")
	w.Offer("two")
	w.Offer("three")
	w.Close()
	if got := b.String(); got != "one\ntwo\nthree\n" {
		t.Fatalf("got %q", got)
	}
}

func dropTest(t *testing.T, extra int, notice string) {
	g := newGate()
	w := NewWriter(g)
	w.Offer("first")
	<-g.entered
	var want []string
	want = append(want, "first")
	for i := 0; i < QueueSize; i++ {
		l := fmt.Sprintf("q%d", i)
		w.Offer(l)
		want = append(want, l)
	}
	for i := 0; i < extra; i++ {
		w.Offer("dropped")
	}
	close(g.release)
	// wait for the queue to drain
	deadline := time.Now().Add(2 * time.Second)
	for len(g.lines()) < 1+QueueSize && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w.Offer("after1")
	w.Offer("after2")
	w.Close()
	want = append(want, notice, "after1", "after2")
	got := g.lines()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %d lines, want %d; tail %q", len(got), len(want), got[max(0, len(got)-5):])
	}
}

func TestWriterDropsWhenFull(t *testing.T) {
	dropTest(t, 37, "… 37 lines dropped")
	dropTest(t, 1, "… 1 line dropped")
}

func TestOfferNeverBlocks(t *testing.T) {
	g := newGate()
	w := NewWriter(g)
	start := time.Now()
	for i := 0; i < 5000; i++ {
		w.Offer("x")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Offer blocked")
	}
	close(g.release)
	w.Close()
}

func TestCloseGivesUpOnABlockedWriter(t *testing.T) {
	g := newGate()
	w := NewWriter(g)
	w.Offer("x")
	w.Offer("y")
	<-g.entered
	start := time.Now()
	w.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close took %v", d)
	}
	close(g.release)
}

func TestCloseWritesEverythingQueued(t *testing.T) {
	var b safeBuf
	w := NewWriter(&b)
	for i := 0; i < QueueSize; i++ {
		w.Offer("l")
	}
	w.Close()
	if n := strings.Count(b.String(), "\n"); n != QueueSize {
		t.Fatalf("wrote %d lines", n)
	}
	w.Offer("late") // after Close: ignored, no panic
	w.Close()
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("broken") }

func TestWriterSurvivesWriteErrors(t *testing.T) {
	w := NewWriter(failing{})
	for i := 0; i < 10; i++ {
		w.Offer("x")
	}
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung")
	}
}

// slow is an io.Writer that takes a fixed time per Write without blocking.
type slow struct {
	mu    sync.Mutex
	lines []string
}

func spin(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
	}
}

func (s *slow) Write(p []byte) (int, error) {
	spin(200 * time.Microsecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func TestWriterKeepsWritingRealLinesUnderOverload(t *testing.T) {
	s := &slow{}
	w := NewWriter(s)
	const run = 1500 * time.Millisecond
	start := time.Now()
	n, lateOffered := 0, 0
	for time.Since(start) < run {
		late := time.Since(start) > run*2/3
		w.Offer(fmt.Sprintf("real %d", n))
		if late {
			lateOffered++
		}
		n++
		spin(100 * time.Microsecond)
	}
	w.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	lateWritten, notices := 0, 0
	last := -1
	for i, l := range s.lines {
		if strings.HasPrefix(l, "…") {
			notices++
			if i > 0 && strings.HasPrefix(s.lines[i-1], "…") {
				t.Errorf("adjacent notices: %q then %q", s.lines[i-1], l)
			}
			continue
		}
		var k int
		fmt.Sscanf(l, "real %d", &k)
		if k <= last {
			t.Errorf("out of order: %d after %d", k, last)
		}
		last = k
		if k >= n-lateOffered {
			lateWritten++
		}
	}
	if lateWritten == 0 || notices == 0 {
		t.Fatalf("late real lines written=%d (offered %d), notices=%d", lateWritten, lateOffered, notices)
	}
}

func TestNoticeAppearsWithoutAnotherOffer(t *testing.T) {
	g := newGate()
	var b safeBuf
	w := NewWriter(&tee{g, &b})
	w.Offer("first")
	<-g.entered
	for i := 0; i < QueueSize+5; i++ {
		w.Offer("x")
	}
	close(g.release)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.HasSuffix(b.String(), "… 5 lines dropped\n") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.HasSuffix(b.String(), "… 5 lines dropped\n") {
		t.Fatal("notice never written without a further Offer")
	}
	w.Close()
}

type tee struct {
	g *gate
	b *safeBuf
}

func (t *tee) Write(p []byte) (int, error) {
	if _, err := t.g.Write(p); err != nil {
		return 0, err
	}
	return t.b.Write(p)
}
