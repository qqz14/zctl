package name

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ──────────────────────────────────────────────
// 旧接口（向后兼容）
// ──────────────────────────────────────────────

func TestIsNamingValid(t *testing.T) {
	style, valid := IsNamingValid("")
	assert.True(t, valid)
	assert.Equal(t, NamingLower, style)

	_, valid = IsNamingValid("lower1")
	assert.False(t, valid)

	_, valid = IsNamingValid("lower")
	assert.True(t, valid)

	_, valid = IsNamingValid("snake")
	assert.True(t, valid)

	_, valid = IsNamingValid("camel")
	assert.True(t, valid)
}

func TestFormatFilename(t *testing.T) {
	assert.Equal(t, "abc", FormatFilename("a_b_c", NamingLower))
	assert.Equal(t, "ABC", FormatFilename("a_b_c", NamingCamel))
	assert.Equal(t, "a_b_c", FormatFilename("a_b_c", NamingSnake))
	assert.Equal(t, "a", FormatFilename("a", NamingSnake))
	assert.Equal(t, "A", FormatFilename("a", NamingCamel))
	assert.Equal(t, "abc", FormatFilename("abc", NamingSnake))
}

// ──────────────────────────────────────────────
// FileSnake
// ──────────────────────────────────────────────

func TestFileSnake(t *testing.T) {
	cases := []struct {
		desc  string
		input string
		want  string
	}{
		// 普通 PascalCase
		{"普通 PascalCase", "UserInfo", "user_info"},
		{"普通 camelCase", "getUser", "get_user"},
		{"全小写", "getuser", "getuser"},
		{"已是 snake_case", "get_user_info", "get_user_info"},

		// 缩写词（三方库能正确处理）
		{"缩写词在中间", "GetHTTPStatus", "get_http_status"},
		{"缩写词在末尾", "GetCIDDetail", "get_cid_detail"},
		{"缩写词开头", "HTTPSHandler", "https_handler"},

		// 缩写词复数（三方库 bug，此包修复）
		{"★ CIDs 复数", "CsGetAccessibleCIDs", "cs_get_accessible_cids"},
		{"★ IDs 复数", "GetUserIDs", "get_user_ids"},
		{"★ URLs 复数", "ListURLs", "list_urls"},
		{"★ UUIDs 复数", "GetUUIDs", "get_uuids"},
		{"★ CIDs 在中间", "GetCIDsByUserID", "get_cids_by_user_id"},

		// 含破折号（原样保留）
		{"破折号输入原样", "cs-agent-rpc", "cs-agent-rpc"},
		{"下划线输入原样", "cs_agent_rpc", "cs_agent_rpc"},
		{"PascalCase 服务名", "CsAgentRpc", "cs_agent_rpc"},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			assert.Equal(t, c.want, FileSnake(c.input), "input=%q", c.input)
		})
	}
}

// ──────────────────────────────────────────────
// RpcLogicFileName / RpcServerFileName / RpcCallFileName
// ──────────────────────────────────────────────

func TestRpcLogicFileName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"CsAuthCallback", "cs_auth_callback_logic"},
		{"CreateCsUserProfile", "create_cs_user_profile_logic"},
		{"GetCIDDetail", "get_cid_detail_logic"},
		{"GetHTTPStatus", "get_http_status_logic"},
		{"CsGetAccessibleCIDs", "cs_get_accessible_cids_logic"},
		{"GetUserIDs", "get_user_ids_logic"},
		{"ListURLs", "list_urls_logic"},
		{"GetUUIDs", "get_uuids_logic"},
		{"GetCIDsByUserID", "get_cids_by_user_id_logic"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, RpcLogicFileName(c.input), "input=%q", c.input)
		})
	}
}

func TestRpcServerFileName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"CsAgentRpc", "cs_agent_rpc_server"},
		{"cs-agent-rpc", "cs-agent-rpc_server"},
		{"cs_agent_rpc", "cs_agent_rpc_server"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, RpcServerFileName(c.input), "input=%q", c.input)
		})
	}
}

func TestRpcCallFileName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"CsAgentRpc", "cs_agent_rpc"},
		{"cs-agent-rpc", "cs-agent-rpc"},
		{"cs_agent_rpc", "cs_agent_rpc"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, RpcCallFileName(c.input), "input=%q", c.input)
		})
	}
}

// ──────────────────────────────────────────────
// DirName / PkgName / EntPkg / ProtoPkg
// ──────────────────────────────────────────────

