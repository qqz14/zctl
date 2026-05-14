package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSQL_SimpleSelect(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE status = ? AND created_at > ?")
	require.NoError(t, err)
	assert.Equal(t, querySelect, p.Type)
	assert.Equal(t, "user", p.PrimaryTable)
	assert.Len(t, p.Conditions, 2)
	assert.Equal(t, "status", p.Conditions[0].Column)
	assert.Equal(t, opEQ, p.Conditions[0].Op)
	assert.Equal(t, "created_at", p.Conditions[1].Column)
	assert.Equal(t, opGT, p.Conditions[1].Op)
}

func TestParseSQL_SelectCount(t *testing.T) {
	p, err := parseSQL("SELECT COUNT(*) FROM user WHERE status = ?")
	require.NoError(t, err)
	assert.True(t, p.IsCount)
	assert.Equal(t, "user", p.PrimaryTable)
	assert.Len(t, p.Conditions, 1)
}

func TestParseSQL_SelectLimit1(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE email = ? LIMIT 1")
	require.NoError(t, err)
	assert.True(t, p.HasLimit)
	assert.Equal(t, "1", p.LimitN)
	assert.False(t, p.HasOffset)
}

func TestParseSQL_SelectPagination(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE status = ? ORDER BY id DESC LIMIT ? OFFSET ?")
	require.NoError(t, err)
	assert.True(t, p.HasLimit)
	assert.True(t, p.HasOffset)
	assert.Equal(t, "?", p.LimitN)
	assert.Equal(t, "?", p.OffsetN)
	assert.Equal(t, "id DESC", p.OrderBy)
}

func TestParseSQL_SelectIN(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE id IN (?, ?, ?)")
	require.NoError(t, err)
	assert.Len(t, p.Conditions, 1)
	assert.Equal(t, opIN, p.Conditions[0].Op)
	assert.Equal(t, "id", p.Conditions[0].Column)
}

func TestParseSQL_SelectLike(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE name LIKE ?")
	require.NoError(t, err)
	assert.Len(t, p.Conditions, 1)
	assert.Equal(t, opLike, p.Conditions[0].Op)
}

func TestParseSQL_SelectBetween(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE created_at BETWEEN ? AND ?")
	require.NoError(t, err)
	assert.Len(t, p.Conditions, 1)
	assert.Equal(t, opBetween, p.Conditions[0].Op)
}

func TestParseSQL_SelectIsNull(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE deleted_at IS NULL AND status = ?")
	require.NoError(t, err)
	assert.Len(t, p.Conditions, 2)
	assert.Equal(t, opIsNull, p.Conditions[0].Op)
	assert.Equal(t, opEQ, p.Conditions[1].Op)
}

func TestParseSQL_SelectIsNotNull(t *testing.T) {
	p, err := parseSQL("SELECT * FROM user WHERE email IS NOT NULL")
	require.NoError(t, err)
	assert.Len(t, p.Conditions, 1)
	assert.Equal(t, opIsNotNull, p.Conditions[0].Op)
}

func TestParseSQL_SelectGroupBy(t *testing.T) {
	p, err := parseSQL("SELECT status, COUNT(*) FROM user WHERE created_at > ? GROUP BY status HAVING COUNT(*) > 1")
	require.NoError(t, err)
	assert.True(t, p.IsCount)
	assert.Len(t, p.GroupBy, 1)
	assert.Equal(t, "status", p.GroupBy[0])
	assert.Contains(t, p.Having, "COUNT(*) > 1")
}

func TestParseSQL_SelectJoin(t *testing.T) {
	p, err := parseSQL("SELECT u.* FROM user u LEFT JOIN order o ON u.id = o.user_id WHERE u.status = ?")
	require.NoError(t, err)
	assert.Equal(t, "user", p.PrimaryTable)
	assert.Equal(t, "u", p.PrimaryAlias)
	assert.Len(t, p.Joins, 1)
	assert.Equal(t, "LEFT", p.Joins[0].JoinType)
	assert.Equal(t, "order", p.Joins[0].Table)
	assert.Equal(t, "o", p.Joins[0].Alias)
}

func TestParseSQL_SelectMultiJoin(t *testing.T) {
	p, err := parseSQL(`SELECT u.* FROM user u 
		INNER JOIN order o ON u.id = o.user_id 
		LEFT JOIN address a ON u.id = a.user_id 
		WHERE u.status = ? AND o.amount > ?`)
	require.NoError(t, err)
	assert.Len(t, p.Joins, 2)
	assert.Equal(t, "INNER", p.Joins[0].JoinType)
	assert.Equal(t, "LEFT", p.Joins[1].JoinType)
}

