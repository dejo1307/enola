package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// Server wraps the MCP server and connects it to the snapshot engine.
type Server struct {
	mcp          *mcp.Server
	eng          *engine.Engine
	cfg          *config.Config
	startTime    time.Time
	toolCallback func(string)
}

// New creates a new MCP server wired to the given engine.
func New(eng *engine.Engine, cfg *config.Config) (*Server, error) {
	s := &Server{
		eng: eng,
		cfg: cfg,
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "enola",
		Version: Version,
	}, nil)

	s.mcp = mcpServer
	s.registerTools()

	return s, nil
}

// Run starts the MCP server on the stdio transport.
func (s *Server) Run(ctx context.Context) error {
	s.startTime = time.Now()
	log.Println("[server] starting MCP server on stdio transport")
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// SetToolCallback sets a callback invoked each time a tool is called.
// The callback receives the tool name. It is safe to call before Run().
func (s *Server) SetToolCallback(cb func(string)) {
	s.toolCallback = cb
}

// GetStartTime returns the time the server started (zero value if Run() hasn't been called).
func (s *Server) GetStartTime() time.Time {
	return s.startTime
}

// MCPServer returns the underlying MCP server so that enterprise (or third-party)
// code can register additional, license-gated tools alongside the OSS tools.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcp
}

// generateSnapshotArgs are the arguments for the generate_snapshot tool.
type generateSnapshotArgs struct {
	RepoPath string `json:"repo_path" jsonschema:"Path to the repository to analyze. Defaults to the configured repo path."`
	Append   bool   `json:"append,omitempty" jsonschema:"If true, keep existing facts and add new ones with repo-prefixed file paths (for multi-repo analysis). Default false."`
}

