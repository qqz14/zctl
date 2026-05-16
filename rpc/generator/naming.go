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
