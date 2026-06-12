// Package crossrepo links the per-repo architectural graphs of an appended
// multi-repo fact set into a single cross-repo "graph of graphs".
//
// In multi-repo (append) mode, facts from several repositories live in one
// store, each tagged with a Repo label, but the graph only ever connects facts
// within a single repo. This package derives the edges *between* repos from
// signals the extractors already emit:
//
//   - HTTP route role matching: a route a repo calls (role="client") whose
//     (path, method) matches a route another repo serves (role="server" or
//     unset) means the caller depends on the servee.
//   - Import / shared-lib references: a dependency whose import target names
//     another loaded repo (by @scope or leading path segment) means the
//     importer depends on that repo.
//
// The result is expressed as synthetic facts: one KindService node per repo and
// one KindDependency edge per (consumer -> provider) pair. Because these are
// ordinary facts, they flow into Store.BuildGraph and make every traversal tool
// (traverse, find_path, impact_analysis, query_facts) cross-repo aware with no
// per-tool changes.
package crossrepo

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// SyntheticMarker tags every fact this package emits, so the engine can drop and
// recompute them idempotently on each append.
const SyntheticMarker = "crossrepo"

// maxSamples bounds how many endpoint / import samples an edge fact carries.
const maxSamples = 25

// minSharedSegments is the fewest trailing path segments a client and server
// route must share for a suffix match. Two segments (e.g. "settings/feedback")
// keeps the join specific enough to avoid false positives while tolerating the
// base-path/prefix differences between a server's full path ("/api/settings/...")
// and a client's base-relative call ("settings/...").
const minSharedSegments = 2

// edge accumulates everything justifying one consumer -> provider dependency.
type edge struct {
	consumer  string
	provider  string
	via       map[string]bool // "http", "import"
	endpoints map[string]bool // "METHOD /path"
	imports   map[string]bool // sample import targets
}

func (e *edge) note(via string) {
	if e.via == nil {
		e.via = map[string]bool{}
	}
	e.via[via] = true
}

// ComputeLinks analyzes a multi-repo fact set and returns synthetic facts that
// connect repositories. It is pure and deterministic: the same input always
// yields the same output in a stable, sorted order, so callers may recompute it
// idempotently after removing the prior synthetic facts.
func ComputeLinks(all []facts.Fact) []facts.Fact {
	normToLabel := repoLabelLookup(all)
	if len(normToLabel) < 2 {
		return nil // need at least two repos to have a cross-repo edge
	}

	edges := map[string]*edge{}
	linkHTTP(all, edges)
	linkImports(all, normToLabel, edges)

	return materialize(edges, repoLabels(normToLabel))
}

