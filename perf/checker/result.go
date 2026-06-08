// Package checker provides individual static analysis checks for zctl perf scan.
package checker

// Level represents the severity of a check result.
type Level int

const (
	LevelPass Level = iota // no issues
	LevelInfo              // informational, not a failure
	LevelWarn              // warning, should be reviewed
	LevelFail              // failure, must be fixed
	LevelSkip              // check was skipped (tool not installed)
)

// Result holds the output of a single checker.
type Result struct {
	Level   Level
	Summary string   // one-line summary shown in terminal
	Issues  []string // list of issues (file:line: message)
	Detail  string   // raw output (written to separate file if needed)
}

func Pass(summary string) *Result {
	return &Result{Level: LevelPass, Summary: summary}
}

func Fail(summary string, issues []string) *Result {
	return &Result{Level: LevelFail, Summary: summary, Issues: issues}
}

func Warn(summary string, issues []string) *Result {
	return &Result{Level: LevelWarn, Summary: summary, Issues: issues}
}

func Info(summary string, issues []string) *Result {
	return &Result{Level: LevelInfo, Summary: summary, Issues: issues}
}

func Skip(reason string) *Result {
	return &Result{Level: LevelSkip, Summary: reason}
}
