package swiftextractor

import (
	"bufio"
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// SwiftExtractor extracts architectural facts from Swift source code using line-based regex parsing.
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

// Extract parses Swift files and emits architectural facts.
// It uses a two-pass approach:
//   - Pass 1: extract declarations and build a type→module index
//   - Pass 2: resolve type references to discover cross-module dependencies
func (e *SwiftExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	isiOS := detectiOSProject(repoPath)

	modules := make(map[string]bool)
	typeIndex := make(map[string]string) // typeName -> module (directory)
	var swiftFiles []string

	// Pass 1: extract declarations and build type index.
	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isSwiftFile(relFile) {
			continue
		}

		swiftFiles = append(swiftFiles, relFile)

		absFile := filepath.Join(repoPath, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			log.Printf("[swift-extractor] error reading %s: %v", relFile, err)
			continue
		}

		fileFacts := extractFile(f, relFile, isiOS)
		f.Close()
		allFacts = append(allFacts, fileFacts...)

		dir := filepath.Dir(relFile)
		modules[dir] = true

		// Index declared types for pass 2.
		for _, fact := range fileFacts {
			if fact.Kind == facts.KindSymbol {
				sk, _ := fact.Props["symbol_kind"].(string)
				if sk == facts.SymbolStruct || sk == facts.SymbolClass || sk == facts.SymbolInterface || sk == "extension" {
					simpleName := lastDotComponent(fact.Name)
					if simpleName != "" {
						typeIndex[simpleName] = dir
					}
				}
			}
		}
	}

	// Post-process: emit View→ViewModel depends_on relations.
	// Scan SwiftUI View signatures for @StateObject/@ObservedObject/@EnvironmentObject references.
	viewModelDepRe := regexp.MustCompile(`@(?:StateObject|ObservedObject|EnvironmentObject)\s+(?:var|let)\s+\w+\s*:\s*(\w+)`)
	for i := range allFacts {
		comp, _ := allFacts[i].Props["ios_component"].(string)
		if comp != "swiftui_view" {
			continue
		}
		sig, _ := allFacts[i].Props["signature"].(string)
		if sig == "" {
			continue
		}
		matches := viewModelDepRe.FindAllStringSubmatch(sig, -1)
		for _, m := range matches {
			vmType := m[1]
			if targetModule, ok := typeIndex[vmType]; ok {
				allFacts[i].Relations = append(allFacts[i].Relations, facts.Relation{
					Kind:   facts.RelDependsOn,
					Target: targetModule + "." + vmType,
				})
			}
		}
	}

	// Emit module facts.
	for dir := range modules {
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

// --- Regex patterns ---

var (
	importRe    = regexp.MustCompile(`^\s*import\s+(\w+)`)
	attributeRe = regexp.MustCompile(`^\s*@(\w+)`)

	// Protocol declarations.
	protocolRe = regexp.MustCompile(
		`^\s*((?:(?:public|private|fileprivate|internal|open)\s+)*)` +
			`protocol\s+(\w+)`)

	// Struct declarations.
	structRe = regexp.MustCompile(
		`^\s*((?:(?:@\w+\s+)*(?:public|private|fileprivate|internal|open)\s+)*)` +
			`struct\s+(\w+)`)

	// Class declarations (handles @MainActor final class, etc.).
	classRe = regexp.MustCompile(
		`^\s*((?:(?:@\w+\s+)*(?:public|private|fileprivate|internal|open|final)\s+)*)` +
			`class\s+(\w+)`)

	// Enum declarations.
	enumRe = regexp.MustCompile(
		`^\s*((?:(?:public|private|fileprivate|internal|open|indirect)\s+)*)` +
			`enum\s+(\w+)`)

	// Actor declarations.
	actorRe = regexp.MustCompile(
		`^\s*((?:(?:@\w+\s+)*(?:public|private|fileprivate|internal|open)\s+)*)` +
			`actor\s+(\w+)`)

	// Extension declarations.
	extensionRe = regexp.MustCompile(`^\s*extension\s+(\w+)`)

	// Function declarations.
	funcRe = regexp.MustCompile(
		`^\s*(?:(?:public|private|fileprivate|internal|open|override|static|class|mutating|nonmutating|@\w+\s+)*\s*)` +
			`func\s+(\w+)\s*[(<]`)

	// Property declarations (let/var).
	propRe = regexp.MustCompile(
		`^\s*(?:(?:public|private|fileprivate|internal|open|static|class|override|lazy|weak|unowned|@\w+\s+)*\s*)` +
			`(let|var)\s+(\w+)`)

	// Typealias declarations.
	typealiasRe = regexp.MustCompile(
		`^\s*(?:(?:public|private|fileprivate|internal)\s+)*` +
			`typealias\s+(\w+)`)

	// Visibility check — private or fileprivate means not exported.
	// private(set) is handled separately in isPrivateAccess to keep the property exported.
	privateRe    = regexp.MustCompile(`\b(private|fileprivate)\b`)
	privateSetRe = regexp.MustCompile(`\b(private|fileprivate)\s*\(set\)`)
)

// pendingDecl tracks a declaration that spans multiple lines (e.g., multi-line where clause or generics).
type pendingDecl struct {
	declType    string // "struct", "class", "enum", "protocol", "actor"
	modifiers   string
	name        string
	line        int
	annotations []string
	parenDepth  int    // tracks unclosed parentheses
	angleDepth  int    // tracks unclosed angle brackets
	lines       string // accumulated text after the name
}

// extractFile parses a single Swift file and returns facts.
func extractFile(f *os.File, relFile string, isiOS bool) []facts.Fact {
	var result []facts.Fact
	dir := filepath.Dir(relFile)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var (
		lineNum            int
		braceDepth         int
		pendingAnnotations []string
		pending            *pendingDecl
		// Signature capture: collect member declarations for the current top-level type.
		sigCapture    bool
		sigTypeIdx    int
		sigMembers    []string
		sigPublished  []string // tracks @Published property names
	)
	const sigMaxMembers = 15

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Track brace depth for top-level detection.
		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")

		// Finalize signature when we exit the type body.
		if sigCapture && braceDepth == 0 {
			if sigTypeIdx < len(result) {
				if len(sigMembers) > 0 {
					result[sigTypeIdx].Props["signature"] = strings.Join(sigMembers, "\n")
				}
				if len(sigPublished) > 0 {
					result[sigTypeIdx].Props["reactive"] = true
					result[sigTypeIdx].Props["published_properties"] = strings.Join(sigPublished, ",")
				}
			}
			sigCapture = false
			sigMembers = nil
			sigPublished = nil
		}

		// Capture member declarations inside a top-level type body.
		if sigCapture && braceDepth >= 1 {
			memberEffective := braceDepth - strings.Count(line, "{")
			if memberEffective == 1 && len(sigMembers) < sigMaxMembers {
				trimmed := strings.TrimSpace(line)
				if trimmed != "" && !strings.HasPrefix(trimmed, "//") &&
					!strings.HasPrefix(trimmed, "/*") && !strings.HasPrefix(trimmed, "*") {
					if propRe.MatchString(line) || funcRe.MatchString(line) {
						sig := trimmed
						if idx := strings.Index(sig, "{"); idx > 0 {
							sig = strings.TrimSpace(sig[:idx])
						}
						sigMembers = append(sigMembers, sig)
						// Track @Published properties for reactive detection.
						if strings.Contains(trimmed, "@Published") {
							if pm := propRe.FindStringSubmatch(line); pm != nil {
								sigPublished = append(sigPublished, pm[2])
							}
						}
					}
				}
			}
		}

		// If we have a pending multi-line declaration, accumulate lines.
		if pending != nil {
			pending.parenDepth += strings.Count(line, "(") - strings.Count(line, ")")
			pending.angleDepth += strings.Count(line, "<") - strings.Count(line, ">")
			pending.lines += " " + strings.TrimSpace(line)

			// Once balanced and we see { or end of declaration, emit the fact.
			if (pending.parenDepth <= 0 && pending.angleDepth <= 0) || strings.Contains(line, "{") {
				supertypes := extractSupertypesFromText(pending.lines)
				fact := buildDeclFact(dir, relFile, pending, supertypes, isiOS)
				result = append(result, fact)
				sigCapture = true
				sigTypeIdx = len(result) - 1
				sigMembers = nil
				sigPublished = nil
				pending = nil
			}
			continue
		}

		// Collect attributes on their own line (apply to the next declaration).
		if m := attributeRe.FindStringSubmatch(line); m != nil {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "@") && !isDeclarationLine(line) {
				pendingAnnotations = append(pendingAnnotations, m[1])
				continue
			}
		}

		effectiveDepth := braceDepth - strings.Count(line, "{")

		inlineAnnotations := collectInlineAnnotations(line)
		allAnnotations := append(pendingAnnotations, inlineAnnotations...)

		if effectiveDepth == 0 {
			// Import statements.
			if m := importRe.FindStringSubmatch(line); m != nil {
				importName := m[1]
				result = append(result, facts.Fact{
					Kind: facts.KindDependency,
					Name: dir + " -> " + importName,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"language": "swift",
					},
					Relations: []facts.Relation{
						{Kind: facts.RelImports, Target: importName},
					},
				})
				pendingAnnotations = nil
				continue
			}

			// Protocol declarations.
			if m := protocolRe.FindStringSubmatch(line); m != nil {
				modifiers := m[1]
				name := m[2]

				exported := !isPrivateAccess(modifiers)

				pf := facts.Fact{
					Kind: facts.KindSymbol,
					Name: dir + "." + name,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"symbol_kind": facts.SymbolInterface,
						"exported":    exported,
						"language":    "swift",
					},
					Relations: []facts.Relation{
						{Kind: facts.RelDeclares, Target: dir},
					},
				}

				// Extract protocol inheritance.
				if colonIdx := strings.Index(line, ":"); colonIdx >= 0 {
					rest := line[colonIdx+1:]
					if braceIdx := strings.Index(rest, "{"); braceIdx >= 0 {
						rest = rest[:braceIdx]
					}
					rest = strings.TrimSpace(rest)
					if rest != "" {
						for _, st := range parseSupertypes(rest) {
							pf.Relations = append(pf.Relations, facts.Relation{
								Kind:   facts.RelImplements,
								Target: st,
							})
						}
					}
				}

				if isiOS {
					addIOSProps(&pf, name, allAnnotations, "")
				}

				result = append(result, pf)
				sigCapture = true
				sigTypeIdx = len(result) - 1
				sigMembers = nil
				sigPublished = nil
				pendingAnnotations = nil
				continue
			}

			// Struct declarations.
			if m := structRe.FindStringSubmatch(line); m != nil {
				modifiers := m[1]
				name := m[2]

				nameIdx := strings.Index(line, "struct "+name)
				restOfLine := line[nameIdx+len("struct ")+len(name):]

				parenDepth := strings.Count(restOfLine, "(") - strings.Count(restOfLine, ")")
				angleDepth := strings.Count(restOfLine, "<") - strings.Count(restOfLine, ">")

				if (parenDepth > 0 || angleDepth > 0) && !strings.Contains(line, "{") {
					pending = &pendingDecl{
						declType:    "struct",
						modifiers:   modifiers,
						name:        name,
						line:        lineNum,
						annotations: append([]string{}, allAnnotations...),
						parenDepth:  parenDepth,
						angleDepth:  angleDepth,
						lines:       restOfLine,
					}
					pendingAnnotations = nil
					continue
				}

				supertypes := extractSupertypesFromText(restOfLine)
				pc := &pendingDecl{
					declType:    "struct",
					modifiers:   modifiers,
					name:        name,
					line:        lineNum,
					annotations: allAnnotations,
				}
				fact := buildDeclFact(dir, relFile, pc, supertypes, isiOS)
				result = append(result, fact)
				sigCapture = true
				sigTypeIdx = len(result) - 1
				sigMembers = nil
				sigPublished = nil
				pendingAnnotations = nil
				continue
			}

			// Class declarations.
			if m := classRe.FindStringSubmatch(line); m != nil {
				modifiers := m[1]
				name := m[2]

				nameIdx := strings.Index(line, "class "+name)
				restOfLine := line[nameIdx+len("class ")+len(name):]

				parenDepth := strings.Count(restOfLine, "(") - strings.Count(restOfLine, ")")
				angleDepth := strings.Count(restOfLine, "<") - strings.Count(restOfLine, ">")

				if (parenDepth > 0 || angleDepth > 0) && !strings.Contains(line, "{") {
					pending = &pendingDecl{
						declType:    "class",
						modifiers:   modifiers,
						name:        name,
						line:        lineNum,
						annotations: append([]string{}, allAnnotations...),
						parenDepth:  parenDepth,
						angleDepth:  angleDepth,
						lines:       restOfLine,
					}
					pendingAnnotations = nil
					continue
				}

				supertypes := extractSupertypesFromText(restOfLine)
				pc := &pendingDecl{
					declType:    "class",
					modifiers:   modifiers,
					name:        name,
					line:        lineNum,
					annotations: allAnnotations,
				}
				fact := buildDeclFact(dir, relFile, pc, supertypes, isiOS)
				result = append(result, fact)
				sigCapture = true
				sigTypeIdx = len(result) - 1
				sigMembers = nil
				sigPublished = nil
				pendingAnnotations = nil
				continue
			}

			// Enum declarations.
			if m := enumRe.FindStringSubmatch(line); m != nil {
				modifiers := m[1]
				name := m[2]

				nameIdx := strings.Index(line, "enum "+name)
				restOfLine := line[nameIdx+len("enum ")+len(name):]

				parenDepth := strings.Count(restOfLine, "(") - strings.Count(restOfLine, ")")
				angleDepth := strings.Count(restOfLine, "<") - strings.Count(restOfLine, ">")

				if (parenDepth > 0 || angleDepth > 0) && !strings.Contains(line, "{") {
					pending = &pendingDecl{
						declType:    "enum",
						modifiers:   modifiers,
						name:        name,
						line:        lineNum,
						annotations: append([]string{}, allAnnotations...),
						parenDepth:  parenDepth,
						angleDepth:  angleDepth,
						lines:       restOfLine,
					}
					pendingAnnotations = nil
					continue
				}

				supertypes := extractSupertypesFromText(restOfLine)
				pc := &pendingDecl{
					declType:    "enum",
					modifiers:   modifiers,
					name:        name,
					line:        lineNum,
					annotations: allAnnotations,
				}
				fact := buildDeclFact(dir, relFile, pc, supertypes, isiOS)
				result = append(result, fact)
				sigCapture = true
				sigTypeIdx = len(result) - 1
				sigMembers = nil
				sigPublished = nil
				pendingAnnotations = nil
				continue
			}

			// Actor declarations.
			if m := actorRe.FindStringSubmatch(line); m != nil {
				modifiers := m[1]
				name := m[2]

				nameIdx := strings.Index(line, "actor "+name)
				restOfLine := line[nameIdx+len("actor ")+len(name):]

				parenDepth := strings.Count(restOfLine, "(") - strings.Count(restOfLine, ")")
				angleDepth := strings.Count(restOfLine, "<") - strings.Count(restOfLine, ">")

				if (parenDepth > 0 || angleDepth > 0) && !strings.Contains(line, "{") {
					pending = &pendingDecl{
						declType:    "actor",
						modifiers:   modifiers,
						name:        name,
						line:        lineNum,
						annotations: append([]string{}, allAnnotations...),
						parenDepth:  parenDepth,
						angleDepth:  angleDepth,
						lines:       restOfLine,
					}
					pendingAnnotations = nil
					continue
				}

				supertypes := extractSupertypesFromText(restOfLine)
				pc := &pendingDecl{
					declType:    "actor",
					modifiers:   modifiers,
					name:        name,
					line:        lineNum,
					annotations: allAnnotations,
				}
				fact := buildDeclFact(dir, relFile, pc, supertypes, isiOS)
				result = append(result, fact)
				sigCapture = true
				sigTypeIdx = len(result) - 1
				sigMembers = nil
				sigPublished = nil
				pendingAnnotations = nil
				continue
			}

			// Extension declarations — emit implements relations only.
			if m := extensionRe.FindStringSubmatch(line); m != nil {
				name := m[1]

				if colonIdx := strings.Index(line, ":"); colonIdx >= 0 {
					rest := line[colonIdx+1:]
					if braceIdx := strings.Index(rest, "{"); braceIdx >= 0 {
						rest = rest[:braceIdx]
					}
					rest = strings.TrimSpace(rest)
					if rest != "" {
						for _, st := range parseSupertypes(rest) {
							result = append(result, facts.Fact{
								Kind: facts.KindSymbol,
								Name: dir + "." + name + "+" + st,
								File: relFile,
								Line: lineNum,
								Props: map[string]any{
									"symbol_kind": "extension",
									"exported":    true,
									"language":    "swift",
								},
								Relations: []facts.Relation{
									{Kind: facts.RelDeclares, Target: dir},
									{Kind: facts.RelImplements, Target: st},
								},
							})
						}
					}
				}
				pendingAnnotations = nil
				continue
			}

			// Function declarations.
			if m := funcRe.FindStringSubmatch(line); m != nil {
				name := m[1]

				exported := !isPrivateAccess(line)

				ff := facts.Fact{
					Kind: facts.KindSymbol,
					Name: dir + "." + name,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"symbol_kind": facts.SymbolFunc,
						"exported":    exported,
						"language":    "swift",
					},
					Relations: []facts.Relation{
						{Kind: facts.RelDeclares, Target: dir},
					},
				}

				if strings.Contains(line, " async") {
					ff.Props["async"] = true
				}
				if strings.Contains(line, " throws") {
					ff.Props["throws"] = true
				}
				if strings.Contains(line, "nonisolated") {
					ff.Props["nonisolated"] = true
				}
				if strings.Contains(line, "@MainActor") {
					ff.Props["main_actor"] = true
				}

				result = append(result, ff)
				pendingAnnotations = nil
				continue
			}

			// Property declarations (let/var).
			if m := propRe.FindStringSubmatch(line); m != nil {
				letOrVar := m[1]
				name := m[2]

				if name == "_" {
					pendingAnnotations = nil
					continue
				}

				exported := !isPrivateAccess(line)

				symbolKind := facts.SymbolVariable
				if letOrVar == "let" {
					symbolKind = facts.SymbolConstant
				}

				result = append(result, facts.Fact{
					Kind: facts.KindSymbol,
					Name: dir + "." + name,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"symbol_kind": symbolKind,
						"exported":    exported,
						"language":    "swift",
					},
					Relations: []facts.Relation{
						{Kind: facts.RelDeclares, Target: dir},
					},
				})
				pendingAnnotations = nil
				continue
			}

			// Typealias declarations.
			if m := typealiasRe.FindStringSubmatch(line); m != nil {
				name := m[1]
				exported := !isPrivateAccess(line)

				result = append(result, facts.Fact{
					Kind: facts.KindSymbol,
					Name: dir + "." + name,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"symbol_kind": facts.SymbolType,
						"exported":    exported,
						"language":    "swift",
					},
					Relations: []facts.Relation{
						{Kind: facts.RelDeclares, Target: dir},
					},
				})
				pendingAnnotations = nil
				continue
			}
		}

		// Reset pending annotations if we hit a non-annotation, non-blank, non-comment line that wasn't a declaration.
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "/*") && !strings.HasPrefix(trimmed, "@") {
			pendingAnnotations = nil
		}
	}

	return result
}

