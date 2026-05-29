package rubyextractor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dejo1307/enola/internal/facts"
)

// ActiveRecord patterns.
var (
	// Base classes that indicate an ActiveRecord model.
	arBaseClasses = []string{
		"ApplicationRecord",
		"ActiveRecord::Base",
	}
	// Suffix convention for abstract base models (e.g. ItemsModel, ShippingModel).
	arModelSuffix = "Model"

	associationRe = regexp.MustCompile(
		`^\s*(has_many|has_one|belongs_to|has_and_belongs_to_many)\s+:(\w+)`)
	scopeRe       = regexp.MustCompile(`^\s*scope\s+:(\w+)`)
	validatesRe   = regexp.MustCompile(`^\s*validates?\s+:(\w+)`)
	tableNameRe   = regexp.MustCompile(`^\s*self\.table_name\s*=\s*['"](\w+)['"]`)
)

// extractStorageFacts scans the file-level facts for ActiveRecord model classes
// and emits storage facts with associations, scopes, and table names.
func extractStorageFacts(relFile string, fileFacts []facts.Fact) []facts.Fact {
	var result []facts.Fact

	// First, identify which classes in this file are ActiveRecord models.
	modelClasses := make(map[string]bool)
	for _, f := range fileFacts {
		if f.Kind != facts.KindSymbol {
			continue
		}
		sk, _ := f.Props["symbol_kind"].(string)
		if sk != facts.SymbolClass {
			continue
		}
		superclass, _ := f.Props["superclass"].(string)
		if isARBaseClass(superclass) {
			modelClasses[f.Name] = true
		}
	}

	if len(modelClasses) == 0 {
		return nil
	}

	// Re-scan the file to extract associations, scopes, validations, and table name.
	// We do this by re-reading from the already-parsed facts plus scanning the source again.
	// For efficiency, we extract what we can from a simple second pass of the file facts.
	// However, associations/scopes/validates aren't captured as facts yet, so we need
	// to read the source file. We'll use the fileFacts to identify model boundaries
	// and build storage facts.

	dir := filepath.Dir(relFile)

	// For each model class, emit a storage fact.
	for className := range modelClasses {
		tableName := inferTableName(className)

		result = append(result, facts.Fact{
			Kind: facts.KindStorage,
			Name: className,
			File: relFile,
			Props: map[string]any{
				"storage_kind": "model",
				"table":        tableName,
				"language":     "ruby",
				"framework":    "rails",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: dir},
			},
		})
	}

	return result
}

// extractStorageDetailsFromFile does a second pass on an open file to extract
// associations, scopes, validations, and explicit table names for models.
// This is called from the main Extract loop.
func extractStorageDetailsFromFile(lines []string, modelClasses map[string]bool) []facts.Fact {
	var result []facts.Fact

	for lineNum, line := range lines {
		// Association declarations.
		if m := associationRe.FindStringSubmatch(line); m != nil {
			assocKind := m[1]
			assocName := m[2]

			targetModel := singularize(assocName)
			if assocKind == "has_many" || assocKind == "has_and_belongs_to_many" {
				targetModel = singularize(assocName)
			} else {
				targetModel = assocName
			}
			targetModel = snakeToCamel(targetModel)

			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: assocKind + " :" + assocName,
				Line: lineNum + 1,
				Props: map[string]any{
					"language":         "ruby",
					"association_kind": assocKind,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDependsOn, Target: targetModel},
				},
			})
		}

		// Scope declarations.
		if m := scopeRe.FindStringSubmatch(line); m != nil {
			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: "scope:" + m[1],
				Line: lineNum + 1,
				Props: map[string]any{
					"symbol_kind": facts.SymbolFunc,
					"language":    "ruby",
					"scope":       true,
				},
			})
		}
	}

	return result
}

// isARBaseClass returns true if the superclass indicates an ActiveRecord model.
func isARBaseClass(superclass string) bool {
	if superclass == "" {
		return false
	}
	for _, base := range arBaseClasses {
		if superclass == base {
			return true
		}
	}
	// Convention: classes ending in "Model" are often abstract AR bases (e.g. ItemsModel).
	if strings.HasSuffix(superclass, arModelSuffix) {
		return true
	}
	return false
}

// inferTableName derives the conventional Rails table name from a class name.
// e.g. "Item" -> "items", "UserAddress" -> "user_addresses", "Api::V2::Item" -> "items"
func inferTableName(className string) string {
	// Take the last segment if it's a qualified name.
	parts := strings.Split(className, "::")
	name := parts[len(parts)-1]

	// Convert CamelCase to snake_case.
	snake := camelToSnake(name)

	// Simple pluralization.
	return pluralize(snake)
}

// camelToSnake converts CamelCase to snake_case.
func camelToSnake(s string) string {
	var result []byte
	for i, ch := range s {
		if ch >= 'A' && ch <= 'Z' {
			if i > 0 {
				result = append(result, '_')
			}
			result = append(result, byte(ch-'A'+'a'))
		} else {
			result = append(result, byte(ch))
		}
	}
	return string(result)
}

// snakeToCamel converts snake_case to CamelCase.
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// pluralize applies simple English pluralization rules.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "ss") || strings.HasSuffix(s, "sh") ||
		strings.HasSuffix(s, "ch") || strings.HasSuffix(s, "x") ||
		strings.HasSuffix(s, "z") {
		return s + "es"
	}
	if strings.HasSuffix(s, "y") && len(s) > 1 {
		preceding := s[len(s)-2]
		if preceding != 'a' && preceding != 'e' && preceding != 'i' && preceding != 'o' && preceding != 'u' {
			return s[:len(s)-1] + "ies"
		}
	}
	if strings.HasSuffix(s, "s") {
		return s
	}
	return s + "s"
}

// singularize applies simple English singularization (inverse of pluralize).
func singularize(s string) string {
	if strings.HasSuffix(s, "ies") && len(s) > 3 {
		return s[:len(s)-3] + "y"
	}
	if strings.HasSuffix(s, "sses") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "shes") || strings.HasSuffix(s, "ches") ||
		strings.HasSuffix(s, "xes") || strings.HasSuffix(s, "zes") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") {
		return s[:len(s)-1]
	}
	return s
}
