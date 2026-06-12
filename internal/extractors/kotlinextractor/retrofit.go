package kotlinextractor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// retrofitAnnotation matches a Retrofit HTTP method annotation on an interface
// method, capturing the verb and the path string literal, e.g.
//
//	@GET("/api/settings/entitlements/users/{userID}/active")
//	@POST("auth/login")
var retrofitAnnotation = regexp.MustCompile(`@(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s*\(\s*"([^"]*)"`)

// extractRetrofitFacts emits a client-route fact for every Retrofit endpoint
// annotation in a Kotlin source file. These represent outbound HTTP calls the
// app makes, so the cross-repo linker can match them (by method + path suffix)
// to the backend route that serves them. Path prefixes are inconsistent across
// services (some "/api/...", some base-relative) — suffix matching reconciles
// that, so we emit the path as written.
func extractRetrofitFacts(src []byte, relFile string) []facts.Fact {
	var out []facts.Fact
	dir := filepath.ToSlash(filepath.Dir(relFile))
	api := apiHint(relFile)

	for i, line := range strings.Split(string(src), "\n") {
		m := retrofitAnnotation.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		method := strings.ToUpper(m[1])
		path := cleanClientPath(m[2])
		if path == "" {
			continue
		}
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: relFile,
			Line: i + 1,
			Props: map[string]any{
				"role":      "client",
				"method":    method,
				"framework": "retrofit",
				"language":  "kotlin",
				"source":    "retrofit",
				"api":       api,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// cleanClientPath strips a query string from a client route path and trims
// surrounding whitespace. Path parameters are left as written; the linker's
// normalizePath collapses {x}/:x/<x> forms.
func cleanClientPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// apiHint returns the source file's base name without extension (e.g.
// "EntitlementApiService"), used as the cross-repo linker's provider
// disambiguation hint.
func apiHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
