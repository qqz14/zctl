package generator

import "testing"

func TestToSnake(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"OrderStatus", "order_status"},
		{"CIDAppStatus", "cid_app_status"},
		{"UserStatus", "user_status"},
		{"MemberStatus", "member_status"},
		{"HTTPSProxy", "https_proxy"},
		{"GetCIDList", "get_cid_list"},
		{"AppStatus", "app_status"},
		{"ID", "id"},
		{"CID", "cid"},
		{"APIKey", "api_key"},
		{"HTMLParser", "html_parser"},
		{"SimpleCase", "simple_case"},
		{"A", "a"},
		{"AB", "ab"},
		{"ABCDef", "abc_def"},
	}
	for _, tt := range tests {
		got := toSnake(tt.input)
		if got != tt.want {
			t.Errorf("toSnake(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestToScreamingSnake(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"OrderStatus", "ORDER_STATUS"},
		{"CIDAppStatus", "CID_APP_STATUS"},
		{"UserStatus", "USER_STATUS"},
	}
	for _, tt := range tests {
		got := toScreamingSnake(tt.input)
		if got != tt.want {
			t.Errorf("toScreamingSnake(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
