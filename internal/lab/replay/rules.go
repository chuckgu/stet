package replay

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
)

// RuleArm is experiment arm A: deterministic checks only, on the state the
// gate recorded. Writes already passed the schema's own FK and CHECK
// constraints, so A0 adds what a careful team would add without a judgment
// model: a unique index on normalized email and a pg_trgm duplicate check.
// A1 also codes the lab's three policy rules by hand. It is reported for
// reference; the go/kill comparison uses A0 (D24). A2 is A1 fixed once, the
// way a team would after seeing it stop ordinary writes (D28).
type RuleArm struct {
	Name   string
	Policy bool // code the policy rules (A1, A2)
	// Hardened (A2) judges a rule only when the write touches a field the
	// rule is about, and reads titles that differ in case, punctuation, or
	// spacing as one title.
	Hardened bool
	// NameSimilarity marks an insert as a duplicate of a same-account
	// contact. Fixed before the first run at pg_trgm's default
	// similarity_threshold.
	NameSimilarity float64
}

var (
	A0 = RuleArm{Name: "A0", NameSimilarity: 0.3}
	A1 = RuleArm{Name: "A1", Policy: true, NameSimilarity: 0.3}
	A2 = RuleArm{Name: "A2", Policy: true, Hardened: true, NameSimilarity: 0.3}
)

// Judge returns a verdict and reasons, as the gate would record them.
func (a RuleArm) Judge(r Record) (string, []string) {
	if r.Verdict == decide.Unsupported {
		return decide.Unsupported, nil
	}
	s := r.State
	var reasons []string
	if r.Op == "insert" {
		reasons = append(reasons, a.duplicate(s)...)
	}
	if a.Policy && r.Table == "contacts" {
		// A1 checks the row as it is after the write, as a CHECK constraint
		// would, so it also stops edits to rows that already break a rule.
		touches := func(fields ...string) bool {
			return !a.Hardened || slices.ContainsFunc(fields, func(f string) bool { _, ok := s.Proposed[f]; return ok })
		}
		if touches("status") && after(s, "status") == "churned" && count(s.Context["open_deals_at_account"]) > 0 {
			reasons = append(reasons, "policy:1")
		}
		if a.overwritesOthersTitle(r) {
			reasons = append(reasons, "policy:2")
		}
		if touches("marketing_consent", "consent_source") && consentWithoutSource(s) {
			reasons = append(reasons, "policy:3")
		}
	}
	if len(reasons) > 0 {
		return decide.Hold, reasons
	}
	return decide.Commit, nil
}

// duplicate flags the first candidate with the same normalized email, or
// failing that, the first at the same account whose name is similar.
func (a RuleArm) duplicate(s gate.State) []string {
	p := s.Proposed
	email := normEmail(text(p["email"]))
	for _, c := range s.Candidates {
		if email != "" && normEmail(text(c["email"])) == email {
			return []string{"unique_email:" + text(c["id"])}
		}
	}
	for _, c := range s.Candidates {
		if text(c["account_id"]) == text(p["account_id"]) && similarity(text(p["name"]), text(c["name"])) >= a.NameSimilarity {
			return []string{"similar_name:" + text(c["id"])}
		}
	}
	return nil
}

// overwritesOthersTitle: rule 2 ("do not overwrite a title with an older
// value from a stale source"). Code cannot tell a stale source, so it
// refuses to change a title another writer set within the recent window.
func (a RuleArm) overwritesOthersTitle(r Record) bool {
	s := r.State
	v, ok := s.Proposed["title"]
	if r.Op != "update" || !ok || text(v) == text(s.Current["title"]) {
		return false
	}
	if a.Hardened && plain(text(v)) == plain(text(s.Current["title"])) {
		return false
	}
	for _, c := range s.RecentChanges {
		if _, set := c.Fields["title"]; set && c.ByAgent != r.Agent {
			return true
		}
	}
	return false
}

// consentWithoutSource: rule 3, on the row as it is after the write. It is
// also how the labels decide F4 for agent writes, which may differ from the
// task input.
func consentWithoutSource(s gate.State) bool {
	return truthy(afterValue(s, "marketing_consent")) && strings.TrimSpace(after(s, "consent_source")) == ""
}

// plain keeps a title's letters and digits, lowercased.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

func afterValue(s gate.State, field string) any {
	if v, ok := s.Proposed[field]; ok {
		return v
	}
	return s.Current[field]
}

// after is a field's value after the write as text. An expression counts as
// a value.
func after(s gate.State, field string) string {
	switch v := afterValue(s, field).(type) {
	case map[string]any:
		return text(v["expr"])
	default:
		return text(v)
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(x))
		return b || strings.EqualFold(strings.TrimSpace(x), "yes") || strings.EqualFold(strings.TrimSpace(x), "on")
	}
	return false
}

func count(v any) int {
	n, _ := strconv.Atoi(text(v))
	return n
}

// normEmail is what a unique index on normalized email would compare:
// lowercase, with dots, dashes, underscores, and +tags dropped from the
// local part.
func normEmail(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	local, domain, ok := strings.Cut(e, "@")
	if !ok {
		return e
	}
	local, _, _ = strings.Cut(local, "+")
	local = strings.NewReplacer(".", "", "-", "", "_", "").Replace(local)
	return local + "@" + domain
}

// similarity is pg_trgm's: each word, lowercased and padded with two
// spaces before and one after, gives a set of trigrams; the score is shared
// trigrams over all distinct trigrams. Characters other than letters and
// digits separate words.
func similarity(a, b string) float64 {
	ta, tb := trigrams(a), trigrams(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	shared := 0
	for t := range ta {
		if tb[t] {
			shared++
		}
	}
	return float64(shared) / float64(len(ta)+len(tb)-shared)
}

func trigrams(s string) map[string]bool {
	set := map[string]bool{}
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, w := range words {
		rs := append([]rune("  "+w), ' ')
		for i := 0; i+3 <= len(rs); i++ {
			set[string(rs[i:i+3])] = true
		}
	}
	return set
}
