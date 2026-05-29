package rubyextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dejo1307/enola/internal/facts"
)

func TestExtractFile_BasicClassAndMethod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "order.rb")
	src := `# frozen_string_literal: true

module Orders
  class Order < ApplicationRecord
    def total
      items.sum(:price)
    end

    def self.recent
      where("created_at > ?", 1.day.ago)
    end
  end
end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "packages/orders/app/models/order.rb", true, false)

	// Collect by kind and name.
	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	// Module Orders.
	mod, ok := byName["Orders"]
	if !ok {
		t.Fatal("missing module Orders")
	}
	if mod.Kind != facts.KindSymbol {
		t.Errorf("Orders kind = %q, want symbol", mod.Kind)
	}
	sk, _ := mod.Props["symbol_kind"].(string)
	if sk != facts.SymbolInterface {
		t.Errorf("Orders symbol_kind = %q, want interface", sk)
	}

	// Class Orders::Order.
	cls, ok := byName["Orders::Order"]
	if !ok {
		t.Fatal("missing class Orders::Order")
	}
	if cls.Kind != facts.KindSymbol {
		t.Errorf("Orders::Order kind = %q, want symbol", cls.Kind)
	}
	sk, _ = cls.Props["symbol_kind"].(string)
	if sk != facts.SymbolClass {
		t.Errorf("Orders::Order symbol_kind = %q, want class", sk)
	}
	superclass, _ := cls.Props["superclass"].(string)
	if superclass != "ApplicationRecord" {
		t.Errorf("superclass = %q, want ApplicationRecord", superclass)
	}
	// Should have implements relation to ApplicationRecord.
	hasImpl := false
	for _, r := range cls.Relations {
		if r.Kind == facts.RelImplements && r.Target == "ApplicationRecord" {
			hasImpl = true
		}
	}
	if !hasImpl {
		t.Error("Orders::Order missing implements relation to ApplicationRecord")
	}

	// Instance method Orders::Order#total.
	meth, ok := byName["Orders::Order#total"]
	if !ok {
		t.Fatal("missing method Orders::Order#total")
	}
	sk, _ = meth.Props["symbol_kind"].(string)
	if sk != facts.SymbolMethod {
		t.Errorf("total symbol_kind = %q, want method", sk)
	}

	// Class method Orders::Order.recent.
	cmeth, ok := byName["Orders::Order.recent"]
	if !ok {
		t.Fatal("missing class method Orders::Order.recent")
	}
	sk, _ = cmeth.Props["symbol_kind"].(string)
	if sk != facts.SymbolFunc {
		t.Errorf("recent symbol_kind = %q, want function", sk)
	}
}

func TestStorageFacts_DeclaresTargetIsDirectory(t *testing.T) {
	relFile := "packages/items/app/models/item.rb"

	fileFacts := []facts.Fact{
		{
			Kind: facts.KindSymbol,
			Name: "Item",
			File: relFile,
			Line: 3,
			Props: map[string]any{
				"symbol_kind": facts.SymbolClass,
				"superclass":  "ApplicationRecord",
				"language":    "ruby",
			},
		},
	}

	result := extractStorageFacts(relFile, fileFacts)
	if len(result) == 0 {
		t.Fatal("expected at least one storage fact")
	}

	storageFact := result[0]
	if storageFact.Name != "Item" {
		t.Errorf("storage fact name = %q, want Item", storageFact.Name)
	}

	// The declares target must be the directory, not the class name.
	if len(storageFact.Relations) == 0 {
		t.Fatal("storage fact has no relations")
	}
	declTarget := storageFact.Relations[0].Target
	want := "packages/items/app/models"
	if declTarget != want {
		t.Errorf("declares target = %q, want %q", declTarget, want)
	}
	if declTarget == "Item" {
		t.Error("declares target must not be the class name (self-loop)")
	}
}

func TestAssociationFactNames_IncludeFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "order.rb")
	src := `class Order < ApplicationRecord
  belongs_to :user
  has_many :items
