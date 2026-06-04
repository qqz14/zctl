// Package name 提供所有代码生成命名转换的唯一入口（SDK）。
//
// 设计原则：
//  1. 三方库能正确处理的，直接用（ettle/strcase）
//  2. 三方库处理不了的（缩写词复数 CIDs/IDs/URLs），在此包内修复
//  3. 所有调用方（rpc/generator、model、api）必须 import 此包，禁止自行实现
//
// 函数分类：
//
//	文件名   → FileSnake / RpcLogicFileName / RpcServerFileName / RpcCallFileName
//	目录名   → DirName / ProtoPkg
//	包名     → PkgName / EntPkg
//	Go标识符 → GoPascal / GoCamel / LowerCamel / ServiceGoIdent
//	其他     → DashName / EnvVarName
//	校验     → IsNamingValid / FormatFilename（旧接口，向后兼容）
package name

import (
	"bytes"
	"strings"

	"github.com/ettle/strcase"
	"github.com/qqz14/zctl/util/stringx"
)

// ──────────────────────────────────────────────
// 旧接口（向后兼容）
// ──────────────────────────────────────────────

// NamingStyle the type of string
type NamingStyle = string

const (
	// NamingLower defines the lower spell case
	NamingLower NamingStyle = "lower"
	// NamingCamel defines the camel spell case
	NamingCamel NamingStyle = "camel"
	// NamingSnake defines the snake spell case
	NamingSnake NamingStyle = "snake"
)

// IsNamingValid validates whether the namingStyle is valid or not, return
// namingStyle and true if it is valid, or else return empty string and false.
func IsNamingValid(namingStyle string) (NamingStyle, bool) {
	if len(namingStyle) == 0 {
		namingStyle = NamingLower
	}
	switch namingStyle {
	case NamingLower, NamingCamel, NamingSnake:
		return namingStyle, true
	default:
		return "", false
	}
}

// FormatFilename converts the filename string to the target naming style.
func FormatFilename(filename string, style NamingStyle) string {
	switch style {
	case NamingCamel:
		return stringx.From(filename).ToCamel()
	case NamingSnake:
		return stringx.From(filename).ToSnake()
	default:
		return strings.ToLower(stringx.From(filename).ToCamel())
	}
}

// ──────────────────────────────────────────────
// 文件名
// ──────────────────────────────────────────────

// FileSnake 将标识符转为 snake_case，用于所有文件名生成。
//
// 正确处理：
//   - 普通 PascalCase：UserInfo → user_info
//   - 缩写词：GetHTTPStatus → get_http_status
//   - 缩写词复数：CIDs → cids，GetUserIDs → get_user_ids（三方库的 bug，此处修复）
//   - 已是 snake_case：get_user_info → get_user_info（原样）
//   - 含破折号：cs-agent-rpc → cs-agent-rpc（原样，破折号不是 Go 标识符边界）
func FileSnake(s string) string {
	tokens, err := splitIdent(s)
	if err != nil || len(tokens) == 0 {
		return strings.ToLower(s)
	}
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = strings.ToLower(t)
	}
	return strings.Join(parts, "_")
}

// RpcLogicFileName 返回 RPC 方法对应 logic 文件的 snake_case 名（不含 .go 后缀）。
//
//	"CsGetAccessibleCIDs" → "cs_get_accessible_cids_logic"
//	"GetUserIDs"          → "get_user_ids_logic"
func RpcLogicFileName(rpcName string) string {
	return FileSnake(rpcName + "_logic")
}

// RpcServerFileName 返回服务 server 文件的 snake_case 名（不含 .go 后缀）。
//
//	"CsAgentRpc" → "cs_agent_rpc_server"
func RpcServerFileName(serviceName string) string {
	return FileSnake(serviceName + "_server")
}

// RpcCallFileName 返回服务 call（client stub）文件的 snake_case 名（不含 .go 后缀）。
//
//	"CsAgentRpc" → "cs_agent_rpc"
func RpcCallFileName(serviceName string) string {
	return FileSnake(serviceName)
}

// ──────────────────────────────────────────────
// 目录名 / 包名
// ──────────────────────────────────────────────

// DirName 将 PascalCase 转为全小写目录名（Go 包命名约定：无下划线）。
//
//	"UserInfo" → "userinfo"
func DirName(s string) string {
	return strings.ToLower(s)
}

