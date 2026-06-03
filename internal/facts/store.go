package facts

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Store provides in-memory storage and querying of facts with JSONL persistence.
type Store struct {
	mu    sync.RWMutex
	facts []Fact

	// Indexes for fast lookups
	byKind map[string][]int // kind -> indices into facts
	byFile map[string][]int // file -> indices into facts
	byName map[string][]int // name -> indices into facts
	byRepo map[string][]int // repo label -> indices into facts

	// Graph provides adjacency-list traversal over fact relations
	graph *Graph
}

// NewStore creates an empty fact store.
func NewStore() *Store {
	return &Store{
		byKind: make(map[string][]int),
		byFile: make(map[string][]int),
		byName: make(map[string][]int),
		byRepo: make(map[string][]int),
	}
}

// Add adds facts to the store.
func (s *Store) Add(ff ...Fact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range ff {
		idx := len(s.facts)
		s.facts = append(s.facts, f)
		s.byKind[f.Kind] = append(s.byKind[f.Kind], idx)
		if f.File != "" {
			s.byFile[f.File] = append(s.byFile[f.File], idx)
		}
		if f.Name != "" {
			s.byName[f.Name] = append(s.byName[f.Name], idx)
		}
		if f.Repo != "" {
			s.byRepo[f.Repo] = append(s.byRepo[f.Repo], idx)
		}
	}
}

// All returns all facts in the store.
func (s *Store) All() []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Fact, len(s.facts))
	copy(result, s.facts)
	return result
}

// Count returns the number of facts in the store.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.facts)
}

// ByKind returns all facts of the given kind.
func (s *Store) ByKind(kind string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byKind[kind])
}

// ByFile returns all facts for the given file.
func (s *Store) ByFile(file string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byFile[file])
}

// ByName returns all facts with the given name.
func (s *Store) ByName(name string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byName[name])
}

// ByRepo returns all facts for the given repo label.
func (s *Store) ByRepo(repo string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byRepo[repo])
}

// ByRelation returns all facts that have a relation of the given kind.
func (s *Store) ByRelation(relKind string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Fact
	for _, f := range s.facts {
		for _, r := range f.Relations {
			if r.Kind == relKind {
				result = append(result, f)
				break
			}
		}
	}
	return result
}

// Query returns facts matching all provided filter criteria.
// Empty filter values are ignored (match all).
func (s *Store) Query(kind, file, name, relKind string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Use the kind index when available to avoid a full scan.
	candidates := s.facts
	if kind != "" {
		if idxs, ok := s.byKind[kind]; ok {
			return s.queryFromIndices(idxs, kind, file, name, relKind)
		}
		return nil // kind specified but no facts of that kind exist
	}

	var result []Fact
	for _, f := range candidates {
		if file != "" && f.File != file {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(f.Name), strings.ToLower(name)) {
			continue
		}
		if relKind != "" {
			hasRel := false
			for _, r := range f.Relations {
				if r.Kind == relKind {
					hasRel = true
					break
				}
			}
			if !hasRel {
				continue
			}
		}
		result = append(result, f)
	}
	return result
}

// queryFromIndices applies file/name/relKind filters over a pre-selected index
// slice, avoiding a full scan of s.facts when a kind (or file) index is used.
// kind is already matched by the caller's index selection and is not re-checked.
func (s *Store) queryFromIndices(indices []int, kind, file, name, relKind string) []Fact {
	nameLower := strings.ToLower(name)
	var result []Fact
	for _, idx := range indices {
		if idx >= len(s.facts) {
			continue
		}
		f := s.facts[idx]
		if file != "" && f.File != file {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(f.Name), nameLower) {
			continue
		}
		if relKind != "" {
			hasRel := false
			for _, r := range f.Relations {
				if r.Kind == relKind {
					hasRel = true
					break
				}
			}
			if !hasRel {
				continue
			}
		}
		result = append(result, f)
	}
	return result
}

