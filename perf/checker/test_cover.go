package checker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PkgCoverage holds per-package coverage stats.
type PkgCoverage struct {
	Pkg     string  // e.g. "internal/logic/user"
	Stmts   int     // total statements
	Hit     int     // covered statements
	Pct     float64 // Hit/Stmts * 100
	Files   int     // number of files in this package
}

// lastCoverPkgs is set by RunTestCover and consumed by buildGroups.
var lastCoverPkgs []PkgCoverage

// RunTestCover runs `go test -coverprofile` on the project, generates an HTML
// coverage report via `go tool cover -html`, and returns a Result with the
// overall coverage percentage.
//
// The HTML report is written to outDir/details/cover.html and embedded in the
// main report via an iframe — same pattern as the lint raw report.
// No changes to business code required.
func RunTestCover(dir, outDir string) *Result {
	// Create details/ first so all output files go directly to the target path.
	// (WriteReportHTML also creates it, but RunTestCover runs before that)
	detailsDir := filepath.Join(outDir, "details")
	if err := os.MkdirAll(detailsDir, 0o755); err != nil {
		return &Result{Level: LevelSkip, Summary: "failed to create details dir: " + err.Error()}
	}

	coverProfile := filepath.Join(detailsDir, "cover.out")
	coverHTML := filepath.Join(detailsDir, "cover.html")

	// Step 1: go test -coverprofile=cover.out ./...
	cmd := exec.Command("go", "test",
		"-count=1",
		"-timeout=120s",
		"-coverprofile="+coverProfile,
		"-covermode=atomic",
		"./...",
	)
	cmd.Dir = dir
	var testOut strings.Builder
	cmd.Stdout = &testOut
	cmd.Stderr = &testOut
	testErr := cmd.Run()

	// Step 2: generate HTML even if some tests failed (partial coverage is useful)
	if _, err := os.Stat(coverProfile); err == nil {
		htmlCmd := exec.Command("go", "tool", "cover",
			"-html="+coverProfile,
			"-o="+coverHTML,
		)
		htmlCmd.Dir = dir
		_ = htmlCmd.Run()
	}

	// Step 3: parse overall coverage percentage from profile
	pct, coveredFiles, totalFiles, pkgs := parseCoverProfile(coverProfile)
	lastCoverPkgs = pkgs

	// Step 4: collect failed test packages from test output
	var failedPkgs []string
	for _, line := range strings.Split(testOut.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FAIL\t") || strings.HasPrefix(line, "--- FAIL:") {
			failedPkgs = append(failedPkgs, line)
		}
	}

	// Build result
	if testErr != nil && len(failedPkgs) > 0 {
		issues := failedPkgs
		if pct >= 0 {
			issues = append([]string{fmt.Sprintf("coverage: %.1f%% (%d/%d files)", pct, coveredFiles, totalFiles)}, issues...)
		}
		return &Result{
			Level:   LevelWarn,
			Summary: fmt.Sprintf("tests failed, coverage %.1f%%", pct),
			Issues:  issues,
		}
	}

	if pct < 0 {
		// No coverage data at all (build error or no test files)
		if testErr != nil {
			lines := strings.Split(strings.TrimSpace(testOut.String()), "\n")
			var errs []string
			for _, l := range lines {
				if l = strings.TrimSpace(l); l != "" {
					errs = append(errs, l)
				}
			}
			return &Result{
				Level:   LevelSkip,
				Summary: "go test failed to run",
				Issues:  errs,
			}
		}
		return Pass("no test files found")
	}

	var level Level
	switch {
	case pct >= 80:
		level = LevelPass
	case pct >= 60:
		level = LevelInfo
	default:
		level = LevelWarn
	}

	return &Result{
		Level:   level,
		Summary: fmt.Sprintf("coverage %.1f%% (%d/%d files)", pct, coveredFiles, totalFiles),
		Issues:  []string{fmt.Sprintf("%.1f%% statement coverage — see details/cover.html", pct)},
	}
}

// parseCoverProfile parses a go cover profile and returns:
// (overallPct, coveredFiles, totalFiles, pkgStats). Returns -1 pct on error.
//
// Profile format:
//
//	mode: atomic
//	module/pkg/file.go:startLine.startCol,endLine.endCol numStatements count
func parseCoverProfile(profilePath string) (pct float64, covered, total int, pkgs []PkgCoverage) {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return -1, 0, 0, nil
	}

	type fileStats struct{ stmts, hit int }
	files := map[string]*fileStats{}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		numStmts, err1 := strconv.Atoi(parts[1])
		count, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			continue
		}
		// parts[0] = "module/pkg/file.go:L.C,L.C" — strip the position suffix
		fileField := parts[0]
		if idx := strings.LastIndex(fileField, ":"); idx >= 0 {
			fileField = fileField[:idx]
		}
		if _, ok := files[fileField]; !ok {
			files[fileField] = &fileStats{}
		}
		files[fileField].stmts += numStmts
		if count > 0 {
			files[fileField].hit += numStmts
		}
	}

	if len(files) == 0 {
		return -1, 0, 0, nil
	}

	// Aggregate by package (directory part of the path)
	type pkgAgg struct {
		stmts, hit, fileCount int
	}
	pkgMap := map[string]*pkgAgg{}
	var totalStmts, hitStmts int

	for filePath, fs := range files {
		totalStmts += fs.stmts
		hitStmts += fs.hit
		total++
		if fs.hit > 0 {
			covered++
		}
		// Strip module prefix: "github.com/foo/bar/internal/pkg/file.go" → "internal/pkg"
		// We use the last 2+ path segments starting after the module root heuristic:
		// find the rightmost slash before "internal/", "pkg/", "cmd/" etc.
		pkg := pkgDir(filePath)
		if _, ok := pkgMap[pkg]; !ok {
			pkgMap[pkg] = &pkgAgg{}
		}
		pkgMap[pkg].stmts += fs.stmts
		pkgMap[pkg].hit += fs.hit
		pkgMap[pkg].fileCount++
	}

	for pkg, agg := range pkgMap {
		var p float64
		if agg.stmts > 0 {
			p = float64(agg.hit) / float64(agg.stmts) * 100
		}
		pkgs = append(pkgs, PkgCoverage{
			Pkg:   pkg,
			Stmts: agg.stmts,
			Hit:   agg.hit,
			Pct:   p,
			Files: agg.fileCount,
		})
	}
	// Sort: low coverage first (worst on top), then alphabetically
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].Pct != pkgs[j].Pct {
			return pkgs[i].Pct < pkgs[j].Pct
		}
		return pkgs[i].Pkg < pkgs[j].Pkg
	})

	if totalStmts == 0 {
		return 0, covered, total, pkgs
	}
	return float64(hitStmts) / float64(totalStmts) * 100, covered, total, pkgs
}

// pkgDir extracts a short package path from a cover profile file path.
// Input: "github.com/foo/bar/internal/logic/user/user_logic.go"
// Output: "internal/logic/user"
func pkgDir(filePath string) string {
	// Strip the file name
	dir := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		dir = filePath[:idx]
	}
	// Find a well-known root segment to trim the module prefix
	for _, root := range []string{"/internal/", "/pkg/", "/cmd/", "/api/", "/app/"} {
		if idx := strings.Index(dir, root); idx >= 0 {
			return dir[idx+1:]
		}
	}
	// Fallback: return last 3 path segments
	parts := strings.Split(dir, "/")
	if len(parts) > 3 {
		parts = parts[len(parts)-3:]
	}
	return strings.Join(parts, "/")
}