end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	relFile := "packages/orders/app/models/order.rb"
	result := extractAssociationsFromFile(path, relFile)

	if len(result) == 0 {
		t.Fatal("expected association facts")
	}

	for _, fact := range result {
		if fact.Kind != facts.KindDependency {
			continue
		}
		if !strings.HasPrefix(fact.Name, relFile+":") {
			t.Errorf("association fact name %q should start with file path %q", fact.Name, relFile+":")
		}
	}

	// Verify specific associations.
	names := make(map[string]bool)
	for _, fact := range result {
		names[fact.Name] = true
	}
	if !names[relFile+":belongs_to :user"] {
		t.Error("missing belongs_to :user with file prefix")
	}
	if !names[relFile+":has_many :items"] {
		t.Error("missing has_many :items with file prefix")
	}
}

// --- RelCalls extraction tests ---

// hasCall returns true if the fact has a RelCalls relation to target.
func hasCall(f facts.Fact, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && r.Target == target {
			return true
		}
	}
	return false
}

func TestExtractFile_QualifiedClassMethodCall(t *testing.T) {
	src := `module Items
  class FetchService
    def call(ids)
      Items::Facade.fetch_item_fields(ids, ITEM_FIELDS)
    end
  end
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fetch_service.rb")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "packages/items/app/services/fetch_service.rb", false, true)

	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	meth, ok := byName["Items::FetchService#call"]
	if !ok {
		t.Fatal("missing method Items::FetchService#call")
	}
	if !hasCall(meth, "Items::Facade.fetch_item_fields") {
		t.Errorf("Items::FetchService#call missing RelCalls -> Items::Facade.fetch_item_fields; relations = %v", meth.Relations)
	}
}

func TestExtractFile_MultiLevelNamespaceCall(t *testing.T) {
	src := `module HomepageSources
  class Builder
    def build(ids)
      HomepageSources::ItemDto.from_ids(ids)
    end
  end
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "builder.rb")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "packages/homepage_sources/app/builder.rb", false, true)

	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	meth, ok := byName["HomepageSources::Builder#build"]
	if !ok {
		t.Fatal("missing method HomepageSources::Builder#build")
	}
	if !hasCall(meth, "HomepageSources::ItemDto.from_ids") {
		t.Errorf("HomepageSources::Builder#build missing RelCalls -> HomepageSources::ItemDto.from_ids; relations = %v", meth.Relations)
	}
}

func TestExtractFile_ReceiverVariableCall(t *testing.T) {
	src := `class OrderProcessor
  def process(order)
    service.call(order)
  end
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "order_processor.rb")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "app/models/order_processor.rb", false, true)

	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	meth, ok := byName["OrderProcessor#process"]
	if !ok {
		t.Fatal("missing method OrderProcessor#process")
	}
	if !hasCall(meth, "service.call") {
		t.Errorf("OrderProcessor#process missing RelCalls -> service.call; relations = %v", meth.Relations)
	}
}

func TestExtractFile_CallsDeduplication(t *testing.T) {
	src := `class Dispatcher
  def run(ids)
    Items::Facade.fetch_item_fields(ids, FIELDS)
    Items::Facade.fetch_item_fields(ids, OTHER_FIELDS)
  end
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatcher.rb")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "app/dispatcher.rb", false, true)

	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	meth, ok := byName["Dispatcher#run"]
	if !ok {
		t.Fatal("missing method Dispatcher#run")
	}

	count := 0
	for _, r := range meth.Relations {
		if r.Kind == facts.RelCalls && r.Target == "Items::Facade.fetch_item_fields" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 RelCalls edge to Items::Facade.fetch_item_fields, got %d", count)
	}
}

