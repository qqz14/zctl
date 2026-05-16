package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var (
	VarStringProtoDocInput string
)

// ──── protoc-gen-doc JSON structures ──────────────────────────────────────

type protoDoc struct {
	Files []protoFile `json:"files"`
}

type protoFile struct {
	Name     string         `json:"name"`
	Services []protoService `json:"services"`
	Messages []protoMessage `json:"messages"`
	Enums    []protoEnum    `json:"enums"`
}

type protoEnum struct {
	Name        string           `json:"name"`
	FullName    string           `json:"fullName"`
	Description string           `json:"description"`
	Values      []protoEnumValue `json:"values"`
}

type protoEnumValue struct {
	Name        string `json:"name"`
	Number      string `json:"number"`
	Description string `json:"description"`
}

type protoService struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Methods     []protoMethod `json:"methods"`
}

type protoMethod struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	RequestType      string `json:"requestType"`
	RequestFullType  string `json:"requestFullType"`
	ResponseType     string `json:"responseType"`
	ResponseFullType string `json:"responseFullType"`
}

type protoMessage struct {
	Name        string       `json:"name"`
	FullName    string       `json:"fullName"`
	Description string       `json:"description"`
	Fields      []protoField `json:"fields"`
}

type protoField struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	FullType    string `json:"fullType"`
}

// docMethod groups a method with its module name (desc subdirectory)
type docMethod struct {
	Module string
	Index  int
	Method protoMethod
}

// ──── Entry point ──────────────────────────────────────────────────────────

