package format

// compare_test.go — 验证 format.FileNamingFormat("go_zero", ...) 在 naming-spec.md 规则下的表现
//
// 覆盖：
//   A. logic 文件名：rpc.Name + "_logic" → go_zero snake（核心痛点）
//   B. etc yaml 文件名：项目目录名（用户原始输入）原样保留
//   C. server 文件名：服务名 + "_server"
//   D. 普通单词分词
//   E. 缩写词复数：CIDs / IDs / URLs / UUIDs
//
// 注：splitAcronymRun / split 的单元测试已迁移到 util/naming/naming_test.go

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// snake 是对 FileNamingFormat("go_zero", input) 的薄包装，用于测试。
func snake(input string) string {
	out, err := FileNamingFormat("go_zero", input)
	if err != nil {
		return "ERROR:" + err.Error()
	}
	return out
}

type snakeCase struct {
	desc  string
	input string
	want  string
}

// TestRpcLogicFileName 验证 rpc.Name+"_logic" → snake 文件名的所有规则。
// 这是 naming-spec.md 中最容易出 bug 的场景。
func TestRpcLogicFileName(t *testing.T) {
	cases := []snakeCase{
		// ── A. 普通 rpc 方法名 ────────────────────────────────────────────────
		{"A1 普通 rpc", "CsAuthCallback_logic", "cs_auth_callback_logic"},
		{"A2 普通 rpc", "CreateCsUserProfile_logic", "create_cs_user_profile_logic"},
		{"A3 缩写词 CID 在末尾", "GetCIDDetail_logic", "get_cid_detail_logic"},
		{"A4 缩写词 HTTP", "GetHTTPStatus_logic", "get_http_status_logic"},

		// ── E. 缩写词复数（核心 bug 场景）────────────────────────────────────
		{"E1 ★缩写复数 CIDs", "CsGetAccessibleCIDs_logic", "cs_get_accessible_cids_logic"},
		{"E2 缩写复数 IDs", "GetUserIDs_logic", "get_user_ids_logic"},
		{"E3 缩写复数 URLs", "ListURLs_logic", "list_urls_logic"},
		{"E4 缩写复数 UUIDs", "GetUUIDs_logic", "get_uuids_logic"},
		{"E5 缩写在中间 CIDs+ID", "GetCIDsByUserID_logic", "get_cids_by_user_id_logic"},

		// ── D. 普通单词（回归）────────────────────────────────────────────────
		{"D1 全小写", "getuser_logic", "getuser_logic"},
		{"D2 camelCase", "getUser_logic", "get_user_logic"},
		{"D3 PascalCase", "GetUserProfile_logic", "get_user_profile_logic"},
		{"D4 下划线分隔", "get_user_profile_logic", "get_user_profile_logic"},
		{"D5 ALLCAPS（非缩写场景）", "GOZERO_logic", "gozero_logic"},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := snake(c.input)
			assert.Equal(t, c.want, got, "input=%q", c.input)
		})
	}
}

// TestEtcYamlFileName 验证 etc yaml 文件名（项目目录名原样保留）。
func TestEtcYamlFileName(t *testing.T) {
	cases := []snakeCase{
		{"B1 dash input 原样保留", "cs-agent-rpc", "cs-agent-rpc"},
		{"B2 underscore input", "cs_agent_rpc", "cs_agent_rpc"},
		{"B3 PascalCase input → snake", "CsAgentRpc", "cs_agent_rpc"},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := snake(c.input)
			assert.Equal(t, c.want, got, "input=%q", c.input)
		})
	}
}

// TestServerFileName 验证 server 文件名（serviceName + "_server"）。
func TestServerFileName(t *testing.T) {
	cases := []snakeCase{
		{"C1 dash style 原样保留", "cs-agent-rpc_server", "cs-agent-rpc_server"},
		{"C2 underscore style", "cs_agent_rpc_server", "cs_agent_rpc_server"},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := snake(c.input)
			assert.Equal(t, c.want, got, "input=%q", c.input)
		})
	}
}