// queryFactsArgs are the arguments for the query_facts tool.
type queryFactsArgs struct {
	Kind      string `json:"kind,omitempty" jsonschema:"Filter by fact kind: module, symbol, route, storage, dependency, or service (service = a whole repo, used as a node in the cross-repo graph)"`
	File      string `json:"file,omitempty" jsonschema:"Filter by file path"`
	Name      string `json:"name,omitempty" jsonschema:"Filter by name using substring match"`
	Relation  string `json:"relation,omitempty" jsonschema:"Filter by relation kind: declares, imports, calls, implements, or depends_on"`
	Prop      string `json:"prop,omitempty" jsonschema:"Filter by property name (e.g. source, symbol_kind, exported, framework, storage_kind)"`
	PropValue string `json:"prop_value,omitempty" jsonschema:"Filter by property value (requires prop to be set)"`

	// Batch filters — OR within dimension, AND across dimensions
	Names      []string `json:"names,omitempty" jsonschema:"Filter by multiple exact names (OR). Use instead of name for batch lookups."`
	Files      []string `json:"files,omitempty" jsonschema:"Filter by multiple file paths (OR). Use instead of file for batch lookups."`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"Filter by multiple kinds (OR). Use instead of kind for batch lookups."`
	FilePrefix string   `json:"file_prefix,omitempty" jsonschema:"Filter by file path prefix (e.g. internal/server to match all files in that directory)"`
	Repo       string   `json:"repo,omitempty" jsonschema:"Filter by repository label (set in multi-repo/append mode, e.g. 'go-service')"`

	// Pagination
	Offset int `json:"offset,omitempty" jsonschema:"Number of results to skip for pagination. Default 0."`
	Limit  int `json:"limit,omitempty" jsonschema:"Maximum number of results to return (1-500). Default 100."`

	// Relation expansion
	IncludeRelated bool `json:"include_related,omitempty" jsonschema:"If true, inline the full fact data for each relation target instead of just the target name"`

	// Output format
	OutputMode string `json:"output_mode,omitempty" jsonschema:"Output format: 'full' (default JSON), 'compact' (markdown table), or 'names' (just names and files)"`
}

// enrichedFact wraps a Fact with resolved relation targets.
type enrichedFact struct {
	facts.Fact
	RelatedFacts []facts.Fact `json:"related_facts,omitempty"`
}

// queryResponse is the structured response for query_facts when advanced features are used.
type queryResponse struct {
	Facts   any  `json:"facts"`
	Total   int  `json:"total"`
	Offset  int  `json:"offset"`
	Limit   int  `json:"limit"`
	HasMore bool `json:"has_more"`
}

// renderCompact formats facts as a markdown table for minimal token usage.
func renderCompact(results []facts.Fact, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results (showing %d):\n\n", total, len(results)))
	sb.WriteString("| Kind | Name | File | Line |\n")
	sb.WriteString("|------|------|------|------|\n")
	for _, f := range results {
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n", f.Kind, f.Name, f.File, f.Line))
	}
	return sb.String()
}

// renderNamesOnly returns just names and files, one per line.
func renderNamesOnly(results []facts.Fact, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results (showing %d):\n\n", total, len(results)))
	for _, f := range results {
		sb.WriteString(fmt.Sprintf("%s  %s:%d\n", f.Name, f.File, f.Line))
	}
	return sb.String()
}

// registerTools adds MCP tools for snapshot generation and fact querying.
func (s *Server) registerTools() {
	// Tool: generate_snapshot
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "generate_snapshot",
		Description: "Index a repository and extract its architecture as queryable facts. " +
			"Supports Go, TypeScript, Kotlin, Ruby, Python, Swift, and OpenAPI. " +
			"Produces facts of kind: module, symbol, route, storage, dependency, service. " +
			"Run this first before any other tool. Re-run after code changes. " +
			"In multi-repo mode, call with append=true for each additional repo after the first; " +
			"enola auto-enables append when it detects you have switched to a different repo.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args generateSnapshotArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("generate_snapshot")
		}
		repoPath := args.RepoPath
		if repoPath == "" {
			repoPath = s.cfg.Repo
		}

		absRepo, err := filepath.Abs(repoPath)
		if err != nil {
			return errorResult(fmt.Sprintf("invalid repo path: %v", err)), nil, nil
		}

		// Auto-enable append mode when switching to a different repo
		// while facts from another repo are already loaded.
		appendMode := args.Append
		autoAppended := false
		if !appendMode && s.eng.Store().Count() > 0 && s.eng.Snapshot() != nil {
			prevRepo := s.eng.Snapshot().Meta.RepoPath
			if prevRepo != "" && prevRepo != absRepo {
				appendMode = true
				autoAppended = true
				log.Printf("[server] auto-enabled append mode: switching from %s to %s", prevRepo, absRepo)
			}
		}

		snapshot, err := s.eng.GenerateSnapshot(ctx, absRepo, appendMode)
		if err != nil {
			return errorResult(fmt.Sprintf("snapshot generation failed: %v", err)), nil, nil
		}

		// Write artifacts to disk
		if err := s.eng.WriteArtifacts(absRepo); err != nil {
			log.Printf("[server] warning: failed to write artifacts: %v", err)
		}

		// Return summary
		summary := fmt.Sprintf(
			"Snapshot generated successfully.\n\n"+
				"- Repository: %s\n"+
				"- Facts: %d\n"+
				"- Insights: %d\n"+
				"- Artifacts: %d\n"+
				"- Duration: %s\n"+
				"- Extractors: %v\n"+
				"- Explainers: %v\n\n"+
				"Use query_facts or explore to inspect the extracted architecture.",
			snapshot.Meta.RepoPath,
			snapshot.Meta.FactCount,
			snapshot.Meta.InsightCount,
			len(snapshot.Artifacts),
			snapshot.Meta.Duration,
			snapshot.Meta.Extractors,
			snapshot.Meta.Explainers,
		)

		if appendMode {
			repoLabel := filepath.Base(absRepo)
			autoNote := ""
			if autoAppended {
				autoNote = " (auto-enabled: different repo detected)"
			}
			summary += fmt.Sprintf(
				"\n\n**Multi-repo mode active%s.** Repo label: %q\n"+
					"- Filter by repo: query_facts(repo=%q)\n"+
					"- File paths are prefixed: e.g. %s/src/...\n"+
					"- Generate additional repos with append=true (sequentially, not in parallel).",
				autoNote, repoLabel, repoLabel, repoLabel,
			)

			// Report the cross-repo "graph of graphs" links derived from this set.
			crossEdges, _ := s.eng.Store().QueryAdvanced(facts.QueryOpts{
				Kind: facts.KindDependency, Prop: "type", PropValue: "cross_repo", Limit: 500,
			})
			services := s.eng.Store().ByKind(facts.KindService)
			summary += fmt.Sprintf(
				"\n- **Cross-repo graph:** %d service node(s), %d cross-repo dependency edge(s). "+
					"Traverse between repos with traverse(start=%q) / find_path, list edges with "+
					"query_facts(kind=\"service\") or query_facts(prop=\"type\", prop_value=\"cross_repo\").",
				len(services), len(crossEdges), repoLabel,
			)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: summary},
			},
		}, nil, nil
	})

	// Tool: query_facts
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "query_facts",
		Description: "Precision filter over extracted facts. Use after explore when you need specific subsets — " +
			"e.g. all symbols in a file, all external dependencies, all routes. " +
			"Fact kinds: module, symbol, route, storage, dependency, service. " +
			"name= is a substring match; names= is exact (batch). files= and kinds= are OR filters; combined with other fields they are AND. " +
			"output_mode='compact' or 'names' saves tokens for large result sets. " +
			"For dependencies, set prop='source' prop_value='internal'|'external'|'stdlib' to filter noise. " +
			"Supports pagination via offset/limit (default 100, max 500).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryFactsArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("query_facts")
		}
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		// Normalize absolute filesystem paths to store-relative paths.
		normFile := s.normalizeToRelative(args.File)
		normPrefix := s.normalizeToRelative(args.FilePrefix)
		var normFiles []string
		for _, f := range args.Files {
			normFiles = append(normFiles, s.normalizeToRelative(f))
		}

		// In multi-repo mode, expand the file prefix to include repo labels
		// if the user provided a bare relative path (e.g. "src/" instead of "golf-ui/src/").
		prefixes := s.expandFilePrefix(normPrefix)

		// Query with the first (or only) prefix.
		opts := facts.QueryOpts{
			Kind:       args.Kind,
			Kinds:      args.Kinds,
			File:       normFile,
			Files:      normFiles,
			FilePrefix: prefixes[0],
			Name:       args.Name,
			Names:      args.Names,
			Repo:       args.Repo,
			RelKind:    args.Relation,
			Prop:       args.Prop,
			PropValue:  args.PropValue,
			Offset:     args.Offset,
			Limit:      args.Limit,
		}

		results, total := store.QueryAdvanced(opts)

		// If multiple repo labels matched, merge results from additional prefixes.
		for _, p := range prefixes[1:] {
			opts.FilePrefix = p
			extra, extraTotal := store.QueryAdvanced(opts)
			results = append(results, extra...)
			total += extraTotal
		}

		// Compact output modes: return text instead of JSON
		switch args.OutputMode {
		case "compact":
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: renderCompact(results, total)},
				},
			}, nil, nil
		case "names":
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: renderNamesOnly(results, total)},
				},
			}, nil, nil
		}

		// Determine if advanced features are in use (triggers structured response)
		useAdvanced := args.IncludeRelated || args.Offset > 0 || args.Limit > 0 ||
			len(args.Names) > 0 || len(args.Files) > 0 || len(args.Kinds) > 0 ||
			args.FilePrefix != "" || args.Repo != ""

		// Enrich with related facts if requested
		var output any
		if args.IncludeRelated {
			enriched := make([]enrichedFact, len(results))
			seen := make(map[string]struct{}) // deduplicate related facts
			for i, f := range results {
				enriched[i] = enrichedFact{Fact: f}
				for _, rel := range f.Relations {
					if _, dup := seen[rel.Target]; dup {
						continue
					}
					seen[rel.Target] = struct{}{}
					related := store.LookupByExactName(rel.Target)
					enriched[i].RelatedFacts = append(enriched[i].RelatedFacts, related...)
				}
			}
			output = enriched
		} else {
			output = results
		}

		if useAdvanced {
			limit := args.Limit
			if limit <= 0 {
				limit = 100
			}
			if limit > 500 {
				limit = 500
			}
			resp := queryResponse{
				Facts:   output,
				Total:   total,
				Offset:  args.Offset,
				Limit:   limit,
				HasMore: total > args.Offset+len(results),
			}
			data, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				return errorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: string(data)},
				},
			}, nil, nil
		}

		// Legacy format: raw JSON array (backwards compatible)
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return errorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
		}

		text := string(data)
		if total > len(results) {
			text += fmt.Sprintf("\n\n... (showing %d of %d results, refine your query or use offset/limit for pagination)", len(results), total)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: text},
			},
		}, nil, nil
	})

	// Tool: show_symbol
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "show_symbol",
		Description: "Return the source code implementation of a named symbol. " +
			"Prefers exact name match; falls back to substring match and returns up to 5 results. " +
			"Default context: 60 lines (asymmetric: ~15 before declaration, ~45 after). " +
			"Use context_lines to widen or narrow the window. " +
			"Works in both single-repo and multi-repo (append) mode.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args showSymbolArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("show_symbol")
		}
		snapshot := s.eng.Snapshot()
		if snapshot == nil {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}

		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if args.Name == "" {
			return errorResult("name is required"), nil, nil
		}

		// Prefer exact match to avoid substring noise (e.g. "Transaction" matching "AutoTransactionsTogglePatch").
		results := store.LookupByExactName(args.Name)
		// Filter to symbols only
		symbolResults := results[:0]
		for _, r := range results {
			if r.Kind == facts.KindSymbol {
				symbolResults = append(symbolResults, r)
			}
		}
		results = symbolResults
		if len(results) == 0 {
			results = store.Query("symbol", "", args.Name, "")
		}
		if len(results) == 0 {
			return errorResult(fmt.Sprintf("No symbols matching %q", args.Name)), nil, nil
		}

		contextLines := args.ContextLines
		if contextLines <= 0 {
			contextLines = 60
		}

		// Limit to 5 results
		if len(results) > 5 {
			results = results[:5]
		}

		var sb strings.Builder

		for i, fact := range results {
			if i > 0 {
				sb.WriteString("\n---\n\n")
			}

			// Header
			sb.WriteString(fmt.Sprintf("### %s\n", fact.Name))
			sb.WriteString(fmt.Sprintf("File: %s  Line: %d\n", fact.File, fact.Line))

			// Show props summary
			if sig, ok := fact.Props["signature"].(string); ok {
				sb.WriteString(fmt.Sprintf("Signature:\n```\n%s\n```\n", sig))
			}
			if comp, ok := fact.Props["ios_component"].(string); ok {
				sb.WriteString(fmt.Sprintf("iOS Component: %s\n", comp))
			}

			sb.WriteString("\n")

			// Read source file (handles both single-repo and multi-repo paths)
			absFile := s.eng.ResolveFactFile(&fact)
			source, err := readSourceWindow(absFile, fact.Line, contextLines)
			if err != nil {
				sb.WriteString(fmt.Sprintf("_Could not read source: %v_\n", err))
				continue
			}

			lang := "go"
			if l, ok := fact.Props["language"].(string); ok && l != "" {
				lang = l
			}
			sb.WriteString(fmt.Sprintf("```%s\n%s\n```\n", lang, source))
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})

	// Tool: explore
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "explore",
		Description: "Primary exploration tool — use this first after generate_snapshot. " +
			"Given a module name, file path, symbol name, or directory prefix, returns a structured markdown summary: " +
			"symbols (with kinds and line numbers), direct dependencies, reverse dependents, and at depth=2 symbol-level relations. " +
			"'Module' means a package-level grouping (e.g. a Go package or TypeScript file group), not a repo. " +
			"Accepts absolute filesystem paths — they are normalised automatically. " +
			"Use query_facts for precise filtering, traverse for multi-hop graph walks.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("explore")
		}
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if args.Focus == "" {
			return errorResult("focus is required"), nil, nil
		}

		depth := args.Depth
		if depth <= 0 {
			depth = 1
		}
		if depth > 2 {
			depth = 2
		}

		var sb strings.Builder

		// Normalize absolute filesystem paths to store-relative paths.
		focus := s.normalizeToRelative(args.Focus)

		// Try to determine focus type by matching against store indexes.
		// Priority: exact module name > exact file > symbol name substring > file prefix (directory)
		// Special case: "." means the repo root (from normalizing an absolute path that
		// equals the snapshot RepoPath). Route directly to directory exploration to avoid
		// "." accidentally substring-matching dotted symbol names.
		switch {
		case focus == "." && s.exploreDirectory(store, focus, &sb):
		case focus != "." && s.exploreModule(store, focus, depth, &sb):
		case focus != "." && s.exploreModuleSubstring(store, focus, depth, &sb):
		case focus != "." && s.exploreFile(store, focus, depth, &sb):
		case focus != "." && s.exploreSymbol(store, focus, depth, &sb):
		case s.exploreDirectory(store, focus, &sb):
		default:
			return errorResult(fmt.Sprintf("No facts matching focus %q. Try a module name, file path, symbol name, or directory prefix.", focus)), nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})

	// Tool: traverse
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "traverse",
		Description: "Walk the dependency/call graph from a starting node. " +
			"direction='forward' answers \"what does X depend on?\"; direction='reverse' answers \"what depends on X?\". " +
			"start= accepts substring match plus scoped prefixes (repo:, kind:, file:) to disambiguate; returns ranked candidates with confidence when ambiguous. " +
			"relation_kinds filter: imports, calls, declares, implements, depends_on, has_method. " +
			"Forward traversal from a struct/interface follows has_method edges to its methods (and then their calls). " +
			"Note: interface method calls cannot be statically bound to a concrete implementation, so such call edges may be absent or appear as unresolved nodes. " +
			"node_kinds filters output (not traversal itself): module, symbol, dependency, route, storage. " +
			"Returns a compact markdown summary grouped by depth (output_mode='full' for the raw JSON node/edge graph). " +
			"Defaults: depth=5, max_nodes=100. Use instead of repeated explore calls for transitive relationships.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args traverseArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("traverse")
		}
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.Start == "" {
			return errorResult("start is required"), nil, nil
		}

		// Resolve start name: try exact match first, then substring
		startName, res, err := s.resolveNodeName(store, args.Start)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		// Over threshold: refuse to guess; return resolution with empty results.
		if res != nil && res.Matched == "" {
			resp := traverseResponse{
				Resolution: res,
				TraversalResult: facts.TraversalResult{
					Nodes: []facts.TraversalNode{},
					Edges: []facts.TraversalEdge{},
				},
			}
			if wantsFullOutput(args.OutputMode) {
				return jsonResult(resp)
			}
			return textResult(renderTraverseCompact(resp, args.Start, "")), nil, nil
		}

		direction := args.Direction
		if direction == "" {
			direction = "forward"
		}
		if direction != "forward" && direction != "reverse" {
			return errorResult("direction must be 'forward' or 'reverse'"), nil, nil
		}

		result := graph.Traverse(startName, direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)

		resp := traverseResponse{Resolution: res, TraversalResult: result}
		if wantsFullOutput(args.OutputMode) {
			return jsonResult(resp)
		}
		return textResult(renderTraverseCompact(resp, startName, direction)), nil, nil
	})

	// Tool: find_path
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "find_path",
		Description: "Find the shortest path (BFS, by hop count) between two nodes in the architectural graph. " +
			"Answers \"how does X reach Y?\" or \"what is the call chain from A to B?\". " +
			"from= and to= use substring match with smart disambiguation, and accept scoped prefixes " +
			"(repo:, kind:, file:) to pin down an ambiguous name, e.g. from=\"repo:go-auth Login\". " +
			"When a name is ambiguous the response carries a resolution object with ranked candidates and a " +
			"confidence score; one dominant candidate (>80% confidence) is auto-resolved so the path is still returned. " +
			"Returns an ordered list of nodes and edges, or reports no path found within max_depth hops.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args findPathArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("find_path")
		}
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.From == "" || args.To == "" {
			return errorResult("both 'from' and 'to' are required"), nil, nil
		}

		fromName, fromRes, err := s.resolveNodeName(store, args.From)
		if err != nil {
			return errorResult(fmt.Sprintf("from: %v", err)), nil, nil
		}
		toName, toRes, err := s.resolveNodeName(store, args.To)
		if err != nil {
			return errorResult(fmt.Sprintf("to: %v", err)), nil, nil
		}
		// If either endpoint was too ambiguous to resolve, refuse to guess and
		// return the resolution(s) with an empty path.
		if (fromRes != nil && fromRes.Matched == "") || (toRes != nil && toRes.Matched == "") {
			return jsonResult(findPathResponse{
				FromResolution: fromRes,
				ToResolution:   toRes,
				PathResult:     facts.PathResult{From: fromName, To: toName, Found: false},
			})
		}

		result := graph.FindPath(fromName, toName, args.RelationKinds, args.MaxDepth)

		return jsonResult(findPathResponse{
			FromResolution: fromRes,
			ToResolution:   toRes,
			PathResult:     result,
		})
	})

	// Tool: impact_analysis
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "impact_analysis",
		Description: "Compute the blast radius of changing a target node: all nodes that transitively depend on it, grouped by hop depth. " +
			"Use for refactoring planning and change risk assessment. " +
			"target= uses substring match with smart disambiguation. " +
			"Default: reverse direction only (what breaks if target changes). " +
			"Set include_forward=true to also see what the target itself depends on (useful for understanding what could break the target). " +
			"Returns a compact markdown summary grouped by hop depth, with an accurate total dependent count (output_mode='full' for the raw JSON). " +
			"Defaults: max_depth=3, max_nodes=200.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args impactAnalysisArgs) (*mcp.CallToolResult, any, error) {
		if s.toolCallback != nil {
			s.toolCallback("impact_analysis")
		}
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.Target == "" {
			return errorResult("target is required"), nil, nil
		}

		targetName, res, err := s.resolveNodeName(store, args.Target)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		// Over threshold: refuse to guess; return resolution with empty results.
		if res != nil && res.Matched == "" {
			resp := impactResponse{
				Resolution: res,
				ImpactResult: facts.ImpactResult{
					Target:  args.Target,
					ByDepth: map[int][]facts.TraversalNode{},
					Edges:   []facts.TraversalEdge{},
				},
			}
			if wantsFullOutput(args.OutputMode) {
				return jsonResult(resp)
			}
			return textResult(renderImpactCompact(resp)), nil, nil
		}

		result := graph.ImpactSet(targetName, args.MaxDepth, args.MaxNodes, args.IncludeForward)

		resp := impactResponse{Resolution: res, ImpactResult: result}
		if wantsFullOutput(args.OutputMode) {
			return jsonResult(resp)
		}
		return textResult(renderImpactCompact(resp)), nil, nil
	})
}

// ambiguousMatchThreshold is the candidate count at or above which
// resolveNodeName refuses to guess and forces the caller to re-invoke with an
// exact name.
const ambiguousMatchThreshold = 3

// maxAlternatives caps how many candidate names are echoed back in a
// nameResolution so the response stays readable.
const maxAlternatives = 10

// maxCandidates caps how many ranked, scored candidates are surfaced in a
// nameResolution.
const maxCandidates = 3

// autoPickConfidence is the pickConfidence threshold above which an ambiguous
// resolution (at or above ambiguousMatchThreshold matches) is resolved
// automatically to its top-scoring candidate instead of being refused.
const autoPickConfidence = 0.80

// nameResolution reports how a user-provided name was resolved to a concrete
// fact name. It is surfaced in tool responses ONLY when the input matched more
// than one fact (i.e. Ambiguous is true), so callers can detect and correct a
// possibly-wrong pick. Matched is empty when the match count crossed
// ambiguousMatchThreshold and no candidate scored confidently enough to
// auto-pick; the caller should then choose from Candidates (optionally using the
// repo:/kind:/file: scope prefixes) or re-invoke with an exact name.
type nameResolution struct {
	Query        string            `json:"query"`
	Matched      string            `json:"matched,omitempty"`
	Alternatives []string          `json:"alternatives,omitempty"`
	Candidates   []scoredCandidate `json:"candidates,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	AutoPicked   bool              `json:"auto_picked,omitempty"`
	Ambiguous    bool              `json:"ambiguous"`
}

