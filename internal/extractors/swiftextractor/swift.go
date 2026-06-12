package swiftextractor

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// SwiftExtractor extracts architectural facts from Swift source code using
// tree-sitter AST parsing (see swift_ast.go for the walker implementation).
type SwiftExtractor struct{}

// New creates a new SwiftExtractor.
func New() *SwiftExtractor {
	return &SwiftExtractor{}
}

func (e *SwiftExtractor) Name() string {
	return "swift"
}

// Detect returns true if the repository looks like a Swift or iOS project.
func (e *SwiftExtractor) Detect(repoPath string) (bool, error) {
	// Check for Package.swift (Swift Package Manager)
	if _, err := os.Stat(filepath.Join(repoPath, "Package.swift")); err == nil {
		return true, nil
	}

	// Check up to two levels deep for .xcodeproj or .xcworkspace
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return false, nil
	}
	for _, entry := range entries {
		if matchesXcodeProject(entry.Name()) {
			return true, nil
		}
		if entry.IsDir() {
			subEntries, err := os.ReadDir(filepath.Join(repoPath, entry.Name()))
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if matchesXcodeProject(sub.Name()) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// Extract parses Swift files with tree-sitter and emits architectural facts.
//
// It uses a two-pass approach:
//   - Pass 1: walk each file's AST (extractFileAST) to emit declaration, import,
//     iOS-classification and call-graph facts, while building a type→module index.
//   - A canonicalisation step rewrites bare call/instantiate/inject/depends_on
//     edge targets to canonical "<dir>.<Type>" fact names using that index, so
//     the graph's reverse traversal (impact_analysis) finds Swift dependents.
//   - Pass 2: scan type references to discover cross-module import dependencies.
func (e *SwiftExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	isiOS := detectiOSProject(repoPath)

	modules := make(map[string]bool)
	typeIndex := make(map[string]string) // simple type name -> module (directory)
	dirToFile := make(map[string]string) // module dir -> a representative source file
	var swiftFiles []string
	var manifestFiles []string // Package.swift manifests, parsed after the walk

	// Pass 1: AST extraction + type index. Package.swift manifests are deferred so
	// that dirToFile is fully populated before the manifest parser resolves each
	// target's representative source file.
	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isSwiftFile(relFile) {
			continue
		}
		if filepath.Base(relFile) == "Package.swift" {
			manifestFiles = append(manifestFiles, relFile)
			continue
		}
		swiftFiles = append(swiftFiles, relFile)

		absFile := filepath.Join(repoPath, relFile)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[swift-extractor] error reading %s: %v", relFile, err)
			continue
		}

		fileFacts := extractFileAST(src, relFile, isiOS)
		allFacts = append(allFacts, fileFacts...)
		allFacts = append(allFacts, extractURLSessionFacts(src, relFile)...)

		dir := filepath.Dir(relFile)
		modules[dir] = true
		if _, ok := dirToFile[dir]; !ok {
			dirToFile[dir] = relFile
		}

		// Index declared types so edge targets and cross-module references resolve.
		for _, fact := range fileFacts {
			if fact.Kind != facts.KindSymbol {
				continue
			}
			sk, _ := fact.Props["symbol_kind"].(string)
			if sk == facts.SymbolStruct || sk == facts.SymbolClass || sk == facts.SymbolInterface {
				if simpleName := lastDotComponent(fact.Name); simpleName != "" {
					typeIndex[simpleName] = dir
				}
			}
		}
	}

	// Parse Package.swift manifests: emit SPM target module facts + the inter-target
	// dependency graph, and (by rerouting) keep the manifest's own `let package`
	// binding and `import PackageDescription` out of the symbol/dependency facts.
	manifestModules := make(map[string]bool)
	for _, relFile := range manifestFiles {
		absFile := filepath.Join(repoPath, relFile)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[swift-extractor] error reading %s: %v", relFile, err)
			continue
		}
		mf := parsePackageManifest(src, relFile, dirToFile)
		allFacts = append(allFacts, mf...)
		for _, f := range mf {
			if f.Kind == facts.KindModule {
				manifestModules[f.Name] = true
			}
		}
	}

	// Canonicalise bare edge targets to "<dir>.<Type>" so reverse traversal
	// (impact_analysis) connects dependents to their targets.
	canonicalizeTargets(allFacts, typeIndex)

	// Emit module facts for directories not already described by a manifest target.
	for dir := range modules {
		if manifestModules[dir] {
			continue
		}
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language": "swift",
			},
		})
	}

	// Pass 2: resolve type references to discover cross-module dependencies.
	type edge struct{ from, to string }
	seenEdges := make(map[edge]bool)

	for _, relFile := range swiftFiles {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		sourceModule := filepath.Dir(relFile)
		absFile := filepath.Join(repoPath, relFile)
		refs := extractTypeReferences(absFile)

		for _, typeName := range refs {
			targetModule, ok := typeIndex[typeName]
			if !ok || targetModule == sourceModule {
				continue
			}
			e := edge{sourceModule, targetModule}
			if seenEdges[e] {
				continue
			}
			seenEdges[e] = true

			allFacts = append(allFacts, facts.Fact{
				Kind: facts.KindDependency,
				Name: sourceModule + " -> " + targetModule,
				File: relFile,
				Props: map[string]any{
					"language": "swift",
					"internal": true,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: targetModule},
				},
			})
		}
	}

	return allFacts, nil
}