func ProtoDoc(_ *cobra.Command, _ []string) error {
	input := VarStringProtoDocInput
	if input == "" {
		matches, _ := filepath.Glob("doc/*.json")
		if len(matches) == 0 {
			return fmt.Errorf("no JSON file found in doc/, please run: protoc --doc_out=./doc --doc_opt=json,xxx.json xxx.proto")
		}
		input = matches[0]
	}

	data, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read %s: %w", input, err)
	}

	var doc protoDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", input, err)
	}

	msgMap := make(map[string]*protoMessage)
	for i := range doc.Files {
		for j := range doc.Files[i].Messages {
			m := &doc.Files[i].Messages[j]
			msgMap[m.Name] = m
			msgMap[m.FullName] = m
		}
	}

	enumMap := make(map[string]*protoEnum)
	var allEnums []*protoEnum
	for i := range doc.Files {
		for j := range doc.Files[i].Enums {
			e := &doc.Files[i].Enums[j]
			enumMap[e.Name] = e
			enumMap[e.FullName] = e
			allEnums = append(allEnums, e)
		}
	}

	validateMap := buildValidateMap(input)

	// Parse module info — methods without "from desc/" inherit from previous method
	var methods []docMethod
	lastModule := "other"
	for _, file := range doc.Files {
		for _, svc := range file.Services {
			for _, m := range svc.Methods {
				module := extractModuleFromDesc(m.Description)
				if module != "other" {
					lastModule = module
				} else {
					module = lastModule
				}
				methods = append(methods, docMethod{Module: module, Method: m})
			}
		}
	}

	// Number methods sequentially
	for i := range methods {
		methods[i].Index = i + 1
	}

	var out strings.Builder
	out.WriteString("# API 接口文档\n\n")

	// ──── Table of Contents ────
	out.WriteString("## 📑 目录\n\n")

	// Group by module
	currentModule := ""
	for _, dm := range methods {
		if dm.Module != currentModule {
			currentModule = dm.Module
			out.WriteString(fmt.Sprintf("### %s\n\n", currentModule))
		}
		anchor := toAnchor(dm.Method.Name)
		out.WriteString(fmt.Sprintf("- [%d. %s](#%s)\n", dm.Index, dm.Method.Name, anchor))
	}
	out.WriteString("\n---\n\n")

	// ──── API Details ────
	for _, file := range doc.Files {
		for _, svc := range file.Services {
			desc := svc.Description
			if desc == "" {
				desc = svc.Name
			}
			out.WriteString(fmt.Sprintf("> 服务：**%s** · %s\n\n---\n\n", svc.Name, desc))
			break // only one service expected
		}
	}

	for _, dm := range methods {
		method := dm.Method
		methodDesc := cleanMethodDesc(method.Description)
		if methodDesc == "" {
			methodDesc = method.Name
		}
		out.WriteString(fmt.Sprintf("## %d. %s\n\n> %s\n\n", dm.Index, method.Name, methodDesc))

		// Request
		out.WriteString("#### 📥 请求参数")
		reqMsg := lookupMsg(msgMap, method.RequestType, method.RequestFullType)
		if reqMsg != nil && len(reqMsg.Fields) > 0 {
			out.WriteString(fmt.Sprintf("（`%s`）\n\n", reqMsg.Name))
			writeDocFieldTable(&out, reqMsg, msgMap, enumMap, validateMap, true)
			out.WriteString("\n**请求示例：**\n\n```json\n")
			writeDocJSON(&out, reqMsg, msgMap, enumMap, 0)
			out.WriteString("\n```\n\n")
		} else {
			out.WriteString("\n\n无参数\n\n")
		}

		// Response
		out.WriteString("#### 📤 响应参数")
		respMsg := lookupMsg(msgMap, method.ResponseType, method.ResponseFullType)
		isEmptyResp := respMsg == nil || len(respMsg.Fields) == 0 || method.ResponseType == "Empty"
		if !isEmptyResp {
			out.WriteString(fmt.Sprintf("（`%s`）\n\n", respMsg.Name))
			writeDocFieldTable(&out, respMsg, msgMap, enumMap, validateMap, false)
			out.WriteString("\n**响应示例：**\n\n```json\n{\n  \"code\": 0,\n  \"msg\": \"\",\n  \"data\": ")
			writeDocJSON(&out, respMsg, msgMap, enumMap, 1)
			out.WriteString("\n}\n```\n\n")
		} else {
			out.WriteString("\n\n空响应\n\n**响应示例：**\n\n```json\n{\n  \"code\": 0,\n  \"msg\": \"\"\n}\n```\n\n")
		}
		out.WriteString("---\n\n")
	}

	// Enum reference section
	if len(allEnums) > 0 {
		out.WriteString("## 📋 枚举定义\n\n")
		for _, e := range allEnums {
			eDesc := e.Description
			if eDesc == "" {
				eDesc = e.Name
			}
			out.WriteString(fmt.Sprintf("### %s\n\n> %s\n\n", e.Name, eDesc))
			out.WriteString("| 枚举值 | 数值 | 说明 |\n")
			out.WriteString("|--------|:----:|------|\n")
			for _, v := range e.Values {
				vDesc := v.Description
				if vDesc == "" {
					vDesc = "—"
				}
				out.WriteString(fmt.Sprintf("| %s | %s | %s |\n", v.Name, v.Number, vDesc))
			}
			out.WriteString("\n")
		}
		out.WriteString("---\n\n")
	}

	out.WriteString("*文档由 `make proto-doc` 自动生成*\n")

	outputPath := strings.TrimSuffix(input, filepath.Ext(input)) + "_api.md"
	if err := os.WriteFile(outputPath, []byte(out.String()), 0644); err != nil {
		return fmt.Errorf("write %s: %w", outputPath, err)
	}
	fmt.Printf("[zctl] Generated %s\n", outputPath)
	return nil
}

// ──── Field table ──────────────────────────────────────────────────────────

func writeDocFieldTable(out *strings.Builder, msg *protoMessage, msgMap map[string]*protoMessage, enumMap map[string]*protoEnum, validateMap map[string]bool, isRequest bool) {
	if isRequest {
		out.WriteString("| 参数名 | 类型 | 必填 | 说明 | 备注 |\n")
		out.WriteString("|--------|------|:----:|------|------|\n")
	} else {
		out.WriteString("| 参数名 | 类型 | 说明 | 备注 |\n")
		out.WriteString("|--------|------|------|------|\n")
	}
	for _, f := range msg.Fields {
		writeFieldRow(out, "", f, msg.Name, msgMap, enumMap, validateMap, isRequest)
	}
}

