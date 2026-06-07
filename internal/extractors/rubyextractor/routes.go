package rubyextractor

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Route DSL regex patterns.
var (
	httpVerbRe  = regexp.MustCompile(`^\s*(get|post|put|patch|delete)\s+['"]([^'"]+)['"](?:\s*,\s*to:\s*['"]([^'"]+)['"])?`)
	resourcesRe = regexp.MustCompile(`^\s*resources?\s+:(\w+)`)
	namespaceRe = regexp.MustCompile(`^\s*namespace\s+:(\w+)`)
	scopePathRe = regexp.MustCompile(`^\s*scope\s+['"]([^'"]+)['"]`)
	scopeModRe  = regexp.MustCompile(`^\s*scope\s+module:\s*[:'"](\w+)`)
	rootRe      = regexp.MustCompile(`^\s*root\s+(?:to:\s*)?['"]([^'"]+)['"]`)
	drawRe      = regexp.MustCompile(`^\s*draw\s*\(\s*:(\w+)\s*\)`)
	memberRe    = regexp.MustCompile(`^\s*(member|collection)\s+do\b`)
	doBlockRe   = regexp.MustCompile(`\bdo\s*(?:\|[^|]*\|)?\s*$`)
	onlyRe      = regexp.MustCompile(`only:\s*\[([^\]]*)\]`)
	exceptRe    = regexp.MustCompile(`except:\s*\[([^\]]*)\]`)
)

// extractAllRoutes finds and parses all Rails route files in the repository.
func extractAllRoutes(repoPath string, files []string) []facts.Fact {
	var allFacts []facts.Fact

	// Collect route files: config/routes.rb, config/routes/*.rb, packages/*/config/routes/*.rb
	var routeFiles []string
	for _, relFile := range files {
		if !isRubyFile(relFile) {
			continue
		}
		if isRouteFile(relFile) {
			routeFiles = append(routeFiles, relFile)
		}
	}

	for _, relFile := range routeFiles {
		absFile := filepath.Join(repoPath, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			log.Printf("[ruby-extractor] error reading route file %s: %v", relFile, err)
			continue
		}
		routeFacts := parseRouteFile(f, relFile)
		f.Close()
		allFacts = append(allFacts, routeFacts...)
	}

	return allFacts
}

// isRouteFile returns true if the file path looks like a Rails route file.
func isRouteFile(relFile string) bool {
	// config/routes.rb
	if relFile == filepath.Join("config", "routes.rb") {
		return true
	}
	// config/routes/*.rb
	if strings.HasPrefix(relFile, filepath.Join("config", "routes")+string(filepath.Separator)) {
		return true
	}
	// packages/*/config/routes/*.rb (packwerk pattern)
	parts := strings.Split(filepath.ToSlash(relFile), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "packages" && parts[i+2] == "config" && parts[i+3] == "routes" {
			return true
		}
	}
	return false
}

// routeScope tracks the current route scope prefix.
type routeScope struct {
	pathPrefix string
	module     string
}