func TestDirName(t *testing.T) {
	cases := []struct{ input, want string }{
		{"UserInfo", "userinfo"},
		{"User", "user"},
		{"CsAgentRpc", "csagentrpc"},
		{"IamUserAppCID", "iamuserappcid"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, DirName(c.input))
			assert.Equal(t, c.want, PkgName(c.input))
			assert.Equal(t, c.want, EntPkg(c.input))
		})
	}
}

func TestProtoPkg(t *testing.T) {
	cases := []struct{ input, want string }{
		{"cs-agent-rpc", "csagentrpc"},
		{"cs_agent_rpc", "csagentrpc"},
		{"CsAgentRpc", "csagentrpc"},
		{"My-Svc_v2", "mysvcv2"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, ProtoPkg(c.input))
		})
	}
}

// ──────────────────────────────────────────────
// GoPascal / GoCamel / LowerCamel
// ──────────────────────────────────────────────

func TestGoPascal(t *testing.T) {
	cases := []struct{ input, want string }{
		{"api_code", "APICode"},
		{"owner_uid", "OwnerUID"},
		{"app_name", "AppName"},
		{"user_id", "UserID"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, GoPascal(c.input))
		})
	}
}

func TestGoCamel(t *testing.T) {
	cases := []struct{ input, want string }{
		{"api_code", "apiCode"},
		{"owner_uid", "ownerUID"},
		{"app_name", "appName"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, GoCamel(c.input))
		})
	}
}

func TestLowerCamel(t *testing.T) {
	cases := []struct{ input, want string }{
		{"UserInfo", "userInfo"},
		{"User", "user"},
		{"", ""},
		{"CsAgentRpc", "csAgentRpc"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, LowerCamel(c.input))
		})
	}
}

// ──────────────────────────────────────────────
// ServiceGoIdent
// ──────────────────────────────────────────────

func TestServiceGoIdent(t *testing.T) {
	cases := []struct{ input, want string }{
		{"cs-agent-rpc", "CsAgentRpc"},
		{"cs_agent_rpc", "CsAgentRpc"},
		{"CsAgentRpc", "CsAgentRpc"},
		{"my-svc_v2", "MySvcV2"},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, ServiceGoIdent(c.input))
		})
	}
}

// ──────────────────────────────────────────────
// DashName / EnvVarName
// ──────────────────────────────────────────────

func TestDashName(t *testing.T) {
	cases := []struct{ input, want string }{
		{"cs-agent-rpc", "cs-agent-rpc"},
		{"cs_agent_rpc", "cs-agent-rpc"},
		{"CsAgentRpc", "cs-agent-rpc"},
		{"csAgentRpc", "cs-agent-rpc"},
		{"My-Svc_v2", "my-svc-v2"},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, DashName(c.input))
		})
	}
}

func TestEnvVarName(t *testing.T) {
	cases := []struct{ input, want string }{
		{"cs-agent-rpc", "CS_AGENT_RPC"},
		{"cs_agent_rpc", "CS_AGENT_RPC"},
		{"CsAgentRpc", "CS_AGENT_RPC"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, EnvVarName(c.input))
		})
	}
}

// ──────────────────────────────────────────────
// splitIdent（内部分词引擎）
// ──────────────────────────────────────────────

func TestSplitIdent(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"GetUser", []string{"Get", "User"}},
		{"GetUserProfile", []string{"Get", "User", "Profile"}},
		{"getUser", []string{"get", "User"}},
		{"get_user_profile", []string{"get", "user", "profile"}},
		{"GetHTTPStatus", []string{"Get", "HTTP", "Status"}},
		{"GetCIDDetail", []string{"Get", "CID", "Detail"}},
		{"HTTPSHandler", []string{"HTTPS", "Handler"}},
		// 缩写词复数（核心修复）
		{"CIDs", []string{"CIDs"}},
		{"IDs", []string{"IDs"}},
		{"URLs", []string{"URLs"}},
		{"UUIDs", []string{"UUIDs"}},
		{"GetCIDs", []string{"Get", "CIDs"}},
		{"ListURLs", []string{"List", "URLs"}},
		{"GetUserIDs", []string{"Get", "User", "IDs"}},
		{"GetCIDsByUser", []string{"Get", "CIDs", "By", "User"}},
		{"CsGetAccessibleCIDs", []string{"Cs", "Get", "Accessible", "CIDs"}},
		// 含破折号（原样）
		{"cs-agent-rpc", []string{"cs-agent-rpc"}},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := splitIdent(c.input)
			assert.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}
