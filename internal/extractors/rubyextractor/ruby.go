package rubyextractor

import (
	"bufio"
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dejo1307/enola/internal/facts"
)

// RubyExtractor extracts architectural facts from Ruby source code using line-based regex parsing.
type RubyExtractor struct{}

// New creates a new RubyExtractor.
func New() *RubyExtractor {
	return &RubyExtractor{}
}

func (e *RubyExtractor) Name() string {
	return "ruby"
}

// Detect returns true if the repository looks like a Ruby project (has a Gemfile).
func (e *RubyExtractor) Detect(repoPath string) (bool, error) {
	if _, err := os.Stat(filepath.Join(repoPath, "Gemfile")); err == nil {
		return true, nil
	}
	return false, nil
}

// Extract parses Ruby files and emits architectural facts.
func (e *RubyExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	isRails := detectRailsProject(repoPath)

	// Pass 1: parse packwerk packages (builds package map and privacy boundaries).
	pkgInfo := parsePackwerk(repoPath)
	allFacts = append(allFacts, pkgInfo.facts...)

	// Track directories that contain Ruby files for module emission.
	modules := make(map[string]bool)

	// Pass 2: parse .rb files.
	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isRubyFile(relFile) {
			continue
		}

		// Skip route files -- they're parsed separately by the route extractor.
		if isRails && isRouteFile(relFile) {
			continue
		}

		absFile := filepath.Join(repoPath, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			log.Printf("[ruby-extractor] error reading %s: %v", relFile, err)
			continue
		}

		exported := isPublicAPI(relFile, pkgInfo)
		fileFacts := extractFile(f, relFile, isRails, exported)
		f.Close()

		// Collect storage facts from ActiveRecord patterns found during file parsing.
		storageFacts := extractStorageFacts(relFile, fileFacts)
		allFacts = append(allFacts, fileFacts...)
		allFacts = append(allFacts, storageFacts...)

		// Re-read the file to extract association details if models were found.
		if len(storageFacts) > 0 {
			assocFacts := extractAssociationsFromFile(filepath.Join(repoPath, relFile), relFile)
			allFacts = append(allFacts, assocFacts...)
		}

		dir := filepath.Dir(relFile)
		modules[dir] = true
	}

	// Emit module facts for directories not already covered by packwerk packages.
	for dir := range modules {
		if pkgInfo.isPackage(dir) {
			continue
		}
		props := map[string]any{
			"language": "ruby",
		}
		if isRails {
			props["framework"] = "rails"
		}
		allFacts = append(allFacts, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}

	// Parse Rails route files.
	if isRails {
		routeFacts := extractAllRoutes(repoPath, files)
		allFacts = append(allFacts, routeFacts...)
	}

	return allFacts, nil
}

// --- Rails detection ---

