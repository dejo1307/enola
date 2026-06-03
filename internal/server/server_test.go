package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dejo1307/enola/internal/config"
	"github.com/dejo1307/enola/internal/engine"
	"github.com/dejo1307/enola/internal/facts"
)

func TestReadSourceWindow(t *testing.T) {
	// Create a 10-line temp file
	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "line "+string(rune('0'+i)))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		centerLine   int
		contextLines int
		wantStart    int
		wantEnd      int
	}{
		// Asymmetric window: 1/4 before, 3/4 after the center line.
		// context=6 → before=1, after=5: 5-1=4 to 5+5=10
		{"center middle", 5, 6, 4, 10},
		// context=10 → before=2, after=8: 1-2=-1→1 to 1+8=9
		{"center at start", 1, 10, 1, 9},
		// context=10 → before=2, after=8: 10-2=8 to 10+8=18→10
		{"center at end", 10, 10, 8, 10},
		// context=20 → before=5, after=15: 5-5=0→1 to 5+15=20→10
		{"context larger than file", 5, 20, 1, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readSourceWindow(path, tt.centerLine, tt.contextLines)
			if err != nil {
				t.Fatalf("readSourceWindow: %v", err)
			}

			outputLines := strings.Split(strings.TrimRight(got, "\n"), "\n")

			// Verify first line starts with expected line number
			firstLine := outputLines[0]
			if !strings.Contains(firstLine, "│") {
				t.Fatalf("expected line number format with │, got: %s", firstLine)
			}

			// Count output lines
			expectedCount := tt.wantEnd - tt.wantStart + 1
			if len(outputLines) != expectedCount {
				t.Errorf("got %d output lines, want %d (lines %d-%d)",
					len(outputLines), expectedCount, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestReadSourceWindow_SingleLineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.go")
	if err := os.WriteFile(path, []byte("only line"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readSourceWindow(path, 1, 30)
	if err != nil {
		t.Fatalf("readSourceWindow: %v", err)
	}

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line for single-line file, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "only line") {
		t.Errorf("expected output to contain 'only line', got: %s", lines[0])
	}
}

func TestReadSourceWindow_LineNumberFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readSourceWindow(path, 3, 4)
	if err != nil {
		t.Fatalf("readSourceWindow: %v", err)
	}

	// Should have format "   N│ content"
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if !strings.Contains(line, "│") {
			t.Errorf("line missing │ separator: %q", line)
		}
	}
}

// --- test helpers ---

// newEngineWithSnapshot creates an engine with a fake snapshot pointing at the given repo path.
func newEngineWithSnapshot(repoPath string) *engine.Engine {
	cfg := config.Default()
	eng, _ := engine.New(cfg)
	eng.SetSnapshot(&facts.Snapshot{
		Meta: facts.SnapshotMeta{RepoPath: repoPath},
	})
	return eng
}

// --- explore helper tests ---

// newTestServer creates a Server with a pre-populated fact store for testing explore methods.
func newTestServer(store *facts.Store) *Server {
	return &Server{}
}

func populateTestStore() *facts.Store {
	store := facts.NewStore()
	store.Add(
		// Module
		facts.Fact{Kind: facts.KindModule, Name: "internal/server", Props: map[string]any{"language": "go", "package": "server"}},
		// Symbols declared in that module
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/server.New", File: "internal/server/server.go", Line: 26,
			Props:     map[string]any{"symbol_kind": "function", "exported": true, "language": "go"},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "internal/server"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/server.Run", File: "internal/server/server.go", Line: 45,
			Props:     map[string]any{"symbol_kind": "method", "exported": true, "language": "go"},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "internal/server"}, {Kind: facts.RelCalls, Target: "internal/engine.Store"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/server.handleQuery", File: "internal/server/handler.go", Line: 10,
			Props:     map[string]any{"symbol_kind": "function", "exported": false, "language": "go"},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "internal/server"}, {Kind: facts.RelCalls, Target: "internal/facts.Store.Query"}}},
		// Another module
		facts.Fact{Kind: facts.KindModule, Name: "internal/facts", Props: map[string]any{"language": "go", "package": "facts"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/facts.Store.Query", File: "internal/facts/store.go", Line: 105,
			Props:     map[string]any{"symbol_kind": "method", "exported": true, "language": "go"},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "internal/facts"}}},
		// Dependency
		facts.Fact{Kind: facts.KindDependency, Name: "internal/server -> internal/facts", File: "internal/server/server.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "internal/facts"}}},
		// Symbol in a different directory
		facts.Fact{Kind: facts.KindSymbol, Name: "cmd.main", File: "cmd/main.go", Line: 1,
			Props: map[string]any{"symbol_kind": "function", "exported": false, "language": "go"}},
	)
	return store
}

func TestExploreModule(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreModule(store, "internal/server", 1, &sb)
	if !found {
		t.Fatal("exploreModule should find 'internal/server'")
	}

	output := sb.String()

	// Should contain the module header
	if !strings.Contains(output, "# Module: internal/server") {
		t.Error("missing module header")
	}
	// Should list symbols
	if !strings.Contains(output, "internal/server.New") {
		t.Error("missing symbol New")
	}
	if !strings.Contains(output, "internal/server.Run") {
		t.Error("missing symbol Run")
	}
	if !strings.Contains(output, "internal/server.handleQuery") {
		t.Error("missing symbol handleQuery")
	}
	// Should show dependents (who imports internal/server -> none in test data)
	// Should show the symbols table
	if !strings.Contains(output, "Symbols (3)") {
		t.Error("missing symbols count")
	}
}

