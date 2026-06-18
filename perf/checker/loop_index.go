package checker

// loop_index.go — Generic for/range loop scanner.
//
// Builds a project-wide map of "file → []LoopRange" by parsing all business
// .go files under internal/ and pkg/. Used by storage IO summary (logic_io.go)
// to decide whether a DB/Redis call site is inside a loop, so the per-Logic
// summary can show "static + N×looped" totals.
//
// The index is built once in BuildCallGraph and is stable independent of
// SSA fset (uses go/parser with a fresh fset).

import (
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoopRange describes one for/range loop body line span.
type LoopRange struct {
	StartLine int // line of the `for`/`range` keyword
	EndLine   int // line of the closing `}` of the loop body
}

// LoopIndex maps absolute file path → list of loop ranges in that file.
type LoopIndex struct {
	byFile map[string][]LoopRange
}

// IsInLoop returns the line of the enclosing loop (or 0 if not in a loop)
// for the given absolute file path and line number.
//
// If multiple loops nest, returns the innermost loop's start line.
func (idx *LoopIndex) IsInLoop(file string, line int) (loopStart int, ok bool) {
	if idx == nil {
		return 0, false
	}
	ranges, found := idx.byFile[file]
	if !found {
		// Try absolute path normalization
		abs, err := filepath.Abs(file)
		if err == nil {
			ranges, found = idx.byFile[abs]
		}
		if !found {
			return 0, false
		}
	}
	innermost := 0
	for _, r := range ranges {
		if line >= r.StartLine && line <= r.EndLine {
			// keep the loop with the largest StartLine (innermost)
			if r.StartLine > innermost {
				innermost = r.StartLine
			}
		}
	}
	if innermost == 0 {
		return 0, false
	}
	return innermost, true
}

// buildLoopIndex walks the project under dir/internal and dir/pkg and records
// every for/range body span. Files in vendor, ent generated code, mocks, tests,
// proto/desc, build, migrations are skipped (mirrors skipDirN1/skipFileN1).
func buildLoopIndex(dir string) *LoopIndex {
	idx := &LoopIndex{byFile: make(map[string][]LoopRange)}
	scanDirs := []string{
		filepath.Join(dir, "internal"),
		filepath.Join(dir, "pkg"),
	}
	fset := token.NewFileSet()
	for _, scanDir := range scanDirs {
		if _, err := os.Stat(scanDir); err != nil {
			continue
		}
		_ = filepath.WalkDir(scanDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipLoopIdxDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			f, err := parseWithGoParser(fset, path, src)
			if err != nil {
				return nil
			}

			abs, _ := filepath.Abs(path)
			var ranges []LoopRange
			ast.Inspect(f, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.ForStmt:
					if s.Body != nil {
						ranges = append(ranges, LoopRange{
							StartLine: fset.Position(s.For).Line,
							EndLine:   fset.Position(s.Body.Rbrace).Line,
						})
					}
				case *ast.RangeStmt:
					if s.Body != nil {
						ranges = append(ranges, LoopRange{
							StartLine: fset.Position(s.For).Line,
							EndLine:   fset.Position(s.Body.Rbrace).Line,
						})
					}
				}
				return true
			})
			if len(ranges) > 0 {
				idx.byFile[abs] = ranges
				if abs != path {
					idx.byFile[path] = ranges
				}
			}
			return nil
		})
	}
	return idx
}

func skipLoopIdxDir(name string) bool {
	switch name {
	case "ent", "types", "mock", "mocks", "vendor", ".git",
		"migrations", "proto", "desc", "build":
		return true
	}
	return false
}
