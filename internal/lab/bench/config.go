package bench

// This file derives the configurations a run uses from the two frozen files of lab/bench (spec 5
// and 6, plan B2): hoop's for a candidate or a reference arm, and Stet's for one of an arm's
// databases. A derived config is written once to a file of its own: a running hoop re-reads its
// config file, so one that is in use must never change under it.

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/stetdb/stet/internal/config"
)

// HoopVariant is one of hoop's configurations: a candidate of spec 6 or a reference arm of spec 3.
type HoopVariant struct {
	Prompt     string // a | b | c
	PromptText string // the guidance of b or c; ignored for a
	Medium     string // allow | block
	Cache      bool   // analyzer.cache: {size: 10000, ttl_sec: 86400}
	Model      string // "" keeps the base's model
	NoEndpoint bool   // drop analyzer.endpoint
}

// The hoop-cache arm's verdict cache (spec 3).
const (
	hoopCacheSize   = 10000
	hoopCacheTTLSec = 86400
)

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// HoopConfig returns the base config with the variant applied: listeners[0].analyzer.medium is
// Medium; for b and c listeners[0].analyzer.prompt is PromptText (for a there is none, so hoop uses
// its own guidance); analyzer.cache is the hoop-cache arm's with Cache and absent without; Model,
// when given, replaces analyzer.model; NoEndpoint drops analyzer.endpoint. Everything else keeps the
// base's order and values. The base's comments are replaced by one naming the variant. The same
// input gives the same bytes.
func HoopConfig(base []byte, v HoopVariant) ([]byte, error) {
	switch v.Prompt {
	case "a":
	case "b", "c":
		if strings.TrimSpace(v.PromptText) == "" {
			// hoop treats an empty prompt as its own guidance: that is (가), not (나) or (다).
			return nil, fmt.Errorf("prompt %s has no text", v.Prompt)
		}
	default:
		return nil, fmt.Errorf("prompt %q: want a, b or c", v.Prompt)
	}
	if v.Medium != "allow" && v.Medium != "block" {
		return nil, fmt.Errorf("medium %q: want allow or block", v.Medium)
	}
	if v.Model != "" && !modelRE.MatchString(v.Model) {
		return nil, fmt.Errorf("model %q is not a model id", v.Model)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("the base: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the base is not a mapping")
	}
	root := doc.Content[0]
	analyzer := mappingValue(root, "analyzer")
	if analyzer == nil {
		return nil, errors.New("the base has no analyzer section")
	}
	listeners := mappingValue(root, "listeners")
	if listeners == nil || listeners.Kind != yaml.SequenceNode || len(listeners.Content) == 0 ||
		listeners.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the base has no listener")
	}
	lane := mappingValue(listeners.Content[0], "analyzer")
	if lane == nil {
		return nil, errors.New("the base's first listener has no analyzer block")
	}

	setKey(lane, "medium", plain(v.Medium))
	if v.Prompt == "a" {
		deleteKey(lane, "prompt")
	} else {
		setKey(lane, "prompt", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v.PromptText, Style: yaml.LiteralStyle})
	}
	if v.Cache {
		setKey(analyzer, "cache", &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle, Content: []*yaml.Node{
			plain("size"), {Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(hoopCacheSize)},
			plain("ttl_sec"), {Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(hoopCacheTTLSec)},
		}})
	} else {
		deleteKey(analyzer, "cache")
	}
	if v.Model != "" {
		setKey(analyzer, "model", plain(v.Model))
	}
	if v.NoEndpoint {
		deleteKey(analyzer, "endpoint")
	}

	clearComments(&doc)
	root.HeadComment = "bench hoop-config " + strings.Join(variantFlags(v), " ") + " (스펙 6장)으로 만든 hoop 설정이다.\n" +
		"돌고 있는 hoop은 이 파일이 바뀌면 다시 읽는다. 고치지 말고 새 파일로 만든다."
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// variantFlags are the bench hoop-config flags that give v.
func variantFlags(v HoopVariant) []string {
	flags := []string{"-prompt", v.Prompt, "-medium", v.Medium}
	if v.Cache {
		flags = append(flags, "-cache")
	}
	if v.Model != "" {
		flags = append(flags, "-model", v.Model)
	}
	if v.NoEndpoint {
		flags = append(flags, "-no-endpoint")
	}
	return flags
}

func plain(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

// mappingValue returns the value of key in mapping m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setKey replaces the value of key in mapping m, or appends key with it.
func setKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, plain(key), value)
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func clearComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		clearComments(c)
	}
}

// upstreamLineRE is the frozen Stet config's top-level upstream line; its first group is the URL.
var upstreamLineRE = regexp.MustCompile(`(?m)^upstream:[ \t]+(\S+)`)

// benchDatabaseRE is a database of the benchmark (DBName): lowercase letters, digits and _ after crm_.
var benchDatabaseRE = regexp.MustCompile(`^crm_[a-z0-9_]+$`)

// StetConfig returns the frozen config with the database of its upstream URL replaced; every other
// byte is kept (plan B2). The copy is checked the way the stet binary loads it.
func StetConfig(frozen []byte, database string) ([]byte, error) {
	if !benchDatabaseRE.MatchString(database) || len(database) > maxIdentifier || strings.Contains(database, "stet") {
		return nil, fmt.Errorf("database %q: want one of the benchmark's, crm_ and then lowercase letters, "+
			"digits and _ (at most %d bytes, without the project's name)", database, maxIdentifier)
	}
	lines := upstreamLineRE.FindAllSubmatchIndex(frozen, -1)
	if len(lines) != 1 {
		return nil, fmt.Errorf("the frozen config has %d top-level upstream lines, want one", len(lines))
	}
	start, end := lines[0][2], lines[0][3]
	raw := string(frozen[start:end])
	old, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	// The path is what follows the authority, up to a query or fragment. Only it is rewritten, so
	// the user, password, host and options stay as they were written.
	scheme := strings.Index(raw, "://")
	if scheme < 0 || !strings.HasPrefix(old.Path, "/") || len(old.Path) < 2 {
		return nil, fmt.Errorf("upstream %s names no database", old.Redacted())
	}
	path := scheme + 3 + strings.IndexByte(raw[scheme+3:], '/')
	tail := len(raw)
	if i := strings.IndexAny(raw[path:], "?#"); i >= 0 {
		tail = path + i
	}
	upstream := raw[:path] + "/" + database + raw[tail:]

	out := make([]byte, 0, len(frozen)+len(database))
	out = append(out, frozen[:start]...)
	out = append(out, upstream...)
	out = append(out, frozen[end:]...)

	c, err := config.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("the copy does not load: %w", err)
	}
	if u, err := url.Parse(c.Upstream); err != nil || u.Path != "/"+database || u.Host != old.Host ||
		u.User.String() != old.User.String() || u.RawQuery != old.RawQuery {
		return nil, fmt.Errorf("the copy's upstream %s is not the frozen one with database %s", c.Upstream, database)
	}
	return out, nil
}

// maxIdentifier is PostgreSQL's identifier limit in bytes.
const maxIdentifier = 63

// WriteConfig writes a derived config to path, which must not exist yet; "-" and /dev/stdout are
// standard output. A config is written once: a running hoop re-reads its file, and a run's config
// is part of its record.
func WriteConfig(path string, b []byte) error {
	if path == "-" || path == "/dev/stdout" {
		_, err := os.Stdout.Write(b)
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists: a config is written once, since a running proxy re-reads its file; "+
			"give this run a new file", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
