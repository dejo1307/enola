// Compiles the external scanner as its own cgo translation unit, separate from
// the generated parser (see cgo_parser.c) to avoid -Wmacro-redefined.
#include "src/scanner.c"