func TestExploreModule_NotFound(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreModule(store, "nonexistent", 1, &sb)
	if found {
		t.Error("exploreModule should return false for nonexistent module")
	}
}

func TestExploreModule_Depth2(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreModule(store, "internal/server", 2, &sb)
	if !found {
		t.Fatal("exploreModule should find 'internal/server'")
	}

	output := sb.String()
	// Depth 2 should include symbol relations section
	if !strings.Contains(output, "Symbol Relations") {
		t.Error("depth=2 should include Symbol Relations section")
	}
	// Should show the calls relation from Run
	if !strings.Contains(output, "internal/engine.Store") {
		t.Error("depth=2 should show call targets")
	}
}

func TestExploreModule_DependsOnAndImplements(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		// A Ruby-style packwerk module with depends_on relations.
		facts.Fact{Kind: facts.KindModule, Name: "packages/orders",
			Props: map[string]any{"language": "ruby", "framework": "rails", "packwerk": true},
			Relations: []facts.Relation{
				{Kind: facts.RelDependsOn, Target: "packages/payments"},
				{Kind: facts.RelDependsOn, Target: "packages/users"},
			}},
		// Target modules.
		facts.Fact{Kind: facts.KindModule, Name: "packages/payments",
			Props: map[string]any{"language": "ruby"},
			Relations: []facts.Relation{
				{Kind: facts.RelDependsOn, Target: "packages/orders"},
			}},
		facts.Fact{Kind: facts.KindModule, Name: "packages/users",
			Props: map[string]any{"language": "ruby"}},
		// A dependency fact with implements (mixin).
		facts.Fact{Kind: facts.KindDependency, Name: "Order -> Cacheable",
			File: "packages/orders/app/models/order.rb",
			Relations: []facts.Relation{
				{Kind: facts.RelImplements, Target: "Cacheable"},
			}},
	)

	srv := newTestServer(store)
	var sb strings.Builder
	found := srv.exploreModule(store, "packages/orders", 1, &sb)
	if !found {
		t.Fatal("exploreModule should find 'packages/orders'")
	}

	output := sb.String()

	// Should render depends_on section with packwerk dependencies.
	if !strings.Contains(output, "### Depends_on") {
		t.Error("missing Depends_on subsection")
	}
	if !strings.Contains(output, "packages/payments") {
		t.Error("missing depends_on target packages/payments")
	}
	if !strings.Contains(output, "packages/users") {
		t.Error("missing depends_on target packages/users")
	}

	// Should render implements section with mixin.
	if !strings.Contains(output, "### Implements") {
		t.Error("missing Implements subsection")
	}
	if !strings.Contains(output, "Cacheable") {
		t.Error("missing implements target Cacheable")
	}

	// Should render dependents (packages/payments depends_on packages/orders).
	if !strings.Contains(output, "## Dependents") {
		t.Error("missing Dependents section")
	}
	if !strings.Contains(output, "packages/payments") {
		t.Error("packages/payments should appear as a dependent")
	}
}

func TestExploreFile(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreFile(store, "internal/server/server.go", 1, &sb)
	if !found {
		t.Fatal("exploreFile should find 'internal/server/server.go'")
	}

	output := sb.String()
	if !strings.Contains(output, "# File: internal/server/server.go") {
		t.Error("missing file header")
	}
	// Should list the symbols in this file (New, Run) and the dependency
	if !strings.Contains(output, "internal/server.New") {
		t.Error("missing symbol New")
	}
	if !strings.Contains(output, "internal/server.Run") {
		t.Error("missing symbol Run")
	}
}

func TestExploreFile_NotFound(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreFile(store, "nonexistent.go", 1, &sb)
	if found {
		t.Error("exploreFile should return false for nonexistent file")
	}
}

func TestExploreSymbol(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreSymbol(store, "Store.Query", 1, &sb)
	if !found {
		t.Fatal("exploreSymbol should find 'Store.Query'")
	}

	output := sb.String()
	if !strings.Contains(output, "# Symbol: Store.Query") {
		t.Error("missing symbol header")
	}
	if !strings.Contains(output, "internal/facts/store.go") {
		t.Error("missing file reference")
	}
	// Should include Referenced By section (handleQuery calls it)
	if !strings.Contains(output, "Referenced By") {
		t.Error("missing Referenced By section")
	}
	if !strings.Contains(output, "internal/server.handleQuery") {
		t.Error("missing caller handleQuery in Referenced By")
	}
}

func TestExploreSymbol_NotFound(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreSymbol(store, "NonExistentSymbol", 1, &sb)
	if found {
		t.Error("exploreSymbol should return false for nonexistent symbol")
	}
}

func TestExploreDirectory(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreDirectory(store, "internal/server", &sb)
	if !found {
		t.Fatal("exploreDirectory should find 'internal/server'")
	}

	output := sb.String()
	if !strings.Contains(output, "# Directory: internal/server") {
		t.Error("missing directory header")
	}
	if !strings.Contains(output, "Summary") {
		t.Error("missing Summary section")
	}
}

func TestExploreDirectory_NotFound(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreDirectory(store, "nonexistent/dir", &sb)
	if found {
		t.Error("exploreDirectory should return false for nonexistent directory")
	}
}