func detectRailsProject(repoPath string) bool {
	candidates := []string{
		filepath.Join(repoPath, "config", "application.rb"),
		filepath.Join(repoPath, "bin", "rails"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// --- Regex patterns ---

var (
	moduleRe       = regexp.MustCompile(`^\s*module\s+([\w:]+)`)
	eigenclassRe   = regexp.MustCompile(`^\s*class\s*<<\s*\w`)
	classOneLineRe = regexp.MustCompile(`^\s*class\s+([\w:]+)(?:\s*<\s*([\w:]+))?`)
	defRe          = regexp.MustCompile(`^\s*def\s+(self\.)?([\w?!=]+)`)
	requireRe      = regexp.MustCompile(`^\s*require\s+['"]([^'"]+)['"]`)
	requireRelRe   = regexp.MustCompile(`^\s*require_relative\s+['"]([^'"]+)['"]`)
	includeRe      = regexp.MustCompile(`^\s*(?:include|extend|prepend)\s+([\w:]+)`)
	mixinKindRe    = regexp.MustCompile(`^\s*(include|extend|prepend)\s+`)
	constantRe     = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]+)\s*=\s*`)
	attrRe         = regexp.MustCompile(`^\s*attr_(reader|writer|accessor)\s+(.+)`)
	symbolListRe   = regexp.MustCompile(`:(\w+)`)
	concernRe      = regexp.MustCompile(`^\s*extend\s+ActiveSupport::Concern`)
	visibilityRe   = regexp.MustCompile(`^\s*(private|protected|public)\s*$`)
	moduleFuncRe   = regexp.MustCompile(`^\s*module_function\s*$`)
	inlineEndRe    = regexp.MustCompile(`;\s*end\s*$`)
	heredocOpenRe  = regexp.MustCompile(`<<[~-]?\s*['"]?([A-Z_]+)['"]?`)
	endRe          = regexp.MustCompile(`^\s*end\b`)
	blockOpenerRe  = regexp.MustCompile(
		`(?:^\s*(?:if|unless|case|while|until|for|begin)\b)|` +
			`\bdo\s*(?:\|[^|]*\|)?\s*$`)

	// openAPISpecPathRe matches openapi_spec_path declarations in Rails API controllers.
	// Example: openapi_spec_path 'packages/items/api/openapi/api.yml'
	openAPISpecPathRe = regexp.MustCompile(`^\s*openapi_spec_path\s+['"]([^'"]+)['"]`)

	// qualifiedCallRe matches calls where the receiver is a constant/class name
	// (PascalCase or Namespace::Class). No trailing char required because Ruby
	// method calls are valid without parentheses: Config.load_defaults
	qualifiedCallRe = regexp.MustCompile(`\b([A-Z]\w*(?:::[A-Z]\w*)*)\.([\w?!=]+)`)
	// receiverCallRe matches calls on lowercase receivers with explicit parens: object.method(
	// Parens are required here to reduce noise from attribute reads.
	receiverCallRe = regexp.MustCompile(`\b([a-z_]\w*)\.([\w?!=]+)\s*\(`)
	// endlessMethodRe detects Ruby 3.0+ endless method syntax:
	//   def name = expr         (no params)
	//   def name(args) = expr   (with params)
	// We require whitespace after = to distinguish from setter defs (def foo=(v))
	// and from == comparisons. RE2 has no lookahead, so we use \s as the guard.
	endlessMethodRe = regexp.MustCompile(`\)\s*=\s|\bdef\s+(?:self\.)?[\w?!]+\s*=\s`)
)

// scopeEntry tracks a class/module nesting level.
type scopeEntry struct {
	name  string
	kind  string // "class", "module", or "eigenclass"
	depth int
}

// methodEntry tracks an active method body for call accumulation.
type methodEntry struct {
	name       string
	startDepth int
}

// extractFile parses a single Ruby file and returns facts.
func extractFile(f *os.File, relFile string, isRails bool, exportedByPackwerk bool) []facts.Fact {
	var result []facts.Fact
	dir := filepath.Dir(relFile)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var (
		lineNum        int
		depth          int
		scopeStack     []scopeEntry
		methodStack    []methodEntry
		visibility     = "public"
		isConcern      bool
		moduleFunction bool
		heredocEnd     string // non-empty when inside a heredoc
	)
	callAccum := make(map[string][]string)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip blank lines and comments.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Heredoc state: skip lines until terminator.
		if heredocEnd != "" {
			if trimmed == heredocEnd || strings.TrimSpace(trimmed) == heredocEnd {
				heredocEnd = ""
			}
			continue
		}

		// Check for heredoc opener on this line (process line normally first, then enter heredoc mode).
		lineHasHeredoc := false
		var heredocTerminator string
		if m := heredocOpenRe.FindStringSubmatch(line); m != nil {
			// Make sure it's actually a heredoc, not a left-shift operator.
			// Heredocs use identifiers like SQL, TEXT, HEREDOC, JSON, MESSAGE, etc.
			heredocTerminator = m[1]
			if len(heredocTerminator) >= 2 {
				lineHasHeredoc = true
			}
		}

		// Track end keywords to manage depth.
		if trimmed == "end" {
			depth--
			if depth < 0 {
				depth = 0
			}
			if len(scopeStack) > 0 && scopeStack[len(scopeStack)-1].depth == depth {
				popped := scopeStack[len(scopeStack)-1]
				scopeStack = scopeStack[:len(scopeStack)-1]
				// Reset visibility when leaving a class/module scope.
				if popped.kind != "eigenclass" {
					visibility = "public"
					moduleFunction = false
				}
			}
			// Pop method stack when the end closes a method body.
			if len(methodStack) > 0 && methodStack[len(methodStack)-1].startDepth == depth {
				methodStack = methodStack[:len(methodStack)-1]
			}
			continue
		}

		// Detect ActiveSupport::Concern.
		if concernRe.MatchString(line) {
			isConcern = true
		}

		// Visibility section markers (bare private/protected/public on its own line).
		if visibilityRe.MatchString(line) {
			m := visibilityRe.FindStringSubmatch(line)
			visibility = m[1]
			continue
		}

		// module_function -- subsequent defs become class methods.
		if moduleFuncRe.MatchString(line) {
			moduleFunction = true
			continue
		}

		// Module declarations.
		if m := moduleRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			qualName := qualifiedName(scopeStack, name)

			props := map[string]any{
				"symbol_kind": facts.SymbolInterface,
				"exported":    exportedByPackwerk,
				"language":    "ruby",
			}
			if isConcern {
				props["concern"] = true
				isConcern = false
			}
			if isRails {
				props["framework"] = "rails"
			}

			result = append(result, facts.Fact{
				Kind:  facts.KindSymbol,
				Name:  qualName,
				File:  relFile,
				Line:  lineNum,
				Props: props,
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			})

			scopeStack = append(scopeStack, scopeEntry{name: name, kind: "module", depth: depth})
			depth++
			continue
		}

		// Eigenclass: class << self (must be checked before class declarations).
		if eigenclassRe.MatchString(line) {
			scopeStack = append(scopeStack, scopeEntry{name: "", kind: "eigenclass", depth: depth})
			depth++
			continue
		}

		// Class declarations.
		if m := classOneLineRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			superclass := m[2]
			qualName := qualifiedName(scopeStack, name)
			isInline := inlineEndRe.MatchString(line)

			exported := visibility == "public" && exportedByPackwerk

			props := map[string]any{
				"symbol_kind": facts.SymbolClass,
				"exported":    exported,
				"language":    "ruby",
			}
			if isRails {
				props["framework"] = "rails"
			}
			if superclass != "" {
				props["superclass"] = superclass
			}

			rels := []facts.Relation{
				{Kind: facts.RelDeclares, Target: dir},
			}
			if superclass != "" {
				rels = append(rels, facts.Relation{
					Kind:   facts.RelImplements,
					Target: superclass,
				})
			}

			result = append(result, facts.Fact{
				Kind:      facts.KindSymbol,
				Name:      qualName,
				File:      relFile,
				Line:      lineNum,
				Props:     props,
				Relations: rels,
			})

			// Only push to scope stack if this is NOT an inline class (class Foo < Bar; end).
			if !isInline {
				scopeStack = append(scopeStack, scopeEntry{name: name, kind: "class", depth: depth})
				depth++
				visibility = "public"
			}

			if lineHasHeredoc {
				heredocEnd = heredocTerminator
			}
			continue
		}

		// Method definitions.
		if m := defRe.FindStringSubmatch(line); m != nil {
			isSelf := m[1] == "self."
			methodName := m[2]
			isInline := inlineEndRe.MatchString(line)
			// Endless method: def name = expr  or  def name(args) = expr
			isEndless := !isInline && endlessMethodRe.MatchString(line)

			// module_function makes subsequent defs into class methods.
			if moduleFunction {
				isSelf = true
			}

			scopeName := qualifiedName(scopeStack, "")
			var fullName string
			if scopeName != "" {
				if isSelf {
					fullName = scopeName + "." + methodName
				} else {
					fullName = scopeName + "#" + methodName
				}
			} else {
				fullName = dir + "." + methodName
			}

			symbolKind := facts.SymbolMethod
			if isSelf {
				symbolKind = facts.SymbolFunc
			}

			exported := visibility == "public" && exportedByPackwerk

			props := map[string]any{
				"symbol_kind": symbolKind,
				"exported":    exported,
				"language":    "ruby",
			}
			if isRails {
				props["framework"] = "rails"
			}

			// For endless methods, extract calls from the expression on this line.
			var defLineCalls []string
			if isEndless {
				defLineCalls = extractRubyCalls(line)
			}

			rels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
			seen := make(map[string]bool)
			for _, callee := range defLineCalls {
				if !seen[callee] {
					seen[callee] = true
					rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: callee})
				}
			}

			result = append(result, facts.Fact{
				Kind:      facts.KindSymbol,
				Name:      fullName,
				File:      relFile,
				Line:      lineNum,
				Props:     props,
				Relations: rels,
			})

			// Endless and inline one-liners have no body — don't push to stack or increment depth.
			if !isInline && !isEndless {
				methodStack = append(methodStack, methodEntry{name: fullName, startDepth: depth})
				depth++
			}

			if lineHasHeredoc {
				heredocEnd = heredocTerminator
			}
			continue
		}

		// Require / require_relative.
		if m := requireRe.FindStringSubmatch(line); m != nil {
			importPath := m[1]
			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: dir + " -> " + importPath,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"language": "ruby",
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: importPath},
				},
			})
			continue
		}
		if m := requireRelRe.FindStringSubmatch(line); m != nil {
			importPath := m[1]
			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: dir + " -> " + importPath,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"language":         "ruby",
					"require_relative": true,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: importPath},
				},
			})
			continue
		}

		// Include / extend / prepend.
		if m := includeRe.FindStringSubmatch(line); m != nil {
			mixinName := m[1]
			kindM := mixinKindRe.FindStringSubmatch(line)
			mixinKind := "include"
			if kindM != nil {
				mixinKind = kindM[1]
			}

			scopeName := qualifiedName(scopeStack, "")
			if scopeName == "" {
				scopeName = dir
			}

			// Don't duplicate the ActiveSupport::Concern detection as a mixin.
			if mixinName == "ActiveSupport::Concern" {
				continue
			}

			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: scopeName + " -> " + mixinName,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"language":   "ruby",
					"mixin_kind": mixinKind,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImplements, Target: mixinName},
				},
			})
			continue
		}

		// openapi_spec_path 'path/to/spec.yml' — links a controller to its OpenAPI spec.
		if m := openAPISpecPathRe.FindStringSubmatch(line); m != nil {
			specFile := m[1]
			scopeName := qualifiedName(scopeStack, "")
			if scopeName == "" {
				scopeName = dir
			}
			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: scopeName + " -> " + specFile,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"language":  "ruby",
					"type":      "openapi_spec",
					"spec_file": specFile,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDependsOn, Target: specFile},
				},
			})
			continue
		}

		// Constants (ALL_CAPS = ...).
		if m := constantRe.FindStringSubmatch(line); m != nil {
			constName := m[1]
			scopeName := qualifiedName(scopeStack, "")
			var fullName string
			if scopeName != "" {
				fullName = scopeName + "::" + constName
			} else {
				fullName = dir + "." + constName
			}

			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: fullName,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"symbol_kind": facts.SymbolConstant,
					"exported":    visibility == "public" && exportedByPackwerk,
					"language":    "ruby",
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			})

			if lineHasHeredoc {
				heredocEnd = heredocTerminator
			}
			continue
		}

		// attr_reader / attr_writer / attr_accessor.
		if m := attrRe.FindStringSubmatch(line); m != nil {
			attrKind := m[1]
			symbolsStr := m[2]
			symbols := symbolListRe.FindAllStringSubmatch(symbolsStr, -1)

			scopeName := qualifiedName(scopeStack, "")
			if scopeName == "" {
				scopeName = dir
			}

			for _, sym := range symbols {
				attrName := sym[1]
				result = append(result, facts.Fact{
					Kind: facts.KindSymbol,
					Name: scopeName + "#" + attrName,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"symbol_kind": facts.SymbolVariable,
						"exported":    visibility == "public" && exportedByPackwerk,
						"language":    "ruby",
						"attr_kind":   attrKind,
					},
					Relations: []facts.Relation{
						{Kind: facts.RelDeclares, Target: dir},
					},
				})
			}
			continue
		}

		// Accumulate method calls for any line inside an active method body.
		if len(methodStack) > 0 {
			mName := methodStack[len(methodStack)-1].name
			callAccum[mName] = append(callAccum[mName], extractRubyCalls(line)...)
		}

		// Track depth for other block openers (if/unless/case/while/do etc.).
		// Note: module/class/def are already handled above and won't reach here.
		if blockOpenerRe.MatchString(line) {
			depth++
		}

		// Enter heredoc mode after processing this line.
		if lineHasHeredoc {
			heredocEnd = heredocTerminator
		}
	}

	// Attach accumulated RelCalls edges to each method/function fact.
	seen := make(map[string]map[string]bool)
	for i, f := range result {
		sk, _ := f.Props["symbol_kind"].(string)
		if f.Kind != facts.KindSymbol ||
			(sk != facts.SymbolMethod && sk != facts.SymbolFunc) {
			continue
		}
		calls, ok := callAccum[f.Name]
		if !ok {
			continue
		}
		if seen[f.Name] == nil {
			seen[f.Name] = make(map[string]bool)
		}
		for _, callee := range calls {
			if seen[f.Name][callee] {
				continue
			}
			seen[f.Name][callee] = true
			result[i].Relations = append(result[i].Relations,
				facts.Relation{Kind: facts.RelCalls, Target: callee})
		}
	}

	return result
}

