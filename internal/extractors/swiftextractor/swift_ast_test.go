package swiftextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractAST runs the tree-sitter walker directly on a source string (no
// canonicalisation post-pass), so edge targets are bare simple names.
func extractAST(t *testing.T, src string, isiOS bool) []facts.Fact {
	t.Helper()
	return extractFileAST([]byte(src), "pkg/test.swift", isiOS)
}

func TestRelInstantiates(t *testing.T) {
	ff := extractAST(t, `
class Coordinator {
    func start() {
        let vm = HomeViewModel()
        vm.load()
    }
}
`, false)

	f, ok := findFact(ff, "pkg.Coordinator.start")
	if !ok {
		t.Fatal("expected fact for pkg.Coordinator.start")
	}
	if !hasRelation(f, facts.RelInstantiates, "HomeViewModel") {
		t.Errorf("expected instantiates HomeViewModel; relations=%v", f.Relations)
	}
}

func TestRelCalls_SameType(t *testing.T) {
	ff := extractAST(t, `
class A {
    func a() {
        b()
        self.c()
    }
    func b() {}
    func c() {}
}
`, false)

	f, ok := findFact(ff, "pkg.A.a")
	if !ok {
		t.Fatal("expected fact for pkg.A.a")
	}
	if !hasRelation(f, facts.RelCalls, "pkg.A.b") {
		t.Errorf("expected calls pkg.A.b; relations=%v", f.Relations)
	}
	if !hasRelation(f, facts.RelCalls, "pkg.A.c") {
		t.Errorf("expected calls pkg.A.c (via self.c()); relations=%v", f.Relations)
	}
}

func TestRelCalls_BuiltinSuppressed(t *testing.T) {
	ff := extractAST(t, `
func go() {
    print("hi")
    fatalError("boom")
}
`, false)

	f, ok := findFact(ff, "pkg.go")
	if !ok {
		t.Fatal("expected fact for pkg.go")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("did not expect any RelCalls (builtins suppressed); got %v", r)
		}
	}
}

func TestRelInjects_InitParameter(t *testing.T) {
	ff := extractAST(t, `
final class HomeViewModel {
    init(repo: UserRepository, name: String) {}
}
`, false)

	f, ok := findFact(ff, "pkg.HomeViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewModel")
	}
	if !hasRelation(f, facts.RelInjects, "UserRepository") {
		t.Errorf("expected injects UserRepository; relations=%v", f.Relations)
	}
	// String is a system type and must not become an inject edge.
	if hasRelation(f, facts.RelInjects, "String") {
		t.Errorf("did not expect injects String; relations=%v", f.Relations)
	}
}

func TestRelInjects_PropertyWrapper(t *testing.T) {
	ff := extractAST(t, `
final class Consumer {
    @Injected var repo: UserRepository
    @Environment(\.theme) var theme: Theme
}
`, false)

	f, ok := findFact(ff, "pkg.Consumer")
	if !ok {
		t.Fatal("expected fact for pkg.Consumer")
	}
	if !hasRelation(f, facts.RelInjects, "UserRepository") {
		t.Errorf("expected injects UserRepository; relations=%v", f.Relations)
	}
}

func TestRelDependsOn_ViewToViewModel(t *testing.T) {
	ff := extractAST(t, `
struct HomeView: View {
    @StateObject var vm: HomeViewModel
    var body: some View { Text("") }
}
`, true)

	f, ok := findFact(ff, "pkg.HomeView")
	if !ok {
		t.Fatal("expected fact for pkg.HomeView")
	}
	if !hasRelation(f, facts.RelDependsOn, "HomeViewModel") {
		t.Errorf("expected depends_on HomeViewModel; relations=%v", f.Relations)
	}
}

func TestRelCalls_DIHubNavigation(t *testing.T) {
	ff := extractAST(t, `
final class FeatureBuilder {
    func make() {
        AppComposition.shared.makeRepository()
    }
}
`, false)

	f, ok := findFact(ff, "pkg.FeatureBuilder.make")
	if !ok {
		t.Fatal("expected fact for pkg.FeatureBuilder.make")
	}
	if !hasRelation(f, facts.RelCalls, "AppComposition") {
		t.Errorf("expected calls AppComposition (DI-hub navigation); relations=%v", f.Relations)
	}
}

// TestImpactAnalysis_DIHub is the headline test: a DI hub (AppComposition) with
// two consumers that inject/construct via it must show non-empty dependents in a
// reverse-BFS impact analysis — exactly what was broken before the call graph.
func TestImpactAnalysis_DIHub(t *testing.T) {
	repo := t.TempDir()
	// Mark as an iOS project so classification runs.
	if err := os.WriteFile(filepath.Join(repo, "Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("App/AppComposition.swift", `
final class AppComposition {
    static let shared = AppComposition()
    func makeProfileRepository() -> ProfileRepository { ProfileRepository() }
}
`)
	write("Profile/ProfileRepository.swift", `
final class ProfileRepository {}
`)
	// Consumer 1: constructor injection of AppComposition.
	write("Profile/ProfileViewModel.swift", `
final class ProfileViewModel {
    init(composition: AppComposition) {}
}
`)
	// Consumer 2: navigation access to the DI hub.
	write("Home/HomeBuilder.swift", `
final class HomeBuilder {
    func build() {
        let repo = AppComposition.shared.makeProfileRepository()
    }
}
`)

	ext := New()
	files := []string{
		"App/AppComposition.swift",
		"Profile/ProfileRepository.swift",
		"Profile/ProfileViewModel.swift",
		"Home/HomeBuilder.swift",
	}
	allFacts, err := ext.Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := findFact(allFacts, "App.AppComposition"); !ok {
		t.Fatalf("expected AppComposition symbol fact; facts=%v", factNames(allFacts))
	}

	g := facts.NewGraph(allFacts)
	impact := g.ImpactSet("App.AppComposition", 0, 0, false)

	deps := impact.ByDepth[1]
	if len(deps) == 0 {
		t.Fatalf("expected non-empty dependents for App.AppComposition, got summary=%q", impact.Summary)
	}

	want := map[string]bool{
		"Profile.ProfileViewModel": false, // injects AppComposition
		"Home.HomeBuilder.build":   false, // calls AppComposition (DI-hub nav)
	}
	for _, n := range impact.ByDepth[1] {
		if _, ok := want[n.Name]; ok {
			want[n.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected %s among depth-1 dependents; got %v", name, depNames(impact.ByDepth[1]))
		}
	}
}

func factNames(ff []facts.Fact) []string {
	var out []string
	for _, f := range ff {
		out = append(out, f.Name)
	}
	return out
}

func depNames(nodes []facts.TraversalNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
