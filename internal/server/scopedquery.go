package server

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// scopedQuery is the parsed form of a node-resolution input that may carry
// scoping prefixes (repo:, kind:, file:) to disambiguate an otherwise-ambiguous
// substring term. A plain input with no recognized prefix yields a scopedQuery
// whose Term equals the input and whose scope fields are empty, preserving the
// legacy substring-match behavior.
type scopedQuery struct {
	Repo       string   // repo:<label>          — exact repo-label filter
	Kinds      []string // kind:<fact-kind>      — fact-kind filter (module, symbol, ...)
	SymbolKind string   // kind:<symbol-kind>    — post-filter on Props["symbol_kind"] (struct, interface, ...)
	FilePrefix string   // file:<path-prefix>    — file path prefix filter
	Term       string   // the remaining bare substring search term
}

// factKinds and symbolKinds classify a kind: value so it can be routed to the
// right filter. Fact kinds map to QueryOpts.Kinds; symbol kinds additionally
// constrain Props["symbol_kind"] (which QueryOpts has no dedicated field for).
var factKinds = map[string]bool{
	facts.KindModule: true, facts.KindSymbol: true, facts.KindRoute: true,
	facts.KindStorage: true, facts.KindDependency: true, facts.KindService: true,
}

var symbolKinds = map[string]bool{
	facts.SymbolFunc: true, facts.SymbolMethod: true, facts.SymbolStruct: true,
	facts.SymbolInterface: true, facts.SymbolType: true, facts.SymbolClass: true,
	facts.SymbolVariable: true, facts.SymbolConstant: true,
}

// parseScopedQuery splits an input into optional scope filters and a bare term.
//
// Two syntaxes are accepted (and may be mixed):
//   - Keyword tokens, space-separated:   "repo:golf kind:struct Currency"
//   - Slash sugar inside a value:        "repo:golf/subtenant", "kind:symbol/Currency",
//     "file:/domain//Currency"
//
// A token is treated as scoped only when it begins with exactly "repo:", "kind:",
// or "file:" followed by a non-empty value; anything else is part of the term.
func parseScopedQuery(input string) scopedQuery {
	var sq scopedQuery
	var terms []string

	for _, tok := range strings.Fields(input) {
		key, val, ok := splitScopeToken(tok)
		if !ok {
			terms = append(terms, tok)
			continue
		}
		switch key {
		case "repo":
			// "golf/subtenant" → repo "golf", remainder narrows the term.
			label, rest := splitOnce(val, "/")
			sq.Repo = label
			if rest != "" {
				terms = append(terms, rest)
			}
		case "kind":
			// "symbol/Currency" → kind "symbol", remainder narrows the term.
			k, rest := splitOnce(val, "/")
			k = strings.ToLower(k)
			if symbolKinds[k] {
				sq.SymbolKind = k
				sq.Kinds = append(sq.Kinds, facts.KindSymbol)
			} else if k != "" {
				sq.Kinds = append(sq.Kinds, k)
			}
			if rest != "" {
				terms = append(terms, rest)
			}
		case "file":
			// "/domain//Currency" → prefix "domain", remainder narrows the term.
			prefix, rest := splitOnce(val, "//")
			sq.FilePrefix = strings.TrimPrefix(prefix, "/")
			if rest != "" {
				terms = append(terms, rest)
			}
		}
	}

	sq.Term = strings.Join(terms, " ")
	return sq
}

// splitScopeToken recognizes "<key>:<value>" where key is one of the scope
// keywords and value is non-empty. Returns ok=false otherwise.
func splitScopeToken(tok string) (key, val string, ok bool) {
	colon := strings.IndexByte(tok, ':')
	if colon <= 0 || colon == len(tok)-1 {
		return "", "", false
	}
	key = strings.ToLower(tok[:colon])
	switch key {
	case "repo", "kind", "file":
		return key, tok[colon+1:], true
	}
	return "", "", false
}