// QueryOpts holds the full set of query filters for QueryAdvanced.
// Multi-value filters within a dimension are OR-combined; filters across
// different dimensions are AND-combined.
type QueryOpts struct {
	Kind       string   // single kind filter (exact match)
	Kinds      []string // multi-kind filter (OR with Kind)
	File       string   // exact file filter
	Files      []string // multi-file filter (OR with File)
	FilePrefix string   // file path prefix filter (e.g. "internal/server")
	Name       string   // substring name filter
	Names      []string // exact name batch filter (OR)
	Repo       string   // repo label filter (exact match, for multi-repo mode)
	RelKind    string   // relation kind filter
	Prop       string   // property name filter
	PropValue  string   // property value filter (requires Prop)
	Offset     int      // number of results to skip
	Limit      int      // max results to return (0 = default 100, max 500)
}

// QueryAdvanced returns facts matching the provided filter options along with
// the total count of matches before offset/limit are applied.
func (s *Store) QueryAdvanced(opts QueryOpts) ([]Fact, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Merge single and multi-value filters into sets for efficient lookup.
	kindSet := mergeIntoSet(opts.Kind, opts.Kinds)
	fileSet := mergeIntoSet(opts.File, opts.Files)
	nameSet := make(map[string]struct{}, len(opts.Names))
	for _, n := range opts.Names {
		if n != "" {
			nameSet[n] = struct{}{}
		}
	}

	nameLower := strings.ToLower(opts.Name)

	// Select the narrowest available index as the candidate set to avoid a
	// full O(N) scan when a high-selectivity filter is present.
	//
	// Priority: single-kind > single-file > exact-name batch > full scan.
	// Multi-kind and multi-file filters still fall back to the full slice
	// because building a union of index slices is only worthwhile when the
	// union is significantly smaller than N, which is hard to determine
	// cheaply; the remaining filters then trim the result.
	type iterMode int
	const (
		iterFull      iterMode = iota // scan all s.facts
		iterKindIndex                 // scan byKind[kind] indices
		iterFileIndex                 // scan byFile[file] indices
		iterNameUnion                 // scan union of byName[name] indices
	)

	mode := iterFull
	var indexSlice []int // for iterKindIndex / iterFileIndex

	if len(kindSet) == 1 {
		for k := range kindSet {
			if idxs, ok := s.byKind[k]; ok {
				indexSlice = idxs
				mode = iterKindIndex
			} else {
				// Kind filter specified but no facts of that kind — fast exit.
				return nil, 0
			}
		}
	} else if len(fileSet) == 1 && opts.FilePrefix == "" {
		for f := range fileSet {
			if idxs, ok := s.byFile[f]; ok {
				indexSlice = idxs
				mode = iterFileIndex
			} else {
				return nil, 0
			}
		}
	} else if len(nameSet) > 0 && opts.Name == "" {
		// Exact-name batch: union of byName index entries.
		// Only switch to this mode when no substring filter is also active,
		// to keep the logic simple.
		total := 0
		for n := range nameSet {
			total += len(s.byName[n])
		}
		if total < len(s.facts) {
			union := make([]int, 0, total)
			for n := range nameSet {
				union = append(union, s.byName[n]...)
			}
			indexSlice = union
			mode = iterNameUnion
		}
	}

	// factAt retrieves a fact by absolute index in s.facts, regardless of mode.
	filterFact := func(f Fact) bool {
		// Kind filter
		if len(kindSet) > 0 {
			if _, ok := kindSet[f.Kind]; !ok {
				return false
			}
		}

		// Repo filter
		if opts.Repo != "" && f.Repo != opts.Repo {
			return false
		}

		// File filter (exact match set OR prefix)
		if len(fileSet) > 0 || opts.FilePrefix != "" {
			fileMatch := false
			if len(fileSet) > 0 {
				_, fileMatch = fileSet[f.File]
			}
			if !fileMatch && opts.FilePrefix != "" {
				fileMatch = strings.HasPrefix(f.File, opts.FilePrefix)
			}
			if !fileMatch {
				return false
			}
		}

		// Name filter: substring (Name) OR exact batch (Names)
		if opts.Name != "" || len(nameSet) > 0 {
			nameMatch := false
			if opts.Name != "" && strings.Contains(strings.ToLower(f.Name), nameLower) {
				nameMatch = true
			}
			if !nameMatch && len(nameSet) > 0 {
				_, nameMatch = nameSet[f.Name]
			}
			if !nameMatch {
				return false
			}
		}

		// Relation kind filter
		if opts.RelKind != "" {
			hasRel := false
			for _, r := range f.Relations {
				if r.Kind == opts.RelKind {
					hasRel = true
					break
				}
			}
			if !hasRel {
				return false
			}
		}

		// Property filter
		if opts.Prop != "" {
			v, ok := f.Props[opts.Prop]
			if !ok {
				return false
			}
			if opts.PropValue != "" && fmt.Sprintf("%v", v) != opts.PropValue {
				return false
			}
		}

		return true
	}

	var matched []Fact

	switch mode {
	case iterKindIndex, iterFileIndex, iterNameUnion:
		for _, idx := range indexSlice {
			if idx >= len(s.facts) {
				continue
			}
			if filterFact(s.facts[idx]) {
				matched = append(matched, s.facts[idx])
			}
		}
	default:
		for _, f := range s.facts {
			if filterFact(f) {
				matched = append(matched, f)
			}
		}
	}

	total := len(matched)

	// Apply offset
	if opts.Offset > 0 {
		if opts.Offset >= len(matched) {
			return nil, total
		}
		matched = matched[opts.Offset:]
	}

	// Apply limit
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}

	return matched, total
}

