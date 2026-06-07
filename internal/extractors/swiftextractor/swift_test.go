package swiftextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

// extractFromString writes Swift source to a temp file and runs extractFile.
func extractFromString(t *testing.T, src string, isiOS bool) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.swift")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return extractFile(f, "pkg/test.swift", isiOS)
}

func findFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func findFactByKind(ff []facts.Fact, kind string) []facts.Fact {
	var result []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			result = append(result, f)
		}
	}
	return result
}

func hasRelation(f facts.Fact, relKind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == relKind && r.Target == target {
			return true
		}
	}
	return false
}

// --- Unit tests for parsing helpers ---

func TestExtractSupertypes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple supertypes", ": Foo, Bar {", "Foo, Bar"},
		{"generic constraint colon skipped", "<T: Equatable>: View {", "View"},
		{"param colon skipped", "(param: Int): Observable {", "Observable"},
		{"no colon", "SomeName {", ""},
		{"with where clause", ": Foo where T: Bar {", "Foo"},
		{"nested generics", "<T>(x: Int): A, B {", "A, B"},
		{"empty after colon", ": {", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSupertypesFromText(tt.input)
			if got != tt.want {
				t.Errorf("extractSupertypesFromText(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseSupertypes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single type", "Foo", []string{"Foo"}},
		{"two types", "Foo, Bar", []string{"Foo", "Bar"}},
		{"generic not a separator", "Foo<A, B>, Bar", []string{"Foo", "Bar"}},
		{"constructor call stripped", "Foo()", []string{"Foo"}},
		{"empty", "", nil},
		{"with spaces", " Foo , Bar ", []string{"Foo", "Bar"}},
		{"complex generics", "Base<T>, Protocol1, Protocol2", []string{"Base", "Protocol1", "Protocol2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSupertypes(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("parseSupertypes(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseSupertypes(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsPrivateAccess(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"private", "private ", true},
		{"fileprivate", "fileprivate ", true},
		{"private(set) only", "private(set) ", false},
		{"fileprivate(set) only", "fileprivate(set) ", false},
		{"public", "public ", false},
		{"empty", "", false},
		{"internal", "internal ", false},
		{"private(set) with private", "private(set) private ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPrivateAccess(tt.text)
			if got != tt.want {
				t.Errorf("isPrivateAccess(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestExtractTypeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Foo", "Foo"},
		{"Foo<T>", "Foo"},
		{"Foo()", "Foo"},
		{" Foo ", "Foo"},
		{"pkg.Foo", "Foo"},
		{"", ""},
		{"  ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractTypeName(tt.input)
			if got != tt.want {
				t.Errorf("extractTypeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSystemType(t *testing.T) {
	systemTypes := []string{"String", "Int", "Bool", "View", "AnyCancellable", "ObservableObject", "Never"}
	for _, st := range systemTypes {
		if !isSystemType(st) {
			t.Errorf("isSystemType(%q) = false, want true", st)
		}
	}

	customTypes := []string{"MyModel", "UserService", "HomeViewModel", "AppState"}
	for _, ct := range customTypes {
		if isSystemType(ct) {
			t.Errorf("isSystemType(%q) = true, want false", ct)
		}
	}
}

// --- Integration tests via extractFile ---

func TestClassify_SwiftUIView(t *testing.T) {
	ff := extractFromString(t, `
struct HomeView: View {
    var body: some View {
        Text("Hello")
    }
}
`, true)

	f, ok := findFact(ff, "pkg.HomeView")
	if !ok {
		t.Fatal("expected fact for pkg.HomeView")
	}
	if f.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("symbol_kind = %v, want struct", f.Props["symbol_kind"])
	}
	if f.Props["ios_component"] != "swiftui_view" {
		t.Errorf("ios_component = %v, want swiftui_view", f.Props["ios_component"])
	}
	if f.Props["framework"] != "swiftui" {
		t.Errorf("framework = %v, want swiftui", f.Props["framework"])
	}
	if !hasRelation(f, facts.RelImplements, "View") {
		t.Error("expected implements relation for View")
	}
}

func TestClassify_ViewModel_Observable(t *testing.T) {
	ff := extractFromString(t, `
@Observable
class HomeViewModel {
    var items: [String] = []
}
`, true)

	f, ok := findFact(ff, "pkg.HomeViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewModel")
	}
	if f.Props["ios_component"] != "viewmodel" {
		t.Errorf("ios_component = %v, want viewmodel", f.Props["ios_component"])
	}
	if f.Props["framework"] != "observation" {
		t.Errorf("framework = %v, want observation", f.Props["framework"])
	}
}

func TestClassify_ViewModel_ObservableObject(t *testing.T) {
	ff := extractFromString(t, `
class SettingsViewModel: ObservableObject {
    @Published var theme: String = "light"
}
`, true)

	f, ok := findFact(ff, "pkg.SettingsViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.SettingsViewModel")
	}
	if f.Props["ios_component"] != "viewmodel" {
		t.Errorf("ios_component = %v, want viewmodel", f.Props["ios_component"])
	}
	if f.Props["framework"] != "combine" {
		t.Errorf("framework = %v, want combine", f.Props["framework"])
	}
}

func TestClassify_MultiLineDecl(t *testing.T) {
	ff := extractFromString(t, `
@MainActor
final class HomeViewController<T: Equatable,
    U: Hashable>: UIViewController {
    func viewDidLoad() {}
}
`, true)

	f, ok := findFact(ff, "pkg.HomeViewController")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewController")
	}
	if f.Props["ios_component"] != "viewcontroller" {
		t.Errorf("ios_component = %v, want viewcontroller", f.Props["ios_component"])
	}
	if f.Props["framework"] != "uikit" {
		t.Errorf("framework = %v, want uikit", f.Props["framework"])
	}
	if f.Props["main_actor"] != true {
		t.Errorf("main_actor = %v, want true", f.Props["main_actor"])
	}
	if f.Props["final"] != true {
		t.Errorf("final = %v, want true", f.Props["final"])
	}
}

func TestClassify_MainApp(t *testing.T) {
	ff := extractFromString(t, `
@main
struct MyApp: App {
    var body: some Scene {
        WindowGroup { ContentView() }
    }
}
`, true)

	f, ok := findFact(ff, "pkg.MyApp")
	if !ok {
		t.Fatal("expected fact for pkg.MyApp")
	}
	if f.Props["ios_component"] != "swiftui_app" {
		t.Errorf("ios_component = %v, want swiftui_app", f.Props["ios_component"])
	}
}

func TestClassify_UIKitController(t *testing.T) {
	ff := extractFromString(t, `
class ProfileVC: UIViewController {
    override func viewDidLoad() {}
}
`, true)

	f, ok := findFact(ff, "pkg.ProfileVC")
	if !ok {
		t.Fatal("expected fact for pkg.ProfileVC")
	}
	if f.Props["ios_component"] != "viewcontroller" {
		t.Errorf("ios_component = %v, want viewcontroller", f.Props["ios_component"])
	}
}

func TestClassify_NameBased(t *testing.T) {
	tests := []struct {
		src           string
		name          string
		wantComponent string
	}{
		{"class UserRepository {\n}\n", "pkg.UserRepository", "repository"},
		{"class FetchUseCase {\n}\n", "pkg.FetchUseCase", "usecase"},
		{"class DIContainer {\n}\n", "pkg.DIContainer", "di_container"},
		{"class AppCoordinator {\n}\n", "pkg.AppCoordinator", "coordinator"},
		{"class NetworkService {\n}\n", "pkg.NetworkService", "service"},
	}
	for _, tt := range tests {
		t.Run(tt.wantComponent, func(t *testing.T) {
			ff := extractFromString(t, tt.src+"\n", true)
			f, ok := findFact(ff, tt.name)
			if !ok {
				t.Fatalf("expected fact for %s", tt.name)
			}
			if f.Props["ios_component"] != tt.wantComponent {
				t.Errorf("ios_component = %v, want %s", f.Props["ios_component"], tt.wantComponent)
			}
		})
	}
}

func TestExtension_WithProtocol(t *testing.T) {
	ff := extractFromString(t, `
extension MyView: Equatable {
}
`, false)

	f, ok := findFact(ff, "pkg.MyView+Equatable")
	if !ok {
		t.Fatal("expected fact for pkg.MyView+Equatable")
	}
	if f.Props["symbol_kind"] != "extension" {
		t.Errorf("symbol_kind = %v, want extension", f.Props["symbol_kind"])
	}
	if !hasRelation(f, facts.RelImplements, "Equatable") {
		t.Error("expected implements relation for Equatable")
	}
}

func TestSignatureCapture(t *testing.T) {
	// Build a struct with 20 members — should capture only 15
	var members []string
	for i := 0; i < 20; i++ {
		members = append(members, "    var prop"+string(rune('A'+i))+": Int")
	}
	src := "struct BigStruct {\n" + strings.Join(members, "\n") + "\n}\n"

	ff := extractFromString(t, src, false)

	f, ok := findFact(ff, "pkg.BigStruct")
	if !ok {
		t.Fatal("expected fact for pkg.BigStruct")
	}
	sig, ok := f.Props["signature"].(string)
	if !ok {
		t.Fatal("expected signature prop")
	}
	sigLines := strings.Split(sig, "\n")
	if len(sigLines) != 15 {
		t.Errorf("signature has %d lines, want 15 (max)", len(sigLines))
	}
}

func TestPublishedProperties(t *testing.T) {
	ff := extractFromString(t, `
class AppState: ObservableObject {
    @Published var items: [String] = []
    @Published var isLoading: Bool = false
    var computed: Int { 42 }
}
`, true)

	f, ok := findFact(ff, "pkg.AppState")
	if !ok {
		t.Fatal("expected fact for pkg.AppState")
	}
	if f.Props["reactive"] != true {
		t.Errorf("reactive = %v, want true", f.Props["reactive"])
	}
	published, ok := f.Props["published_properties"].(string)
	if !ok {
		t.Fatalf("expected published_properties string, got %T", f.Props["published_properties"])
	}
	if !strings.Contains(published, "items") || !strings.Contains(published, "isLoading") {
		t.Errorf("published_properties = %q, want to contain items and isLoading", published)
	}
}

func TestBraceDepthTracking(t *testing.T) {
	// Only top-level declarations should be extracted
	ff := extractFromString(t, `
struct Outer {
    struct Inner {
        func innerFunc() {
        }
    }
    func outerFunc() {
    }
}
func topLevel() {
}
`, false)

	// Outer should be extracted
	if _, ok := findFact(ff, "pkg.Outer"); !ok {
		t.Error("expected top-level struct Outer")
	}
	// topLevel function should be extracted
	if _, ok := findFact(ff, "pkg.topLevel"); !ok {
		t.Error("expected top-level func topLevel")
	}
	// Inner struct and innerFunc should NOT be top-level facts
	if _, ok := findFact(ff, "pkg.Inner"); ok {
		t.Error("nested struct Inner should not be extracted as top-level fact")
	}
	if _, ok := findFact(ff, "pkg.innerFunc"); ok {
		t.Error("nested func innerFunc should not be extracted as top-level fact")
	}
}

func TestEnumDeclaration(t *testing.T) {
	ff := extractFromString(t, `
enum NetworkError: Error {
    case timeout
    case serverError(code: Int)
}
`, false)

	f, ok := findFact(ff, "pkg.NetworkError")
	if !ok {
		t.Fatal("expected fact for pkg.NetworkError")
	}
	if f.Props["enum"] != true {
		t.Errorf("enum = %v, want true", f.Props["enum"])
	}
	if !hasRelation(f, facts.RelImplements, "Error") {
		t.Error("expected implements relation for Error")
	}
}

func TestImportStatements(t *testing.T) {
	ff := extractFromString(t, `
import Foundation
import UIKit
`, false)

	deps := findFactByKind(ff, facts.KindDependency)
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependency facts, got %d", len(deps))
	}

	hasFoundation := false
	hasUIKit := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Target == "Foundation" {
				hasFoundation = true
			}
			if r.Target == "UIKit" {
				hasUIKit = true
			}
		}
	}
	if !hasFoundation {
		t.Error("expected import for Foundation")
	}
	if !hasUIKit {
		t.Error("expected import for UIKit")
	}
}

func TestUnexportedDeclarations(t *testing.T) {
	ff := extractFromString(t, `
private struct InternalModel {
}
fileprivate func helper() {
}
`, false)

	model, ok := findFact(ff, "pkg.InternalModel")
	if !ok {
		t.Fatal("expected fact for pkg.InternalModel")
	}
	if model.Props["exported"] != false {
		t.Errorf("InternalModel exported = %v, want false", model.Props["exported"])
	}

	helper, ok := findFact(ff, "pkg.helper")
	if !ok {
		t.Fatal("expected fact for pkg.helper")
	}
	if helper.Props["exported"] != false {
		t.Errorf("helper exported = %v, want false", helper.Props["exported"])
	}
}

func TestActorDeclaration(t *testing.T) {
	ff := extractFromString(t, `
actor DataStore {
    func fetch() async {}
}
`, false)

	f, ok := findFact(ff, "pkg.DataStore")
	if !ok {
		t.Fatal("expected fact for pkg.DataStore")
	}
	if f.Props["concurrency"] != "actor" {
		t.Errorf("concurrency = %v, want actor", f.Props["concurrency"])
	}
}

func TestProtocolDeclaration(t *testing.T) {
	ff := extractFromString(t, `
protocol Repository: Sendable {
    func fetch() async throws
}
`, false)

	f, ok := findFact(ff, "pkg.Repository")
	if !ok {
		t.Fatal("expected fact for pkg.Repository")
	}
	if f.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("symbol_kind = %v, want interface", f.Props["symbol_kind"])
	}
	if !hasRelation(f, facts.RelImplements, "Sendable") {
		t.Error("expected implements relation for Sendable")
	}
}
