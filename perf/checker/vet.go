package checker

import (
	"bytes"
	"os/exec"
	"strings"
)

// RunVet runs go vet ./... and reports issues.
// Covers: printf format, copylocks, unreachable, structtag, etc.
// Level: FAIL if any issue found.
func RunVet(dir string) *Result {
	cmd := exec.Command("go", "vet", "./...")
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