// --- normalizeToRelative tests ---

func TestNormalizeToRelative_AbsolutePath(t *testing.T) {
	srv := &Server{
		eng: newEngineWithSnapshot("/Users/me/development"),
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"absolute subdir", "/Users/me/development/go-service", "go-service"},
		{"absolute file", "/Users/me/development/go-service/lib/foo.rb", "go-service/lib/foo.rb"},
		{"absolute repo root", "/Users/me/development", "."},
		{"already relative", "internal/server", "internal/server"},
		{"unrelated absolute", "/other/path/foo", "/other/path/foo"},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := srv.normalizeToRelative(tt.input)
			if got != tt.want {
				t.Errorf("normalizeToRelative(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeToRelative_MultiRepo(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/workspace")
	eng.SetRepoPaths(map[string]string{
		"go-service":    "/Users/me/development/go-service",
		"ruby-monolith": "/Users/me/development/ruby-monolith",
	})
	srv := &Server{eng: eng}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"multi-repo go-service dir", "/Users/me/development/go-service", "go-service"},
		{"multi-repo go-service file", "/Users/me/development/go-service/lib/foo.rb", "go-service/lib/foo.rb"},
		{"multi-repo ruby-monolith file", "/Users/me/development/ruby-monolith/lib/bar.rb", "ruby-monolith/lib/bar.rb"},
		{"unrelated absolute", "/other/path/foo", "/other/path/foo"},
		{"relative passthrough", "internal/server", "internal/server"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := srv.normalizeToRelative(tt.input)
			if got != tt.want {
				t.Errorf("normalizeToRelative(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- Integration tests: original reported use cases ---

// TestScenario_QueryFactsWithFilePrefixCrossRepo simulates the first reported issue:
// query_facts with file_prefix like "go-service/..." or "ruby-monolith/..." returned nothing.
func TestScenario_QueryFactsWithFilePrefixCrossRepo(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/development/workspace")
	eng.SetRepoPaths(map[string]string{
		"go-service":    "/Users/me/development/go-service",
		"ruby-monolith": "/Users/me/development/ruby-monolith",
	})
	srv := &Server{eng: eng}

	store := eng.Store()

	// Simulate repo A facts (go-service) - files prefixed as in append mode
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "Pricing", File: "go-service/lib/pricing.rb", Repo: "go-service",
			Props: map[string]any{"language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "PricingService", File: "go-service/lib/pricing_service.rb", Line: 5, Repo: "go-service",
			Props:     map[string]any{"symbol_kind": "class", "exported": true, "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "CoreUtils"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "go-service -> ruby-monolith", File: "go-service/lib/pricing_service.rb", Repo: "go-service",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "ruby-monolith"}}},
	)

	// Simulate repo B facts (ruby-monolith)
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "Core", File: "ruby-monolith/lib/core.rb", Repo: "ruby-monolith",
			Props: map[string]any{"language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "CoreUtils", File: "ruby-monolith/lib/utils.rb", Line: 10, Repo: "ruby-monolith",
			Props: map[string]any{"symbol_kind": "class", "exported": true, "language": "ruby"}},
	)

	// Test 1: query_facts with file_prefix "go-service" should find go-service facts
	results, total := store.QueryAdvanced(facts.QueryOpts{FilePrefix: "go-service"})
	if total != 3 {
		t.Errorf("file_prefix=go-service: total=%d, want 3", total)
	}
	for _, f := range results {
		if !strings.HasPrefix(f.File, "go-service") {
			t.Errorf("unexpected file %q in go-service results", f.File)
		}
	}

	// Test 2: query_facts with file_prefix "ruby-monolith" should find ruby-monolith facts
	results, total = store.QueryAdvanced(facts.QueryOpts{FilePrefix: "ruby-monolith"})
	if total != 2 {
		t.Errorf("file_prefix=ruby-monolith: total=%d, want 2", total)
	}

	// Test 3: repo filter should work
	results, total = store.QueryAdvanced(facts.QueryOpts{Repo: "go-service"})
	if total != 3 {
		t.Errorf("repo=go-service: total=%d, want 3", total)
	}

	// Test 4: normalize absolute path to file_prefix
	normalized := srv.normalizeToRelative("/Users/me/development/go-service/lib")
	if normalized != "go-service/lib" {
		t.Errorf("normalize(/Users/me/development/go-service/lib) = %q, want go-service/lib", normalized)
	}
	results, total = store.QueryAdvanced(facts.QueryOpts{FilePrefix: normalized})
	if total != 3 {
		t.Errorf("normalized file_prefix: total=%d, want 3 (module + symbol + dep all in go-service/lib/)", total)
	}
}

// TestScenario_ExploreWithAbsolutePathCrossRepo simulates the second reported issue:
// explore with focus "/Users/.../go-service" returned "No facts matching focus".
func TestScenario_ExploreWithAbsolutePathCrossRepo(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/workspace")
	eng.SetRepoPaths(map[string]string{
		"go-service":    "/Users/me/development/go-service",
		"ruby-monolith": "/Users/me/development/ruby-monolith",
	})
	srv := &Server{eng: eng}

	store := eng.Store()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "PricingService", File: "go-service/lib/pricing_service.rb", Line: 5, Repo: "go-service",
			Props: map[string]any{"symbol_kind": "class", "exported": true}},
		facts.Fact{Kind: facts.KindSymbol, Name: "CoreUtils", File: "ruby-monolith/lib/utils.rb", Line: 10, Repo: "ruby-monolith",
			Props: map[string]any{"symbol_kind": "class", "exported": true}},
	)

	// Test: explore with absolute path to go-service repo root
	focus := srv.normalizeToRelative("/Users/me/development/go-service")
	t.Logf("normalized focus: %q", focus)

	var sb strings.Builder
	found := srv.exploreDirectory(store, focus, &sb)
	if !found {
		t.Errorf("exploreDirectory should find facts for normalized focus %q", focus)
	}
	output := sb.String()
	if !strings.Contains(output, "PricingService") {
		t.Errorf("explore output should contain PricingService, got:\n%s", output)
	}

	// Test: explore with absolute path to ruby-monolith repo root
	focus = srv.normalizeToRelative("/Users/me/development/ruby-monolith")
	t.Logf("normalized focus: %q", focus)

	sb.Reset()
	found = srv.exploreDirectory(store, focus, &sb)
	if !found {
		t.Errorf("exploreDirectory should find facts for normalized focus %q", focus)
	}
	output = sb.String()
	if !strings.Contains(output, "CoreUtils") {
		t.Errorf("explore output should contain CoreUtils, got:\n%s", output)
	}

	// Test: explore with absolute path to subdirectory
	focus = srv.normalizeToRelative("/Users/me/development/go-service/lib")
	if focus != "go-service/lib" {
		t.Errorf("normalized subdir = %q, want go-service/lib", focus)
	}

	sb.Reset()
	found = srv.exploreDirectory(store, focus, &sb)
	if !found {
		t.Errorf("exploreDirectory should find facts for subdir focus %q", focus)
	}
}

// TestScenario_ExploreSingleRepoAbsolutePath tests the single-repo case where
// someone passes the repo root as an absolute path to explore.
func TestScenario_ExploreSingleRepoAbsolutePath(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/development/go-service")
	srv := &Server{eng: eng}

	store := eng.Store()
	// Use Go-style names with dots — these would be falsely matched if "."
	// were used as a substring query on symbol names.
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "pricing.Service", File: "lib/pricing_service.go", Line: 5,
			Props: map[string]any{"symbol_kind": "struct", "exported": true}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pricing.Calculator", File: "lib/price_calculator.go", Line: 10,
			Props: map[string]any{"symbol_kind": "struct", "exported": true}},
	)

	// Normalize the exact repo root
	focus := srv.normalizeToRelative("/Users/me/development/go-service")
	t.Logf("single-repo root normalized to: %q", focus)

	// Raw exploreSymbol WOULD match "." as substring of "pricing.Service" etc.
	// This is the bug we're guarding against at the handler level.
	var sb strings.Builder
	rawSymbolMatch := srv.exploreSymbol(store, focus, 1, &sb)
	if !rawSymbolMatch {
		t.Log("(note: exploreSymbol didn't match — names may not contain dots)")
	} else {
		t.Log("exploreSymbol falsely matches '.' — handler switch must prevent this")
	}

	// The handler-level fix: exploreDirectory handles "." as repo root.
	// In the explore switch, "." routes directly to exploreDirectory, skipping exploreSymbol.
	sb.Reset()
	found := srv.exploreDirectory(store, focus, &sb)
	if !found {
		t.Errorf("exploreDirectory should handle %q as repo root", focus)
	}
	output := sb.String()
	if !strings.Contains(output, "pricing.Service") {
		t.Errorf("root directory explore should contain pricing.Service, got:\n%s", output)
	}
	// Verify it's a directory-style output (not a symbol dump)
	if !strings.Contains(output, "Directory:") {
		t.Error("root explore should produce Directory-style output")
	}

	// Normalize a subdirectory - should work
	focus = srv.normalizeToRelative("/Users/me/development/go-service/lib")
	if focus != "lib" {
		t.Errorf("normalized subdir = %q, want lib", focus)
	}

	sb.Reset()
	found = srv.exploreDirectory(store, focus, &sb)
	if !found {
		t.Errorf("exploreDirectory should find facts for focus %q", focus)
	}
	output = sb.String()
	if !strings.Contains(output, "pricing.Service") {
		t.Errorf("should contain pricing.Service, got:\n%s", output)
	}

	// Normalize a specific file path
	focus = srv.normalizeToRelative("/Users/me/development/go-service/lib/pricing_service.go")
	if focus != "lib/pricing_service.go" {
		t.Errorf("normalized file = %q, want lib/pricing_service.go", focus)
	}

	sb.Reset()
	found = srv.exploreFile(store, focus, 1, &sb)
	if !found {
		t.Errorf("exploreFile should find facts for focus %q", focus)
	}
}

