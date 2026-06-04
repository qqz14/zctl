package format

import (
	"testing"

	"github.com/qqz14/zctl/util/name"
	"github.com/stretchr/testify/assert"
)

// TestSplit 验证分词引擎（委托给 name.SplitIdent）。
func TestSplit(t *testing.T) {
	assert.Equal(t, []string{"A"}, name.SplitIdent("A"))
	assert.Equal(t, []string{"go", "Zero"}, name.SplitIdent("goZero"))
	assert.Equal(t, []string{"Gozero"}, name.SplitIdent("Gozero"))
	assert.Equal(t, []string{"go", "zero"}, name.SplitIdent("go_zero"))
	assert.Equal(t, []string{"tal", "Go", "zero"}, name.SplitIdent("talGo_zero"))
	assert.Equal(t, []string{"GOZERO"}, name.SplitIdent("GOZERO"))
	assert.Equal(t, []string{"gozero"}, name.SplitIdent("gozero"))
	assert.Equal(t, 0, len(name.SplitIdent("")))
	assert.Equal(t, []string{"a", "b", "CD", "EF"}, name.SplitIdent("a_b_CD_EF"))
	assert.Equal(t, 0, len(name.SplitIdent("_")))
	assert.Equal(t, 0, len(name.SplitIdent("__")))
	assert.Equal(t, []string{"A"}, name.SplitIdent("_A"))
	assert.Equal(t, []string{"A"}, name.SplitIdent("_A_"))
	assert.Equal(t, []string{"A"}, name.SplitIdent("A_"))
	assert.Equal(t, []string{"welcome", "to", "go", "zero"}, name.SplitIdent("welcome_to_go_zero"))
	// 缩写词
	assert.Equal(t, []string{"Get", "CID", "Detail"}, name.SplitIdent("GetCIDDetail"))
	assert.Equal(t, []string{"CID", "App", "Status"}, name.SplitIdent("CIDAppStatus"))
	assert.Equal(t, []string{"HTTPS", "Proxy"}, name.SplitIdent("HTTPSProxy"))
	assert.Equal(t, []string{"Get", "CID", "Detail", "logic"}, name.SplitIdent("GetCIDDetail_logic"))
}

func TestFileNamingFormat(t *testing.T) {
	testFileNamingFormat(t, "gozero", "welcome_to_go_zero", "welcometogozero")
	testFileNamingFormat(t, "_go#zero_", "welcome_to_go_zero", "_welcome#to#go#zero_")
	testFileNamingFormat(t, "Go#zero", "welcome_to_go_zero", "Welcome#to#go#zero")
	testFileNamingFormat(t, "Go#Zero", "welcome_to_go_zero", "Welcome#To#Go#Zero")
	testFileNamingFormat(t, "Go_Zero", "welcome_to_go_zero", "Welcome_To_Go_Zero")
	testFileNamingFormat(t, "go_Zero", "welcome_to_go_zero", "welcome_To_Go_Zero")
	testFileNamingFormat(t, "goZero", "welcome_to_go_zero", "welcomeToGoZero")
	testFileNamingFormat(t, "GoZero", "welcome_to_go_zero", "WelcomeToGoZero")
	testFileNamingFormat(t, "GOZero", "welcome_to_go_zero", "WELCOMEToGoZero")
	testFileNamingFormat(t, "GoZERO", "welcome_to_go_zero", "WelcomeTOGOZERO")
	testFileNamingFormat(t, "GOZERO", "welcome_to_go_zero", "WELCOMETOGOZERO")
	testFileNamingFormat(t, "GO*ZERO", "welcome_to_go_zero", "WELCOME*TO*GO*ZERO")
	testFileNamingFormat(t, "[GO#ZERO]", "welcome_to_go_zero", "[WELCOME#TO#GO#ZERO]")
	testFileNamingFormat(t, "{go###zero}", "welcome_to_go_zero", "{welcome###to###go###zero}")
	testFileNamingFormat(t, "{go###zerogo_zero}", "welcome_to_go_zero", "{welcome###to###go###zerogo_zero}")
	testFileNamingFormat(t, "GogoZerozero", "welcome_to_go_zero", "WelcomegoTogoGogoZerozero")
	testFileNamingFormat(t, "前缀GoZero后缀", "welcome_to_go_zero", "前缀WelcomeToGoZero后缀")
	testFileNamingFormat(t, "GoZero", "welcometogozero", "Welcometogozero")
	testFileNamingFormat(t, "GoZero", "WelcomeToGoZero", "WelcomeToGoZero")
	testFileNamingFormat(t, "gozero", "WelcomeToGoZero", "welcometogozero")
	testFileNamingFormat(t, "go_zero", "WelcomeToGoZero", "welcome_to_go_zero")
	testFileNamingFormat(t, "Go_Zero", "WelcomeToGoZero", "Welcome_To_Go_Zero")
	testFileNamingFormat(t, "Go_Zero", "", "")
	testFileNamingFormatErr(t, "go", "")
	testFileNamingFormatErr(t, "gOZero", "")
	testFileNamingFormatErr(t, "zero", "")
	testFileNamingFormatErr(t, "goZEro", "welcome_to_go_zero")
	testFileNamingFormatErr(t, "goZERo", "welcome_to_go_zero")
	testFileNamingFormatErr(t, "zerogo", "welcome_to_go_zero")

	// Acronym-aware FileNamingFormat tests
	testFileNamingFormat(t, "go_zero", "GetCIDDetail_logic", "get_cid_detail_logic")
	testFileNamingFormat(t, "go_zero", "CIDAppStatus", "cid_app_status")
	testFileNamingFormat(t, "gozero", "GetCIDDetail_logic", "getciddetaillogic")
	testFileNamingFormat(t, "goZero", "GetCIDDetail_logic", "getCIDDetailLogic")
}

func testFileNamingFormat(t *testing.T, format, in, expected string) {
	format, err := FileNamingFormat(format, in)
	assert.Nil(t, err)
	assert.Equal(t, expected, format)
}

func testFileNamingFormatErr(t *testing.T, format, in string) {
	_, err := FileNamingFormat(format, in)
	assert.Error(t, err)
}