// resolveNodeName resolves a user-provided name to an exact fact name.
//
// The input may carry scope prefixes — repo:<label>, kind:<k>, file:<prefix> —
// to disambiguate an otherwise-ambiguous term (see parseScopedQuery). A plain
// input keeps the legacy substring-match behavior.
//
// It returns the resolved name and, when resolution was ambiguous (more than one
// fact matched and no confident pick existed), a non-nil *nameResolution
// describing the ambiguity (ranked Candidates with scores + a Confidence). For
// exact matches, single matches, and confident suffix-exact matches the
// resolution is nil.
//
// When the candidate count reaches ambiguousMatchThreshold the method picks the
// top-scoring candidate only if its pickConfidence exceeds autoPickConfidence;
// otherwise it returns an empty name with a resolution-only response so the
// caller can choose from Candidates or re-invoke with a scoped/exact name.
func (s *Server) resolveNodeName(store *facts.Store, input string) (string, *nameResolution, error) {
	query := input
	sq := parseScopedQuery(input)
	scoped := sq.Repo != "" || len(sq.Kinds) > 0 || sq.FilePrefix != "" || sq.SymbolKind != ""

	term := s.normalizeToRelative(sq.Term)

	// Try exact match first (unscoped only — a scope filter signals the caller
	// wants the candidate set narrowed, not bypassed).
	if !scoped {
		if exact := store.LookupByExactName(term); len(exact) > 0 {
			return exact[0].Name, nil, nil
		}
	}

	results := s.gatherCandidates(store, sq, term)
	if len(results) == 0 {
		// Fallback 1: exact name match on KindService facts (service nodes are
		// named after repo labels and are often missed by substring search).
		for _, svc := range store.ByKind(facts.KindService) {
			if strings.EqualFold(svc.Name, term) {
				return svc.Name, nil, nil
			}
		}
		// Fallback 2: input matches a known repo label whose service node may
		// have an empty name (primary repo) or hasn't been loaded yet.
		if name, ok := s.resolveRepoLabelToServiceNode(store, term); ok {
			return name, nil, nil
		}
		return "", nil, fmt.Errorf("no facts matching %q", query)
	}
	if len(results) == 1 {
		return results[0].Name, nil, nil
	}

	// A service node whose name exactly matches the term is a confident pick.
	for _, svc := range store.ByKind(facts.KindService) {
		if strings.EqualFold(svc.Name, term) {
			return svc.Name, nil, nil
		}
	}

	// Multiple matches: rank by relevance score and judge how decisively the top
	// candidate wins.
	ranked := rankCandidates(results, sq)
	confidence := pickConfidence(ranked, sq.Term)
	top := ranked[0].Name

	// One candidate clearly dominates (e.g. a unique suffix-exact name among
	// substring matches). Auto-resolve to it, but surface the resolution with its
	// confidence and the alternatives so the caller can see — and override — the
	// pick rather than it being silent.
	if confidence > autoPickConfidence {
		return top, &nameResolution{
			Query:        query,
			Matched:      top,
			Alternatives: candidateNames(results, top),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			AutoPicked:   true,
			Ambiguous:    true,
		}, nil
	}

	// Below the ambiguity threshold, return the best guess (flagged ambiguous),
	// preserving the long-standing "small ambiguity → pick anyway" behavior.
	if len(results) < ambiguousMatchThreshold {
		return top, &nameResolution{
			Query:        query,
			Matched:      top,
			Alternatives: candidateNames(results, top),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			Ambiguous:    true,
		}, nil
	}

	// Too ambiguous to guess: surface ranked candidates and refuse to pick.
	return "", &nameResolution{
		Query:        query,
		Alternatives: candidateNames(results, ""),
		Candidates:   topCandidates(ranked),
		Confidence:   confidence,
		Ambiguous:    true,
	}, nil
}

