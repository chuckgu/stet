// Package jev is the decide.Decider adapter for TypeSafe's Jev
// (POST /v1/systemone). Everything Jev-specific stays here (rule 7).
//
// Questions map to Jev primitives: a choice question becomes a Choice whose
// criteria are the choice descriptions; a score question becomes a Score
// whose levels are the descriptions in choice order. Jev scores levels from
// 0; answers are shifted back to the question's own scale.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	apiKeyEnv      = "TYPESAFE_API_KEY"
	baseURLEnv     = "TYPESAFE_BASE_URL"

	// Input price per token for every current Jev model ($42 per billion,
	// output free), per docs.typesafe.ai/models as of 2026-10-28.
	usdPerInputToken = 42e-9

	maxErrorBody = 2048
)

// Versioned model IDs only. Aliases such as jev-latest, jev-preview, or a
// bare minor version can change answers without a config change (rule 2).
var pinned = regexp.MustCompile(`^jev-\d+\.\d+\.\d+$`)

type Options struct {
	BaseURL    string
	APIKey     string
	Version    string
	HTTPClient *http.Client
}

type Client struct {
	url     string
	key     string
	version string
	http    *http.Client
}

// New builds the adapter from config. The key comes from TYPESAFE_API_KEY,
// never from the config file.
func New(m config.Model) (decide.Decider, error) {
	key := os.Getenv(apiKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("model.adapter jev: set %s", apiKeyEnv)
	}
	base := os.Getenv(baseURLEnv)
	if base == "" {
		base = DefaultBaseURL
	}
	return NewClient(Options{BaseURL: base, APIKey: key, Version: m.Version})
}

func NewClient(o Options) (*Client, error) {
	if !pinned.MatchString(o.Version) {
		return nil, fmt.Errorf("model.version %q: jev needs a versioned model ID such as jev-1.14.0", o.Version)
	}
	hc := o.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = 16 // keep connections warm; a fresh TLS handshake costs more than a decision
		hc = &http.Client{Transport: tr}
	}
	return &Client{url: o.BaseURL + "/v1/systemone", key: o.APIKey, version: o.Version, http: hc}, nil
}

func (c *Client) ModelID() string      { return "jev" }
func (c *Client) ModelVersion() string { return c.version }

type request struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]question `json:"questions"`
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria"`
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

func (c *Client) Decide(ctx context.Context, state json.RawMessage, qs []decide.Question) (decide.Answers, error) {
	req := request{Model: c.version, State: state, Questions: map[string]question{}}
	for _, q := range qs {
		jq := question{Type: q.Type, Instructions: q.Text}
		switch q.Type {
		case "choice":
			criteria := map[string]string{}
			for _, ch := range q.Choices {
				criteria[ch] = q.Descriptions[ch]
			}
			jq.Criteria = criteria
		case "score":
			levels := make([]string, len(q.Choices))
			for i, ch := range q.Choices {
				levels[i] = q.Descriptions[ch]
			}
			jq.Criteria = levels
		default:
			return decide.Answers{}, fmt.Errorf("jev: question %s has unsupported type %q", q.ID, q.Type)
		}
		req.Questions[q.ID] = jq
	}
	body, err := json.Marshal(req)
	if err != nil {
		return decide.Answers{}, err
	}

	resp, err := c.post(ctx, body)
	if err != nil {
		return decide.Answers{}, err
	}
	if resp.Model != c.version {
		return decide.Answers{}, fmt.Errorf("jev: asked %s but %s answered", c.version, resp.Model)
	}

	out := decide.Answers{ByQuestion: map[string]decide.Answer{}, CostUSD: float64(resp.Usage.InputTokens) * usdPerInputToken}
	for _, q := range qs {
		a, ok := resp.Answers[q.ID]
		if !ok {
			continue // decide.Verdict treats a missing answer as low confidence
		}
		switch q.Type {
		case "choice":
			out.ByQuestion[q.ID] = decide.Answer{Choice: a.Choice, Confidence: a.Confidence, Distribution: a.Probabilities}
		case "score":
			out.ByQuestion[q.ID] = scoreAnswer(q, a)
		}
	}
	return out, nil
}

// scoreAnswer moves a Jev score (levels 0..n-1) onto the question's choices.
// Choice is the most likely level; Expected is Jev's expected score on the
// question's scale, valid only when the choices are consecutive integers.
func scoreAnswer(q decide.Question, a answer) decide.Answer {
	out := decide.Answer{Confidence: a.Confidence, Distribution: map[string]float64{}}
	best := -1.0
	for i, ch := range q.Choices {
		p := a.Probabilities[strconv.Itoa(i)]
		out.Distribution[ch] = p
		if p > best {
			best, out.Choice = p, ch
		}
	}
	if first, err := strconv.Atoi(q.Choices[0]); err == nil {
		e := a.Score + float64(first)
		out.Expected = &e
	}
	return out
}

// post sends one request, retrying once on 429 or 529 if the wait fits
// before the deadline.
func (c *Client) post(ctx context.Context, body []byte) (*response, error) {
	for attempt := 1; ; attempt++ {
		resp, retryAfter, err := c.once(ctx, body)
		if err == nil || retryAfter < 0 || attempt == 2 {
			return resp, err
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < retryAfter {
			return nil, err
		}
		select {
		case <-time.After(retryAfter):
		case <-ctx.Done():
			return nil, err
		}
	}
}

// once returns retryAfter >= 0 when the error is worth retrying.
func (c *Client) once(ctx context.Context, body []byte) (*response, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, -1, fmt.Errorf("jev: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		err := fmt.Errorf("jev: HTTP %d: %s", res.StatusCode, bytes.TrimSpace(detail))
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == 529 {
			wait := 100 * time.Millisecond
			if s, perr := strconv.Atoi(res.Header.Get("Retry-After")); perr == nil {
				wait = time.Duration(s) * time.Second
			}
			return nil, wait, err
		}
		return nil, -1, err
	}
	var out response
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, -1, fmt.Errorf("jev: decode response: %w", err)
	}
	if out.Answers == nil {
		return nil, -1, errors.New("jev: response has no answers")
	}
	return &out, -1, nil
}
