package format

import (
	"errors"
	"fmt"
	"strings"

	"github.com/qqz14/zctl/util/name"
)

const (
	flagGo   = "GO"
	flagZero = "ZERO"

	unknown style = iota
	title
	lower
	upper
)

// ErrNamingFormat defines an error for unknown format
var ErrNamingFormat = errors.New("unsupported format")

type (
	styleFormat struct {
		before    string
		through   string
		after     string
		goStyle   style
		zeroStyle style
	}

	style int
)

// FileNamingFormat is used to format the file name. You can define the format style
// through the go and zero formatting characters. For example, you can define the snake
// format as go_zero, and the camel case format as goZero. You can even specify the split
// character, such as go#Zero, theoretically any combination can be used, but the prerequisite
// must meet the naming conventions of each operating system file name.
// Note: Formatting is based on snake or camel string.
//
// 内部实现委托给 util/name.FileSnake（snake 场景）或逐 token 转换（其他场景）。
func FileNamingFormat(format, content string) (string, error) {
	upperFormat := strings.ToUpper(format)
	indexGo := strings.Index(upperFormat, flagGo)
	indexZero := strings.Index(upperFormat, flagZero)
	if indexGo < 0 || indexZero < 0 || indexGo > indexZero {
		return "", ErrNamingFormat
	}
	var (
		before, through, after string
		flagGoStr, flagZeroStr string
		goStyle, zeroStyle     style
		err                    error
	)
	before = format[:indexGo]
	flagGoStr = format[indexGo : indexGo+2]
	through = format[indexGo+2 : indexZero]
	flagZeroStr = format[indexZero : indexZero+4]
	after = format[indexZero+4:]

	goStyle, err = getStyle(flagGoStr)
	if err != nil {
		return "", err
	}
	zeroStyle, err = getStyle(flagZeroStr)
	if err != nil {
		return "", err
	}

	// 快速路径：go_zero（最常用的 snake_case 格式）直接委托给 naming.FileSnake，
	// 它正确处理缩写词复数（CIDs/IDs/URLs）等三方库无法处理的场景。
	if goStyle == lower && zeroStyle == lower && through == "_" && before == "" && after == "" {
		return name.FileSnake(content), nil
	}

	// 通用路径：其他格式（goZero、Go_Zero 等）走逐 token 转换。
	return doFormat(styleFormat{
		goStyle:   goStyle,
		zeroStyle: zeroStyle,
		before:    before,
		through:   through,
		after:     after,
	}, content)
}

func doFormat(f styleFormat, content string) (string, error) {
	// 使用 naming 包的分词引擎，保证与 FileSnake 行为一致。
	tokens := name.SplitIdent(content)
	var join []string
	for index, tok := range tokens {
		if index == 0 {
			join = append(join, transferTo(tok, f.goStyle))
			continue
		}
		join = append(join, transferTo(tok, f.zeroStyle))
	}
	joined := strings.Join(join, f.through)
	return f.before + joined + f.after, nil
}

func transferTo(in string, style style) string {
	switch style {
	case upper:
		return strings.ToUpper(in)
	case lower:
		return strings.ToLower(in)
	case title:
		return strings.Title(in) //nolint:staticcheck
	default:
		return in
	}
}

func getStyle(flag string) (style, error) {
	compare := strings.ToLower(flag)
	switch flag {
	case strings.ToLower(compare):
		return lower, nil
	case strings.ToUpper(compare):
		return upper, nil
	case strings.Title(compare): //nolint:staticcheck
		return title, nil
	default:
		return unknown, fmt.Errorf("unexpected format: %s", flag)
	}
}