// gatherCandidates returns the facts matching a (possibly scoped) query. Unscoped
// inputs use the legacy substring Query to preserve existing semantics. Scoped
// inputs use QueryAdvanced with repo/kind/file-prefix filters (expanding the file
// prefix across repos in multi-repo mode) and an optional symbol_kind post-filter.
func (s *Server) gatherCandidates(store *facts.Store, sq scopedQuery, term string) []facts.Fact {
	scoped := sq.Repo != "" || len(sq.Kinds) > 0 || sq.FilePrefix != "" || sq.SymbolKind != ""
	if !scoped {
		return store.Query("", "", term, "")
	}

	prefixes := []string{""}
	if sq.FilePrefix != "" {
		prefixes = s.expandFilePrefix(sq.FilePrefix)
	}

	seen := make(map[string]struct{})
	var out []facts.Fact
	for _, pfx := range prefixes {
		res, _ := store.QueryAdvanced(facts.QueryOpts{
			Repo:       sq.Repo,
			Kinds:      sq.Kinds,
			FilePrefix: pfx,
			Name:       term,
			Limit:      500,
		})
		for _, f := range res {
			if sq.SymbolKind != "" {
				if sk, _ := f.Props["symbol_kind"].(string); sk != sq.SymbolKind {
					continue
				}
			}
			if _, dup := seen[f.Name]; dup {
				continue
			}
			seen[f.Name] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

// topCandidates returns up to maxCandidates ranked candidates.
func topCandidates(ranked []scoredCandidate) []scoredCandidate {
	if len(ranked) > maxCandidates {
		return ranked[:maxCandidates]
	}
	return ranked
}

// resolveRepoLabelToServiceNode maps a user-supplied label to the corresponding
// KindService fact name. Handles appended repos (label in RepoPaths) and the
// primary repo (base name of Snapshot.Meta.RepoPath), whose service node has
// Repo == "" and Name == "".
func (s *Server) resolveRepoLabelToServiceNode(store *facts.Store, input string) (string, bool) {
	if s.eng == nil {
		return "", false
	}
	services := store.ByKind(facts.KindService)
	inputLower := strings.ToLower(input)

	for label := range s.eng.RepoPaths() {
		if strings.ToLower(label) == inputLower {
			for _, svc := range services {
				if strings.ToLower(svc.Repo) == inputLower || strings.ToLower(svc.Name) == inputLower {
					return svc.Name, true
				}
			}
		}
	}

	if snap := s.eng.Snapshot(); snap != nil {
		if strings.ToLower(filepath.Base(snap.Meta.RepoPath)) == inputLower {
			for _, svc := range services {
				if svc.Repo == "" {
					return svc.Name, true
				}
			}
		}
	}

	return "", false
}

// candidateNames collects up to maxAlternatives fact names from results,
// preserving store order and excluding exclude (the chosen match) when set.
func candidateNames(results []facts.Fact, exclude string) []string {
	names := make([]string, 0, len(results))
	for _, r := range results {
		if r.Name == exclude {
			continue
		}
		names = append(names, r.Name)
		if len(names) >= maxAlternatives {
			break
		}
	}
	return names
}

// exploreArgs are the arguments for the explore tool.
type exploreArgs struct {
	Focus string `json:"focus" jsonschema:"required,Module name, file path, or symbol name to explore"`
	Depth int    `json:"depth,omitempty" jsonschema:"How deep to follow relations (1=direct only, 2=include relations of relations). Default 1, max 2."`
}

// traverseArgs are the arguments for the traverse tool.
type traverseArgs struct {
	Start         string   `json:"start" jsonschema:"required,Starting node name (fact name, module name, or symbol name). Substring match; supports scoped prefixes repo:/kind:/file: to disambiguate (e.g. 'repo:go-auth kind:struct AuthHandler')."`
	Direction     string   `json:"direction,omitempty" jsonschema:"'forward' follows outgoing relations (what does X depend on?), 'reverse' follows incoming relations (what depends on X?). Default: forward."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Filter to specific relation types: imports, calls, declares, implements, depends_on, has_method. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum traversal depth (1-20). Default: 5."`
	MaxNodes      int      `json:"max_nodes,omitempty" jsonschema:"Maximum nodes to return (1-500). Traversal stops when this limit is reached. Default: 100."`
	NodeKinds     []string `json:"node_kinds,omitempty" jsonschema:"Filter results to specific fact kinds: module, symbol, dependency, route, storage. Default: all."`
	OutputMode    string   `json:"output_mode,omitempty" jsonschema:"'compact' (default) returns a readable markdown summary grouped by depth; 'full' returns the complete JSON node/edge graph (can be large)."`
}

// findPathArgs are the arguments for the find_path tool.
type findPathArgs struct {
	From          string   `json:"from" jsonschema:"required,Source node name. Substring match; supports scoped prefixes repo:/kind:/file: to disambiguate (e.g. 'repo:go-auth Login')."`
	To            string   `json:"to" jsonschema:"required,Target node name. Substring match; supports scoped prefixes repo:/kind:/file: to disambiguate (e.g. 'kind:struct AuthMiddleware')."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Filter to specific relation types. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum path length to search (1-20). Default: 10."`
}

// impactAnalysisArgs are the arguments for the impact_analysis tool.
type impactAnalysisArgs struct {
	Target         string `json:"target" jsonschema:"required,The node being changed (fact name, substring match). Supports scoped prefixes repo:/kind:/file: to disambiguate."`
	MaxDepth       int    `json:"max_depth,omitempty" jsonschema:"How many hops of impact to compute (1-10). Default: 3."`
	MaxNodes       int    `json:"max_nodes,omitempty" jsonschema:"Maximum impacted nodes to return (1-500). Default: 200."`
	IncludeForward bool   `json:"include_forward,omitempty" jsonschema:"Include what the target depends on (what might break the target). Default: false."`
	OutputMode     string `json:"output_mode,omitempty" jsonschema:"'compact' (default) returns a readable markdown summary grouped by depth; 'full' returns the complete JSON by_depth/edges graph (can be large)."`
}

// exploreModule renders a module exploration if the focus matches a module name.
func (s *Server) exploreModule(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	modules := store.LookupByExactName(focus)
	// Filter to only module-kind facts
	var mod *facts.Fact
	for i := range modules {
		if modules[i].Kind == facts.KindModule {
			mod = &modules[i]
			break
		}
	}
	if mod == nil {
		return false
	}

	sb.WriteString(fmt.Sprintf("# Module: %s\n\n", mod.Name))

	// Props summary
	if lang, ok := mod.Props["language"].(string); ok {
		sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
	}
	if pkg, ok := mod.Props["package"].(string); ok {
		sb.WriteString(fmt.Sprintf("- Package: %s\n", pkg))
	}
	sb.WriteString("\n")

	// Find symbols declared in this module (symbols whose "declares" relation targets this module)
	declaredSymbols := store.ReverseLookup(mod.Name, facts.RelDeclares)
	if len(declaredSymbols) > 0 {
		sb.WriteString(fmt.Sprintf("## Symbols (%d)\n\n", len(declaredSymbols)))
		sb.WriteString("| Name | Kind | File | Line | Exported |\n")
		sb.WriteString("|------|------|------|------|----------|\n")
		for _, sym := range declaredSymbols {
			symKind, _ := sym.Props["symbol_kind"].(string)
			exported := "no"
			if exp, ok := sym.Props["exported"].(bool); ok && exp {
				exported = "yes"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %s |\n",
				sym.Name, symKind, sym.File, sym.Line, exported))
		}
		sb.WriteString("\n")
	}

	// Dependencies: facts with kind=dependency whose file starts with the module path,
	// plus direct depends_on relations from the module fact itself (packwerk).
	deps, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindDependency, FilePrefix: mod.Name + "/"})
	// Collect all dependency targets grouped by relation kind.
	depsByKind := make(map[string][]string) // relKind → targets
	seen := make(map[string]struct{})
	for _, dep := range deps {
		for _, r := range dep.Relations {
			key := r.Kind + ":" + r.Target
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			depsByKind[r.Kind] = append(depsByKind[r.Kind], r.Target)
		}
	}
	// Also include the module's own depends_on relations (from packwerk).
	for _, r := range mod.Relations {
		key := r.Kind + ":" + r.Target
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		depsByKind[r.Kind] = append(depsByKind[r.Kind], r.Target)
	}
	totalDeps := 0
	for _, targets := range depsByKind {
		totalDeps += len(targets)
	}
	if totalDeps > 0 {
		sb.WriteString(fmt.Sprintf("## Dependencies (%d)\n\n", totalDeps))
		for _, relKind := range []string{facts.RelDependsOn, facts.RelImports, facts.RelImplements} {
			targets := depsByKind[relKind]
			if len(targets) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s (%d)\n\n", capitalize(relKind), len(targets)))
			for _, t := range targets {
				sb.WriteString(fmt.Sprintf("- %s\n", t))
			}
			sb.WriteString("\n")
		}
	}

	// Reverse dependencies: who depends on or imports this module
	dependents := store.ReverseLookup(mod.Name, facts.RelImports)
	revDeps := store.ReverseLookup(mod.Name, facts.RelDependsOn)
	allDependents := append(dependents, revDeps...)
	if len(allDependents) > 0 {
		depSeen := make(map[string]struct{})
		sb.WriteString(fmt.Sprintf("## Dependents (%d)\n\n", len(allDependents)))
		for _, dep := range allDependents {
			if _, dup := depSeen[dep.Name]; dup {
				continue
			}
			depSeen[dep.Name] = struct{}{}
			sb.WriteString(fmt.Sprintf("- %s\n", dep.Name))
		}
		sb.WriteString("\n")
	}

	// Nested subtree: modules and symbols beneath this directory. TypeScript/JS
	// (and any per-directory module language) nests modules per directory, so a
	// directory that is itself a module often has a large subtree of child
	// modules whose symbols would otherwise be hidden by this exact-module match.
	s.writeNestedModules(store, mod.Name, len(declaredSymbols), sb)

	// If depth=2, show key symbol relations
	if depth >= 2 && len(declaredSymbols) > 0 {
		sb.WriteString("## Symbol Relations\n\n")
		limit := len(declaredSymbols)
		if limit > 20 {
			limit = 20
		}
		for _, sym := range declaredSymbols[:limit] {
			if len(sym.Relations) <= 1 {
				continue // skip symbols with only a "declares" relation
			}
			sb.WriteString(fmt.Sprintf("**%s**\n", sym.Name))
			for _, r := range sym.Relations {
				if r.Kind == facts.RelDeclares {
					continue
				}
				sb.WriteString(fmt.Sprintf("  - %s → %s\n", r.Kind, r.Target))
			}
			sb.WriteString("\n")
		}
	}

	return true
}

