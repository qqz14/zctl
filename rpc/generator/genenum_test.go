package generator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

func TestParseProtoEnumsIgnoresOptionBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.proto")
	content := `syntax = "proto3";

enum Status {
  option (grpc.gateway.protoc_gen_openapiv2.options.openapiv2_enum) = {
    description: "Status description."
  };
  STATUS_UNSPECIFIED = 0;
  STATUS_OPEN = 1;
}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := parseProtoEnums(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []protoEnum{{
		Name: "Status",
		Values: []protoEnumValue{
			{Name: "unspecified", Number: 0},
			{Name: "open", Number: 1},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProtoEnums() = %#v, want %#v", got, want)
	}
}

func TestGenEnumsFromProtoPreservesNumericValues(t *testing.T) {
	projectDir := t.TempDir()
	descDir := filepath.Join(projectDir, "desc")
	if err := os.MkdirAll(descDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `syntax = "proto3";

enum MemberStatus {
  MEMBER_STATUS_UNSPECIFIED = 0;
  MEMBER_STATUS_ACTIVE = 1;
  MEMBER_STATUS_LEFT = 10;
}
`
	if err := os.WriteFile(filepath.Join(descDir, "common.proto"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := GenEnumsFromProto(projectDir); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(filepath.Join(projectDir, "pkg", "enums", "member_status.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "MemberStatusLeft MemberStatus = 10") {
		t.Fatalf("generated enum did not preserve numeric value 10:\n%s", generated)
	}
}