// canonicalizeTargets rewrites bare simple-name targets of call-graph relations
// to their canonical "<dir>.<Type>" fact names using the type index. Targets that
// already contain "." (resolved methods/functions) or that name an unknown
// (external) type are left unchanged.
func canonicalizeTargets(allFacts []facts.Fact, typeIndex map[string]string) {
	for i := range allFacts {
		for j := range allFacts[i].Relations {
			r := &allFacts[i].Relations[j]
			switch r.Kind {
			case facts.RelInstantiates, facts.RelInjects, facts.RelCalls, facts.RelDependsOn:
				if strings.Contains(r.Target, ".") {
					continue
				}
				if dir, ok := typeIndex[r.Target]; ok {
					r.Target = dir + "." + r.Target
				}
			}
		}
	}
}

// extractFile reads a Swift file and delegates to the tree-sitter walker. It
// preserves the legacy signature used by the test helper extractFromString.
func extractFile(f *os.File, relFile string, isiOS bool) []facts.Fact {
	src, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return extractFileAST(src, relFile, isiOS)
}

// importRe matches a Swift import statement and captures the module name. Used by
// the AST walker to render import dependency facts.
var importRe = regexp.MustCompile(`^\s*import\s+(\w+)`)

// typeRefRe matches type annotations like "name: TypeName" in property declarations and parameters.
var typeRefRe = regexp.MustCompile(`:\s*([A-Z][A-Za-z0-9_]+)`)

// extractTypeReferences scans a Swift file for type references (property types, parameter types).
func extractTypeReferences(absFile string) []string {
	f, err := os.Open(absFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	seen := make(map[string]bool)
	var refs []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip comments and blank lines.
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}

		matches := typeRefRe.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			typeName := m[1]
			// Skip common Swift/system types.
			if isSystemType(typeName) {
				continue
			}
			if !seen[typeName] {
				seen[typeName] = true
				refs = append(refs, typeName)
			}
		}
	}

	return refs
}