func writeFieldRow(out *strings.Builder, prefix string, f protoField, msgName string, msgMap map[string]*protoMessage, enumMap map[string]*protoEnum, validateMap map[string]bool, isRequest bool) {
	typ := docMapType(f, enumMap)
	desc := docCleanDesc(f.Description)
	if desc == "" {
		desc = "—"
	}
	remark := docRemark(f, enumMap)
	name := f.Name
	if prefix != "" {
		name = prefix + "." + f.Name
	}

	nested := lookupMsg(msgMap, f.Type, f.FullType)
	if nested != nil {
		// Parent row: bold, but don't bold empty remark
		if isRequest {
			required := docRequired(f, msgName, validateMap)
			out.WriteString(fmt.Sprintf("| **%s** | **%s** | **%s** | **%s** | %s |\n", name, typ, required, desc, remark))
		} else {
			out.WriteString(fmt.Sprintf("| **%s** | **%s** | **%s** | %s |\n", name, typ, desc, remark))
		}
		// Child fields
		for _, nf := range nested.Fields {
			writeFieldRow(out, name, nf, nested.Name, msgMap, enumMap, validateMap, isRequest)
		}
	} else {
		if isRequest {
			required := docRequired(f, msgName, validateMap)
			out.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n", name, typ, required, desc, remark))
		} else {
			out.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", name, typ, desc, remark))
		}
	}
}

// docRemark generates the remark column content.
// For enum fields: multi-line with <br/> separator.
func docRemark(f protoField, enumMap map[string]*protoEnum) string {
	e := lookupEnum(enumMap, f.Type, f.FullType)
	if e == nil {
		return ""
	}
	var parts []string
	for _, v := range e.Values {
		label := v.Description
		if label == "" {
			label = v.Name
		}
		parts = append(parts, fmt.Sprintf("%s=%s", v.Number, label))
	}
	return "枚举：<br/>" + strings.Join(parts, "<br/>")
}

// ──── JSON example ─────────────────────────────────────────────────────────

func writeDocJSON(out *strings.Builder, msg *protoMessage, msgMap map[string]*protoMessage, enumMap map[string]*protoEnum, indent int) {
	pad := strings.Repeat("  ", indent)
	out.WriteString(pad + "{\n")
	for i, f := range msg.Fields {
		fPad := strings.Repeat("  ", indent+1)
		val := docExampleValue(f, msgMap, enumMap, indent+1)
		comma := ","
		if i == len(msg.Fields)-1 {
			comma = ""
		}
		out.WriteString(fmt.Sprintf("%s\"%s\": %s%s\n", fPad, f.Name, val, comma))
	}
	out.WriteString(pad + "}")
}

func docExampleValue(f protoField, msgMap map[string]*protoMessage, enumMap map[string]*protoEnum, indent int) string {
	nested := lookupMsg(msgMap, f.Type, f.FullType)
	if f.Label == "repeated" {
		if nested != nil {
			var buf strings.Builder
			buf.WriteString("[\n")
			writeDocJSON(&buf, nested, msgMap, enumMap, indent+1)
			buf.WriteString("\n" + strings.Repeat("  ", indent) + "]")
			return buf.String()
		}
		return "[" + docScalarExample(f.Type, enumMap) + "]"
	}
	if nested != nil {
		var buf strings.Builder
		writeDocJSON(&buf, nested, msgMap, enumMap, indent)
		return buf.String()
	}
	return docScalarExample(f.Type, enumMap)
}

func docScalarExample(typ string, enumMap map[string]*protoEnum) string {
	switch typ {
	case "string":
		return `""`
	case "uint64", "int64", "uint32", "int32", "sint32", "sint64", "fixed32", "fixed64":
		return "0"
	case "float", "double":
		return "0.0"
	case "bool":
		return "false"
	default:
		if e := lookupEnum(enumMap, typ); e != nil && len(e.Values) > 0 {
			for _, v := range e.Values {
				if v.Number != "0" {
					return v.Number
				}
			}
			return "0"
		}
		return "{}"
	}
}

