package bench

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

	mu    sync.Mutex // guards out and spent
	out   io.Writer
	spent float64
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
		p = Prices["claude-opus-5-5"]
		for _, q := range Prices {
			p[0], p[1] = max(p[0], q[0]), max(p[1], q[1])
		}
	}
	return float64(in)/1e6*p[0] + float64(out)/1e6*p[1]
}

// write appends c to the output with the running total; costs are added to spent.
func (r *Recorder) write(c Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spent += c.CostUSD
	c.TotalUSD = r.spent
	line, err := json.Marshal(c)
	if err != nil {
		return
	}
	r.out.Write(append(line, '\n'))
}

func (r *Recorder) overCap() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spent >= r.capUSD
}

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "the request body could not be read", http.StatusBadRequest)
		return
	}
	c := Call{Time: start.UTC()}
	var rq struct {
		Model    string `json:"model"`
		Messages []struct {
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

	if r.overCap() {
		const refusal = `{"type":"error","error":{"type":"cost_cap","message":"the benchmark's cost cap is reached"}}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, refusal)
		c.Status, c.Event = http.StatusForbidden, "cap_reached"
		c.LatencyMS = ms(time.Since(start))
		r.write(c)
		return
	}

	out, err := http.NewRequestWithContext(req.Context(), req.Method, r.upstream+req.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	out.Header = req.Header.Clone()
	stripHop(out.Header)
	resp, err := r.client.Do(out)
	var respBody []byte
	if err == nil {
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		// The error text is not passed on: it is the transport's, not the API's.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"type":"error","error":{"type":"recorder_upstream","message":"the upstream call failed"}}`)
		c.Status = http.StatusBadGateway
		c.LatencyMS = ms(time.Since(start))
		r.write(c)
		return
	}
	c.Status = resp.StatusCode
	c.LatencyMS = ms(time.Since(start))
	parseReply(&c, resp.Header.Get("Content-Encoding"), respBody)
	if c.Status/100 == 2 {
		c.CostUSD = price(c.Model, c.InputTokens, c.OutputTokens)
	}

	h := w.Header()
	for k, v := range resp.Header {
		h[k] = v
	}
	stripHop(h)
	h.Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
	r.write(c)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func stripHop(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// parseReply fills c from a response body; what cannot be read stays empty.
func parseReply(c *Call, encoding string, body []byte) {
	if strings.EqualFold(encoding, "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return
		}
		if body, err = io.ReadAll(zr); err != nil {
			return
		}
	}
	var rs struct {
		StopReason string `json:"stop_reason"`
		Usage      struct {
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
		return
	}
	c.StopReason = rs.StopReason
	c.InputTokens, c.OutputTokens = rs.Usage.InputTokens, rs.Usage.OutputTokens
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
		return
	}
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