// isSystemType returns true for built-in Swift and framework types that should not be resolved.
func isSystemType(name string) bool {
	switch name {
	case "String", "Int", "Int8", "Int16", "Int32", "Int64",
		"UInt", "UInt8", "UInt16", "UInt32", "UInt64",
		"Float", "Double", "Bool", "Void", "Any", "AnyObject",
		"Data", "Date", "URL", "UUID", "Error",
		"Array", "Dictionary", "Set", "Optional",
		"Published", "State", "Binding", "ObservedObject", "StateObject", "EnvironmentObject", "Environment",
		"View", "App", "Scene", "Body",
		"Color", "Image", "Text", "Button", "NavigationView", "NavigationLink", "NavigationStack",
		"VStack", "HStack", "ZStack", "List", "ScrollView", "LazyVStack", "LazyHStack",
		"CGFloat", "CGPoint", "CGSize", "CGRect",
		"NSObject", "NSLock", "NSError",
		"URLRequest", "URLResponse", "HTTPURLResponse", "URLSession", "URLComponents", "URLQueryItem",
		"JSONDecoder", "JSONEncoder", "CodingKey", "CodingKeys",
		"AnyPublisher", "CurrentValueSubject", "PassthroughSubject", "AnyCancellable",
		"Task", "MainActor",
		"ObservableObject", "Identifiable", "Equatable", "Hashable", "Comparable",
		"Codable", "Decodable", "Encodable", "Sendable",
		"LocalizedError", "CustomStringConvertible",
		"Never":
		return true
	}
	return false
}