// repoLabels returns the actual repo labels (the values of the
// normalized-label lookup), sorted for deterministic output.
func repoLabels(normToLabel map[string]string) []string {
	out := make([]string, 0, len(normToLabel))
	for _, label := range normToLabel {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// repoLabelLookup returns a map from normalized repo label to the actual label,
// covering every distinct non-empty Repo tag in the fact set.
func repoLabelLookup(all []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range all {
		if f.Repo == "" {
			continue
		}
		out[normalizeLabel(f.Repo)] = f.Repo
	}
	return out
}

// --- signal (A): HTTP route role matching ---

type routeRef struct {
	repo   string
	method string
	path   string
}

func linkHTTP(all []facts.Fact, edges map[string]*edge) {
	// Index server routes by normalized path + method.
	server := map[string][]routeRef{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Repo == "" {
			continue
		}
		if roleOf(f) == "client" {
			continue
		}
		method := normalizeMethod(propString(f, "method"))
		if method == "" {
			continue
		}
		ref := routeRef{repo: f.Repo, method: method, path: f.Name}
		// Index every trailing-segment suffix of each server path, so a client
		// that calls a base-relative subpath ("settings/x") still matches a
		// server serving the full path ("/api/settings/x"). serverPaths already
		// returns normalized paths.
		for _, p := range serverPaths(f) {
			for _, suf := range pathSuffixes(p) {
				key := routeKey(suf, method)
				server[key] = append(server[key], ref)
			}
		}
	}

	// Match client routes against the server index.
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Repo == "" || roleOf(f) != "client" {
			continue
		}
		method := normalizeMethod(propString(f, "method"))
		if method == "" {
			continue
		}
		np := normalizePath(f.Name)
		if isGenericPath(np) {
			continue
		}
		// Canonicalize the leading slash so a base-relative client path
		// ("settings/x") matches the indexed suffix form ("/settings/x").
		matches := server[routeKey(canonicalLeadingSlash(np), method)]
		provider := pickProvider(f, matches)
		if provider == "" || provider == f.Repo {
			continue
		}
		e := edgeFor(edges, f.Repo, provider)
		e.note("http")
		if e.endpoints == nil {
			e.endpoints = map[string]bool{}
		}
		e.endpoints[method+" "+f.Name] = true
	}
}

// pickProvider resolves which provider repo a client route points at. With a
// single candidate repo it returns it directly; with several it uses the
// client's service hint (api / spec basename) to disambiguate, and returns ""
// (skip) when still ambiguous.
func pickProvider(client facts.Fact, matches []routeRef) string {
	providers := map[string]bool{}
	for _, m := range matches {
		if m.repo != client.Repo {
			providers[m.repo] = true
		}
	}
	switch len(providers) {
	case 0:
		return ""
	case 1:
		for p := range providers {
			return p
		}
	}
	hint := normalizeLabel(serviceHint(client))
	if hint == "" {
		return "" // ambiguous, no hint
	}
	for p := range providers {
		if normalizeLabel(p) == hint || strings.Contains(normalizeLabel(p), hint) || strings.Contains(hint, normalizeLabel(p)) {
			return p
		}
	}
	return ""
}

// serverPaths returns the normalized paths a server route is reachable at: its
// own path and, when present, its gateway path (prefix + path).
func serverPaths(f facts.Fact) []string {
	out := []string{normalizePath(f.Name)}
	if gw := propString(f, "gateway_path"); gw != "" {
		out = append(out, normalizePath(gw))
	}
	return out
}

func serviceHint(f facts.Fact) string {
	if api := propString(f, "api"); api != "" {
		return api
	}
	if spec := propString(f, "spec_file"); spec != "" {
		base := filepath.Base(spec)
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	return ""
}

// --- signal (B): import / shared-lib references ---

func linkImports(all []facts.Fact, normToLabel map[string]string, edges map[string]*edge) {
	for _, f := range all {
		if f.Repo == "" || f.Kind == facts.KindService {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelImports && rel.Kind != facts.RelDependsOn {
				continue
			}
			provider := importProvider(rel.Target, f.Repo, normToLabel)
			if provider == "" {
				continue
			}
			e := edgeFor(edges, f.Repo, provider)
			e.note("import")
			if e.imports == nil {
				e.imports = map[string]bool{}
			}
			e.imports[rel.Target] = true
		}
	}
}

// importProvider maps an import target to another loaded repo, or "" if none.
// It checks candidate identifiers from the target (the @scope, then each leading
// path segment) against the normalized repo labels, skipping self-matches.
func importProvider(target, consumer string, normToLabel map[string]string) string {
	target = strings.TrimSpace(target)
	// Skip relative / absolute filesystem imports — they are intra-repo.
	if target == "" || strings.HasPrefix(target, ".") || strings.HasPrefix(target, "/") {
		return ""
	}
	for _, cand := range importCandidates(target) {
		if label, ok := normToLabel[normalizeLabel(cand)]; ok && label != consumer {
			return label
		}
	}
	return ""
}

// importCandidates extracts the identifier tokens an import target may name a
// repo by, most-significant first: e.g. "@app-web/lib-api" ->
// ["app-web", "lib-api"], "lib-core/foo" -> ["lib-core", "foo"].
func importCandidates(target string) []string {
	t := strings.TrimPrefix(target, "@")
	parts := strings.Split(t, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- materialization ---

func edgeFor(edges map[string]*edge, consumer, provider string) *edge {
	key := consumer + "\x00" + provider
	e, ok := edges[key]
	if !ok {
		e = &edge{consumer: consumer, provider: provider}
		edges[key] = e
	}
	return e
}

func materialize(edges map[string]*edge, allRepos []string) []facts.Fact {
	// Stable order over edges.
	keys := make([]string, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// providers[consumer] = sorted set of providers it depends on. Used to attach
	// the traversable depends_on relations to each consumer's service node.
	providers := map[string][]string{}
	repoSet := map[string]bool{}

	// Detailed per-pair dependency facts carry the evidence (endpoints/imports)
	// and are queryable, but hold NO relations: the traversable graph edge lives
	// on the service node, so we avoid creating a stray "a -> b" graph node.
	var depFacts []facts.Fact
	for _, k := range keys {
		e := edges[k]
		repoSet[e.consumer] = true
		repoSet[e.provider] = true
		providers[e.consumer] = append(providers[e.consumer], e.provider)

		props := map[string]any{
			"type":      "cross_repo",
			"synthetic": SyntheticMarker,
			"via":       sortedKeys(e.via),
		}
		if len(e.endpoints) > 0 {
			eps := sortedKeys(e.endpoints)
			props["endpoint_count"] = len(eps)
			props["endpoints"] = cap25(eps)
		}
		if len(e.imports) > 0 {
			imps := sortedKeys(e.imports)
			props["import_count"] = len(imps)
			props["import_samples"] = cap25(imps)
		}

		depFacts = append(depFacts, facts.Fact{
			Kind:  facts.KindDependency,
			Name:  fmt.Sprintf("%s -> %s", e.consumer, e.provider),
			Repo:  e.consumer,
			Props: props,
		})
	}

	// Every loaded repo becomes an addressable service node, even with no
	// cross-repo edges, so find_path/traverse can resolve isolated repo labels.
	for _, r := range allRepos {
		repoSet[r] = true
	}

	// Service nodes first (sorted), then dependency edges. Each consumer's
	// service node gets a depends_on relation per provider, which BuildGraph
	// turns into the cross-repo graph edge.
	repos := make([]string, 0, len(repoSet))
	for r := range repoSet {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	out := make([]facts.Fact, 0, len(repos)+len(depFacts))
	for _, r := range repos {
		var rels []facts.Relation
		for _, p := range providers[r] {
			rels = append(rels, facts.Relation{Kind: facts.RelDependsOn, Target: p})
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindService,
			Name:      r,
			Repo:      r,
			Props:     map[string]any{"synthetic": SyntheticMarker},
			Relations: rels,
		})
	}
	out = append(out, depFacts...)
	return out
}

// --- small helpers ---

func roleOf(f facts.Fact) string { return propString(f, "role") }

func propString(f facts.Fact, key string) string {
	if f.Props == nil {
		return ""
	}
	if v, ok := f.Props[key].(string); ok {
		return v
	}
	return ""
}

func normalizeMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	switch m {
	case "", "USE", "ALL", "MIDDLEWARE":
		return ""
	}
	return m
}

func routeKey(normPath, method string) string { return normPath + "|" + method }

// normalizePath trims a trailing slash and collapses path parameters in any
// framework syntax ({id}, :id, <id>) to a single "{}" placeholder, so a client
// path matches the server path it calls regardless of param naming.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, ":"):
			segs[i] = "{}"
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			segs[i] = "{}"
		case strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// pathSuffixes returns every trailing-segment suffix of a normalized path that
// has at least minSharedSegments non-empty segments, longest first. Each suffix
// is rendered leading-slash-canonical ("/seg/seg/..."), so a server path
// "/api/settings/entitlements/definitions" yields:
//
//	/api/settings/entitlements/definitions
//	/settings/entitlements/definitions
//	/entitlements/definitions
//
// ("definitions" alone is dropped: below minSharedSegments). This lets a client
// calling a base-relative subpath match the server serving the full path.
func pathSuffixes(normPath string) []string {
	var segs []string
	for _, s := range strings.Split(normPath, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	var out []string
	for start := 0; start+minSharedSegments <= len(segs); start++ {
		out = append(out, "/"+strings.Join(segs[start:], "/"))
	}
	return out
}

// canonicalLeadingSlash ensures a non-empty path starts with "/", so a
// base-relative client path ("settings/x") compares equal to the indexed
// suffix form ("/settings/x").
func canonicalLeadingSlash(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// isGenericPath reports whether a path is too low-signal to safely link on
// (e.g. /health, /status) — no path parameter and fewer than two segments.
func isGenericPath(normPath string) bool {
	var segs []string
	for _, s := range strings.Split(normPath, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if strings.Contains(normPath, "{}") {
		return false
	}
	return len(segs) < 2
}

// normalizeLabel lowercases and strips '-'/'_' so "app-web",
// "app_web", and "AppWeb" all compare equal.
func normalizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cap25(ss []string) []string {
	if len(ss) > maxSamples {
		return ss[:maxSamples]
	}
	return ss
}