// TestScenario_FirstRepoNoAppendThenAppend simulates the exact reported issue:
// 1. generate_snapshot(repo_path="/path/ruby-monolith") — no append, facts have Repo: ""
// 2. generate_snapshot(repo_path="/path/go-service", append=true)
// 3. query_facts(repo: "ruby-monolith") should return results (retroactively tagged)
func TestScenario_FirstRepoNoAppendThenAppend(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/development/ruby-monolith")
	srv := &Server{eng: eng}

	store := eng.Store()

	// Step 1: Simulate facts from "ruby-monolith" (the first non-append snapshot).
	// These facts have Repo: "" and unprefixed file paths — exactly like
	// what GenerateSnapshot produces without append.
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "Core", File: "lib/core.rb",
			Props: map[string]any{"language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "CoreUtils", File: "lib/utils.rb", Line: 10,
			Props: map[string]any{"symbol_kind": "class", "exported": true, "language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "CoreLogger", File: "lib/logger.rb", Line: 1,
			Props: map[string]any{"symbol_kind": "class", "exported": true, "language": "ruby"}},
	)

	// In the new flow, SetRepoRange is called right after extraction even in
	// non-append mode, so Repo is already set.
	store.SetRepoRange(0, "ruby-monolith")

	// Verify: repo filter works immediately (before any append)
	results, total := store.QueryAdvanced(facts.QueryOpts{Repo: "ruby-monolith"})
	if total != 3 {
		t.Errorf("before append: repo=ruby-monolith should return 3, got %d", total)
	}

	// Step 2: Simulate entering append mode.
	// TagUntagged retroactively prefixes file paths for facts that already
	// have Repo set but lack the file prefix.
	prevLabel := "ruby-monolith" // filepath.Base("/Users/me/development/ruby-monolith")
	tagged := store.TagUntagged(prevLabel, prevLabel+"/")
	t.Logf("retroactively prefixed %d facts with file prefix %q", tagged, prevLabel+"/")

	if tagged != 3 {
		t.Errorf("expected 3 file paths prefixed, got %d", tagged)
	}

	// Now add go-service facts (simulating what TagRange does)
	preCount := store.Count()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "PricingService", File: "lib/pricing_service.rb", Line: 5,
			Props:     map[string]any{"symbol_kind": "class", "exported": true, "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "CoreUtils"}}},
	)
	store.TagRange(preCount, "go-service", "go-service/")

	// Step 3: Verify both repos are now queryable

	// repo: "ruby-monolith" should now return results
	results, total = store.QueryAdvanced(facts.QueryOpts{Repo: "ruby-monolith"})
	if total != 3 {
		t.Errorf("repo=ruby-monolith: total=%d, want 3", total)
	}
	for _, f := range results {
		if f.Repo != "ruby-monolith" {
			t.Errorf("expected Repo=ruby-monolith, got %q for %s", f.Repo, f.Name)
		}
	}

	// repo: "go-service" should return results
	results, total = store.QueryAdvanced(facts.QueryOpts{Repo: "go-service"})
	if total != 1 {
		t.Errorf("repo=go-service: total=%d, want 1", total)
	}

	// file_prefix: "ruby-monolith" should work (files are now ruby-monolith/lib/...)
	results, total = store.QueryAdvanced(facts.QueryOpts{FilePrefix: "ruby-monolith/"})
	if total != 3 {
		t.Errorf("file_prefix=ruby-monolith/: total=%d, want 3", total)
	}

	// file_prefix: "go-service" should work
	results, total = store.QueryAdvanced(facts.QueryOpts{FilePrefix: "go-service/"})
	if total != 1 {
		t.Errorf("file_prefix=go-service/: total=%d, want 1", total)
	}

	// explore with absolute path to ruby-monolith should resolve
	focus := srv.normalizeToRelative("/Users/me/development/ruby-monolith")
	t.Logf("normalized focus for ruby-monolith: %q", focus)

	// In multi-repo mode, normalizeToRelative should find ruby-monolith in repoPaths
	eng.SetRepoPaths(map[string]string{
		"ruby-monolith": "/Users/me/development/ruby-monolith",
		"go-service":    "/Users/me/development/go-service",
	})
	focus = srv.normalizeToRelative("/Users/me/development/ruby-monolith")
	t.Logf("normalized focus for ruby-monolith (with repoPaths): %q", focus)
	if focus != "ruby-monolith" {
		t.Errorf("expected focus=ruby-monolith, got %q", focus)
	}

	var sb strings.Builder
	found := srv.exploreDirectory(store, focus, &sb)
	if !found {
		t.Errorf("exploreDirectory should find facts for focus %q", focus)
	}
	output := sb.String()
	if !strings.Contains(output, "CoreUtils") {
		t.Errorf("explore output should contain CoreUtils, got:\n%s", output)
	}
	if !strings.Contains(output, "CoreLogger") {
		t.Errorf("explore output should contain CoreLogger, got:\n%s", output)
	}

	_ = results // avoid unused
}