// lastDotComponent returns the part after the last "." in a name.
func lastDotComponent(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// extractSupertypesFromText finds the supertype clause after ":" in text that may
// contain generic parameters. It skips content inside balanced parentheses and angle brackets.
//
// It is retained for direct unit testing of the supertype-clause parsing logic;
// the AST walker reads supertypes structurally from inheritance_specifier nodes.
func extractSupertypesFromText(text string) string {
	depth := 0
	for i, ch := range text {
		switch ch {
		case '(', '<':
			depth++
		case ')', '>':
			depth--
		case ':':
			if depth <= 0 {
				rest := text[i+1:]
				if braceIdx := strings.Index(rest, "{"); braceIdx >= 0 {
					rest = rest[:braceIdx]
				}
				// Stop at "where" clause.
				if whereIdx := strings.Index(rest, " where "); whereIdx >= 0 {
					rest = rest[:whereIdx]
				}
				return strings.TrimSpace(rest)
			}
		}
	}
	return ""
}

// --- iOS detection helpers ---

// detectiOSProject checks for Info.plist or other iOS markers.
func detectiOSProject(repoPath string) bool {
	// Walk up to two levels looking for Info.plist or Assets.xcassets
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "Info.plist" {
			return true
		}
		if entry.IsDir() {
			subEntries, _ := os.ReadDir(filepath.Join(repoPath, entry.Name()))
			for _, sub := range subEntries {
				if sub.Name() == "Info.plist" || sub.Name() == "Assets.xcassets" {
					return true
				}
				if sub.IsDir() {
					deepEntries, _ := os.ReadDir(filepath.Join(repoPath, entry.Name(), sub.Name()))
					for _, deep := range deepEntries {
						if deep.Name() == "Info.plist" || deep.Name() == "Assets.xcassets" {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// addIOSProps classifies a declaration as an iOS component.
func addIOSProps(f *facts.Fact, name string, annotations []string, supertypes string) {
	// SwiftUI App entry point.
	if containsAnnotation(annotations, "main") && supertypeMatches(supertypes, "App") {
		f.Props["ios_component"] = "swiftui_app"
		f.Props["framework"] = "swiftui"
		return
	}

	// SwiftUI Views.
	if supertypeMatches(supertypes, "View") {
		f.Props["ios_component"] = "swiftui_view"
		f.Props["framework"] = "swiftui"
		return
	}

	// SwiftUI Scene.
	if supertypeMatches(supertypes, "Scene") {
		f.Props["ios_component"] = "swiftui_scene"
		f.Props["framework"] = "swiftui"
		return
	}

	// Combine ViewModels (ObservableObject conformance).
	if supertypeMatches(supertypes, "ObservableObject") {
		f.Props["ios_component"] = "viewmodel"
		f.Props["framework"] = "combine"
		return
	}

	// Swift 5.9+ Observable ViewModels.
	if containsAnnotation(annotations, "Observable") {
		f.Props["ios_component"] = "viewmodel"
		f.Props["framework"] = "observation"
		return
	}

	// UIKit ViewControllers.
	if supertypeMatches(supertypes, "UIViewController", "UITableViewController",
		"UICollectionViewController", "UINavigationController", "UITabBarController",
		"UIPageViewController") {
		f.Props["ios_component"] = "viewcontroller"
		f.Props["framework"] = "uikit"
		return
	}

	// UIKit Views.
	if supertypeMatches(supertypes, "UIView", "UITableViewCell", "UICollectionViewCell",
		"UIStackView", "UIScrollView") {
		f.Props["ios_component"] = "uiview"
		f.Props["framework"] = "uikit"
		return
	}

	// NSObject subclasses acting as delegates.
	if supertypeMatches(supertypes, "NSObject") {
		f.Props["framework"] = "foundation"
	}

	// Name-based architectural classification.
	if strings.HasSuffix(name, "ViewModel") {
		f.Props["ios_component"] = "viewmodel"
		return
	}
	if strings.HasSuffix(name, "Repository") || strings.HasSuffix(name, "RepositoryImpl") {
		f.Props["ios_component"] = "repository"
		return
	}
	if strings.HasSuffix(name, "UseCase") {
		f.Props["ios_component"] = "usecase"
		return
	}
	if strings.HasSuffix(name, "Coordinator") {
		f.Props["ios_component"] = "coordinator"
		return
	}
	if strings.HasSuffix(name, "APIService") || (strings.HasSuffix(name, "Service") && !strings.HasSuffix(name, "ServiceInterface")) {
		f.Props["ios_component"] = "service"
		return
	}
	if name == "DIContainer" || strings.HasSuffix(name, "Container") {
		f.Props["ios_component"] = "di_container"
		return
	}
}

// --- Parsing helpers ---

// parseSupertypes splits a supertype clause like "Foo, Bar, Baz<T>" into type names.
func parseSupertypes(clause string) []string {
	var result []string
	depth := 0
	start := 0
	for i, ch := range clause {
		switch ch {
		case '<', '(':
			depth++
		case '>', ')':
			depth--
		case ',':
			if depth == 0 {
				if t := extractTypeName(clause[start:i]); t != "" {
					result = append(result, t)
				}
				start = i + 1
			}
		}
	}
	if t := extractTypeName(clause[start:]); t != "" {
		result = append(result, t)
	}
	return result
}

// extractTypeName extracts the simple type name from a supertype entry like "Foo" or "Bar<T>".
func extractTypeName(s string) string {
	s = strings.TrimSpace(s)
	for i, ch := range s {
		if ch == '<' || ch == '(' || ch == ' ' {
			s = s[:i]
			break
		}
	}
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		s = s[idx+1:]
	}
	if s == "" {
		return ""
	}
	return s
}

func containsAnnotation(annotations []string, name string) bool {
	for _, a := range annotations {
		if a == name {
			return true
		}
	}
	return false
}

func supertypeMatches(supertypes string, names ...string) bool {
	if supertypes == "" {
		return false
	}
	parsed := parseSupertypes(supertypes)
	for _, st := range parsed {
		for _, name := range names {
			if st == name {
				return true
			}
		}
	}
	return false
}

// privateRe / privateSetRe support isPrivateAccess.
var (
	privateRe    = regexp.MustCompile(`\b(private|fileprivate)\b`)
	privateSetRe = regexp.MustCompile(`\b(private|fileprivate)\s*\(set\)`)
)

// isPrivateAccess returns true if the text contains private or fileprivate access control
// that is NOT the private(set) pattern (which only restricts the setter, keeping the getter public).
func isPrivateAccess(text string) bool {
	if !privateRe.MatchString(text) {
		return false
	}
	// Remove all private(set) / fileprivate(set) occurrences and re-check.
	cleaned := privateSetRe.ReplaceAllString(text, "")
	return privateRe.MatchString(cleaned)
}

func isSwiftFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".swift")
}

func matchesXcodeProject(name string) bool {
	return strings.HasSuffix(name, ".xcodeproj") || strings.HasSuffix(name, ".xcworkspace")
}
