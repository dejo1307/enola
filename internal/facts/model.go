package facts

// Fact represents a language-agnostic architectural fact extracted from source code.
type Fact struct {
	Kind      string         `json:"kind"`                // e.g. "module", "symbol", "route", "storage", "dependency"
	Name      string         `json:"name"`                // Canonical name
	File      string         `json:"file,omitempty"`      // Source file (relative to repo root, or repo-prefixed in multi-repo mode)
	Line      int            `json:"line,omitempty"`      // Line number in file
	Repo      string         `json:"repo,omitempty"`      // Repository label (set in multi-repo/append mode)
	Props     map[string]any `json:"props,omitempty"`     // Kind-specific properties
	Relations []Relation     `json:"relations,omitempty"` // Edges to other facts
}

// Relation represents a directed edge between two facts.
type Relation struct {
	Kind   string `json:"kind"`   // e.g. "declares", "imports", "calls", "implements", "depends_on"
	Target string `json:"target"` // Target fact name
}

// Fact kind constants.
const (
	KindModule     = "module"
	KindSymbol     = "symbol"
	KindRoute      = "route"
	KindStorage    = "storage"
	KindDependency = "dependency"
)

// Relation kind constants.
const (
	RelDeclares     = "declares"
	RelImports      = "imports"
	RelCalls        = "calls"
	RelImplements   = "implements"
	RelDependsOn    = "depends_on"
	RelInstantiates = "instantiates" // Source constructs an instance of target via a constructor call.
	RelInjects      = "injects"      // Source declares target as a DI-injected constructor parameter.
)

// Symbol kind property values.
const (
	SymbolFunc      = "function"
	SymbolMethod    = "method"
	SymbolStruct    = "struct"
	SymbolInterface = "interface"
	SymbolType      = "type"
	SymbolClass     = "class"
	SymbolVariable  = "variable"
	SymbolConstant  = "constant"
)

// Insight represents an architectural insight produced by an explainer.
type Insight struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Confidence  float64    `json:"confidence"` // 0.0 - 1.0
	Evidence    []Evidence `json:"evidence"`
	Actions     []string   `json:"suggested_actions,omitempty"`
}

// Evidence links an insight back to concrete facts/files/symbols.
type Evidence struct {
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Fact   string `json:"fact,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Artifact represents a generated output file.
type Artifact struct {
	Name    string `json:"name"`    // e.g. "llm_context.md"
	Content []byte `json:"-"`       // Raw content
	Type    string `json:"type"`    // MIME type hint
}

// Snapshot holds the complete result of an analysis run.
type Snapshot struct {
	Meta      SnapshotMeta `json:"meta"`
	Facts     []Fact       `json:"facts"`
	Insights  []Insight    `json:"insights"`
	Artifacts []Artifact   `json:"artifacts"`
}

// SnapshotMeta contains metadata about a snapshot generation run.
type SnapshotMeta struct {
	RepoPath    string     `json:"repo_path"`
	GeneratedAt string     `json:"generated_at"`
	Duration    string     `json:"duration"`
	Extractors  []string   `json:"extractors"`
	Explainers  []string   `json:"explainers"`
	Renderers   []string   `json:"renderers"`
	FileHashes  []FileHash `json:"file_hashes,omitempty"`
	FactCount   int        `json:"fact_count"`
	InsightCount int       `json:"insight_count"`
}

// FileHash tracks a file's content hash for incremental updates.
type FileHash struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	ModTime string `json:"mod_time"`
}