// --- exploreModuleSubstring tests ---

func TestExploreModuleSubstring_SingleMatch(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	// "server" should substring-match "internal/server" (the only module containing "server")
	found := srv.exploreModuleSubstring(store, "server", 1, &sb)
	if !found {
		t.Fatal("exploreModuleSubstring should find a module matching 'server'")
	}

	output := sb.String()
	// Single match delegates to full exploreModule rendering
	if !strings.Contains(output, "# Module: internal/server") {
		t.Errorf("expected full module exploration, got:\n%s", output)
	}
}

func TestExploreModuleSubstring_MultipleMatches(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	// "internal" should substring-match both "internal/server" and "internal/facts"
	found := srv.exploreModuleSubstring(store, "internal", 1, &sb)
	if !found {
		t.Fatal("exploreModuleSubstring should find modules matching 'internal'")
	}

	output := sb.String()
	if !strings.Contains(output, "Multiple modules matching") {
		t.Errorf("expected disambiguation list, got:\n%s", output)
	}
	if !strings.Contains(output, "internal/server") {
		t.Error("should list internal/server")
	}
	if !strings.Contains(output, "internal/facts") {
		t.Error("should list internal/facts")
	}
}

func TestExploreModuleSubstring_NoMatch(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreModuleSubstring(store, "nonexistent", 1, &sb)
	if found {
		t.Error("exploreModuleSubstring should return false for nonexistent")
	}
}

