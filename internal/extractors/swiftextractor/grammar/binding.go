// Package grammar vendors the tree-sitter Swift grammar (alex-pinkus/tree-sitter-swift).
//
// The upstream Go binding (github.com/alex-pinkus/tree-sitter-swift/bindings/go)
// cannot be consumed directly because that repository git-ignores the generated
// src/parser.c, so `go build` against the published module fails to compile. To
// avoid that, the parser is generated from the grammar's grammar.json (vendored
// here alongside the hand-written scanner.c) and committed under src/. Regenerate
// with: cd grammar && tree-sitter generate grammar.json --abi 14
//
// The generated parser (src/parser.c) and the external scanner (src/scanner.c)
// are compiled as separate translation units via the cgo_*.c shims in this
// directory, NOT #include'd together here. Both files define grammar-local macros
// (e.g. TOKEN_COUNT), so combining them into one translation unit triggers
// -Wmacro-redefined warnings; compiling them separately is the standard
// go-tree-sitter layout and is warning-free.
package grammar

// #cgo CFLAGS: -std=c11 -fPIC -I${SRCDIR}/src
// #include "tree_sitter/parser.h"
// const TSLanguage *tree_sitter_swift(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language pointer for Swift, suitable for
// sitter.NewLanguage(...).
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_swift())
}