// LookupByExactName returns all facts with the given exact name using the index.
func (s *Store) LookupByExactName(name string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byName[name])
}

// ReverseLookup returns all facts that have a relation targeting the given name.
// If relKind is non-empty, only relations of that kind are considered.
// When the graph index is available it delegates to the O(1) reverse map;
// otherwise it falls back to a linear scan of all facts.
func (s *Store) ReverseLookup(targetName, relKind string) []Fact {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()

	if g != nil {
		return g.ReverseFacts(targetName, relKind)
	}

	// Graph not yet built — linear scan fallback.
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Fact
	for _, f := range s.facts {
		for _, r := range f.Relations {
			if r.Target == targetName && (relKind == "" || r.Kind == relKind) {
				result = append(result, f)
				break
			}
		}
	}
	return result
}

// mergeIntoSet combines a single value and a slice into a set.
// Empty strings are ignored.
func mergeIntoSet(single string, multi []string) map[string]struct{} {
	set := make(map[string]struct{}, len(multi)+1)
	if single != "" {
		set[single] = struct{}{}
	}
	for _, v := range multi {
		if v != "" {
			set[v] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// SetRepoRange sets the Repo field for facts at indices [startIdx, current
// length) without modifying file paths. Used in non-append mode so the repo
// filter works even for single-repo snapshots.
func (s *Store) SetRepoRange(startIdx int, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		if f.Repo == "" {
			f.Repo = repo
			s.byRepo[repo] = append(s.byRepo[repo], i)
		}
	}
}

// TagRange sets the Repo field and prefixes File paths for facts added at
// indices [startIdx, current length). Used by the engine in append mode to
// namespace facts from different repositories.
func (s *Store) TagRange(startIdx int, repo, filePrefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		f.Repo = repo
		if f.File != "" {
			oldFile := f.File
			f.File = filePrefix + f.File
			// Update byFile index: remove old key, add new key.
			s.removeFromIndex(s.byFile, oldFile, i)
			s.byFile[f.File] = append(s.byFile[f.File], i)
		}
		s.byRepo[repo] = append(s.byRepo[repo], i)
	}
}

func (s *Store) removeFromIndex(idx map[string][]int, key string, target int) {
	indices := idx[key]
	for j, v := range indices {
		if v == target {
			idx[key] = append(indices[:j], indices[j+1:]...)
			break
		}
	}
	if len(idx[key]) == 0 {
		delete(idx, key)
	}
}

// TagUntagged sets the Repo field and prefixes File paths for all facts that
// belong to the given repo (or have an empty Repo). This is used when entering
// append mode to retroactively label and prefix facts from a prior single-repo
// snapshot so they become filterable alongside newly appended facts.
func (s *Store) TagUntagged(repo, filePrefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for i := range s.facts {
		f := &s.facts[i]

		// Skip facts belonging to a DIFFERENT repo (from a prior append).
		if f.Repo != "" && f.Repo != repo {
			continue
		}

		// Set Repo if not already set.
		if f.Repo == "" {
			f.Repo = repo
			s.byRepo[repo] = append(s.byRepo[repo], i)
		}

		// Prefix the file path if it doesn't already have the prefix.
		if f.File != "" && !strings.HasPrefix(f.File, filePrefix) {
			oldFile := f.File
			f.File = filePrefix + f.File
			s.removeFromIndex(s.byFile, oldFile, i)
			s.byFile[f.File] = append(s.byFile[f.File], i)
			count++
		}
	}
	return count
}

// RemoveWhere removes every fact for which pred returns true and rebuilds all
// indices from the surviving facts. The graph index is invalidated (set to nil)
// because removing facts shifts slice positions; callers should rebuild it via
// BuildGraph afterwards. Returns the number of facts removed.
//
// This is used to drop previously-synthesized facts (e.g. cross-repo links)
// before recomputing them, so repeated append/link cycles stay idempotent.
func (s *Store) RemoveWhere(pred func(Fact) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := s.facts[:0:0]
	removed := 0
	for _, f := range s.facts {
		if pred(f) {
			removed++
			continue
		}
		kept = append(kept, f)
	}
	if removed == 0 {
		return 0
	}

	// Rebuild facts slice and all indices from scratch.
	s.facts = kept
	s.byKind = make(map[string][]int)
	s.byFile = make(map[string][]int)
	s.byName = make(map[string][]int)
	s.byRepo = make(map[string][]int)
	for idx, f := range s.facts {
		s.byKind[f.Kind] = append(s.byKind[f.Kind], idx)
		if f.File != "" {
			s.byFile[f.File] = append(s.byFile[f.File], idx)
		}
		if f.Name != "" {
			s.byName[f.Name] = append(s.byName[f.Name], idx)
		}
		if f.Repo != "" {
			s.byRepo[f.Repo] = append(s.byRepo[f.Repo], idx)
		}
	}
	s.graph = nil
	return removed
}

// Modules returns all module facts.
func (s *Store) Modules() []Fact {
	return s.ByKind(KindModule)
}

// Symbols returns all symbol facts.
func (s *Store) Symbols() []Fact {
	return s.ByKind(KindSymbol)
}

// Clear removes all facts from the store.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = nil
	s.byKind = make(map[string][]int)
	s.byFile = make(map[string][]int)
	s.byName = make(map[string][]int)
	s.byRepo = make(map[string][]int)
	s.graph = nil
}

