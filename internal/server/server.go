package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/dejo1307/enola/internal/config"
	"github.com/dejo1307/enola/internal/engine"
	"github.com/dejo1307/enola/internal/facts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps the MCP server and connects it to the snapshot engine.
type Server struct {
	mcp *mcp.Server
	eng *engine.Engine
	cfg *config.Config
}

// New creates a new MCP server wired to the given engine.
func New(eng *engine.Engine, cfg *config.Config) (*Server, error) {
	s := &Server{
		eng: eng,
		cfg: cfg,
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "enola",
		Version: "0.1.0",
	}, nil)

	s.mcp = mcpServer
	s.registerTools()

	return s, nil
}

// Run starts the MCP server on the stdio transport.
func (s *Server) Run(ctx context.Context) error {
	log.Println("[server] starting MCP server on stdio transport")
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
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
		Name:        "generate_snapshot",
		Description: "Generate an architectural snapshot of a repository. Parses source code, extracts facts, detects patterns, and produces an LLM-ready context summary. Use append=true to add a second repository without clearing existing facts (for cross-repo analysis).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args generateSnapshotArgs) (*mcp.CallToolResult, any, error) {
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
		Name:        "query_facts",
		Description: "Query the extracted architectural facts by kind, file, name, or relation type. Returns matching facts as JSON. Supports batch filters (names, files, kinds), file prefix matching, pagination (offset/limit), and relation expansion (include_related). For dependencies, filter with prop='source' and prop_value='internal'|'external'|'stdlib' to control noise.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryFactsArgs) (*mcp.CallToolResult, any, error) {
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
		Name:        "show_symbol",
		Description: "Show source code for a symbol found in the architectural snapshot. Returns the actual implementation with surrounding context lines.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args showSymbolArgs) (*mcp.CallToolResult, any, error) {
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
		Name:        "explore",
		Description: "Explore a module, file, symbol, or directory in a single call. Returns a rich markdown summary with symbols, dependencies, dependents, and relations — replacing many query_facts calls with one.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
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
		Name:        "traverse",
		Description: "Walk the dependency/call graph from a starting point. Use direction='forward' to answer 'what does X depend on?' and direction='reverse' to answer 'what depends on X?'. Returns a list of nodes and edges up to the specified depth. Use this instead of multiple explore calls when you need to understand transitive relationships.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args traverseArgs) (*mcp.CallToolResult, any, error) {
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
			return jsonResult(traverseResponse{
				Resolution: res,
				TraversalResult: facts.TraversalResult{
					Nodes: []facts.TraversalNode{},
					Edges: []facts.TraversalEdge{},
				},
			})
		}

		direction := args.Direction
		if direction == "" {
			direction = "forward"
		}
		if direction != "forward" && direction != "reverse" {
			return errorResult("direction must be 'forward' or 'reverse'"), nil, nil
		}

		result := graph.Traverse(startName, direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)

		return jsonResult(traverseResponse{Resolution: res, TraversalResult: result})
	})

	// Tool: find_path
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "find_path",
		Description: "Find the shortest path between two nodes in the architectural graph. Use this to answer 'how does X reach Y?' or 'what is the call chain from main to this function?'. Returns the path as an ordered list of nodes and edges, or reports that no path exists.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args findPathArgs) (*mcp.CallToolResult, any, error) {
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
		Name:        "impact_analysis",
		Description: "Analyze the impact of changing a module, symbol, or file. Returns all nodes that transitively depend on the target (i.e., what would be affected if the target changes), grouped by depth. Use this for refactoring planning, understanding blast radius, and change risk assessment.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args impactAnalysisArgs) (*mcp.CallToolResult, any, error) {
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
			return jsonResult(impactResponse{
				Resolution: res,
				ImpactResult: facts.ImpactResult{
					Target:  args.Target,
					ByDepth: map[int][]facts.TraversalNode{},
					Edges:   []facts.TraversalEdge{},
				},
			})
		}

		result := graph.ImpactSet(targetName, args.MaxDepth, args.MaxNodes, args.IncludeForward)

		return jsonResult(impactResponse{Resolution: res, ImpactResult: result})
	})
}

// ambiguousMatchThreshold is the candidate count at or above which
// resolveNodeName refuses to guess and forces the caller to re-invoke with an
// exact name.
const ambiguousMatchThreshold = 3

// maxAlternatives caps how many candidate names are echoed back in a
// nameResolution so the response stays readable.
const maxAlternatives = 10

// nameResolution reports how a user-provided name was resolved to a concrete
// fact name. It is surfaced in tool responses ONLY when the input matched more
// than one fact (i.e. Ambiguous is true), so callers can detect and correct a
// possibly-wrong pick. Matched is empty when the match count crossed
// ambiguousMatchThreshold and the caller must re-invoke with an exact name.
type nameResolution struct {
	Query        string   `json:"query"`
	Matched      string   `json:"matched,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"`
	Ambiguous    bool     `json:"ambiguous"`
}

