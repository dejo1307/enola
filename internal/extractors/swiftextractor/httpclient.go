package swiftextractor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// swiftPathComponent matches a URLRequest path built from a base URL, capturing
// the path literal, e.g. baseURL.appendingPathComponent("settings/feedback").
var swiftPathComponent = regexp.MustCompile(`appendingPathComponent\(\s*"([^"]*)"`)

// swiftHTTPMethod matches an explicit method assignment, e.g.
// request.httpMethod = "POST".
var swiftHTTPMethod = regexp.MustCompile(`\.httpMethod\s*=\s*"([A-Za-z]+)"`)

// swiftInterpolation matches Swift string interpolation, e.g. \(userID).
var swiftInterpolation = regexp.MustCompile(`\\\([^)]*\)`)

// methodWindow is how many lines after an appendingPathComponent call to scan
// for the associated httpMethod assignment.
const methodWindow = 5

// extractURLSessionFacts emits a client-route fact for every URLSession request
// in a Swift source file. The path comes from baseURL.appendingPathComponent("…")
// and the HTTP method from a nearby `.httpMethod = "…"` assignment (defaulting
// to GET when none is found within methodWindow lines). Paths are base-relative
// (no /api prefix); the cross-repo linker's suffix matching reconciles that
// against the backend's full path.
func extractURLSessionFacts(src []byte, relFile string) []facts.Fact {
	lines := strings.Split(string(src), "\n")
	dir := filepath.ToSlash(filepath.Dir(relFile))
	api := swiftAPIHint(relFile)

	var out []facts.Fact
	for i, line := range lines {
		pm := swiftPathComponent.FindStringSubmatch(line)
		if pm == nil {
			continue
		}
		path := cleanSwiftPath(pm[1])
		if path == "" {
			continue
		}
		method := methodNear(lines, i)
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: relFile,
			Line: i + 1,
			Props: map[string]any{
				"role":      "client",
				"method":    method,
				"framework": "urlsession",
				"language":  "swift",
				"source":    "urlsession",
				"api":       api,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// methodNear returns the HTTP method assigned within methodWindow lines at or
// after idx, or "GET" when none is found (the URLSession default).
func methodNear(lines []string, idx int) string {
	end := idx + methodWindow
	if end >= len(lines) {
		end = len(lines) - 1
	}
	for j := idx; j <= end; j++ {
		if m := swiftHTTPMethod.FindStringSubmatch(lines[j]); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return "GET"
}

// cleanSwiftPath converts Swift interpolation to the {} placeholder the linker
// understands and strips any query string.
func cleanSwiftPath(p string) string {
	p = swiftInterpolation.ReplaceAllString(p, "{}")
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// swiftAPIHint returns the source file's base name without extension (e.g.
// "EntitlementAPIService"), used as the cross-repo linker's disambiguation hint.
func swiftAPIHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