// writeNestedModules appends a summary of the modules and symbols nested beneath
// modName (i.e. facts whose file path is under "<modName>/"). It lets a directory
// that is itself a module surface its descendant modules and an aggregate symbol
// count, instead of stopping at the module's own directly-declared symbols.
func (s *Server) writeNestedModules(store *facts.Store, modName string, directSymbols int, sb *strings.Builder) {
	prefix := modName + "/"

	// Accurate totals (QueryAdvanced returns the pre-limit total). Symbols declared
	// directly in this module also live under "<modName>/" (e.g. src/app/layout.tsx),
	// so subtract them to count only symbols in nested child modules.
	_, symTotal := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindSymbol, FilePrefix: prefix, Limit: 1})
	nestedSymbols := symTotal - directSymbols
	if nestedSymbols < 0 {
		nestedSymbols = 0
	}
	modFacts, modTotal := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindModule, FilePrefix: prefix, Limit: 500})
	if modTotal == 0 && nestedSymbols == 0 {
		return
	}

	// Group descendant modules by their immediate child segment under modName,
	// counting how many modules live under each child.
	childCounts := make(map[string]int)
	for _, m := range modFacts {
		rest := strings.TrimPrefix(m.Name, prefix)
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg = rest[:i]
		}
		if seg == "" {
			continue
		}
		childCounts[seg]++
	}

	children := make([]string, 0, len(childCounts))
	for seg := range childCounts {
		children = append(children, seg)
	}
	sort.Strings(children)

	sb.WriteString(fmt.Sprintf("## Nested modules (%d)\n\n", len(children)))
	sb.WriteString(fmt.Sprintf("Subtree: %d modules, %d symbols\n\n", modTotal, nestedSymbols))

	const maxChildren = 50
	shown := children
	if len(shown) > maxChildren {
		shown = shown[:maxChildren]
	}
	for _, seg := range shown {
		full := prefix + seg
		if n := childCounts[seg]; n > 1 {
			sb.WriteString(fmt.Sprintf("- %s (%d modules)\n", full, n))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", full))
		}
	}
	if len(children) > maxChildren {
		sb.WriteString(fmt.Sprintf("\n... and %d more nested modules\n", len(children)-maxChildren))
	}
	sb.WriteString("\nUse explore on a nested module above to drill in.\n\n")
}