// resolveNodeName resolves a user-provided name to an exact fact name.
//
// It returns the resolved name and, when resolution was ambiguous (the input
// substring-matched more than one fact and no confident pick existed), a
// non-nil *nameResolution describing the ambiguity. For exact matches, single
// substring matches, and confident suffix-exact matches the resolution is nil.
//
// When the candidate count reaches ambiguousMatchThreshold the method refuses
// to guess: it returns an empty name together with a resolution whose Matched
// is empty, signalling the caller to emit a resolution-only response and
// require an exact re-invocation.
func (s *Server) resolveNodeName(store *facts.Store, input string) (string, *nameResolution, error) {
	query := input
	input = s.normalizeToRelative(input)

	// Try exact match first
	exact := store.LookupByExactName(input)
	if len(exact) > 0 {
		return exact[0].Name, nil, nil
	}

	// Try substring match
	results := store.Query("", "", input, "")
	if len(results) == 0 {
		return "", nil, fmt.Errorf("no facts matching %q", input)
	}
	if len(results) == 1 {
		return results[0].Name, nil, nil
	}

	// Smart disambiguation: when multiple matches exist, try to find the
	// most likely intended target rather than immediately erroring.

	// 1. Prefer exact name suffix match (e.g., "AuthHandler" matching
	//    "adapters.AuthHandler" over "adapters.AuthHandler.Login"). A unique
	//    suffix-exact match is a confident pick, so no resolution is surfaced.
	var suffixMatches []facts.Fact
	for _, r := range results {
		parts := strings.Split(r.Name, ".")
		if len(parts) > 0 && parts[len(parts)-1] == input {
			suffixMatches = append(suffixMatches, r)
		}
	}
	if len(suffixMatches) == 1 {
		return suffixMatches[0].Name, nil, nil
	}

	// Beyond this point the pick is a heuristic guess, so the result is
	// ambiguous. If the candidate count crosses the threshold, refuse to guess
	// and force an exact re-invocation.
	if len(results) >= ambiguousMatchThreshold {
		return "", &nameResolution{
			Query:        query,
			Alternatives: candidateNames(results, ""),
			Ambiguous:    true,
		}, nil
	}

	// Below threshold: pick the most likely candidate but flag the ambiguity.

	// 2. Among suffix matches (or all results), prefer struct/class/interface
	candidates := results
	if len(suffixMatches) > 0 {
		candidates = suffixMatches
	}
	matched := results[0].Name
	for _, r := range candidates {
		sk, _ := r.Props["symbol_kind"].(string)
		if sk == "struct" || sk == "class" || sk == "interface" {
			matched = r.Name
			break
		}
	}
	// 3. Prefer module-level facts over symbol-level (only if no struct/class/
	//    interface was chosen above).
	if matched == results[0].Name {
		for _, r := range candidates {
			if r.Kind == facts.KindModule {
				matched = r.Name
				break
			}
		}
	}

	return matched, &nameResolution{
		Query:        query,
		Matched:      matched,
		Alternatives: candidateNames(results, matched),
		Ambiguous:    true,
	}, nil
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
	Start         string   `json:"start" jsonschema:"required,Starting node name (fact name, module name, or symbol name). Substring match."`
	Direction     string   `json:"direction,omitempty" jsonschema:"'forward' follows outgoing relations (what does X depend on?), 'reverse' follows incoming relations (what depends on X?). Default: forward."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Filter to specific relation types: imports, calls, declares, implements, depends_on. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum traversal depth (1-20). Default: 5."`
	MaxNodes      int      `json:"max_nodes,omitempty" jsonschema:"Maximum nodes to return (1-500). Traversal stops when this limit is reached. Default: 100."`
	NodeKinds     []string `json:"node_kinds,omitempty" jsonschema:"Filter results to specific fact kinds: module, symbol, dependency, route, storage. Default: all."`
}

// findPathArgs are the arguments for the find_path tool.
type findPathArgs struct {
	From          string   `json:"from" jsonschema:"required,Source node name (substring match)."`
	To            string   `json:"to" jsonschema:"required,Target node name (substring match)."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Filter to specific relation types. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum path length to search (1-20). Default: 10."`
}

// impactAnalysisArgs are the arguments for the impact_analysis tool.
type impactAnalysisArgs struct {
	Target         string `json:"target" jsonschema:"required,The node being changed (fact name, substring match)."`
	MaxDepth       int    `json:"max_depth,omitempty" jsonschema:"How many hops of impact to compute (1-10). Default: 3."`
	MaxNodes       int    `json:"max_nodes,omitempty" jsonschema:"Maximum impacted nodes to return (1-500). Default: 200."`
	IncludeForward bool   `json:"include_forward,omitempty" jsonschema:"Include what the target depends on (what might break the target). Default: false."`
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