// qualifiedName builds a fully-qualified Ruby name from the scope stack.
func qualifiedName(stack []scopeEntry, name string) string {
	var parts []string
	for _, entry := range stack {
		// Skip eigenclass entries -- they don't contribute to the qualified name.
		if entry.kind == "eigenclass" || entry.name == "" {
			continue
		}
		parts = append(parts, entry.name)
	}
	if name != "" {
		parts = append(parts, name)
	}
	return strings.Join(parts, "::")
}

// extractRubyCalls returns callee names found on a single source line.
// It detects two tiers:
//   - Qualified (high-confidence): ConstantName.method or Ns::Class.method
//   - Receiver (medium-confidence): variable.method(
func extractRubyCalls(line string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range qualifiedCallRe.FindAllStringSubmatch(line, -1) {
		add(m[1] + "." + m[2])
	}
	for _, m := range receiverCallRe.FindAllStringSubmatch(line, -1) {
		add(m[1] + "." + m[2])
	}
	return out
}

// isRubyFile returns true if the file has a .rb extension.
func isRubyFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".rb")
}

// isPublicAPI checks if a file is within a packwerk package's app/public/ directory.
func isPublicAPI(relFile string, pkg *packwerkInfo) bool {
	if pkg == nil || len(pkg.packages) == 0 {
		return true
	}

	ownerPkg := pkg.ownerPackage(relFile)
	if ownerPkg == "" {
		return true
	}

	pkgCfg, ok := pkg.packages[ownerPkg]
	if !ok || !pkgCfg.enforcePrivacy {
		return true
	}

	publicDir := filepath.Join(ownerPkg, "app", "public")
	return strings.HasPrefix(relFile, publicDir+"/") || strings.HasPrefix(relFile, publicDir+"\\")
}