// exploreModuleSubstring tries substring matching on module names when exact
// module match fails. If exactly one module matches, it delegates to the full
// exploreModule rendering. If multiple match, it lists them so the user can
// pick the right one.
func (s *Server) exploreModuleSubstring(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	matches, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindModule, Name: focus})
	if len(matches) == 0 {
		return false
	}
	if len(matches) == 1 {
		return s.exploreModule(store, matches[0].Name, depth, sb)
	}
	// Multiple matches — list them so the user can refine.
	sb.WriteString(fmt.Sprintf("# Multiple modules matching %q (%d)\n\n", focus, len(matches)))
	for _, m := range matches {
		sb.WriteString(fmt.Sprintf("- `%s` (%s)\n", m.Name, m.File))
	}
	sb.WriteString("\nUse the full module name for detailed exploration.\n")
	return true
}

// exploreFile renders a file exploration if the focus matches an exact file path.
// In multi-repo mode, it also tries repo-label prefixed paths and common extensions.
func (s *Server) exploreFile(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	fileFacts := store.ByFile(focus)

	// In multi-repo mode, try repo-label prefixed paths.
	if len(fileFacts) == 0 {
		for _, label := range s.repoLabels() {
			fileFacts = store.ByFile(label + "/" + focus)
			if len(fileFacts) > 0 {
				focus = label + "/" + focus
				break
			}
		}
	}

	// Try appending common extensions (with and without repo labels).
	if len(fileFacts) == 0 {
		extensions := []string{".go", ".ts", ".tsx", ".kt", ".swift", ".rb"}
		candidates := make([]string, 0, len(extensions)*(1+len(s.repoLabels())))
		for _, ext := range extensions {
			candidates = append(candidates, focus+ext)
			for _, label := range s.repoLabels() {
				candidates = append(candidates, label+"/"+focus+ext)
			}
		}
		for _, c := range candidates {
			fileFacts = store.ByFile(c)
			if len(fileFacts) > 0 {
				focus = c
				break
			}
		}
	}

	if len(fileFacts) == 0 {
		return false
	}

	sb.WriteString(fmt.Sprintf("# File: %s\n\n", focus))
	sb.WriteString(fmt.Sprintf("Total facts: %d\n\n", len(fileFacts)))

	// Group by kind
	byKind := make(map[string][]facts.Fact)
	for _, f := range fileFacts {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}

	for _, kind := range []string{facts.KindModule, facts.KindSymbol, facts.KindDependency, facts.KindRoute, facts.KindStorage} {
		ff := byKind[kind]
		if len(ff) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("## %ss (%d)\n\n", capitalize(kind), len(ff)))
		for _, f := range ff {
			sb.WriteString(fmt.Sprintf("- **%s**", f.Name))
			if f.Line > 0 {
				sb.WriteString(fmt.Sprintf(" (line %d)", f.Line))
			}
			if sk, ok := f.Props["symbol_kind"].(string); ok {
				sb.WriteString(fmt.Sprintf(" [%s]", sk))
			}
			sb.WriteString("\n")
			if depth >= 2 {
				for _, r := range f.Relations {
					sb.WriteString(fmt.Sprintf("  - %s → %s\n", r.Kind, r.Target))
				}
			}
		}
		sb.WriteString("\n")
	}

	return true
}

