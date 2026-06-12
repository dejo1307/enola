package tsextractor

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// httpClientCall matches a fetch()/makeRequest() call whose first argument is a
// string or template literal, capturing the URL literal into one of three
// groups (double-quote, single-quote, or backtick — RE2 has no backreferences).
// e.g. this.makeRequest<T>('/api/settings/feedback', { method: 'POST' })
//
//	fetch(`${API_BASE_URL}/api/user/current`, { method: 'GET' })
var httpClientCall = regexp.MustCompile("(?:fetch|makeRequest)\\s*(?:<[^>]*>)?\\s*\\(\\s*(?:\"([^\"]*)\"|'([^']*)'|`([^`]*)`)")

// httpClientMethod matches a `method: 'POST'` option within a call's options
// object.
var httpClientMethod = regexp.MustCompile(`method\s*:\s*['"]([A-Za-z]+)['"]`)

// tsInterpolation matches a template-literal interpolation, e.g. ${id}.
var tsInterpolation = regexp.MustCompile(`\$\{[^}]*\}`)

// httpMethodWindow is how many bytes after the URL literal to scan for the
// request's method option.
const httpMethodWindow = 200

// extractHTTPClientFacts emits a client-route fact for every hand-written
// fetch()/makeRequest() call to the backend. The path is the call's first
// string argument (its leading ${baseURL} token stripped) and the method comes
// from a nearby `method:` option, defaulting to GET. Paths include the /api
// prefix here; the cross-repo linker's suffix matching reconciles prefixes.
func extractHTTPClientFacts(src []byte, relFile string) []facts.Fact {
	dir := filepath.ToSlash(filepath.Dir(relFile))
	api := tsAPIHint(relFile)

	var out []facts.Fact
	for _, m := range httpClientCall.FindAllSubmatchIndex(src, -1) {
		raw := firstNonEmptyGroup(src, m, 1, 2, 3)
		path, ok := cleanTSPath(raw)
		if !ok {
			continue
		}
		method := "GET"
		end := m[1] + httpMethodWindow
		if end > len(src) {
			end = len(src)
		}
		if mm := httpClientMethod.FindSubmatch(src[m[1]:end]); mm != nil {
			method = strings.ToUpper(string(mm[1]))
		}
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: relFile,
			Line: 1 + bytes.Count(src[:m[0]], []byte("\n")),
			Props: map[string]any{
				"role":      "client",
				"method":    method,
				"framework": "fetch",
				"language":  "typescript",
				"source":    "ts-http-client",
				"api":       api,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// firstNonEmptyGroup returns the text of the first matched capture group among
// the given group indices (FindAllSubmatchIndex layout).
func firstNonEmptyGroup(src []byte, m []int, groups ...int) string {
	for _, g := range groups {
		s, e := m[2*g], m[2*g+1]
		if s >= 0 && e > s {
			return string(src[s:e])
		}
	}
	return ""
}

// cleanTSPath turns a fetch/makeRequest URL literal into a matchable route path,
// or returns ok=false when it is not a backend path (fully dynamic, external,
// or empty). It strips a leading ${...} base-URL token, drops the query string,
// and collapses interpolations to the {} placeholder.
func cleanTSPath(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	// Strip a leading ${...} base-URL token (e.g. ${API_BASE_URL}).
	if strings.HasPrefix(p, "${") {
		if i := strings.IndexByte(p, '}'); i >= 0 {
			p = p[i+1:]
		}
	}
	// A remaining absolute URL points at a third-party API, not our backend.
	if strings.HasPrefix(p, "http") {
		return "", false
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = tsInterpolation.ReplaceAllString(p, "{}")
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "", false
	}
	// Require at least one concrete (non-placeholder) segment so a fully dynamic
	// URL (e.g. just "${endpoint}") is skipped.
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && seg != "{}" {
			return p, true
		}
	}
	return "", false
}

// tsAPIHint returns the source file's base name without extension (e.g.
// "feedback"), used as the cross-repo linker's disambiguation hint.
func tsAPIHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