// PkgName 将 PascalCase 转为全小写 Go 包名，与 DirName 等价。
func PkgName(s string) string {
	return strings.ToLower(s)
}

// EntPkg 返回 ent 生成的包名（全小写）。
func EntPkg(s string) string {
	return strings.ToLower(s)
}

// ProtoPkg 将任意服务名转为 proto3 package 标识符（全小写，无分隔符）。
//
//	"cs-agent-rpc" → "csagentrpc"
//	"cs_agent_rpc" → "csagentrpc"
//	"CsAgentRpc"   → "csagentrpc"
func ProtoPkg(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ──────────────────────────────────────────────
// Go 标识符
// ──────────────────────────────────────────────

// GoPascal 将 snake_case 转为 PascalCase，遵循 Go initialism 规范。
//
//	"api_code" → "APICode"，"owner_uid" → "OwnerUID"
func GoPascal(s string) string {
	return strcase.ToGoPascal(s)
}

// GoCamel 将 snake_case 转为 lowerCamelCase，遵循 Go initialism 规范。
//
//	"api_code" → "apiCode"，"owner_uid" → "ownerUID"
func GoCamel(s string) string {
	return strcase.ToGoCamel(s)
}

// LowerCamel 将 PascalCase 转为 lowerCamelCase（仅首字母小写，不做 initialism 处理）。
//
//	"UserInfo" → "userInfo"
func LowerCamel(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ServiceGoIdent 将任意服务名转为规范的 PascalCase Go 标识符（不展开 initialism）。
//
//	"cs-agent-rpc" → "CsAgentRpc"
//	"cs_agent_rpc" → "CsAgentRpc"
//	"CsAgentRpc"   → "CsAgentRpc"
func ServiceGoIdent(s string) string {
	if s == "" {
		return s
	}
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

// ──────────────────────────────────────────────
// 其他
// ──────────────────────────────────────────────

// DashName 将任意服务名转为 dash-case。
//
//	"cs-agent-rpc" → "cs-agent-rpc"
//	"cs_agent_rpc" → "cs-agent-rpc"
//	"CsAgentRpc"   → "cs-agent-rpc"
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
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// EnvVarName 将任意服务名转为 UPPER_SNAKE_CASE 环境变量名。
//
//	"cs-agent-rpc" → "CS_AGENT_RPC"
//	"CsAgentRpc"   → "CS_AGENT_RPC"
func EnvVarName(s string) string {
	return strings.ToUpper(strings.ReplaceAll(DashName(s), "-", "_"))
}

// ──────────────────────────────────────────────
// 分词引擎
// ──────────────────────────────────────────────

// SplitIdent 将 Go 标识符（PascalCase / camelCase / snake_case）分词为 token 列表。
//
// 核心规则：
//  1. 下划线作为分隔符，不进入 token
//  2. lower→Upper 边界切词（"getUser" → ["get","User"]）
//  3. 连续大写序列（缩写词）整体作为一个 token（"HTTP" → ["HTTP"]）
//  4. 缩写词复数（大写序列 + 小写's' + 词边界）整体保留（"CIDs" → ["CIDs"]）
//  5. 破折号不处理，原样保留在 token 中（"cs-agent-rpc" → ["cs-agent-rpc"]）
func SplitIdent(content string) []string {
	tokens, _ := splitIdent(content)
	return tokens
}

func splitIdent(content string) ([]string, error) {
	runes := []rune(content)
	var (
		list   []string
		buffer = bytes.NewBuffer(nil)
	)

	flush := func() {
		if buffer.Len() == 0 {
			return
		}
		list = append(list, buffer.String())
		buffer.Reset()
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '_' {
			flush()
			continue
		}

		if r >= 'A' && r <= 'Z' {
			if buffer.Len() > 0 {
				prevRune := runes[i-1]
				if prevRune >= 'a' && prevRune <= 'z' {
					flush()
				} else if prevRune >= 'A' && prevRune <= 'Z' {
					if i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z' {
						nextIsS := runes[i+1] == 's'
						afterS := i+2 >= len(runes) || runes[i+2] == '_' || (runes[i+2] >= 'A' && runes[i+2] <= 'Z')
						if nextIsS && afterS {
							// 缩写词复数，不切词
						} else {
							flush()
						}
					}
				}
			}
		}
		buffer.WriteRune(r)
	}
	flush()
	return list, nil
}
