package checker

import (
	"bytes"
	"os/exec"
	"strings"
)

// RunFmt runs gofmt -l to find unformatted files.
// Level: FAIL if any file is not gofmt-clean.
func RunFmt(dir string) *Result {
	cmd := exec.Command("gofmt", "-l", ".")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	raw := strings.TrimSpace(out.String())
	if raw == "" {
		return Pass("all files are gofmt-clean")
	}

	files := strings.Split(raw, "\n")
	var issues []string
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			issues = append(issues, f+": not gofmt-clean (run: gofmt -w "+f+")")
		}
	}
	return Fail("unformatted files found", issues)
}
