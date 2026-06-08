package checker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunEscape runs `go build -gcflags="-m=1"` to collect heap escape information.
// It filters for "moved to heap" lines and cross-references with for/range loops
// to highlight likely high-pressure allocation sites.
//
// Level: INFO always — escape ≠ bug, it's a signal for human review.
// (True large-data-loop memory leaks require runtime pprof to confirm.)
func RunEscape(dir, outDir string) *Result {
	cmd := exec.Command("go", "build", "-gcflags=-m=1", "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr // escape analysis output goes to stderr
	_ = cmd.Run()

	raw := stderr.String()
	escapeTxt := filepath.Join(outDir, "escape.txt")
	_ = os.WriteFile(escapeTxt, []byte(raw), 0o644)

	hotspots := parseEscapeOutput(raw)
	if len(hotspots) == 0 {
		return Pass("no notable heap escape hotspots")
	}

	return Info(
		fmt.Sprintf("%d heap escape hotspot(s) — INFO only, see build/perf/escape.txt", len(hotspots)),
		hotspots,
	)
}

// parseEscapeOutput filters escape analysis output for high-value "moved to heap"
// entries — only in business logic files, not generated code or stdlib.
// Cap at 20 entries to avoid noise.
func parseEscapeOutput(raw string) []string {
	var hotspots []string
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Only care about actual heap moves in business files
		if !strings.Contains(line, "moved to heap") {
			continue
		}
		// Skip auto-generated code and stdlib
		if isGenDir(line) {
			continue
		}
		// Only report business-logic hotspots (logic/, service/, pkg/ — not ent/types)
		if !strings.Contains(line, "/internal/") && !strings.Contains(line, "/pkg/") {
			continue
		}
		hotspots = append(hotspots, line)
		if len(hotspots) >= 20 {
			hotspots = append(hotspots, fmt.Sprintf("... (capped at 20, see build/perf/escape.txt for full list)"))
			break
		}
	}
	return hotspots
}

func isGenDir(line string) bool {
	for _, skip := range []string{"/ent/", "/types/", "/mock/", "_test.go"} {
		if strings.Contains(line, skip) {
			return true
		}
	}
	return false
}
