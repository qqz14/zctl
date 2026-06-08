package checker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunVuln runs govulncheck to detect known CVEs in dependencies.
// Level: FAIL if any vulnerability found; PASS otherwise.
func RunVuln(dir, outDir string) *Result {
	if _, err := exec.LookPath("govulncheck"); err != nil {
		return Skip("govulncheck not installed (run: go install golang.org/x/vuln/cmd/govulncheck@latest)")
	}

	txtOut := filepath.Join(outDir, "vuln.txt")

	cmd := exec.Command("govulncheck", "./...")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	raw := out.String()
	_ = os.WriteFile(txtOut, []byte(raw), 0o644)

	issues := parseVulnOutput(raw)
	if len(issues) == 0 {
		return Pass("no known vulnerabilities found")
	}
	return Fail(fmt.Sprintf("%d vulnerability(ies) found — see build/perf/vuln.txt", len(issues)), issues)
}

// parseVulnOutput extracts vulnerability IDs from govulncheck plain text output.
// Looks for lines like:
//
//	Vulnerability #1: GO-2024-XXXX
//	  ...
func parseVulnOutput(raw string) []string {
	var issues []string
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Vulnerability #") {
			// Next non-empty line is the description
			desc := ""
			for j := i + 1; j < len(lines) && j < i+5; j++ {
				if d := strings.TrimSpace(lines[j]); d != "" {
					desc = d
					break
				}
			}
			if desc != "" {
				issues = append(issues, line+": "+desc)
			} else {
				issues = append(issues, line)
			}
		}
	}
	return issues
}
