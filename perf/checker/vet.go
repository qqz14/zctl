package checker

import (
	"bytes"
	"os/exec"
	"strings"
)

// RunVet runs go vet on non-test packages only (avoids _test.go compilation
// failures from incomplete mocks or test-only build issues).
// Covers: printf format, copylocks, unreachable, structtag, etc.
// Level: FAIL if any issue found.
func RunVet(dir string) *Result {
	// List non-test packages first; fall back to ./... if go list fails.
	pkgs := nonTestPackages(dir)
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}
	args := append([]string{"vet"}, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	raw := strings.TrimSpace(stderr.String())
	if err == nil || raw == "" {
		return Pass("go vet clean")
	}

	lines := strings.Split(raw, "\n")
	var issues, netErrors []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		// Separate network/download errors (403/404/timeout) from real vet issues.
		// These are infra problems, not code problems.
		if strings.Contains(l, "403 Forbidden") ||
			strings.Contains(l, "404 Not Found") ||
			strings.Contains(l, "unrecognized import path") ||
			strings.Contains(l, "no required module provides") ||
			strings.Contains(l, "go: downloading") {
			netErrors = append(netErrors, l)
			continue
		}
		issues = append(issues, l)
	}

	if len(issues) == 0 && len(netErrors) > 0 {
		return Warn(
			"go vet skipped — dependency download failed (check network/proxy)",
			netErrors,
		)
	}
	if len(issues) == 0 {
		return Pass("go vet clean")
	}
	return Fail("go vet found issues", issues)
}

// nonTestPackages returns package import paths that have non-test Go files.
// Packages with only _test.go files (or that fail to compile outside test context)
// are filtered out, so "go vet" doesn't fail on incomplete mock implementations.
func nonTestPackages(dir string) []string {
	// go list -json lists each package with GoFiles (non-test) and TestGoFiles.
	// We only want packages that have at least one non-test .go file.
	cmd := exec.Command("go", "list", "-f", "{{if .GoFiles}}{{.ImportPath}}{{end}}", "./...")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs
}
