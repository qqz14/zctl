package generator

import (
	"fmt"
	goformat "go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// scanDaoInterfaces returns sorted Dao interface names found in dir
// (e.g. ["OrderDao", "UserDao"]). Used by EnsureEntInfra to decide which
// DAO fields/inits to render into ServiceContext.
func scanDaoInterfaces(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // empty / not exist
	}
	re := regexp.MustCompile(`type\s+(\w+Dao)\s+interface`)
	seen := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_dao.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			seen[m[1]] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// replaceRegion replaces text strictly between begin and end markers, keeping
// the markers themselves intact so subsequent runs can find them again.
//
// Used for regions that should be REGENERATED on each run (e.g. DAO field
// list, which must reflect the current set of *_dao.go files exactly).
// Differs from fillIfEmpty (in ensureent.go) which preserves user edits.
func replaceRegion(src, begin, end, body string) string {
	bIdx := strings.Index(src, begin)
	eIdx := strings.Index(src, end)
	if bIdx < 0 || eIdx < 0 || eIdx < bIdx {
		return src
	}
	lineEnd := strings.Index(src[bIdx:], "\n")
	if lineEnd < 0 {
		return src
	}
	insertAt := bIdx + lineEnd + 1

	// Determine indentation of the end-marker line so the inserted body
	// aligns with the closing marker (handles struct fields and literal init).
	lineStart := strings.LastIndex(src[:eIdx], "\n") + 1
	indent := ""
	for i := lineStart; i < eIdx && (src[i] == ' ' || src[i] == '\t'); i++ {
		indent += string(src[i])
	}

	return src[:insertAt] + body + indent + src[eIdx:]
}

// ensureImport inserts importPath into the first import block if absent.
// Idempotent.
func ensureImport(src, importPath string) string {
	quoted := `"` + importPath + `"`
	if strings.Contains(src, quoted) {
		return src
	}
	openIdx := strings.Index(src, "import (")
	if openIdx < 0 {
		return src
	}
	closeIdx := strings.Index(src[openIdx:], ")")
	if closeIdx < 0 {
		return src
	}
	closeIdx += openIdx
	return src[:closeIdx] + "\t" + quoted + "\n" + src[closeIdx:]
}

// ensureNamedImport inserts a named import (alias "path") into the first
// import block if absent. Idempotent: matches by either the alias-quoted
// form or the bare path so we never duplicate when the user previously
// imported the same package without an alias.
//
// Used by the REPO sentinel injection where the impl package name
// `repoimpl` differs from its directory name `impl` and must be made
// explicit at the import site.
func ensureNamedImport(src, alias, importPath string) string {
	quoted := alias + ` "` + importPath + `"`
	if strings.Contains(src, quoted) {
		return src
	}
	// If the bare import is already present (without alias), upgrade it in-place
	// so we end up with a single named line — avoids duplicate imports.
	bare := `"` + importPath + `"`
	if strings.Contains(src, bare) {
		return strings.Replace(src, bare, quoted, 1)
	}
	openIdx := strings.Index(src, "import (")
	if openIdx < 0 {
		return src
	}
	closeIdx := strings.Index(src[openIdx:], ")")
	if closeIdx < 0 {
		return src
	}
	closeIdx += openIdx
	return src[:closeIdx] + "\t" + quoted + "\n" + src[closeIdx:]
}

// formatGoSource runs gofmt on a Go source string. On parse failure it
// returns the raw text plus a stderr warning so the broken output is still
// inspectable. Used by every generator that emits .go files to guarantee:
//
//  1. Output is gofmt-stable: re-running the generator on already-formatted
//     code is a no-op, which is critical for `git diff` cleanliness when
//     adding/removing models.
//  2. Struct-literal field alignment is computed by gofmt itself rather
//     than by ad-hoc string concatenation. Hand-rolled alignment fights
//     gofmt the moment the longest identifier in a column changes — that
//     was the root cause of the unstable DAOS_INIT diffs after adding
//     IamRolePermissionDao (gofmt's tab-vs-single-space heuristic flipped
//     when one column outgrew the others).
//
// The function intentionally uses the standard library's go/format rather
// than zero's golang.FormatCode wrapper, to avoid an import cycle from the
// top-level generator package back into the rpc-specific helper packages.
func formatGoSource(code, filePath string) string {
	out, err := goformat.Source([]byte(code))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[zctl] gofmt failed for %s: %v (writing raw)\n", filePath, err)
		return code
	}
	return string(out)
}