// parseRouteFile parses a single Rails route file.
func parseRouteFile(f *os.File, relFile string) []facts.Fact {
	var result []facts.Fact

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var (
		lineNum    int
		scopeStack []routeScope
		depth      int
		currentResource string
	)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Track end keywords.
		if trimmed == "end" {
			depth--
			if depth < 0 {
				depth = 0
			}
			if depth < len(scopeStack) {
				scopeStack = scopeStack[:depth]
			}
			currentResource = ""
			continue
		}

		prefix := buildPrefix(scopeStack)

		// draw(:package_name) -- delegation to packwerk package routes.
		if m := drawRe.FindStringSubmatch(line); m != nil {
			result = append(result, facts.Fact{
				Kind: facts.KindRoute,
				Name: prefix + "/" + m[1],
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"method":    "DRAW",
					"framework": "rails",
					"language":  "ruby",
					"delegate":  m[1],
				},
			})
			continue
		}

		// Namespace.
		if m := namespaceRe.FindStringSubmatch(line); m != nil {
			scopeStack = append(scopeStack, routeScope{
				pathPrefix: "/" + m[1],
				module:     m[1],
			})
			depth++
			continue
		}

		// Scope with path.
		if m := scopePathRe.FindStringSubmatch(line); m != nil {
			path := m[1]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			scopeStack = append(scopeStack, routeScope{pathPrefix: path})
			if doBlockRe.MatchString(line) {
				depth++
			}
			continue
		}

		// Scope with module.
		if m := scopeModRe.FindStringSubmatch(line); m != nil {
			scopeStack = append(scopeStack, routeScope{module: m[1]})
			if doBlockRe.MatchString(line) {
				depth++
			}
			continue
		}

		// Root route.
		if m := rootRe.FindStringSubmatch(line); m != nil {
			result = append(result, facts.Fact{
				Kind: facts.KindRoute,
				Name: prefix + "/",
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"method":    "GET",
					"framework": "rails",
					"language":  "ruby",
					"handler":   m[1],
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: filepath.Dir(relFile)},
				},
			})
			continue
		}

		// HTTP verb routes: get '/path', post '/path', etc.
		if m := httpVerbRe.FindStringSubmatch(line); m != nil {
			method := strings.ToUpper(m[1])
			path := m[2]
			handler := m[3]

			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			fullPath := prefix + path

			props := map[string]any{
				"method":    method,
				"framework": "rails",
				"language":  "ruby",
			}
			if handler != "" {
				props["handler"] = handler
			}

			result = append(result, facts.Fact{
				Kind:  facts.KindRoute,
				Name:  fullPath,
				File:  relFile,
				Line:  lineNum,
				Props: props,
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: filepath.Dir(relFile)},
				},
			})
			continue
		}

		// resources / resource.
		if m := resourcesRe.FindStringSubmatch(line); m != nil {
			resourceName := m[1]
			currentResource = resourceName
			resourcePath := prefix + "/" + resourceName

			actions := restfulActions(line)
			for _, action := range actions {
				method := action.method
				path := resourcePath + action.suffix

				props := map[string]any{
					"method":    method,
					"framework": "rails",
					"language":  "ruby",
					"resource":  resourceName,
					"action":    action.name,
				}

				result = append(result, facts.Fact{
					Kind:  facts.KindRoute,
					Name:  path,
					File:  relFile,
					Line:  lineNum,
					Props: props,
					Relations: []facts.Relation{
						{Kind: facts.RelDeclares, Target: filepath.Dir(relFile)},
					},
				})
			}

			// If there's a do block, push resource as a scope.
			if doBlockRe.MatchString(line) {
				scopeStack = append(scopeStack, routeScope{pathPrefix: "/" + resourceName})
				depth++
			}
			continue
		}

		// member do / collection do.
		if m := memberRe.FindStringSubmatch(line); m != nil {
			blockType := m[1]
			memberPrefix := ""
			if blockType == "member" && currentResource != "" {
				memberPrefix = "/:id"
			}
			scopeStack = append(scopeStack, routeScope{pathPrefix: memberPrefix})
			depth++
			continue
		}

		// Track other do blocks for depth.
		if doBlockRe.MatchString(line) {
			depth++
		}
	}

	return result
}

// buildPrefix constructs the current URL prefix from the scope stack.
func buildPrefix(stack []routeScope) string {
	var parts []string
	for _, s := range stack {
		if s.pathPrefix != "" {
			parts = append(parts, s.pathPrefix)
		}
	}
	return strings.Join(parts, "")
}

// restAction describes a single RESTful action.
type restAction struct {
	name   string
	method string
	suffix string
}

// restfulActions returns the set of REST actions for a resources declaration.
func restfulActions(line string) []restAction {
	all := []restAction{
		{name: "index", method: "GET", suffix: ""},
		{name: "create", method: "POST", suffix: ""},
		{name: "new", method: "GET", suffix: "/new"},
		{name: "show", method: "GET", suffix: "/:id"},
		{name: "update", method: "PATCH", suffix: "/:id"},
		{name: "edit", method: "GET", suffix: "/:id/edit"},
		{name: "destroy", method: "DELETE", suffix: "/:id"},
	}

	// Check for only: [...] filter.
	if m := onlyRe.FindStringSubmatch(line); m != nil {
		allowed := parseSymbolList(m[1])
		return filterActions(all, allowed, true)
	}

	// Check for except: [...] filter.
	if m := exceptRe.FindStringSubmatch(line); m != nil {
		excluded := parseSymbolList(m[1])
		return filterActions(all, excluded, false)
	}

	return all
}

// parseSymbolList extracts symbol names from a string like ":index, :show, :create".
func parseSymbolList(s string) map[string]bool {
	result := make(map[string]bool)
	for _, m := range symbolListRe.FindAllStringSubmatch(s, -1) {
		result[m[1]] = true
	}
	return result
}

// filterActions returns actions filtered by an allow or deny list.
func filterActions(all []restAction, names map[string]bool, isAllow bool) []restAction {
	var result []restAction
	for _, a := range all {
		if isAllow {
			if names[a.name] {
				result = append(result, a)
			}
		} else {
			if !names[a.name] {
				result = append(result, a)
			}
		}
	}
	return result
}