// ──── Helpers ──────────────────────────────────────────────────────────────

func docCleanDesc(desc string) string {
	// Strip "---- from desc/xxx/xxx.proto ----\n" prefix
	if idx := strings.Index(desc, "\n"); idx >= 0 && strings.Contains(desc[:idx], "---- from desc/") {
		desc = strings.TrimSpace(desc[idx+1:])
	}
	desc = strings.ReplaceAll(desc, "@required", "")
	desc = strings.ReplaceAll(desc, "@optional", "")
	return strings.TrimSpace(desc)
}

// extractModuleFromDesc extracts module name from method description.
// Format: "---- from desc/{module}/xxx.proto ----\n..."
func extractModuleFromDesc(desc string) string {
	re := regexp.MustCompile(`from desc/(\w+)/`)
	if m := re.FindStringSubmatch(desc); len(m) > 1 {
		return m[1]
	}
	return "other"
}

// cleanMethodDesc removes the "---- from desc/..." prefix line from method description.
func cleanMethodDesc(desc string) string {
	if idx := strings.Index(desc, "\n"); idx >= 0 && strings.Contains(desc[:idx], "---- from desc/") {
		return strings.TrimSpace(desc[idx+1:])
	}
	return strings.TrimSpace(desc)
}

// toAnchor converts a method name to a markdown anchor.
// GitHub-style: lowercase, spaces to hyphens.
func toAnchor(name string) string {
	return strings.ToLower(name)
}

func lookupMsg(msgMap map[string]*protoMessage, names ...string) *protoMessage {
	for _, n := range names {
		if m, ok := msgMap[n]; ok {
			return m
		}
	}
	return nil
}

func lookupEnum(enumMap map[string]*protoEnum, names ...string) *protoEnum {
	for _, n := range names {
		if e, ok := enumMap[n]; ok {
			return e
		}
	}
	return nil
}

// docMapType returns the display type. Enum → int32, not enum name.
func docMapType(f protoField, enumMap map[string]*protoEnum) string {
	base := f.Type
	switch base {
	case "string":
		base = "string"
	case "uint64", "int64":
		base = "int64"
	case "uint32", "int32":
		base = "int32"
	case "bool":
		base = "bool"
	case "float":
		base = "float"
	case "double":
		base = "double"
	case "bytes":
		base = "bytes"
	default:
		if lookupEnum(enumMap, f.Type, f.FullType) != nil {
			base = "int32"
		} else {
			base = "object"
		}
	}
	if f.Label == "repeated" {
		return "[]" + base
	}
	return base
}

func docRequired(f protoField, msgName string, validateMap map[string]bool) string {
	key := msgName + "." + f.Name
	if validateMap[key] {
		return "必填"
	}
	if f.Label == "optional" || f.Label == "repeated" {
		return "选填"
	}
	return "必填"
}

func buildValidateMap(jsonInput string) map[string]bool {
	result := make(map[string]bool)

	base := filepath.Base(jsonInput)
	protoName := strings.TrimSuffix(base, filepath.Ext(base)) + ".proto"

	data, err := os.ReadFile(protoName)
	if err != nil {
		return result
	}

	lines := strings.Split(string(data), "\n")

	var currentMsg string
	msgRe := regexp.MustCompile(`^\s*message\s+(\w+)\s*\{`)
	fieldValidateRe := regexp.MustCompile(`\s+(\w+)\s*=\s*\d+\s*\[.*buf\.validate`)
	braceDepth := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := msgRe.FindStringSubmatch(trimmed); m != nil {
			currentMsg = m[1]
			braceDepth = 1
			continue
		}

		if currentMsg != "" {
			braceDepth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
			if braceDepth <= 0 {
				currentMsg = ""
				continue
			}

			if m := fieldValidateRe.FindStringSubmatch(line); m != nil {
				result[currentMsg+"."+m[1]] = true
			}
		}
	}

	return result
}
