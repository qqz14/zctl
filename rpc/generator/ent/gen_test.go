package ent

import (
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