func TestParseSQL_Update(t *testing.T) {
	p, err := parseSQL("UPDATE user SET status = ?, updated_at = ? WHERE id = ?")
	require.NoError(t, err)
	assert.Equal(t, queryUpdate, p.Type)
	assert.Equal(t, "user", p.PrimaryTable)
	assert.Len(t, p.SetColumns, 2)
	assert.Equal(t, "status", p.SetColumns[0].Column)
	assert.Equal(t, "updated_at", p.SetColumns[1].Column)
	assert.Len(t, p.Conditions, 1)
	assert.Equal(t, "id", p.Conditions[0].Column)
}

func TestParseSQL_UpdateMultiWhere(t *testing.T) {
	p, err := parseSQL("UPDATE user SET name = ? WHERE status = ? AND role != ?")
	require.NoError(t, err)
	assert.Len(t, p.SetColumns, 1)
	assert.Len(t, p.Conditions, 2)
	assert.Equal(t, opNEQ, p.Conditions[1].Op)
}

func TestParseSQL_Delete(t *testing.T) {
	p, err := parseSQL("DELETE FROM user WHERE id = ?")
	require.NoError(t, err)
	assert.Equal(t, queryDelete, p.Type)
	assert.Equal(t, "user", p.PrimaryTable)
	assert.Len(t, p.Conditions, 1)
}

func TestParseSQL_Insert(t *testing.T) {
	p, err := parseSQL("INSERT INTO user (name, email, status) VALUES (?, ?, ?)")
	require.NoError(t, err)
	assert.Equal(t, queryInsert, p.Type)
	assert.Equal(t, "user", p.PrimaryTable)
	assert.Equal(t, []string{"name", "email", "status"}, p.InsertColumns)
}

func TestParseSQL_BacktickTable(t *testing.T) {
	p, err := parseSQL("SELECT * FROM `user_token` WHERE `user_id` = ?")
	require.NoError(t, err)
	assert.Equal(t, "user_token", p.PrimaryTable)
}

func TestParseSQL_Empty(t *testing.T) {
	_, err := parseSQL("")
	assert.Error(t, err)
}

func TestParseSQL_Unsupported(t *testing.T) {
	_, err := parseSQL("CREATE TABLE user (id INT)")
	assert.Error(t, err)
}

// ── Method name generation tests ──

func TestGenerateMethodName_SimpleFind(t *testing.T) {
	p := &parsedSQL{
		Type: querySelect,
		Conditions: []condition{
			{Column: "status", Op: opEQ},
			{Column: "created_at", Op: opGT},
		},
	}
	assert.Equal(t, "FindByStatusAndCreatedAtGt", generateMethodName(p))
}

func TestGenerateMethodName_Count(t *testing.T) {
	p := &parsedSQL{
		Type:    querySelect,
		IsCount: true,
		Conditions: []condition{
			{Column: "status", Op: opEQ},
		},
	}
	assert.Equal(t, "CountByStatus", generateMethodName(p))
}

func TestGenerateMethodName_GetByLimit1(t *testing.T) {
	p := &parsedSQL{
		Type:     querySelect,
		HasLimit: true,
		LimitN:   "1",
		Conditions: []condition{
			{Column: "email", Op: opEQ},
		},
	}
	assert.Equal(t, "GetByEmail", generateMethodName(p))
}

func TestGenerateMethodName_FindAll(t *testing.T) {
	p := &parsedSQL{
		Type: querySelect,
	}
	assert.Equal(t, "FindAll", generateMethodName(p))
}

func TestGenerateMethodName_GroupBy(t *testing.T) {
	p := &parsedSQL{
		Type:    querySelect,
		GroupBy: []string{"status"},
		Conditions: []condition{
			{Column: "created_at", Op: opGTE},
		},
	}
	assert.Equal(t, "GroupByCreatedAtGte", generateMethodName(p))
}

func TestGenerateMethodName_UpdateSet(t *testing.T) {
	p := &parsedSQL{
		Type:       queryUpdate,
		SetColumns: []updateSet{{Column: "status"}, {Column: "name"}},
		Conditions: []condition{
			{Column: "id", Op: opEQ},
		},
	}
	assert.Equal(t, "UpdateStatusAndNameById", generateMethodName(p))
}

func TestGenerateMethodName_DeleteBy(t *testing.T) {
	p := &parsedSQL{
		Type: queryDelete,
		Conditions: []condition{
			{Column: "user_id", Op: opEQ},
			{Column: "status", Op: opIN},
		},
	}
	assert.Equal(t, "DeleteByUserIdAndStatusIn", generateMethodName(p))
}

