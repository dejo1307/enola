package goextractor

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

var (
	reCreateTable = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + "`?" + `"?'?(\w+)`)
	reInsertInto  = regexp.MustCompile(`(?i)INSERT\s+INTO\s+` + "`?" + `"?'?(\w+)`)
	reUpdate      = regexp.MustCompile(`(?i)UPDATE\s+` + "`?" + `"?'?(\w+)\s+SET\b`)
	reDeleteFrom  = regexp.MustCompile(`(?i)DELETE\s+FROM\s+` + "`?" + `"?'?(\w+)`)
	reSelectFrom  = regexp.MustCompile(`(?i)\bFROM\s+` + "`?" + `"?'?(\w+)`)
	reAlterTable  = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+` + "`?" + `"?'?(\w+)`)
)

// extractStorage walks a Go file looking for SQL table references and S3 storage patterns.
func extractStorage(fset *token.FileSet, f *ast.File, relFile, pkgDir string) []facts.Fact {
	var result []facts.Fact

	// Track seen (table, operation) pairs per file to deduplicate
	type tableOp struct{ table, op string }
	seen := make(map[tableOp]bool)

	// Check for S3 imports
	hasS3Import := false
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, "aws-sdk-go") && strings.Contains(path, "/s3") {
			hasS3Import = true
			break
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			// Unquote the string literal
			val, err := strconv.Unquote(node.Value)
			if err != nil {
				// Try raw string (backtick) — already unquoted by removing backticks
				val = strings.Trim(node.Value, "`")
			}

			line := fset.Position(node.Pos()).Line

			// Check SQL patterns
			sqlPatterns := []struct {
				re        *regexp.Regexp
				operation string
				kind      string
			}{
				{reCreateTable, "CREATE", "table"},
				{reAlterTable, "ALTER", "table"},
				{reInsertInto, "INSERT", "table_reference"},
				{reUpdate, "UPDATE", "table_reference"},
				{reDeleteFrom, "DELETE", "table_reference"},
				{reSelectFrom, "SELECT", "table_reference"},
			}

			for _, pat := range sqlPatterns {
				matches := pat.re.FindAllStringSubmatch(val, -1)
				for _, m := range matches {
					tableName := m[1]
					// Skip common SQL noise words
					if isSQLNoise(tableName) {
						continue
					}
					key := tableOp{tableName, pat.operation}
					if seen[key] {
						continue
					}
					seen[key] = true

					result = append(result, facts.Fact{
						Kind: facts.KindStorage,
						Name: tableName,
						File: relFile,
						Line: line,
						Props: map[string]any{
							"storage_kind": pat.kind,
							"operation":    pat.operation,
							"language":     "go",
						},
						Relations: []facts.Relation{
							{Kind: facts.RelDeclares, Target: pkgDir},
						},
					})
				}
			}

		case *ast.TypeSpec:
			// Detect S3 storage structs
			if !hasS3Import {
				return true
			}
			st, ok := node.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				typeName := typeExprToString(field.Type)
				if strings.Contains(typeName, "s3.Client") || strings.Contains(typeName, "s3.S3") {
					result = append(result, facts.Fact{
						Kind: facts.KindStorage,
						Name: node.Name.Name,
						File: relFile,
						Line: fset.Position(node.Pos()).Line,
						Props: map[string]any{
							"storage_kind": "s3",
							"language":     "go",
						},
						Relations: []facts.Relation{
							{Kind: facts.RelDeclares, Target: pkgDir},
						},
					})
					break
				}
			}
		}
		return true
	})

	return result
}

// isSQLNoise returns true for common SQL keywords that are not table names.
func isSQLNoise(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "select", "from", "where", "set", "into", "values", "table",
		"index", "view", "trigger", "procedure", "function",
		"dual", "information_schema", "pg_catalog":
		return true
	}
	return false
}
