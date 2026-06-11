// Compiles the generated parser as its own cgo translation unit.
//
// Kept separate from the external scanner (see cgo_scanner.c): both src files
// define grammar-local macros such as TOKEN_COUNT, so sharing a translation unit
// would warn with -Wmacro-redefined. See binding.go.
#include "src/parser.c"