func TestGenerateMethodName_InsertTable(t *testing.T) {
	p := &parsedSQL{
		Type:         queryInsert,
		PrimaryTable: "user_token",
	}
	assert.Equal(t, "InsertUserToken", generateMethodName(p))
}

func TestGenerateMethodName_SelectWithIN(t *testing.T) {
	p := &parsedSQL{
		Type: querySelect,
		Conditions: []condition{
			{Column: "id", Op: opIN},
		},
	}
	assert.Equal(t, "FindByIdIn", generateMethodName(p))
}

func TestGenerateMethodName_SelectWithBetween(t *testing.T) {
	p := &parsedSQL{
		Type: querySelect,
		Conditions: []condition{
			{Column: "created_at", Op: opBetween},
		},
	}
	assert.Equal(t, "FindByCreatedAtBetween", generateMethodName(p))
}

// ── Return kind tests ──

func TestDetermineReturnKind(t *testing.T) {
	tests := []struct {
		name string
		p    *parsedSQL
		want returnKind
	}{
		{"select list", &parsedSQL{Type: querySelect}, returnList},
		{"select count", &parsedSQL{Type: querySelect, IsCount: true}, returnCount},
		{"select one", &parsedSQL{Type: querySelect, HasLimit: true, LimitN: "1"}, returnOne},
		{"select paginated", &parsedSQL{Type: querySelect, HasLimit: true, LimitN: "?", HasOffset: true}, returnList},
		{"insert", &parsedSQL{Type: queryInsert}, returnID},
		{"update", &parsedSQL{Type: queryUpdate}, returnAffected},
		{"delete", &parsedSQL{Type: queryDelete}, returnAffected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, determineReturnKind(tt.p))
		})
	}
}

// ── Ent predicate generation tests ──

func TestBuildEntPredicate(t *testing.T) {
	tests := []struct {
		cond condition
		pkg  string
		want string
	}{
		{condition{Column: "status", Op: opEQ}, "user", "user.StatusEQ(status)"},
		{condition{Column: "age", Op: opGTE}, "user", "user.AgeGTE(age)"},
		{condition{Column: "name", Op: opLike}, "user", "user.NameContains(name)"},
		{condition{Column: "id", Op: opIN}, "user", "user.IdIn(idList...)"},
		{condition{Column: "deleted_at", Op: opIsNull}, "user", "user.DeletedAtIsNil()"},
		{condition{Column: "email", Op: opIsNotNull}, "user", "user.EmailNotNil()"},
		{condition{Column: "created_at", Op: opBetween}, "user", "user.CreatedAtGTE(created_atMin), user.CreatedAtLTE(created_atMax)"},
	}

	for _, tt := range tests {
		t.Run(tt.cond.Column+"_"+tt.cond.Op.String(), func(t *testing.T) {
			assert.Equal(t, tt.want, buildEntPredicate(tt.cond, tt.pkg))
		})
	}
}

// ── Complex SQL parsing tests ──

func TestParseSQL_ComplexJoinGroupBy(t *testing.T) {
	sql := `SELECT u.status, COUNT(o.id) as order_count 
		FROM user u 
		INNER JOIN order o ON u.id = o.user_id 
		WHERE u.created_at >= ? AND o.amount > ? 
		GROUP BY u.status 
		HAVING COUNT(o.id) > 5 
		ORDER BY order_count DESC 
		LIMIT 10`
	p, err := parseSQL(sql)
	require.NoError(t, err)

	assert.Equal(t, "user", p.PrimaryTable)
	assert.Equal(t, "u", p.PrimaryAlias)
	assert.True(t, p.IsCount)
	assert.Len(t, p.Joins, 1)
	assert.Len(t, p.Conditions, 2)
	assert.Len(t, p.GroupBy, 1)
	assert.Contains(t, p.Having, "COUNT(o.id) > 5")
	assert.Contains(t, p.OrderBy, "order_count DESC")
	assert.True(t, p.HasLimit)
	assert.Equal(t, "10", p.LimitN)
}

func TestParseSQL_MultipleOperators(t *testing.T) {
	sql := `SELECT * FROM user WHERE status != ? AND age >= ? AND score < ? AND name LIKE ? AND deleted_at IS NULL`
	p, err := parseSQL(sql)
	require.NoError(t, err)

	assert.Len(t, p.Conditions, 5)
	assert.Equal(t, opIsNull, p.Conditions[0].Op) // IS NULL parsed first
	assert.Equal(t, opLike, p.Conditions[1].Op)    // LIKE parsed second
	assert.Equal(t, opNEQ, p.Conditions[2].Op)
	assert.Equal(t, opGTE, p.Conditions[3].Op)
	assert.Equal(t, opLT, p.Conditions[4].Op)
}
