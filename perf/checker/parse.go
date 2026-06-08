package checker

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// parseWithGoParser parses a Go source file using go/parser.
// Exported as a variable in n1.go so tests can replace it.
func parseWithGoParser(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	return parser.ParseFile(fset, filename, src, 0)
}
