package swiftextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writeManifestRepo builds a temp SPM package: a Package.swift plus a stub source
// file per target, and an Info.plist so iOS classification runs.
func writeManifestRepo(t *testing.T, manifest string, targets []string) (string, []string) {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []string{"Packages/Mods/Package.swift"}
	mustWrite(t, repo, "Packages/Mods/Package.swift", manifest)
	for _, tgt := range targets {
		rel := "Packages/Mods/Sources/" + tgt + "/" + tgt + "Placeholder.swift"
		mustWrite(t, repo, rel, "public enum "+tgt+"Placeholder { public static let moduleName = \""+tgt+"\" }\n")
		files = append(files, rel)
	}
	return repo, files
}

func mustWrite(t *testing.T, repo, rel, content string) {
	t.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sampleManifest = `// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Mods",
    products: [
        .library(name: "AppShell", targets: ["AppShell"])
    ],
    targets: [
        .target(name: "CoreTypes"),
        .target(name: "DesignSystem", dependencies: ["CoreTypes"]),
        .target(name: "FeatureDashboard", dependencies: ["DesignSystem"]),
        .target(name: "AppComposition", dependencies: ["DesignSystem"]),
        .target(name: "AppShell", dependencies: ["AppComposition", "FeatureDashboard"]),
        .testTarget(name: "AppShellTests", dependencies: ["AppShell"])
    ]
)
`

func TestManifest_TargetDependencyEdges(t *testing.T) {
	targets := []string{"CoreTypes", "DesignSystem", "FeatureDashboard", "AppComposition", "AppShell"}
	repo, files := writeManifestRepo(t, sampleManifest, targets)

	allFacts, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	base := "Packages/Mods/Sources"
	// Dependency facts present with resolved module-dir targets.
	wantDeps := [][2]string{
		{base + "/AppShell", base + "/AppComposition"},
		{base + "/AppShell", base + "/FeatureDashboard"},
		{base + "/DesignSystem", base + "/CoreTypes"},
		{base + "/FeatureDashboard", base + "/DesignSystem"},
	}
	for _, wd := range wantDeps {
		if !hasDependencyEdge(allFacts, wd[0], wd[1]) {
			t.Errorf("missing SPM dependency edge %s -> %s", wd[0], wd[1])
		}
	}

	// Target module facts carry SPM identity.
	if m, ok := findFact(allFacts, base+"/AppShell"); !ok {
		t.Error("expected module fact for AppShell target")
	} else if m.Kind != facts.KindModule || m.Props["spm_target"] != "AppShell" {
		t.Errorf("AppShell module fact wrong: kind=%s props=%v", m.Kind, m.Props)
	}
}

func TestManifest_GraphTraversal(t *testing.T) {
	targets := []string{"CoreTypes", "DesignSystem", "FeatureDashboard", "AppComposition", "AppShell"}
	repo, files := writeManifestRepo(t, sampleManifest, targets)
	allFacts, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	base := "Packages/Mods/Sources"
	g := facts.NewGraph(allFacts)

	// Forward from AppShell reaches its transitive dependencies.
	fwd := g.Traverse(base+"/AppShell", "forward", []string{"imports"}, nil, 0, 0)
	reached := map[string]bool{}
	for _, n := range fwd.Nodes {
		reached[n.Name] = true
	}
	for _, want := range []string{base + "/AppComposition", base + "/FeatureDashboard", base + "/DesignSystem", base + "/CoreTypes"} {
		if !reached[want] {
			t.Errorf("forward traverse from AppShell did not reach %s; reached=%v", want, reached)
		}
	}

	// Reverse impact on DesignSystem lists the targets that depend on it.
	impact := g.ImpactSet(base+"/DesignSystem", 0, 0, false)
	dependents := map[string]bool{}
	for _, nodes := range impact.ByDepth {
		for _, n := range nodes {
			dependents[n.Name] = true
		}
	}
	for _, want := range []string{base + "/FeatureDashboard", base + "/AppComposition", base + "/AppShell"} {
		if !dependents[want] {
			t.Errorf("impact on DesignSystem missing dependent %s; got %v", want, dependents)
		}
	}
}

func TestManifest_JunkSuppressed(t *testing.T) {
	targets := []string{"CoreTypes", "DesignSystem", "FeatureDashboard", "AppComposition", "AppShell"}
	repo, files := writeManifestRepo(t, sampleManifest, targets)
	allFacts, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range allFacts {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, ".package") {
			t.Errorf("manifest produced junk symbol fact: %s", f.Name)
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && r.Target == "PackageDescription" {
				t.Errorf("manifest produced junk PackageDescription import on %s", f.Name)
			}
		}
	}

	// Sanity: the real placeholder enums in target dirs are still extracted.
	if _, ok := findFact(allFacts, "Packages/Mods/Sources/AppShell.AppShellPlaceholder"); !ok {
		t.Error("expected placeholder enum AppShellPlaceholder to still be extracted")
	}
}

func hasDependencyEdge(ff []facts.Fact, fromFileDir, target string) bool {
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		if fileDir(f.File) != fromFileDir {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && r.Target == target {
				return true
			}
		}
	}
	return false
}

func fileDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}