// splitOnce splits s on the first occurrence of sep, returning the left part and
// the remainder (without sep). When sep is absent, rest is empty.
func splitOnce(s, sep string) (left, rest string) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):]
	}
	return s, ""
}

// scoredCandidate is a ranked resolution candidate surfaced when a name is
// ambiguous, so callers can pick the right one or trust the auto-pick.
type scoredCandidate struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Repo  string  `json:"repo,omitempty"`
	File  string  `json:"file,omitempty"`
	Score float64 `json:"score"`
}

// matchTier classifies how directly a fact's name answers the term, as a coarse
// rank that dominates the finer per-kind tweaks in scoreCandidate. This is the
// key intent signal: a symbol whose short name IS the term (suffix-exact) must
// always outrank one that merely contains the term as a substring, regardless of
// kind. 2 = whole-name exact, 1 = suffix-exact (last dotted segment), 0 = substring.
func matchTier(name, term string) int {
	if term == "" {
		return 0
	}
	n := strings.ToLower(name)
	if n == term {
		return 2
	}
	if strings.ToLower(lastSegment(name)) == term {
		return 1
	}
	return 0
}

// scoreCandidate assigns a heuristic relevance score (higher is better). The
// match tier (whole-name/suffix/substring) dominates via a large weight, so kind
// and scope bonuses only break ties WITHIN a tier — a struct that merely contains
// the term can never outrank a method whose name exactly is the term.
func scoreCandidate(f facts.Fact, sq scopedQuery) float64 {
	term := strings.ToLower(sq.Term)
	score := float64(matchTier(f.Name, term)) // 0, 1, or 2 — dominant term

	// Within-tier tie-breakers (small relative to the tier step of 1.0).
	switch sk, _ := f.Props["symbol_kind"].(string); sk {
	case facts.SymbolStruct, facts.SymbolClass, facts.SymbolInterface, facts.SymbolType:
		score += 0.15 // type-level: the usual target of a bare name
	case facts.SymbolFunc, facts.SymbolMethod:
		score -= 0.15 // members are slightly less likely for a bare name
	}
	if f.Kind == facts.KindModule {
		score += 0.1
	}
	if sq.Repo != "" && strings.EqualFold(f.Repo, sq.Repo) {
		score += 0.15
	}
	if sq.FilePrefix != "" && strings.HasPrefix(f.File, sq.FilePrefix) {
		score += 0.1
	}
	return score
}

// rankCandidates scores and sorts facts by descending score (stable, so store
// order breaks ties deterministically).
func rankCandidates(results []facts.Fact, sq scopedQuery) []scoredCandidate {
	ranked := make([]scoredCandidate, 0, len(results))
	for _, r := range results {
		ranked = append(ranked, scoredCandidate{
			Name:  r.Name,
			Kind:  r.Kind,
			Repo:  r.Repo,
			File:  r.File,
			Score: scoreCandidate(r, sq),
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})
	return ranked
}

// pickConfidence expresses how decisively the top candidate beats the runner-up
// in [0,1]. A candidate in a strictly higher match tier than the runner-up is
// decisive (the term names it more directly than anything else) → high
// confidence. Within the same tier, confidence is top/(top+runnerUp): a lone
// candidate scores 1.0, an even tie 0.5.
func pickConfidence(ranked []scoredCandidate, term string) float64 {
	if len(ranked) == 0 || ranked[0].Score <= 0 {
		return 0
	}
	if len(ranked) == 1 {
		return 1
	}
	term = strings.ToLower(term)
	if matchTier(ranked[0].Name, term) > matchTier(ranked[1].Name, term) {
		return 0.9 // sole highest-tier match: clearly the intended target
	}
	top, runnerUp := ranked[0].Score, ranked[1].Score
	if runnerUp < 0 {
		runnerUp = 0
	}
	denom := top + runnerUp
	if denom <= 0 {
		return 0
	}
	return top / denom
}

// lastSegment returns the substring after the final ".", or the whole string.
func lastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}