// --- expandFilePrefix tests ---

func TestExpandFilePrefix_SingleRepo(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/development/myrepo")
	srv := &Server{eng: eng}

	// No repoPaths set — single repo mode. Should pass through.
	prefixes := srv.expandFilePrefix("src/")
	if len(prefixes) != 1 || prefixes[0] != "src/" {
		t.Errorf("single-repo: expected [src/], got %v", prefixes)
	}
}

func TestExpandFilePrefix_MultiRepoExpands(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/workspace")
	eng.SetRepoPaths(map[string]string{
		"golf-ui": "/Users/me/development/golf-ui",
		"golf":    "/Users/me/development/golf",
	})
	srv := &Server{eng: eng}

	store := eng.Store()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "AuthForm", File: "golf-ui/src/components/Auth.tsx", Repo: "golf-ui"},
		facts.Fact{Kind: facts.KindSymbol, Name: "LoginPage", File: "golf-ui/src/pages/login.tsx", Repo: "golf-ui"},
		facts.Fact{Kind: facts.KindModule, Name: "internal/auth", File: "golf/internal/auth/auth.go", Repo: "golf"},
	)

	// "src/" doesn't start with a repo label — should expand to "golf-ui/src/"
	prefixes := srv.expandFilePrefix("src/")
	if len(prefixes) != 1 || prefixes[0] != "golf-ui/src/" {
		t.Errorf("expected [golf-ui/src/], got %v", prefixes)
	}

	// "internal/" should expand to "golf/internal/"
	prefixes = srv.expandFilePrefix("internal/")
	if len(prefixes) != 1 || prefixes[0] != "golf/internal/" {
		t.Errorf("expected [golf/internal/], got %v", prefixes)
	}
}

func TestExpandFilePrefix_AlreadyPrefixed(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/workspace")
	eng.SetRepoPaths(map[string]string{
		"golf-ui": "/Users/me/development/golf-ui",
	})
	srv := &Server{eng: eng}

	store := eng.Store()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "AuthForm", File: "golf-ui/src/Auth.tsx", Repo: "golf-ui"},
	)

	// Already prefixed — should pass through unchanged.
	prefixes := srv.expandFilePrefix("golf-ui/src/")
	if len(prefixes) != 1 || prefixes[0] != "golf-ui/src/" {
		t.Errorf("already-prefixed: expected [golf-ui/src/], got %v", prefixes)
	}
}

func TestExpandFilePrefix_Empty(t *testing.T) {
	eng := newEngineWithSnapshot("/Users/me/workspace")
	srv := &Server{eng: eng}

	prefixes := srv.expandFilePrefix("")
	if len(prefixes) != 1 || prefixes[0] != "" {
		t.Errorf("empty: expected [\"\"], got %v", prefixes)
	}
}

// --- exploreFile fallback tests ---

func TestExploreFile_RepoLabelFallback(t *testing.T) {
	// Simulate multi-repo mode where files are stored with repo-label prefix.
	eng := newEngineWithSnapshot("/Users/me/workspace")
	eng.SetRepoPaths(map[string]string{
		"golf-ui": "/Users/me/development/golf-ui",
		"golf":    "/Users/me/development/golf",
	})
	srv := &Server{eng: eng}

	store := eng.Store()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "src/stores.useAuthStore", File: "golf-ui/src/stores/authStore.ts", Line: 5, Repo: "golf-ui",
			Props: map[string]any{"symbol_kind": "function", "exported": true, "language": "typescript"}},
		facts.Fact{Kind: facts.KindModule, Name: "src/stores/authStore", File: "golf-ui/src/stores/authStore.ts", Repo: "golf-ui",
			Props: map[string]any{"language": "typescript"}},
	)

	// Bare path without repo label — should fall back to golf-ui/src/stores/authStore.ts
	var sb strings.Builder
	found := srv.exploreFile(store, "src/stores/authStore.ts", 1, &sb)
	if !found {
		t.Fatal("exploreFile should find 'src/stores/authStore.ts' via repo-label fallback")
	}
	output := sb.String()
	if !strings.Contains(output, "golf-ui/src/stores/authStore.ts") {
		t.Errorf("expected resolved file path in output, got:\n%s", output)
	}
	if !strings.Contains(output, "useAuthStore") {
		t.Error("expected useAuthStore symbol in output")
	}
}

func TestExploreFile_ExtensionFallback(t *testing.T) {
	// Simulate multi-repo mode where user omits the file extension.
	eng := newEngineWithSnapshot("/Users/me/workspace")
	eng.SetRepoPaths(map[string]string{
		"golf-ui": "/Users/me/development/golf-ui",
	})
	srv := &Server{eng: eng}

	store := eng.Store()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "src/stores.useAuthStore", File: "golf-ui/src/stores/authStore.ts", Line: 5, Repo: "golf-ui",
			Props: map[string]any{"symbol_kind": "function", "exported": true}},
	)

	// No extension + no repo label — should try "src/stores/authStore" + ".ts" + "golf-ui/" prefix
	var sb strings.Builder
	found := srv.exploreFile(store, "src/stores/authStore", 1, &sb)
	if !found {
		t.Fatal("exploreFile should find 'src/stores/authStore' via extension + repo-label fallback")
	}
	output := sb.String()
	if !strings.Contains(output, "golf-ui/src/stores/authStore.ts") {
		t.Errorf("expected resolved file path in output, got:\n%s", output)
	}
}