// exploreSymbol renders a symbol exploration if the focus matches symbol names via substring.
func (s *Server) exploreSymbol(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	results := store.Query(facts.KindSymbol, "", focus, "")
	if len(results) == 0 {
		return false
	}

	if len(results) > 10 {
		results = results[:10]
	}

	sb.WriteString(fmt.Sprintf("# Symbol: %s\n\n", focus))

	for i, sym := range results {
		if i > 0 {
			sb.WriteString("---\n\n")
		}

		sb.WriteString(fmt.Sprintf("## %s\n\n", sym.Name))
		sb.WriteString(fmt.Sprintf("- File: %s\n", sym.File))
		sb.WriteString(fmt.Sprintf("- Line: %d\n", sym.Line))
		if sk, ok := sym.Props["symbol_kind"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Kind: %s\n", sk))
		}
		if lang, ok := sym.Props["language"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
		}
		if exp, ok := sym.Props["exported"].(bool); ok {
			sb.WriteString(fmt.Sprintf("- Exported: %v\n", exp))
		}
		sb.WriteString("\n")

		// Relations
		if len(sym.Relations) > 0 {
			sb.WriteString("### Relations\n\n")
			for _, r := range sym.Relations {
				sb.WriteString(fmt.Sprintf("- %s → %s\n", r.Kind, r.Target))
			}
			sb.WriteString("\n")
		}

		// Resolve relation targets (depth >= 1)
		if depth >= 1 && len(sym.Relations) > 0 {
			sb.WriteString("### Related Facts\n\n")
			seen := make(map[string]struct{})
			for _, r := range sym.Relations {
				if _, dup := seen[r.Target]; dup {
					continue
				}
				seen[r.Target] = struct{}{}
				related := store.LookupByExactName(r.Target)
				for _, rf := range related {
					sb.WriteString(fmt.Sprintf("- **%s** (%s) — %s", rf.Name, rf.Kind, rf.File))
					if rf.Line > 0 {
						sb.WriteString(fmt.Sprintf(":%d", rf.Line))
					}
					sb.WriteString("\n")
				}
			}
			sb.WriteString("\n")
		}

		// Reverse relations: who calls/imports/depends on this symbol
		callers := store.ReverseLookup(sym.Name, "")
		if len(callers) > 0 {
			sb.WriteString("### Referenced By\n\n")
			limit := len(callers)
			if limit > 20 {
				limit = 20
			}
			for _, c := range callers[:limit] {
				for _, r := range c.Relations {
					if r.Target == sym.Name {
						sb.WriteString(fmt.Sprintf("- %s (%s)\n", c.Name, r.Kind))
						break
					}
				}
			}
			if len(callers) > 20 {
				sb.WriteString(fmt.Sprintf("- ... and %d more\n", len(callers)-20))
			}
			sb.WriteString("\n")
		}
	}

	return true
}

// exploreDirectory renders a directory summary if the focus matches a file prefix.
func (s *Server) exploreDirectory(store *facts.Store, focus string, sb *strings.Builder) bool {
	prefix := focus
	if prefix == "." {
		// "." means repo root — match all files (no prefix filter).
		prefix = ""
	} else if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// In multi-repo mode, expand bare prefixes to include repo labels.
	prefixes := s.expandFilePrefix(prefix)
	dirFacts, total := store.QueryAdvanced(facts.QueryOpts{FilePrefix: prefixes[0], Limit: 500})
	for _, p := range prefixes[1:] {
		extra, extraTotal := store.QueryAdvanced(facts.QueryOpts{FilePrefix: p, Limit: 500})
		dirFacts = append(dirFacts, extra...)
		total += extraTotal
	}
	if total == 0 {
		return false
	}

	sb.WriteString(fmt.Sprintf("# Directory: %s\n\n", focus))
	sb.WriteString(fmt.Sprintf("Total facts: %d\n\n", total))

	// Count by kind
	kindCount := make(map[string]int)
	files := make(map[string]struct{})
	for _, f := range dirFacts {
		kindCount[f.Kind]++
		if f.File != "" {
			files[f.File] = struct{}{}
		}
	}

	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("- Files: %d\n", len(files)))
	for _, kind := range []string{facts.KindModule, facts.KindSymbol, facts.KindDependency, facts.KindRoute, facts.KindStorage} {
		if c, ok := kindCount[kind]; ok {
			sb.WriteString(fmt.Sprintf("- %ss: %d\n", capitalize(kind), c))
		}
	}
	sb.WriteString("\n")

	// List modules
	var modules []facts.Fact
	var symbols []facts.Fact
	for _, f := range dirFacts {
		switch f.Kind {
		case facts.KindModule:
			modules = append(modules, f)
		case facts.KindSymbol:
			symbols = append(symbols, f)
		}
	}

	if len(modules) > 0 {
		sb.WriteString(fmt.Sprintf("## Modules (%d)\n\n", len(modules)))
		for _, m := range modules {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Name))
		}
		sb.WriteString("\n")
	}

	if len(symbols) > 0 {
		sb.WriteString(fmt.Sprintf("## Key Symbols (showing up to 30)\n\n"))
		limit := len(symbols)
		if limit > 30 {
			limit = 30
		}
		sb.WriteString("| Name | Kind | File | Line |\n")
		sb.WriteString("|------|------|------|------|\n")
		for _, sym := range symbols[:limit] {
			symKind, _ := sym.Props["symbol_kind"].(string)
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n",
				sym.Name, symKind, sym.File, sym.Line))
		}
		if len(symbols) > 30 {
			sb.WriteString(fmt.Sprintf("\n... and %d more symbols\n", len(symbols)-30))
		}
		sb.WriteString("\n")
	}

	return true
}