func TestExtractFile_TopLevelMethodCalls(t *testing.T) {
	// Ruby allows method calls without parentheses; qualifiedCallRe must capture
	// them even when there is no trailing '(' character.
	src := `def bootstrap
  Config.load_defaults
  Rails.application.initialize!
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "init.rb")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "config/init.rb", false, true)

	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	meth, ok := byName["config.bootstrap"]
	if !ok {
		t.Fatal("missing top-level method config.bootstrap")
	}
	// Config.load_defaults has no parens — qualifiedCallRe must still fire.
	if !hasCall(meth, "Config.load_defaults") {
		t.Errorf("config.bootstrap missing RelCalls -> Config.load_defaults; relations = %v", meth.Relations)
	}
	// Rails.application.initialize! — qualifiedCallRe captures the first segment: Rails.application.
	if !hasCall(meth, "Rails.application") {
		t.Errorf("config.bootstrap missing RelCalls -> Rails.application; relations = %v", meth.Relations)
	}
}

func TestExtractFile_EndlessMethodCall(t *testing.T) {
	// Ruby 3.0+ endless method: def name(args) = Expr.call(args)
	// The call is on the same line as the def — must be captured directly.
	src := `module HomepageSources
  class ItemDto
    ITEM_FIELDS = %i[id title].freeze

    def fields_by_id(item_ids) = Items::Facade.fetch_item_fields(item_ids, ITEM_FIELDS).index_by { |item| item[:id] }
  end
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "item_dto.rb")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := extractFile(f, "packages/homepage_sources/app/public/homepage_sources/item_dto.rb", false, true)

	byName := make(map[string]facts.Fact)
	for _, fact := range result {
		byName[fact.Name] = fact
	}

	meth, ok := byName["HomepageSources::ItemDto#fields_by_id"]
	if !ok {
		t.Fatal("missing method HomepageSources::ItemDto#fields_by_id")
	}
	if !hasCall(meth, "Items::Facade.fetch_item_fields") {
		t.Errorf("fields_by_id missing RelCalls -> Items::Facade.fetch_item_fields; relations = %v", meth.Relations)
	}
}

func TestExtractRubyCalls_QualifiedAndReceiver(t *testing.T) {
	cases := []struct {
		line  string
		want  []string
	}{
		{
			line: "      Items::Facade.fetch_item_fields(ids, ITEM_FIELDS)",
			want: []string{"Items::Facade.fetch_item_fields"},
		},
		{
			line: "      service.call(x)",
			want: []string{"service.call"},
		},
		{
			line: "      Foo::Bar::Baz.do_thing(a, b)",
			want: []string{"Foo::Bar::Baz.do_thing"},
		},
		{
			// Chained call: qualifiedCallRe captures Rails.logger (first segment);
			// receiverCallRe captures logger.info (lowercase receiver with parens).
			line: "      Rails.logger.info('msg')",
			want: []string{"Rails.logger", "logger.info"},
		},
	}

	for _, tc := range cases {
		got := extractRubyCalls(tc.line)
		gotSet := make(map[string]bool)
		for _, g := range got {
			gotSet[g] = true
		}
		for _, w := range tc.want {
			if !gotSet[w] {
				t.Errorf("extractRubyCalls(%q): missing %q in %v", tc.line, w, got)
			}
		}
	}
}

func TestPackwerk_RootDependencyNormalization(t *testing.T) {
	dir := t.TempDir()

	// Create packwerk.yml.
	packwerkYml := `package_paths:
  - "."
  - "packages/*"
`
	if err := os.WriteFile(filepath.Join(dir, "packwerk.yml"), []byte(packwerkYml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Root package.yml.
	rootPkg := `enforce_dependencies: true
`
	if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte(rootPkg), 0o644); err != nil {
		t.Fatal(err)
	}

	// A sub-package that depends on root (".").
	pkgDir := filepath.Join(dir, "packages", "orders")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ordersPkg := `enforce_dependencies: true
dependencies:
  - "."
  - "packages/payments"
`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.yml"), []byte(ordersPkg), 0o644); err != nil {
		t.Fatal(err)
	}

	info := parsePackwerk(dir)
	if !info.detected {
		t.Fatal("packwerk should be detected")
	}

	// Find the orders module fact.
	var ordersFact *facts.Fact
	for i, f := range info.facts {
		if f.Name == "packages/orders" {
			ordersFact = &info.facts[i]
			break
		}
	}
	if ordersFact == nil {
		t.Fatal("missing packages/orders module fact")
	}

	// The dependency on "." should be normalized to "root".
	hasDotTarget := false
	hasRootTarget := false
	for _, r := range ordersFact.Relations {
		if r.Kind == facts.RelDependsOn {
			if r.Target == "." {
				hasDotTarget = true
			}
			if r.Target == "root" {
				hasRootTarget = true
			}
		}
	}
	if hasDotTarget {
		t.Error("dependency target '.' should have been normalized to 'root'")
	}
	if !hasRootTarget {
		t.Error("expected dependency target 'root' after normalization")
	}

	// The root module should be named "root", not ".".
	var rootFact *facts.Fact
	for i, f := range info.facts {
		if f.Name == "root" {
			rootFact = &info.facts[i]
			break
		}
	}
	if rootFact == nil {
		t.Fatal("missing root module fact (should be named 'root', not '.')")
	}
}