func TestExploreFile_ExtensionFallback_SingleRepo(t *testing.T) {
	// Single-repo mode — extension guessing should still work without repo labels.
	srv := newTestServer(nil)

	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/server.New", File: "internal/server/server.go", Line: 26,
			Props: map[string]any{"symbol_kind": "function", "exported": true}},
	)

	// Without extension — should try "internal/server/server" + ".go"
	var sb strings.Builder
	found := srv.exploreFile(store, "internal/server/server", 1, &sb)
	if !found {
		t.Fatal("exploreFile should find 'internal/server/server' via .go extension fallback")
	}
	output := sb.String()
	if !strings.Contains(output, "internal/server/server.go") {
		t.Errorf("expected resolved file path, got:\n%s", output)
	}
}

func TestExploreFile_NoFallbackNeeded(t *testing.T) {
	// Exact match — no fallback should be needed.
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreFile(store, "internal/server/server.go", 1, &sb)
	if !found {
		t.Fatal("exploreFile should find exact match")
	}
	output := sb.String()
	if !strings.Contains(output, "# File: internal/server/server.go") {
		t.Error("exact match should use original focus in header")
	}
}

func TestShowSymbol_PrefersExactMatch(t *testing.T) {
	// Simulate the show_symbol handler's lookup logic:
	// exact match via LookupByExactName should take priority over substring.
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "Transaction",
			File: "models/transaction.rb", Line: 5,
			Props: map[string]any{"symbol_kind": "class", "language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "AutoTransactionsTogglePatch",
			File: "initializers/patches.rb", Line: 8,
			Props: map[string]any{"symbol_kind": "interface", "language": "ruby"}},
		// A non-symbol fact named "Transaction" should be ignored.
		facts.Fact{Kind: facts.KindStorage, Name: "Transaction",
			File:  "models/transaction.rb",
			Props: map[string]any{"storage_kind": "model"}},
	)

	// Replicate the handler's resolution: exact match, filter to symbols.
	results := store.LookupByExactName("Transaction")
	var symbolResults []facts.Fact
	for _, r := range results {
		if r.Kind == facts.KindSymbol {
			symbolResults = append(symbolResults, r)
		}
	}

	if len(symbolResults) != 1 {
		t.Fatalf("expected 1 symbol result, got %d", len(symbolResults))
	}
	if symbolResults[0].Name != "Transaction" {
		t.Errorf("expected exact match 'Transaction', got %q", symbolResults[0].Name)
	}
	if symbolResults[0].File != "models/transaction.rb" {
		t.Errorf("expected file models/transaction.rb, got %q", symbolResults[0].File)
	}

	// Substring search would return both -- verify the exact path avoids this.
	substring := store.Query("symbol", "", "Transaction", "")
	if len(substring) < 2 {
		t.Errorf("substring search should match at least 2 symbols, got %d", len(substring))
	}
}

// --- resolveNodeName / nameResolution tests ---

// populateAmbiguousStore builds a store with several modules that share the
// "svc-beta" prefix plus a symbol, modeling the real-world ambiguity the
// resolution object addresses.
func populateAmbiguousStore() *facts.Store {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "cmd/svc-beta", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "cmd/svc-beta-consumer", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "cmd/svc-beta-asynq", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "cmd/svc-beta-filters", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "cmd/svc-beta-task", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "cmd/svc-beta-asynq.jobServerResources",
			File: "cmd/svc-beta-asynq/main.go", Line: 12,
			Props: map[string]any{"symbol_kind": "function", "language": "go"}},
	)
	return store
}

func TestResolveNodeName_ExactMatch(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	name, res, err := srv.resolveNodeName(store, "internal/server")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "internal/server" {
		t.Errorf("matched = %q, want internal/server", name)
	}
	if res != nil {
		t.Errorf("expected nil resolution for exact match, got %+v", res)
	}
}

func TestResolveNodeName_SingleSubstring(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	// "handleQuery" substring-matches exactly one fact.
	name, res, err := srv.resolveNodeName(store, "handleQuery")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "internal/server.handleQuery" {
		t.Errorf("matched = %q, want internal/server.handleQuery", name)
	}
	if res != nil {
		t.Errorf("expected nil resolution for single substring match, got %+v", res)
	}
}

func TestResolveNodeName_ConfidentSuffixExact(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	// "Query" hits internal/facts.Store.Query and internal/server.handleQuery,
	// but only the former's last segment equals "Query" — a confident pick.
	name, res, err := srv.resolveNodeName(store, "Query")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "internal/facts.Store.Query" {
		t.Errorf("matched = %q, want internal/facts.Store.Query", name)
	}
	if res != nil {
		t.Errorf("expected nil resolution for confident suffix match, got %+v", res)
	}
}

