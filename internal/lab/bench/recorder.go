package bench

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Call is one line of a recorder file.
type Call struct {
	Time         time.Time `json:"time"`
	LatencyMS    float64   `json:"latency_ms"`
	Status       int       `json:"status"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Level        string    `json:"level"` // low | medium | high, from the tool the model called; "" if none
	Title        string    `json:"title"`
	Explanation  string    `json:"explanation"`
	StopReason   string    `json:"stop_reason"`
	Tag          string    `json:"tag"`   // the w: tag found in the request's user message; "" if none
	Input        string    `json:"input"` // the request's user message
	CostUSD      float64   `json:"cost_usd"`
	TotalUSD     float64   `json:"total_usd"`       // spent so far, the files beside this one included
	Event        string    `json:"event,omitempty"` // "cap_reached" on a line that records a refused call
}

// Prices are dollars per million input and output tokens. A model not listed is priced as the most expensive one.
var Prices = map[string][2]float64{"claude-haiku-5-5": {0.10, 0.50}, "claude-opus-5-5": {4, 20}}

// forwardTimeout bounds one forwarded call.
const forwardTimeout = 120 * time.Second

var recorderTagRE = regexp.MustCompile(`/\* w:(\d{12}) \*/`)

// hopHeaders are the hop-by-hop headers a forwarder must not pass on.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// Recorder is an http.Handler that forwards requests unchanged to an upstream
// and appends one Call per request to its output. It never writes a header.
type Recorder struct {
	upstream string
	client   *http.Client
	capUSD   float64

	mu      sync.Mutex // guards the fields below
	out     io.Writer
	spent   float64
	capSaid bool // the cap has been announced on stderr
	broken  bool // a line could not be written: nothing is forwarded any more
}

// NewRecorder forwards to upstream (https://api.anthropic.com) and appends a Call per request to out.
// spent is what earlier files already cost.
func NewRecorder(upstream string, out io.Writer, capUSD, spent float64) *Recorder {
	return &Recorder{
		upstream: strings.TrimRight(upstream, "/"),
		client: &http.Client{
			Timeout:       forwardTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		out:    out,
		capUSD: capUSD,
		spent:  spent,
	}
}

// price returns the cost of a call's tokens.
func price(model string, in, out int) float64 {
	p, ok := Prices[model]
	if !ok {
		for _, q := range Prices {
			p[0], p[1] = max(p[0], q[0]), max(p[1], q[1])
		}
	}
	return float64(in)/1e6*p[0] + float64(out)/1e6*p[1]
}

// worstCase prices a call whose usage is unknown: the request's bytes as input
// tokens (a token is never shorter than a byte) and the request's max_tokens
// as output (8192 when it names none).
func worstCase(model string, requestBytes, maxTokens int) float64 {
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	return price(model, requestBytes, maxTokens)
}

// write appends c to the output with the running total, after adding its cost
// to spent. When the line cannot be written the recorder stops forwarding.
func (r *Recorder) write(c Call) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spent += c.CostUSD
	c.TotalUSD = r.spent
	line, err := json.Marshal(c)
	if err == nil {
		_, err = r.out.Write(append(line, '\n'))
	}
	if err != nil {
		if !r.broken {
			fmt.Fprintf(os.Stderr, "llm-recorder: the call log cannot be written (%v): no more calls are forwarded\n", err)
		}
		r.broken = true
	}
	return err
}

// state reports whether the recorder has stopped and whether the cap is
// reached; it announces the cap on stderr the first time.
func (r *Recorder) state() (broken, capped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	capped = r.spent >= r.capUSD
	if capped && !r.capSaid {
		r.capSaid = true
		fmt.Fprintf(os.Stderr, "llm-recorder: the cost cap of $%.2f is reached ($%.4f spent): no more calls are forwarded\n", r.capUSD, r.spent)
	}
	return r.broken, capped
}

func jsonError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
}

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/v1/messages" {
		jsonError(w, http.StatusNotFound, "not_found", "the recorder forwards only POST /v1/messages")
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed", "the recorder forwards only POST /v1/messages")
		return
	}
	broken, capped := r.state()
	if broken {
		jsonError(w, http.StatusServiceUnavailable, "recorder_stopped", "the recorder cannot write its log and forwards nothing")
		return
	}
	start := time.Now()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad_request", "the request body could not be read")
		return
	}
	c := Call{Time: start.UTC()}
	var rq struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &rq) == nil {
		c.Model = rq.Model
		if len(rq.Messages) > 0 {
			var s string
			if json.Unmarshal(rq.Messages[0].Content, &s) == nil {
				c.Input = s
			}
		}
	}
	if m := recorderTagRE.FindStringSubmatch(c.Input); m != nil {
		c.Tag = m[1]
	}

	if capped {
		c.Status, c.Event = http.StatusForbidden, "cap_reached"
		c.LatencyMS = ms(time.Since(start))
		if r.write(c) != nil {
			jsonError(w, http.StatusServiceUnavailable, "recorder_stopped", "the recorder cannot write its log and forwards nothing")
			return
		}
		jsonError(w, http.StatusForbidden, "cost_cap", "the benchmark's cost cap is reached")
		return
	}

	// The call is not tied to the client's context: once sent it may be billed,
	// so it is read to the end and logged even if the client gave up.
	var sent atomic.Bool
	ctx := httptrace.WithClientTrace(context.WithoutCancel(req.Context()),
		&httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) }})
	out, err := http.NewRequestWithContext(ctx, req.Method, r.upstream+req.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad_request", "the request could not be forwarded")
		return
	}
	out.Header = req.Header.Clone()
	stripHop(out.Header)
	// Ask for an encoding the recorder can read: gzip when the client takes it
	// (relayed as is), identity otherwise.
	clientGzip := slices.ContainsFunc(strings.Split(strings.Join(req.Header.Values("Accept-Encoding"), ","), ","), func(e string) bool {
		name, q, _ := strings.Cut(strings.TrimSpace(e), ";")
		return strings.EqualFold(name, "gzip") && !strings.Contains(q, "q=0")
	})
	if clientGzip {
		out.Header.Set("Accept-Encoding", "gzip")
	} else {
		out.Header.Set("Accept-Encoding", "identity")
	}

	resp, err := r.client.Do(out)
	var respBody []byte
	if err == nil {
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	c.LatencyMS = ms(time.Since(start))
	if err != nil {
		// The error text is not passed on: it is the transport's, not the API's.
		// A request that was sent may have been billed.
		c.Status, c.Event = http.StatusBadGateway, "upstream_error"
		if sent.Load() {
			c.CostUSD = worstCase(c.Model, len(body), rq.MaxTokens)
		}
		if r.write(c) != nil {
			jsonError(w, http.StatusServiceUnavailable, "recorder_stopped", "the recorder cannot write its log and forwards nothing")
			return
		}
		jsonError(w, http.StatusBadGateway, "recorder_upstream", "the upstream call failed")
		return
	}
	c.Status = resp.StatusCode
	if c.Status/100 == 2 {
		if parseReply(&c, resp.Header.Get("Content-Encoding"), respBody) {
			c.CostUSD = price(c.Model, c.InputTokens, c.OutputTokens)
		} else {
			c.Event = "unparsed"
			c.CostUSD = worstCase(c.Model, len(body), rq.MaxTokens)
		}
	} else {
		parseReply(&c, resp.Header.Get("Content-Encoding"), respBody)
	}

	// The line is written before the client hears back.
	if r.write(c) != nil {
		jsonError(w, http.StatusServiceUnavailable, "recorder_stopped", "the recorder cannot write its log and forwards nothing")
		return
	}
	h := w.Header()
	for k, v := range resp.Header {
		h[k] = v
	}
	stripHop(h)
	h.Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func stripHop(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// parseReply fills c from a response body; what cannot be read stays empty.
// It reports whether the body held a usage.
func parseReply(c *Call, encoding string, body []byte) bool {
	if strings.EqualFold(encoding, "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return false
		}
		if body, err = io.ReadAll(zr); err != nil {
			return false
		}
	}
	var rs struct {
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Title       string `json:"title"`
				Explanation string `json:"explanation"`
			} `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &rs) != nil {
		return false
	}
	c.StopReason = rs.StopReason
	if rs.Usage != nil {
		c.InputTokens, c.OutputTokens = rs.Usage.InputTokens, rs.Usage.OutputTokens
	}
	for _, b := range rs.Content {
		if b.Type != "tool_use" {
			continue
		}
		switch b.Name {
		case "report_low_risk":
			c.Level = "low"
		case "report_medium_risk":
			c.Level = "medium"
		case "report_high_risk":
			c.Level = "high"
		default:
			continue
		}
		c.Title, c.Explanation = b.Input.Title, b.Input.Explanation
		break
	}
	return rs.Usage != nil
}

// ReadCalls returns the calls of a recorder file, in order.
func ReadCalls(path string) ([]Call, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var calls []Call
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		calls = append(calls, c)
	}
	return calls, sc.Err()
}

// SpentIn sums cost_usd over every *.jsonl file in dir.
func SpentIn(dir string) (float64, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, f := range files {
		calls, err := ReadCalls(f)
		if err != nil {
			return 0, err
		}
		for _, c := range calls {
			total += c.CostUSD
		}
	}
	return total, nil
}
