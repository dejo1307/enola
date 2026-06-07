package kotlinextractor

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

// KotlinExtractor extracts architectural facts from Kotlin source code using
// tree-sitter AST parsing (see kotlin_ast.go for the walker implementation).
type KotlinExtractor struct{}

// New creates a new KotlinExtractor.
func New() *KotlinExtractor {
	return &KotlinExtractor{}
}

func (e *KotlinExtractor) Name() string {
	return "kotlin"
}

// Detect returns true if the repository looks like a Kotlin or Android project.
func (e *KotlinExtractor) Detect(repoPath string) (bool, error) {
	for _, name := range []string{"build.gradle.kts", "build.gradle"} {
		path := filepath.Join(repoPath, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, "kotlin") || strings.Contains(content, "android") {
			return true, nil
		}
	}
	return false, nil
}

// Extract parses Kotlin files and emits architectural facts.
//
// Each file is parsed with tree-sitter and walked by the AST visitor in
// kotlin_ast.go, which produces declaration symbol facts, import dependency
// facts, Room storage facts, and call-graph relations (RelInstantiates,
// RelInjects) suitable for reverse-dependency queries.
func (e *KotlinExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	isAndroid := detectAndroidProject(repoPath)
	sourceRoot := detectKotlinSourceRoot(repoPath, files)
	basePackage := detectKotlinBasePackage(repoPath)

	modules := make(map[string]bool)

	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isKotlinFile(relFile) {
			continue
		}

		absFile := filepath.Join(repoPath, relFile)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[kotlin-extractor] error reading %s: %v", relFile, err)
			continue
		}

		allFacts = append(allFacts, extractFileAST(src, relFile, isAndroid, sourceRoot, basePackage)...)
		modules[filepath.Dir(relFile)] = true
	}

	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language": "kotlin",
			},
		})
	}

	return allFacts, nil
}

// --- Regex helpers shared with the AST walker ---

var (
	// packageRe extracts a Kotlin file's package declaration. Used to locate
	// the source-root prefix when resolving internal imports to filesystem paths.
	packageRe = regexp.MustCompile(`^\s*package\s+([\w.]+)`)

	// privateOrInternalRe is used by the AST walker to determine whether a
	// declaration's modifier text contains a visibility keyword that excludes
	// it from the project's exported surface.
	privateOrInternalRe = regexp.MustCompile(`\b(private|internal)\b`)
)

func isKotlinFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".kt")
}

// --- Android & framework detection helpers (called by the AST walker) ---