func TestResolveNodeName_AmbiguousBelowThreshold(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "svc-foo", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "svc-bar", Props: map[string]any{"language": "go"}},
	)
	srv := newTestServer(store)

	// "svc" matches both modules (2, below threshold) with no suffix winner.
	name, res, err := srv.resolveNodeName(store, "svc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name == "" {
		t.Fatal("expected a best-guess match below threshold, got empty")
	}
	if res == nil {
		t.Fatal("expected a non-nil resolution for ambiguous match")
	}
	if !res.Ambiguous {
		t.Error("resolution should be marked ambiguous")
	}
	if res.Matched != name {
		t.Errorf("resolution.Matched = %q, want %q", res.Matched, name)
	}
	if res.Query != "svc" {
		t.Errorf("resolution.Query = %q, want svc", res.Query)
	}
	if len(res.Alternatives) != 1 {
		t.Errorf("expected 1 alternative, got %v", res.Alternatives)
	}
	for _, alt := range res.Alternatives {
		if alt == name {
			t.Errorf("alternatives should exclude the matched name %q", name)
		}
	}
}

func TestResolveNodeName_OverThreshold(t *testing.T) {
	store := populateAmbiguousStore()
	srv := newTestServer(store)

	name, res, err := srv.resolveNodeName(store, "svc-beta")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "" {
		t.Errorf("expected empty match over threshold, got %q", name)
	}
	if res == nil {
		t.Fatal("expected a non-nil resolution over threshold")
	}
	if res.Matched != "" {
		t.Errorf("resolution.Matched should be empty over threshold, got %q", res.Matched)
	}
	if !res.Ambiguous {
		t.Error("resolution should be marked ambiguous")
	}
	if res.Query != "svc-beta" {
		t.Errorf("resolution.Query = %q, want svc-beta", res.Query)
	}
	if len(res.Alternatives) < ambiguousMatchThreshold {
		t.Errorf("expected at least %d alternatives, got %v", ambiguousMatchThreshold, res.Alternatives)
	}
}

func TestResolveNodeName_OverThresholdCapsAlternatives(t *testing.T) {
	store := facts.NewStore()
	for i := 0; i < maxAlternatives+5; i++ {
		store.Add(facts.Fact{Kind: facts.KindModule, Name: "pkg/widget" + itoa(i), Props: map[string]any{"language": "go"}})
	}
	srv := newTestServer(store)

	_, res, err := srv.resolveNodeName(store, "pkg/widget")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected a resolution")
	}
	if len(res.Alternatives) > maxAlternatives {
		t.Errorf("alternatives = %d, want <= %d", len(res.Alternatives), maxAlternatives)
	}
}

func TestResolveNodeName_NoMatch(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	_, res, err := srv.resolveNodeName(store, "doesNotExistAnywhere")
	if err == nil {
		t.Fatal("expected an error for no match")
	}
	if res != nil {
		t.Errorf("expected nil resolution on no match, got %+v", res)
	}
}

// itoa is a tiny local helper for building distinct test fact names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// --- response wrapper marshaling tests ---

func TestTraverseResponse_MarshalWithResolution(t *testing.T) {
	resp := traverseResponse{
		Resolution: &nameResolution{
			Query:        "svc-beta",
			Matched:      "cmd/svc-beta",
			Alternatives: []string{"cmd/svc-beta-asynq"},
			Ambiguous:    true,
		},
		TraversalResult: facts.TraversalResult{
			Nodes: []facts.TraversalNode{{Name: "cmd/svc-beta", Kind: "module"}},
			Edges: []facts.TraversalEdge{},
		},
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	out := string(data)
	for _, want := range []string{`"resolution"`, `"matched"`, `"alternatives"`, `"ambiguous": true`, `"nodes"`, `"edges"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}

func TestTraverseResponse_MarshalOmitsResolutionWhenNil(t *testing.T) {
	resp := traverseResponse{
		TraversalResult: facts.TraversalResult{
			Nodes: []facts.TraversalNode{{Name: "internal/server", Kind: "module"}},
		},
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(data), "resolution") {
		t.Errorf("expected no resolution key, got:\n%s", string(data))
	}
}

func TestTraverseResponse_MarshalOverThresholdEmptyArrays(t *testing.T) {
	resp := traverseResponse{
		Resolution: &nameResolution{Query: "svc-beta", Ambiguous: true},
		TraversalResult: facts.TraversalResult{
			Nodes: []facts.TraversalNode{},
			Edges: []facts.TraversalEdge{},
		},
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, `"nodes": []`) {
		t.Errorf("expected empty nodes array (not null), got:\n%s", out)
	}
	if !strings.Contains(out, `"edges": []`) {
		t.Errorf("expected empty edges array (not null), got:\n%s", out)
	}
	if strings.Contains(out, `"matched"`) {
		t.Errorf("matched should be omitted when empty, got:\n%s", out)
	}
}

func TestFindPathResponse_MarshalIndependentResolutions(t *testing.T) {
	resp := findPathResponse{
		FromResolution: &nameResolution{Query: "svc-beta", Ambiguous: true},
		PathResult:     facts.PathResult{From: "", To: "internal/facts", Found: false},
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, `"from_resolution"`) {
		t.Errorf("expected from_resolution, got:\n%s", out)
	}
	if strings.Contains(out, `"to_resolution"`) {
		t.Errorf("to_resolution should be omitted when nil, got:\n%s", out)
	}
}

func TestCapitalize(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"module", "Module"},
		{"symbol", "Symbol"},
		{"", ""},
		{"A", "A"},
	}
	for _, tt := range tests {
		if got := capitalize(tt.input); got != tt.want {
			t.Errorf("capitalize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
