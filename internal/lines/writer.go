package lines

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// QueueSize is how many lines wait for the output before new ones are dropped.
const QueueSize = 1024

// closeTimeout is how long Close waits for a writer that does not take its lines.
const closeTimeout = 2 * time.Second

// Writer writes lines to w from a queue of QueueSize, on its own goroutine.
type Writer struct {
	w       io.Writer
	queue   chan string
	done    chan struct{}
	mu      sync.Mutex // guards dropped and closed; never held across a blocking call
	dropped int
	closed  bool
}

// NewWriter starts a Writer that writes to w.
func NewWriter(w io.Writer) *Writer {
	wr := &Writer{w: w, queue: make(chan string, QueueSize), done: make(chan struct{})}
	go wr.run()
	return wr
}

func (w *Writer) run() {
	defer close(w.done)
	for line := range w.queue {
		// A failing output must not stop the others; there is nowhere to report it.
		_, _ = io.WriteString(w.w, line+"\n")
	}
}

// Offer queues line. It never blocks: with the queue full the line is
// dropped and counted, and "… N lines dropped" is written once there is room.
func (w *Writer) Offer(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.flushNotice()
	if w.dropped > 0 {
		w.dropped++
		return
	}
	select {
	case w.queue <- line:
	default:
		w.dropped++
	}
}

// flushNotice queues the dropped-lines notice when there is room. w.mu is held.
func (w *Writer) flushNotice() {
	if w.dropped == 0 {
		return
	}
	notice := "… 1 line dropped"
	if w.dropped > 1 {
		notice = fmt.Sprintf("… %d lines dropped", w.dropped)
	}
	select {
	case w.queue <- notice:
		w.dropped = 0
	default:
	}
}

// Close writes what is queued and stops. It gives up after 2 seconds when w
// does not take it.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.flushNotice()
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(closeTimeout):
	}
}
