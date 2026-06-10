package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func setupTSProject(t *testing.T, files map[string]string, nextjs bool) string {
	t.Helper()
	dir := t.TempDir()

	// Create tsconfig.json for detection
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Optionally create next.config.js for Next.js detection
	if nextjs {
		if err := os.WriteFile(filepath.Join(dir, "next.config.js"), []byte(`module.exports = {}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func extractAll(t *testing.T, files map[string]string, nextjs bool) []facts.Fact {
	t.Helper()
	dir := setupTSProject(t, files, nextjs)

	var relFiles []string
	for f := range files {
		relFiles = append(relFiles, f)
	}

	ext := New()
	result, err := ext.Extract(context.Background(), dir, relFiles)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return result
}

func findFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func findFactsByKind(ff []facts.Fact, kind string) []facts.Fact {
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

// --- Route detection tests ---

func TestDetectRoute_AppRouter(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantRoute  string
		wantMethod string
		wantType   string
	}{
		{"root page", "src/app/page.tsx", "/", "GET", "page"},
		{"nested page", "src/app/about/page.tsx", "/about", "GET", "page"},
		{"dynamic segment", "src/app/users/[id]/page.tsx", "/users/[id]", "GET", "page"},
		{"api route", "src/app/api/users/route.tsx", "/api/users", "ALL", "route"},
		{"layout", "src/app/layout.tsx", "/", "GET", "layout"},
		{"loading", "src/app/loading.tsx", "/", "GET", "loading"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectRoute(tt.file)
			if got == nil {
				t.Fatal("expected route fact, got nil")
			}
			if got.Name != tt.wantRoute {
				t.Errorf("route path = %q, want %q", got.Name, tt.wantRoute)
			}
			if got.Props["method"] != tt.wantMethod {
				t.Errorf("method = %v, want %s", got.Props["method"], tt.wantMethod)
			}
			if got.Props["type"] != tt.wantType {
				t.Errorf("type = %v, want %s", got.Props["type"], tt.wantType)
			}
			if got.Props["router"] != "app" {
				t.Errorf("router = %v, want app", got.Props["router"])
			}
		})
	}
}

func TestDetectRoute_PagesRouter(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantRoute  string
		wantMethod string
	}{
		{"index page", "pages/index.tsx", "/", "GET"},
		{"about page", "pages/about.tsx", "/about", "GET"},
		{"dynamic", "pages/users/[id].tsx", "/users/[id]", "GET"},
		{"api route", "pages/api/hello.ts", "/api/hello", "ALL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectRoute(tt.file)
			if got == nil {
				t.Fatal("expected route fact, got nil")
			}
			if got.Name != tt.wantRoute {
				t.Errorf("route path = %q, want %q", got.Name, tt.wantRoute)
			}
			if got.Props["method"] != tt.wantMethod {
				t.Errorf("method = %v, want %s", got.Props["method"], tt.wantMethod)
			}
			if got.Props["router"] != "pages" {
				t.Errorf("router = %v, want pages", got.Props["router"])
			}
		})
	}
}

func TestDetectRoute_PagesRouter_SkipsSpecialFiles(t *testing.T) {
	for _, file := range []string{"pages/_app.tsx", "pages/_document.tsx", "pages/_error.tsx"} {
		got := detectRoute(file)
		if got != nil {
			t.Errorf("detectRoute(%q) should return nil for special pages, got %+v", file, got)
		}
	}
}

func TestDetectRoute_NonRoute(t *testing.T) {
	got := detectRoute("src/components/Button.tsx")
	if got != nil {
		t.Error("non-route file should return nil")
	}
}

// --- Full extraction tests ---

func TestExtract_FunctionDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/utils.ts": `export function fetchUsers() { return [] }`,
	}, false)

	f, ok := findFact(ff, "src.fetchUsers")
	if !ok {
		t.Fatal("expected fact for src.fetchUsers")
	}
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function", f.Props["symbol_kind"])
	}
	if f.Props["exported"] != true {
		t.Errorf("exported = %v, want true", f.Props["exported"])
	}
}

func TestExtract_ArrowFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/handler.ts": `export const handler = () => { return "ok" }`,
	}, false)

	f, ok := findFact(ff, "src.handler")
	if !ok {
		t.Fatal("expected fact for src.handler")
	}
	// Arrow functions should be classified as function, not variable
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function (arrow function should be detected)", f.Props["symbol_kind"])
	}
}

func TestExtract_ClassWithImplements(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `export class UserService implements Service, Loggable {}`,
	}, false)

	f, ok := findFact(ff, "src.UserService")
	if !ok {
		t.Fatal("expected fact for src.UserService")
	}
	if f.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("symbol_kind = %v, want class", f.Props["symbol_kind"])
	}

	if !hasRelation(f, facts.RelImplements, "Service") {
		t.Error("expected implements relation for Service")
	}
	if !hasRelation(f, facts.RelImplements, "Loggable") {
		t.Error("expected implements relation for Loggable")
	}
}

func TestExtract_InterfaceAndTypeAlias(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/types.ts": `
export interface User { name: string }
export type UserId = string
`,
	}, false)

	iface, ok := findFact(ff, "src.User")
	if !ok {
		t.Fatal("expected fact for src.User")
	}
	if iface.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("User symbol_kind = %v, want interface", iface.Props["symbol_kind"])
	}

	typAlias, ok := findFact(ff, "src.UserId")
	if !ok {
		t.Fatal("expected fact for src.UserId")
	}
	if typAlias.Props["symbol_kind"] != facts.SymbolType {
		t.Errorf("UserId symbol_kind = %v, want type", typAlias.Props["symbol_kind"])
	}
}

func TestExtract_ImportExtraction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/app.ts": `
import { foo } from './utils'
import React from 'react'
`,
	}, false)

	deps := findFactsByKind(ff, facts.KindDependency)
	hasUtils := false
	hasReact := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Target == "src/utils" {
				hasUtils = true
			}
			if r.Target == "react" {
				hasReact = true
			}
		}
	}
	if !hasUtils {
		t.Error("expected import for src/utils")
	}
	if !hasReact {
		t.Error("expected import for react")
	}
}

func TestExtract_NonExportedDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/internal.ts": `function helper() { return 42 }`,
	}, false)

	f, ok := findFact(ff, "src.helper")
	if !ok {
		t.Fatal("expected fact for src.helper")
	}
	if f.Props["exported"] != false {
		t.Errorf("exported = %v, want false (no export keyword)", f.Props["exported"])
	}
}

func TestExtract_ClassMethods(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `export class ApiClient {
  private baseUrl: string = '/api'

  login(username: string, password: string) {
    return fetch(this.baseUrl + '/login')
  }

  logout() {
    return fetch(this.baseUrl + '/logout')
  }

  private refreshToken() {
    return fetch(this.baseUrl + '/refresh')
  }
}`,
	}, false)

	// Class itself should be extracted
	cls, ok := findFact(ff, "src.ApiClient")
	if !ok {
		t.Fatal("expected fact for src.ApiClient")
	}
	if cls.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("class symbol_kind = %v, want class", cls.Props["symbol_kind"])
	}

	// Public methods should be extracted
	login, ok := findFact(ff, "src.ApiClient.login")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.login")
	}
	if login.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("login symbol_kind = %v, want method", login.Props["symbol_kind"])
	}
	if login.Props["exported"] != true {
		t.Errorf("login exported = %v, want true", login.Props["exported"])
	}
	if login.Props["receiver"] != "ApiClient" {
		t.Errorf("login receiver = %v, want ApiClient", login.Props["receiver"])
	}

	// logout should be extracted
	_, ok = findFact(ff, "src.ApiClient.logout")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.logout")
	}

	// Private method should be extracted but marked as not exported
	refresh, ok := findFact(ff, "src.ApiClient.refreshToken")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.refreshToken")
	}
	if refresh.Props["exported"] != false {
		t.Errorf("refreshToken exported = %v, want false (private)", refresh.Props["exported"])
	}

	// Constructor should NOT be extracted
	_, ok = findFact(ff, "src.ApiClient.constructor")
	if ok {
		t.Error("constructor should not be extracted as a method")
	}
}

func TestExtract_CallExtraction_SameModule(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main.ts": `export function doWork() {
  helper()
}

function helper() {}`,
	}, false)

	doWork, ok := findFact(ff, "src.doWork")
	if !ok {
		t.Fatal("expected fact for src.doWork")
	}
	if !hasRelation(doWork, facts.RelCalls, "src.helper") {
		t.Errorf("doWork should call src.helper; relations: %v", doWork.Relations)
	}
}

func TestExtract_CallExtraction_ThisMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `export class ApiClient {
  login() {
    this.refresh()
  }

  refresh() {}
}`,
	}, false)

	login, ok := findFact(ff, "src.ApiClient.login")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.login")
	}
	if !hasRelation(login, facts.RelCalls, "src.ApiClient.refresh") {
		t.Errorf("login should call src.ApiClient.refresh; relations: %v", login.Relations)
	}
}

func TestExtract_CallExtraction_ImportedFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main.ts": `import { formatName } from './utils'

export function render() {
  formatName()
}`,
		"src/utils.ts": `export function formatName() {}`,
	}, false)

	render, ok := findFact(ff, "src.render")
	if !ok {
		t.Fatal("expected fact for src.render")
	}
	// formatName imported from "./utils" → resolves to src.formatName, which is
	// the canonical fact name of the declaration in src/utils.ts.
	if !hasRelation(render, facts.RelCalls, "src.formatName") {
		t.Errorf("render should call src.formatName; relations: %v", render.Relations)
	}
	// Confirm the callee fact actually exists, so the edge is not dangling.
	if _, ok := findFact(ff, "src.formatName"); !ok {
		t.Error("expected callee fact src.formatName to exist")
	}
}

func TestExtract_CallExtraction_ArrowFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main.ts": `const handler = () => {
  process()
}

function process() {}`,
	}, false)

	handler, ok := findFact(ff, "src.handler")
	if !ok {
		t.Fatal("expected fact for src.handler")
	}
	if !hasRelation(handler, facts.RelCalls, "src.process") {
		t.Errorf("handler should call src.process; relations: %v", handler.Relations)
	}
}

func TestExtract_CallExtraction_MethodOnReceiver_NoEdge(t *testing.T) {
	// A method call on a value of unknown type is left unresolved.
	ff := extractAll(t, map[string]string{
		"src/main.ts": `export function run(client: ApiClient) {
  client.login()
}`,
	}, false)

	run, ok := findFact(ff, "src.run")
	if !ok {
		t.Fatal("expected fact for src.run")
	}
	for _, r := range run.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("unexpected RelCalls edge for receiver method call: %v", r)
		}
	}
}

func TestIsTypeScriptFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"src/app.ts", true},
		{"src/app.tsx", true},
		{"src/app.js", false},
		{"src/app.go", false},
	}
	for _, tt := range tests {
		if got := isTypeScriptFile(tt.path); got != tt.want {
			t.Errorf("isTypeScriptFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
