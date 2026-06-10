// Package checker provides individual static analysis checks for zctl perf scan.
package checker

import (
	"fmt"
	"runtime/debug"
)

// Level represents the severity of a check result.
type Level int

const (
	LevelPass  Level = iota // no issues
	LevelInfo               // informational, not a failure
	LevelWarn               // warning, should be reviewed
	LevelFail               // failure, must be fixed
	LevelSkip               // check was skipped (tool not installed)
	LevelPanic              // checker panicked — result is unknown, NOT clean
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

func panicResult(name string, v any, stack []byte) *Result {
	msg := fmt.Sprintf("%v", v)
	return &Result{
		Level:   LevelPanic,
		Summary: fmt.Sprintf("checker %s panicked: %s", name, msg),
		Issues: []string{
			"⚠️ 此模块在运行时发生 panic，结果未知（不代表代码无问题）",
			"panic: " + msg,
			"stack trace:",
			string(stack),
		},
	}
}

// SafeRun wraps a checker func that returns *Result, catching any panic.
// On panic it returns a LevelPanic result instead of crashing the whole scan.
//
// Usage:
//
//	res = SafeRun("RunFmt", func() *Result { return RunFmt(dir) })
func SafeRun(name string, fn func() *Result) (r *Result) {
	defer func() {
		if v := recover(); v != nil {
			r = panicResult(name, v, debug.Stack())
		}
	}()
	return fn()
}

// SafeRunCG wraps BuildCallGraph, catching any panic.
// Returns (nil, error) on panic so callers treat it as "call graph unavailable".
func SafeRunCG(fn func() (*CallGraphCache, error)) (cg *CallGraphCache, err error) {
	defer func() {
		if v := recover(); v != nil {
			stack := debug.Stack()
			err = fmt.Errorf("call graph panic: %v\n%s", v, stack)
			cg = nil
		}
	}()
	return fn()
}

// safeIssues returns Issues, never nil — safe for range/len.
func (r *Result) safeIssues() []string {
	if r == nil {
		return nil
	}
	return r.Issues
}

// safeLevel returns the result Level, or LevelSkip if r is nil.
func (r *Result) safeLevel() Level {
	if r == nil {
		return LevelSkip
	}
	return r.Level
}