// extractAssociationsFromFile re-reads a file to extract ActiveRecord associations and scopes.
func extractAssociationsFromFile(absPath string, relFile string) []facts.Fact {
	f, err := os.Open(absPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// Find model classes in the file to determine context.
	modelClasses := make(map[string]bool)
	for _, line := range lines {
		if m := classOneLineRe.FindStringSubmatch(line); m != nil {
			superclass := m[2]
			if isARBaseClass(superclass) {
				modelClasses[m[1]] = true
			}
		}
	}

	if len(modelClasses) == 0 {
		return nil
	}

	var result []facts.Fact
	for lineNum, line := range lines {
		// Association declarations.
		if m := associationRe.FindStringSubmatch(line); m != nil {
			assocKind := m[1]
			assocName := m[2]

			targetModel := assocName
			if assocKind == "has_many" || assocKind == "has_and_belongs_to_many" {
				targetModel = singularize(assocName)
			}
			targetModel = snakeToCamel(targetModel)

			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: relFile + ":" + assocKind + " :" + assocName,
				File: relFile,
				Line: lineNum + 1,
				Props: map[string]any{
					"language":         "ruby",
					"association_kind": assocKind,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDependsOn, Target: targetModel},
				},
			})
		}

		// Scope declarations on models.
		if m := scopeRe.FindStringSubmatch(line); m != nil {
			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: "scope:" + m[1],
				File: relFile,
				Line: lineNum + 1,
				Props: map[string]any{
					"symbol_kind": facts.SymbolFunc,
					"language":    "ruby",
					"scope":       true,
				},
			})
		}

		// Explicit table name: self.table_name = 'foo'
		if m := tableNameRe.FindStringSubmatch(line); m != nil {
			result = append(result, facts.Fact{
				Kind: facts.KindStorage,
				Name: m[1],
				File: relFile,
				Line: lineNum + 1,
				Props: map[string]any{
					"storage_kind": "table",
					"language":     "ruby",
					"framework":    "rails",
					"explicit":     true,
				},
			})
		}
	}

	return result
}