// BuildGraph constructs the adjacency-list graph index from the current facts.
// Call this after all facts have been added and tagged (e.g. after snapshot generation).
func (s *Store) BuildGraph() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graph = NewGraph(s.facts)
}

// Graph returns the current graph index, or nil if BuildGraph has not been called.
func (s *Store) Graph() *Graph {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.graph
}

// WriteJSONL writes all facts as JSONL to the given writer.
func (s *Store) WriteJSONL(w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	enc := json.NewEncoder(w)
	for _, f := range s.facts {
		if err := enc.Encode(f); err != nil {
			return fmt.Errorf("encoding fact %q: %w", f.Name, err)
		}
	}
	return nil
}

// WriteJSONLFile writes all facts as JSONL to the given file path.
func (s *Store) WriteJSONLFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	if err := s.WriteJSONL(bw); err != nil {
		return err
	}
	return bw.Flush()
}

// ReadJSONL reads facts from a JSONL reader and adds them to the store.
func (s *Store) ReadJSONL(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// Allow large lines
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Fact
		if err := json.Unmarshal(line, &f); err != nil {
			return fmt.Errorf("decoding fact: %w", err)
		}
		s.Add(f)
	}
	return scanner.Err()
}

// ReadJSONLFile reads facts from a JSONL file and adds them to the store.
func (s *Store) ReadJSONLFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	return s.ReadJSONL(f)
}

func (s *Store) collectByIndex(indices []int) []Fact {
	result := make([]Fact, 0, len(indices))
	for _, idx := range indices {
		if idx < len(s.facts) {
			result = append(result, s.facts[idx])
		}
	}
	return result
}