// showSymbolArgs are the arguments for the show_symbol tool.
type showSymbolArgs struct {
	Name         string `json:"name" jsonschema:"required,Symbol name to look up (substring match)"`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"Number of source lines to show around the symbol (default 60)"`
}

// readSourceWindow reads lines from a file around the given line number.
// The window is asymmetric: 1/4 of context before the line, 3/4 after,
// since symbol declarations are at the start of the interesting code.
func readSourceWindow(absFile string, centerLine, contextLines int) (string, error) {
	data, err := os.ReadFile(absFile)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	before := contextLines / 4
	after := contextLines - before
	startLine := centerLine - before
	if startLine < 1 {
		startLine = 1
	}
	endLine := centerLine + after
	if endLine > len(lines) {
		endLine = len(lines)
	}

	var sb strings.Builder
	for i := startLine; i <= endLine; i++ {
		sb.WriteString(fmt.Sprintf("%4d│ %s\n", i, lines[i-1]))
	}
	return sb.String(), nil
}

// capitalize returns s with its first letter uppercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// normalizeToRelative converts an absolute filesystem path to a store-relative
// path by stripping known repo root prefixes. If the path is already relative
// or doesn't match any known repo root, it is returned unchanged.
func (s *Server) normalizeToRelative(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}

	// Try multi-repo paths first (populated in append mode).
	for label, absRoot := range s.eng.RepoPaths() {
		rel, err := filepath.Rel(absRoot, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			// Prefix with repo label so it matches the prefixed fact files.
			return filepath.ToSlash(filepath.Join(label, rel))
		}
	}

	// Fall back to the single-snapshot repo path.
	snap := s.eng.Snapshot()
	if snap != nil {
		rel, err := filepath.Rel(snap.Meta.RepoPath, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}

	return p
}

// repoLabels returns the known repo labels from multi-repo mode, or nil.
func (s *Server) repoLabels() []string {
	if s.eng == nil {
		return nil
	}
	rp := s.eng.RepoPaths()
	if len(rp) == 0 {
		return nil
	}
	labels := make([]string, 0, len(rp))
	for l := range rp {
		labels = append(labels, l)
	}
	return labels
}

// expandFilePrefix expands a relative file prefix for multi-repo mode.
// When repoPaths are configured and the prefix doesn't already start with a
// known repo label, it returns all "{label}/{prefix}" variants that have
// matches in the store. If only one repo matches, it returns that single
// expanded prefix. If multiple repos match, it returns all variants.
// In single-repo mode or when the prefix already has a repo label, it returns
// the input unchanged.
func (s *Server) expandFilePrefix(prefix string) []string {
	if prefix == "" || filepath.IsAbs(prefix) || s.eng == nil {
		return []string{prefix}
	}

	repoPaths := s.eng.RepoPaths()
	if len(repoPaths) == 0 {
		return []string{prefix}
	}

	// Check if prefix already starts with a known repo label.
	for label := range repoPaths {
		if prefix == label || strings.HasPrefix(prefix, label+"/") {
			return []string{prefix}
		}
	}

	// Try prefixing with each repo label and check for matches.
	store := s.eng.Store()
	var expanded []string
	for label := range repoPaths {
		candidate := label + "/" + prefix
		// Quick check: does the store have any facts with this file prefix?
		_, total := store.QueryAdvanced(facts.QueryOpts{FilePrefix: candidate, Limit: 1})
		if total > 0 {
			expanded = append(expanded, candidate)
		}
	}

	if len(expanded) == 0 {
		// No matches with any repo label; return original (maybe it matches as-is).
		return []string{prefix}
	}
	return expanded
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: msg},
		},
		IsError: true,
	}
}

// jsonResult marshals v as indented JSON into a tool result, returning an error
// result if marshaling fails.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(data)},
		},
	}, nil, nil
}

// textResult returns markdown/plain text as a (non-error) tool result.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

// wantsFullOutput reports whether the caller asked for the raw JSON graph rather
// than the default compact markdown summary.
func wantsFullOutput(mode string) bool {
	return strings.EqualFold(mode, "full")
}

// compactPerDepthCap bounds how many nodes are listed per depth bucket in the
// compact graph-tool output, keeping responses well under the MCP token budget
// even for high-fan-in nodes. The accurate totals still reflect the full set.
const compactPerDepthCap = 40

// writeResolutionNote renders the name-resolution context (if any) for the
// compact graph-tool output: when a name was ambiguous and refused, it lists the
// candidates so the caller can re-run with an exact name; when it was auto-picked
// it notes the match and alternatives.
func writeResolutionNote(sb *strings.Builder, res *nameResolution) {
	if res == nil {
		return
	}
	if res.Matched == "" {
		fmt.Fprintf(sb, "> ⚠ Ambiguous %q — refine with an exact name or a repo:/kind:/file: scope. Candidates:\n", res.Query)
		writeCandidateList(sb, res)
		sb.WriteString("\n")
		return
	}
	fmt.Fprintf(sb, "> Resolved %q → %s", res.Query, res.Matched)
	if res.Confidence > 0 {
		fmt.Fprintf(sb, " (confidence %.2f)", res.Confidence)
	}
	sb.WriteString("\n")
	if len(res.Alternatives) > 0 {
		fmt.Fprintf(sb, "> alternatives: %s\n", strings.Join(res.Alternatives, ", "))
	}
	sb.WriteString("\n")
}

// writeCandidateList prints a resolution's ranked candidates (falling back to the
// plain alternative names when no scored candidates are present).
func writeCandidateList(sb *strings.Builder, res *nameResolution) {
	if len(res.Candidates) > 0 {
		for _, c := range res.Candidates {
			line := "> - " + c.Name
			if c.Kind != "" {
				line += " (" + c.Kind + ")"
			}
			if c.File != "" {
				line += " — " + c.File
			}
			sb.WriteString(line + "\n")
		}
		return
	}
	for _, a := range res.Alternatives {
		sb.WriteString("> - " + a + "\n")
	}
}

// compactNodeLine formats a single traversal node as a markdown list item.
func compactNodeLine(n facts.TraversalNode) string {
	s := "- " + n.Name
	if n.Kind != "" {
		s += " (" + n.Kind + ")"
	}
	if n.Unresolved {
		s += " [unresolved]"
	}
	if n.File != "" {
		s += " — " + n.File
		if n.Line > 0 {
			s += fmt.Sprintf(":%d", n.Line)
		}
	}
	return s
}

// writeNodesByDepth groups nodes by their depth (skipping depth 0, the origin)
// and writes one capped section per depth.
func writeNodesByDepth(sb *strings.Builder, nodes []facts.TraversalNode) {
	byDepth := map[int][]facts.TraversalNode{}
	for _, n := range nodes {
		if n.Depth > 0 {
			byDepth[n.Depth] = append(byDepth[n.Depth], n)
		}
	}
	depths := make([]int, 0, len(byDepth))
	for d := range byDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	for _, d := range depths {
		group := byDepth[d]
		fmt.Fprintf(sb, "## Depth %d (%d)\n\n", d, len(group))
		shown := group
		if len(shown) > compactPerDepthCap {
			shown = shown[:compactPerDepthCap]
		}
		for _, n := range shown {
			sb.WriteString(compactNodeLine(n) + "\n")
		}
		if len(group) > compactPerDepthCap {
			fmt.Fprintf(sb, "... and %d more at depth %d\n", len(group)-compactPerDepthCap, d)
		}
		sb.WriteString("\n")
	}
}

// renderImpactCompact renders an impact analysis as a compact markdown summary.
func renderImpactCompact(resp impactResponse) string {
	var sb strings.Builder
	r := resp.ImpactResult
	fmt.Fprintf(&sb, "# Impact: %s\n\n", r.Target)
	writeResolutionNote(&sb, resp.Resolution)

	// Ambiguous-and-refused: no traversal was run; the candidate list above is
	// the actionable content.
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	if r.Summary != "" {
		sb.WriteString(r.Summary + "\n\n")
	} else {
		sb.WriteString("No dependents found.\n\n")
	}

	// Flatten ByDepth into a node slice for grouped rendering.
	var nodes []facts.TraversalNode
	for _, group := range r.ByDepth {
		nodes = append(nodes, group...)
	}
	writeNodesByDepth(&sb, nodes)

	if r.Forward != nil && len(r.Forward.Nodes) > 0 {
		fmt.Fprintf(&sb, "# Forward dependencies of %s (what it depends on)\n\n", r.Target)
		writeNodesByDepth(&sb, r.Forward.Nodes)
	}

	fmt.Fprintf(&sb, "_Stats: %d visited, max depth %d", r.Stats.NodesVisited, r.Stats.MaxDepthReached)
	if r.Stats.Truncated {
		sb.WriteString(", truncated — use max_nodes to widen or output_mode=full for the raw graph")
	}
	sb.WriteString("._\n")
	return sb.String()
}

// renderTraverseCompact renders a graph traversal as a compact markdown summary.
func renderTraverseCompact(resp traverseResponse, start, direction string) string {
	var sb strings.Builder
	if direction == "" {
		direction = "forward"
	}
	fmt.Fprintf(&sb, "# Traverse: %s (%s)\n\n", start, direction)
	writeResolutionNote(&sb, resp.Resolution)

	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	// Node count excludes depth-0 origin(s).
	reached := 0
	for _, n := range resp.Nodes {
		if n.Depth > 0 {
			reached++
		}
	}
	fmt.Fprintf(&sb, "Reached **%d** nodes", reached)
	if resp.Stats.Truncated {
		sb.WriteString(" (truncated — more exist beyond the cap)")
	}
	sb.WriteString("\n\n")

	writeNodesByDepth(&sb, resp.Nodes)

	fmt.Fprintf(&sb, "_Edges: %d (use output_mode=full for the raw node/edge graph). Stats: %d visited, max depth %d._\n",
		len(resp.Edges), resp.Stats.NodesVisited, resp.Stats.MaxDepthReached)
	return sb.String()
}

// traverseResponse wraps a graph traversal with the optional name resolution.
// The embedded TraversalResult inlines nodes/edges/stats alongside resolution.
type traverseResponse struct {
	Resolution *nameResolution `json:"resolution,omitempty"`
	facts.TraversalResult
}

// impactResponse wraps an impact analysis with the optional name resolution.
type impactResponse struct {
	Resolution *nameResolution `json:"resolution,omitempty"`
	facts.ImpactResult
}

// findPathResponse wraps a shortest-path result. find_path resolves two names,
// so it carries one resolution per side.
type findPathResponse struct {
	FromResolution *nameResolution `json:"from_resolution,omitempty"`
	ToResolution   *nameResolution `json:"to_resolution,omitempty"`
	facts.PathResult
}
