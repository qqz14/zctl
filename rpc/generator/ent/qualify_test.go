package ent

import "testing"

func TestQualifyDaoTypes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Params with dao-local types
		{
			"ctx context.Context, filter *IamCIDListFilter, page *model.PageInfo",
			"ctx context.Context, filter *dao.IamCIDListFilter, page *model.PageInfo",
		},
		{
			"ctx context.Context, filter *IamAppListFilter, page *model.PageInfo",
			"ctx context.Context, filter *dao.IamAppListFilter, page *model.PageInfo",
		},
		// Params without dao-local types (should not change)
		{
			"ctx context.Context, data *ent.User",
			"ctx context.Context, data *ent.User",
		},
		{
			"ctx context.Context, id int",
			"ctx context.Context, id int",
		},
		// Return types
		{
			"(*ent.User, error)",
			"(*ent.User, error)",
		},
		{
			"([]*ent.User, int, error)",
			"([]*ent.User, int, error)",
		},
		// Edge case: dao interface return type
		{
			"IamCIDDao",
			"dao.IamCIDDao",
		},
		// Empty
		{"", ""},
	}
	for _, tt := range tests {
		got := qualifyDaoTypes(tt.input)
		if got != tt.want {
			t.Errorf("qualifyDaoTypes(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestQualifyTypeToken(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"*IamCIDListFilter", "*dao.IamCIDListFilter"},
		{"*ent.User", "*ent.User"},
		{"*model.PageInfo", "*model.PageInfo"},
		{"context.Context", "context.Context"},
		{"int", "int"},
		{"error", "error"},
		{"string", "string"},
		{"[]*ent.User", "[]*ent.User"},
		{"IamCIDDao", "dao.IamCIDDao"},
		{"*IamAppListFilter", "*dao.IamAppListFilter"},
	}
	for _, tt := range tests {
		got := qualifyTypeToken(tt.input)
		if got != tt.want {
			t.Errorf("qualifyTypeToken(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
