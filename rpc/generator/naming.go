package generator

import "strings"

// ──── Naming convention functions ────
//
// All code generation MUST use these functions for name conversion.
// Never call strings.ToLower / toSnakeCase directly on model names.
//
// Rules:
//   File names    → snake_case ("UserInfo" → "user_info")     → use FileSnake()
//   Directory names → all lowercase  ("UserInfo" → "userinfo") → use DirName()
//   Go package names → all lowercase ("UserInfo" → "userinfo") → use PkgName()
//   Ent package names → all lowercase ("UserInfo" → "userinfo")→ use EntPkg()
//   Go struct/type  → PascalCase (keep as-is)                  → use original

// FileSnake converts PascalCase to snake_case for **file names only**.
// "UserInfo" → "user_info", "User" → "user"
func FileSnake(s string) string {
	var result strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result.WriteByte('_')
		}
		result.WriteRune(r)
	}
	return strings.ToLower(result.String())
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

// LowerCamel converts PascalCase to lowerCamelCase for struct field / variable names.
// "UserInfo" → "userInfo", "User" → "user"
func LowerCamel(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
