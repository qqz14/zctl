package ent

import (
	"encoding/json"
	"os"
	"testing"

	"entgo.io/ent/entc/load"
	"entgo.io/ent/schema/field"
)

// TestCollectUniqueFields verifies that unique fields are correctly identified.
func TestCollectUniqueFields(t *testing.T) {
	schema := &load.Schema{
		Name: "UserInfo",
		Fields: []*load.Field{
			{Name: "username", Unique: true, Info: &field.TypeInfo{Type: field.TypeString}},
			{Name: "email", Unique: true, Info: &field.TypeInfo{Type: field.TypeString}},
			{Name: "password_hash", Unique: false, Info: &field.TypeInfo{Type: field.TypeString}},
			{Name: "status", Unique: false, Info: &field.TypeInfo{Type: field.TypeInt8}},
		},
	}

	result := collectUniqueFields(schema)
	if len(result) != 2 {
		t.Fatalf("expected 2 unique fields, got %d", len(result))
	}
	if result[0].Name != "username" {
		t.Errorf("expected first unique field 'username', got %q", result[0].Name)
	}
	if result[1].Name != "email" {
		t.Errorf("expected second unique field 'email', got %q", result[1].Name)
	}
}

// TestHasDeletedAtField verifies soft delete detection.
func TestHasDeletedAtField(t *testing.T) {
	schemaWithDelete := &load.Schema{
		Name: "Order",
		Fields: []*load.Field{
			{Name: "status", Info: &field.TypeInfo{Type: field.TypeInt8}},
			{Name: "deleted_at", Info: &field.TypeInfo{Type: field.TypeTime}},
		},
	}
	schemaWithoutDelete := &load.Schema{
		Name: "UserInfo",
		Fields: []*load.Field{
			{Name: "status", Info: &field.TypeInfo{Type: field.TypeInt8}},
		},
	}

	if !hasDeletedAtField(schemaWithDelete) {
		t.Error("expected hasDeletedAtField=true for schema with deleted_at")
	}
	if hasDeletedAtField(schemaWithoutDelete) {
		t.Error("expected hasDeletedAtField=false for schema without deleted_at")
	}
}

// TestMapEntFieldTypeToGo verifies Go type mapping.
func TestMapEntFieldTypeToGo(t *testing.T) {
	cases := []struct {
		input uniqueFieldInfo
		want  string
	}{
		{uniqueFieldInfo{Name: "name", TypeName: "string"}, "string"},
		{uniqueFieldInfo{Name: "age", TypeName: "int"}, "int"},
		{uniqueFieldInfo{Name: "status", TypeName: "int8"}, "int8"},
		{uniqueFieldInfo{Name: "score", TypeName: "float64"}, "float64"},
		{uniqueFieldInfo{Name: "ts", TypeName: "time.Time"}, "time.Time"},
	}
	for _, c := range cases {
		got := mapEntFieldTypeToGo(c.input)
		if got != c.want {
			t.Errorf("mapEntFieldTypeToGo(%v) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestCollectIndexedFields verifies that indexed fields include unique + status.
func TestCollectIndexedFields(t *testing.T) {
	schema := &load.Schema{
		Name: "UserInfo",
		Fields: []*load.Field{
			{Name: "username", Unique: true, Info: &field.TypeInfo{Type: field.TypeString}},
			{Name: "email", Unique: true, Info: &field.TypeInfo{Type: field.TypeString}},
			{Name: "password_hash", Unique: false, Info: &field.TypeInfo{Type: field.TypeString}},
			{Name: "status", Unique: false, Info: &field.TypeInfo{Type: field.TypeInt8}},
		},
	}

	result := collectIndexedFields(schema)
	// Should include: username, email (unique) + status (special)
	if len(result) != 3 {
		t.Fatalf("expected 3 indexed fields, got %d: %+v", len(result), result)
	}

	names := make(map[string]bool)
	for _, f := range result {
		names[f.Name] = true
	}
	for _, want := range []string{"username", "email", "status"} {
		if !names[want] {
			t.Errorf("expected indexed field %q not found", want)
		}
	}
}

// TestMergeI18nCodes_OnlyWritesErrcode verifies that only errcode section is written to locale JSON.
func TestMergeI18nCodes_OnlyWritesErrcode(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		codes    []int
		wantBiz  bool
		wantErr  bool
	}{
		{
			name: "bizcode section is removed",
			input: `{
  "errcode": {
    "95000": "Internal error"
  },
  "bizcode": {
    "11101": "User not found"
  }
}`,
			codes:   []int{},
			wantBiz: false,
			wantErr: false,
		},
		{
			name: "errcode preserved when bizcode removed",
			input: `{
  "errcode": {
    "95000": "Internal error"
  },
  "bizcode": {
    "11101": "User not found"
  }
}`,
			codes:   []int{11201},
			wantBiz: false,
			wantErr: false,
		},
		{
			name: "only errcode is written, other sections ignored",
			input: `{
  "errcode": {
    "95000": "Internal error"
  },
  "bizcode": {
    "11101": "User not found"
  },
  "ui": {
    "welcome": "Welcome"
  }
}`,
			codes:   []int{},
			wantBiz: false,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile, err := os.CreateTemp("", "locale*.json")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(tmpFile.Name())

			if _, err := tmpFile.WriteString(tt.input); err != nil {
				t.Fatal(err)
			}
			tmpFile.Close()

			err = mergeI18nCodes(tmpFile.Name(), tt.codes)
			if (err != nil) != tt.wantErr {
				t.Errorf("mergeI18nCodes() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			data, err := os.ReadFile(tmpFile.Name())
			if err != nil {
				t.Fatal(err)
			}

			var root map[string]map[string]string
			if err := json.Unmarshal(data, &root); err != nil {
				t.Fatalf("failed to unmarshal output: %v", err)
			}

			if _, hasBiz := root["bizcode"]; hasBiz != tt.wantBiz {
				t.Errorf("mergeI18nCodes() bizcode present = %v, want %v", hasBiz, tt.wantBiz)
			}

			if _, hasErr := root["errcode"]; !hasErr {
				t.Error("mergeI18nCodes() errcode section should be preserved")
			}

			if len(root) != 1 {
				t.Errorf("mergeI18nCodes() expected only errcode section, got %d sections", len(root))
			}
		})
	}
}
