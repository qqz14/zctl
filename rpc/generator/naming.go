package generator

import (
	"strings"

	"github.com/ettle/strcase"
)

// ──── Naming convention functions ────
//
// All code generation MUST use these functions for name conversion.
// Never call strings.ToLower / manual snake/camel conversion directly.
//
// Rules:
//   File names       → snake_case ("UserInfo" → "user_info")      → use FileSnake()
//   Directory names   → all lowercase ("UserInfo" → "userinfo")    → use DirName()
//   Go package names  → all lowercase ("UserInfo" → "userinfo")    → use PkgName()
//   Ent package names → all lowercase ("UserInfo" → "userinfo")    → use EntPkg()
//   Go PascalCase     → with initialisms ("api_code" → "APICode") → use GoPascal()
//   Go camelCase      → with initialisms ("api_code" → "apiCode") → use GoCamel()

// FileSnake converts PascalCase to snake_case for **file names only**.
// Uses strcase.ToSnake which correctly handles initialisms and edge cases.
// "UserInfo" → "user_info", "User" → "user", "IamApp" → "iam_app"
func FileSnake(s string) string {
	return strcase.ToSnake(s)
}

// DirName converts PascalCase to all-lowercase for **directory names**.
// Follows Go package naming convention: no underscores.
// "UserInfo" → "userinfo", "User" → "user"
func DirName(s string) string {
	return strings.ToLower(s)
}

// PkgName converts PascalCase to all-lowercase for **Go package names**.
// Same as DirName — Go convention requires all-lowercase, no underscores.
// "UserInfo" → "userinfo", "User" → "user"
func PkgName(s string) string {
	return strings.ToLower(s)
}

// EntPkg returns the ent-generated package name (used in import paths).
// ent uses all-lowercase: "UserInfo" → "userinfo"
// This MUST match what `go run entgo.io/ent/cmd/ent generate` produces.
func EntPkg(s string) string {
	return strings.ToLower(s)
}

// GoPascal converts snake_case to PascalCase with Go initialisms support.
// "api_code" → "APICode", "owner_uid" → "OwnerUID", "app_name" → "AppName"
// Uses ettle/strcase.ToGoPascal which follows Go naming conventions.
func GoPascal(s string) string {
	return strcase.ToGoPascal(s)
}

// GoCamel converts snake_case to lowerCamelCase with Go initialisms support.
// "api_code" → "apiCode", "owner_uid" → "ownerUID"
func GoCamel(s string) string {
	return strcase.ToGoCamel(s)
}

// LowerCamel converts PascalCase to lowerCamelCase for struct field / variable names.
// "UserInfo" → "userInfo", "User" → "user"
func LowerCamel(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ProtoPkg converts an arbitrary service name to the canonical "directory /
// proto-package" identifier — a single all-lowercase token with no separators.
//
// This is the single source of truth for both:
//   - proto3 `package xxx;` / `option go_package = "./xxx";`
//   - all generated directory names that derive from the service name
//     (e.g. types/{xxx}/, {xxx}_client/)
//
// Rules: drop everything that is not [A-Za-z0-9], then ToLower. We drop "_" as
// well so the three accepted user input styles collapse to the same token,
// matching the agreed convention "directory name = csagentrpc, regardless of
// whether the user typed cs-agent-rpc / cs_agent_rpc / CsAgentRpc".
//
//	"cs-agent-rpc" → "csagentrpc"
//	"cs_agent_rpc" → "csagentrpc"
//	"CsAgentRpc"   → "csagentrpc"
//	"My-Svc_v2"    → "mysvcv2"
func ProtoPkg(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			// every other char (incl. '-' and '_') is dropped
		}
	}
	return b.String()
}

// ServiceGoIdent converts an arbitrary service name to the canonical PascalCase
// Go identifier used as the proto `service Xxx` name and Go struct receiver.
//
// Unlike GoPascal (which leans on strcase's Go-style initialism table and turns
// "rpc"/"api"/"id" into "RPC"/"API"/"ID"), this function intentionally does NOT
// expand initialisms — every word is Title-cased only. This keeps the user's
// service name predictable and round-trip stable across the three accepted input
// styles.
//
//	"cs-agent-rpc" → "CsAgentRpc"
//	"cs_agent_rpc" → "CsAgentRpc"
//	"CsAgentRpc"   → "CsAgentRpc"
//	"my-svc_v2"    → "MySvcV2"
func ServiceGoIdent(s string) string {
	if s == "" {
		return s
	}
	// Split on any non-letter / non-digit boundary, plus the lower→upper boundary
	// inside camelCase / PascalCase input.
	var (
		words   []string
		current strings.Builder
	)
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '-' || r == '_' || r == ' ' || r == '.':
			flush()
		case r >= 'A' && r <= 'Z':
			// Treat lower→Upper as a word boundary so that "csAgentRpc"
			// → ["cs","Agent","Rpc"] and "CsAgentRpc" → ["Cs","Agent","Rpc"].
			if i > 0 {
				prev := runes[i-1]
				if prev >= 'a' && prev <= 'z' {
					flush()
				}
			}
			current.WriteRune(r)
		default:
			current.WriteRune(r)
		}
	}
	flush()

	var b strings.Builder
	for _, w := range words {
		if w == "" {
			continue
		}
		// Title-case the word: first rune Upper, rest lowered.
		first := []rune(w)[0]
		if first >= 'a' && first <= 'z' {
			first = first - ('a' - 'A')
		}
		b.WriteRune(first)
		for _, r := range []rune(w)[1:] {
			if r >= 'A' && r <= 'Z' {
				r = r + ('a' - 'A')
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DashName converts an arbitrary service name to dash form, which is what we
// emit as the Docker image tag (Makefile's `SERVICE_DASH`).
//
//	"cs-agent-rpc" → "cs-agent-rpc"
//	"cs_agent_rpc" → "cs-agent-rpc"
//	"CsAgentRpc"   → "cs-agent-rpc"
//	"csAgentRpc"   → "cs-agent-rpc"
//	"My-Svc_v2"    → "my-svc-v2"
//
// Rules:
//  1. lower→Upper boundary inside camelCase becomes "-"
//  2. "_" and " " become "-"
//  3. result is lower-cased
//  4. consecutive dashes are collapsed
func DashName(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteRune('-')
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				prev := runes[i-1]
				if prev >= 'a' && prev <= 'z' {
					b.WriteRune('-')
				}
			}
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	// Collapse repeated dashes and trim edges.
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-")
	return out
}

// EnvVarName converts an arbitrary service name to an UPPER_SNAKE_CASE token
// suitable for embedding in shell environment variable names (e.g. used in
// `etc/{name}.yaml.template` to build `${CS_AGENT_RPC_RPC_PORT}`).
//
//	"cs-agent-rpc" → "CS_AGENT_RPC"
//	"cs_agent_rpc" → "CS_AGENT_RPC"
//	"CsAgentRpc"   → "CS_AGENT_RPC"
func EnvVarName(s string) string {
	dash := DashName(s)
	return strings.ToUpper(strings.ReplaceAll(dash, "-", "_"))
}