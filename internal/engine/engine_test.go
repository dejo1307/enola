package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dejo1307/enola/internal/config"
	"github.com/dejo1307/enola/internal/facts"
)

func TestIsIgnored(t *testing.T) {
	tests := []struct {
		name     string
		relPath  string
		isDir    bool
		patterns []string
		want     bool
	}{
		{
			"vendor directory",
			"vendor/foo/bar.go", false,
			[]string{"vendor/**"},
			true,
		},
		{
			"vendor dir itself",
			"vendor", true,
			[]string{"vendor/**"},
			true,
		},
		{
			"node_modules",
			"node_modules/react/index.js", false,
			[]string{"node_modules/**"},
			true,
		},
		{
			"git directory",
			".git/HEAD", false,
			[]string{".git/**"},
			true,
		},
		{
			"test files with ** prefix",
			"src/main_test.go", false,
			[]string{"**/*_test.go"},
			true,
		},
		{
			"non-test file not ignored",
			"src/main.go", false,
			[]string{"**/*_test.go"},
			false,
		},
		{
			"spec files",
			"src/utils.spec.ts", false,
			[]string{"**/*.spec.ts"},
			true,
		},
		{
			"enola output dir",
			".enola/facts.jsonl", false,
			[]string{".enola/**"},
			true,
		},
		{
			"normal source not ignored",
			"src/app.go", false,
			[]string{"vendor/**"},
			false,
		},
		{
			"build directory",
			"build/output.kt", false,
			[]string{"build/**"},
			true,
		},
		{
			"nested test file",
			"internal/pkg/foo_test.go", false,
			[]string{"**/*_test.go"},
			true,
		},
		{
			"deeply nested vendor",
			"vendor/github.com/foo/bar/baz.go", false,
			[]string{"vendor/**"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Ignore = tt.patterns

			eng, _ := New(cfg)
			got := eng.isIgnored(tt.relPath, tt.isDir)
			if got != tt.want {
				t.Errorf("isIgnored(%q, isDir=%v) with patterns %v = %v, want %v",
					tt.relPath, tt.isDir, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestResolveFactFile_SingleRepo(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)
	eng.SetSnapshot(&facts.Snapshot{
		Meta: facts.SnapshotMeta{RepoPath: "/Users/me/myrepo"},
	})

	f := &facts.Fact{File: "internal/server/server.go"}
	got := eng.ResolveFactFile(f)
	want := filepath.Join("/Users/me/myrepo", "internal/server/server.go")
	if got != want {
		t.Errorf("ResolveFactFile = %q, want %q", got, want)
	}
}

func TestResolveFactFile_MultiRepo(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)
	eng.SetRepoPaths(map[string]string{
		"go-service":    "/Users/me/development/go-service",
		"ruby-monolith": "/Users/me/development/ruby-monolith",
	})
	eng.SetSnapshot(&facts.Snapshot{
		Meta: facts.SnapshotMeta{RepoPath: "/Users/me/workspace"},
	})

	tests := []struct {
		name string
		fact facts.Fact
		want string
	}{
		{
			"multi-repo go-service",
			facts.Fact{File: "go-service/lib/foo.rb", Repo: "go-service"},
			filepath.Join("/Users/me/development/go-service", "lib/foo.rb"),
		},
		{
			"multi-repo ruby-monolith",
			facts.Fact{File: "ruby-monolith/lib/bar.rb", Repo: "ruby-monolith"},
			filepath.Join("/Users/me/development/ruby-monolith", "lib/bar.rb"),
		},
		{
			"no repo label falls back to snapshot",
			facts.Fact{File: "internal/server.go"},
			filepath.Join("/Users/me/workspace", "internal/server.go"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eng.ResolveFactFile(&tt.fact)
			if got != tt.want {
				t.Errorf("ResolveFactFile = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLinkCrossRepo_ConnectsServicesInGraph verifies that the cross-repo linking
// step produces service nodes and dependency edges that make the graph traversable
// across repos, and that re-running it does not duplicate facts.
func TestLinkCrossRepo_ConnectsServicesInGraph(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)

	// Two repos: svc-alpha calls an endpoint svc-beta serves.
	eng.Store().Add(
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "svc-alpha",
			Props: map[string]any{"method": "GET", "role": "client"},
		},
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "svc-beta",
			Props: map[string]any{"method": "GET", "role": "server"},
		},
	)

	eng.linkCrossRepo()

	if got := eng.Store().ByKind(facts.KindService); len(got) != 2 {
		t.Fatalf("service nodes = %d, want 2", len(got))
	}
	depFacts, _ := eng.Store().QueryAdvanced(facts.QueryOpts{Prop: "type", PropValue: "cross_repo"})
	if len(depFacts) != 1 {
		t.Fatalf("cross_repo dep facts = %d, want 1", len(depFacts))
	}
	if depFacts[0].Repo != "svc-alpha" || depFacts[0].Name != "svc-alpha -> svc-beta" {
		t.Errorf("edge = %+v, want svc-alpha -> svc-beta", depFacts[0])
	}

	// The graph now connects the two service nodes.
	eng.Store().BuildGraph()
	g := eng.Store().Graph()
	res := g.Traverse("svc-alpha", "forward", nil, nil, 5, 100)
	reached := false
	for _, n := range res.Nodes {
		if n.Name == "svc-beta" {
			reached = true
		}
	}
	if !reached {
		t.Errorf("traverse from svc-alpha did not reach svc-beta: %+v", res.Nodes)
	}
	if path := g.FindPath("svc-alpha", "svc-beta", nil, 10); !path.Found {
		t.Errorf("FindPath(svc-alpha, svc-beta) not found")
	}

	// Idempotent: re-running linking keeps exactly one service node per repo
	// and one edge (no duplicates).
	eng.linkCrossRepo()
	if got := eng.Store().ByKind(facts.KindService); len(got) != 2 {
		t.Errorf("after relink, service nodes = %d, want 2", len(got))
	}
	deps2, _ := eng.Store().QueryAdvanced(facts.QueryOpts{Prop: "type", PropValue: "cross_repo"})
	if len(deps2) != 1 {
		t.Errorf("after relink, cross_repo dep facts = %d, want 1", len(deps2))
	}
}

// TestGenerateSnapshot_ConcurrentCallsSerialized verifies that the engine mutex
// prevents concurrent GenerateSnapshot calls from corrupting shared state.
func TestGenerateSnapshot_ConcurrentCallsSerialized(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)

	// Use a non-existent repo path — GenerateSnapshot will fail at walkRepo,
	// but that's fine: we're testing that concurrent calls don't panic or
	// produce a data race. The mutex should serialize them.
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = eng.GenerateSnapshot(context.Background(), t.TempDir(), false)
		}(i)
	}
	wg.Wait()

	// All calls should complete without panic. Errors from missing extractors
	// or empty repos are expected — the key thing is no race.
	for i, err := range errs {
		if err != nil {
			t.Logf("goroutine %d error (expected): %v", i, err)
		}
	}
}