// detectAndroidProject checks for AndroidManifest.xml.
func detectAndroidProject(repoPath string) bool {
	candidates := []string{
		filepath.Join(repoPath, "app", "src", "main", "AndroidManifest.xml"),
		filepath.Join(repoPath, "src", "main", "AndroidManifest.xml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// addAndroidProps classifies a class/interface declaration as an Android component.
func addAndroidProps(f *facts.Fact, name string, annotations []string, supertypes string) {
	if containsAnnotation(annotations, "HiltAndroidApp") {
		f.Props["android_component"] = "application"
		f.Props["framework"] = "android"
		return
	}
	if containsAnnotation(annotations, "HiltViewModel") {
		f.Props["android_component"] = "viewmodel"
		f.Props["framework"] = "android"
		return
	}
	if containsAnnotation(annotations, "AndroidEntryPoint") {
		f.Props["framework"] = "android"
		if supertypeMatches(supertypes, "Activity", "ComponentActivity", "AppCompatActivity", "FragmentActivity") {
			f.Props["android_component"] = "activity"
		} else if supertypeMatches(supertypes, "Fragment") {
			f.Props["android_component"] = "fragment"
		} else if supertypeMatches(supertypes, "Service") {
			f.Props["android_component"] = "service"
		} else if supertypeMatches(supertypes, "BroadcastReceiver") {
			f.Props["android_component"] = "broadcast_receiver"
		}
		return
	}
	if containsAnnotation(annotations, "Module") {
		f.Props["android_component"] = "di_module"
		f.Props["framework"] = "android"
		return
	}

	if strings.HasSuffix(name, "ViewModel") || supertypeMatches(supertypes, "ViewModel") {
		f.Props["android_component"] = "viewmodel"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "Application") {
		f.Props["android_component"] = "application"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "Activity", "ComponentActivity", "AppCompatActivity") {
		f.Props["android_component"] = "activity"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "Fragment") {
		f.Props["android_component"] = "fragment"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "Service", "FirebaseMessagingService", "IntentService", "JobIntentService") {
		f.Props["android_component"] = "service"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "BroadcastReceiver") {
		f.Props["android_component"] = "broadcast_receiver"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "ContentProvider") {
		f.Props["android_component"] = "content_provider"
		f.Props["framework"] = "android"
		return
	}
	if supertypeMatches(supertypes, "Worker", "CoroutineWorker", "ListenableWorker") {
		f.Props["android_component"] = "worker"
		f.Props["framework"] = "android"
		return
	}
	if strings.HasSuffix(name, "Repository") || strings.HasSuffix(name, "RepositoryImpl") {
		f.Props["android_component"] = "repository"
		f.Props["framework"] = "android"
		return
	}
	if strings.HasSuffix(name, "UseCase") {
		f.Props["android_component"] = "usecase"
		f.Props["framework"] = "android"
		return
	}
}

// detectRoomStorage emits a storage fact for Room-annotated classes/interfaces.
func detectRoomStorage(name string, annotations []string, relFile string, line int, dir string) *facts.Fact {
	var storageKind string
	switch {
	case containsAnnotation(annotations, "Entity"):
		storageKind = "entity"
	case containsAnnotation(annotations, "Dao"):
		storageKind = "dao"
	case containsAnnotation(annotations, "Database"):
		storageKind = "database"
	default:
		return nil
	}
	return &facts.Fact{
		Kind: facts.KindStorage,
		Name: dir + "." + name,
		File: relFile,
		Line: line,
		Props: map[string]any{
			"storage_kind": storageKind,
			"language":     "kotlin",
			"framework":    "room",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: dir},
		},
	}
}

// containsAnnotation reports whether the simple-name list contains `name`.
func containsAnnotation(annotations []string, name string) bool {
	for _, a := range annotations {
		if a == name {
			return true
		}
	}
	return false
}

// supertypeMatches reports whether any of the comma-joined supertype names
// matches one of the provided candidates. Used by addAndroidProps to classify
// Android components by their parent type.
func supertypeMatches(supertypes string, names ...string) bool {
	if supertypes == "" {
		return false
	}
	for _, st := range parseSupertypes(supertypes) {
		for _, name := range names {
			if st == name {
				return true
			}
		}
	}
	return false
}

// parseSupertypes splits a supertype clause like "Foo(), Bar, Baz<T>" into type
// names. It tolerates nested generic and constructor-argument parentheses.
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

// extractTypeName strips generic parameters and constructor calls off a
// supertype entry like "Foo()" or "Bar<T>" and returns the simple type name.
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
	return s
}

// --- Source-root and import resolution (project-level) ---

// detectKotlinSourceRoot derives the source root directory from the first
// Kotlin file's package declaration. For "app/src/main/java/com/foo/Bar.kt"
// declaring `package com.foo`, it returns "app/src/main/java/".
func detectKotlinSourceRoot(repoPath string, files []string) string {
	for _, relFile := range files {
		if !isKotlinFile(relFile) {
			continue
		}
		absFile := filepath.Join(repoPath, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if m := packageRe.FindStringSubmatch(line); m != nil {
				pkg := m[1]
				pkgPath := strings.ReplaceAll(pkg, ".", "/")
				dir := filepath.ToSlash(filepath.Dir(relFile))
				if strings.HasSuffix(dir, pkgPath) {
					root := strings.TrimSuffix(dir, pkgPath)
					f.Close()
					return root
				}
				f.Close()
				return ""
			}
		}
		f.Close()
	}
	return ""
}

// detectKotlinBasePackage reads the Android namespace from build.gradle.kts so
// that internal imports (matching the project's package) can be resolved to
// filesystem paths rather than being treated as external library imports.
func detectKotlinBasePackage(repoPath string) string {
	candidates := []string{
		filepath.Join(repoPath, "app", "build.gradle.kts"),
		filepath.Join(repoPath, "app", "build.gradle"),
		filepath.Join(repoPath, "build.gradle.kts"),
		filepath.Join(repoPath, "build.gradle"),
	}
	nsRe := regexp.MustCompile(`namespace\s*=?\s*"([^"]+)"`)
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if m := nsRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// resolveKotlinImport normalizes a Kotlin import path. Internal imports
// (matching the project's base package) become filesystem-relative paths so
// the graph can connect them to module facts; everything else is treated as
// an external dependency.
func resolveKotlinImport(importPath, sourceRoot, basePackage string) (string, bool) {
	if basePackage != "" && sourceRoot != "" && strings.HasPrefix(importPath, basePackage+".") {
		asPath := strings.ReplaceAll(importPath, ".", "/")
		return filepath.ToSlash(filepath.Clean(sourceRoot + asPath)), false
	}
	return importPath, true
}