// buildDeclFact creates a symbol fact for a struct/class/enum/protocol declaration.
func buildDeclFact(dir, relFile string, pd *pendingDecl, supertypes string, isiOS bool) facts.Fact {
	symbolKind := facts.SymbolClass
	switch pd.declType {
	case "struct":
		symbolKind = facts.SymbolStruct
	case "protocol":
		symbolKind = facts.SymbolInterface
	case "enum":
		symbolKind = facts.SymbolClass // enums map to class with enum=true
	case "actor":
		symbolKind = facts.SymbolClass // actors are reference types like classes
	}

	exported := !isPrivateAccess(pd.modifiers)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: dir + "." + pd.name,
		File: relFile,
		Line: pd.line,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    exported,
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: dir},
		},
	}

	if pd.declType == "enum" {
		f.Props["enum"] = true
	}
	if pd.declType == "actor" {
		f.Props["concurrency"] = "actor"
	}
	if strings.Contains(pd.modifiers, "final") {
		f.Props["final"] = true
	}
	if containsAnnotation(pd.annotations, "MainActor") {
		f.Props["main_actor"] = true
	}

	if supertypes != "" {
		for _, st := range parseSupertypes(supertypes) {
			f.Relations = append(f.Relations, facts.Relation{
				Kind:   facts.RelImplements,
				Target: st,
			})
		}
	}

	if isiOS {
		addIOSProps(&f, pd.name, pd.annotations, supertypes)
	}

	return f
}

// extractSupertypesFromText finds the supertype clause after ":" in text that may
// contain generic parameters. It skips content inside balanced parentheses and angle brackets.
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

// collectInlineAnnotations extracts attribute names from a line that also contains a declaration.
func collectInlineAnnotations(line string) []string {
	var result []string
	re := regexp.MustCompile(`@(\w+)`)
	matches := re.FindAllStringSubmatch(line, -1)
	for _, m := range matches {
		result = append(result, m[1])
	}
	return result
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

// isDeclarationLine checks if a line contains a Swift declaration keyword.
func isDeclarationLine(line string) bool {
	return funcRe.MatchString(line) || classRe.MatchString(line) ||
		structRe.MatchString(line) || enumRe.MatchString(line) ||
		protocolRe.MatchString(line) || extensionRe.MatchString(line) ||
		actorRe.MatchString(line)
}

func matchesXcodeProject(name string) bool {
	return strings.HasSuffix(name, ".xcodeproj") || strings.HasSuffix(name, ".xcworkspace")
}
