package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// ──── Entry point ──────────────────────────────────────────────────────────

// ProtoDoc generates a Markdown API doc from protoc-gen-doc JSON output.
func ProtoDoc(_ *cobra.Command, _ []string) error {
	input := VarStringProtoDocInput
	if input == "" {
		// Auto-detect: look for doc/*.json
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

	// Build message lookup
	msgMap := make(map[string]*protoMessage)
	for i := range doc.Files {
		for j := range doc.Files[i].Messages {
			m := &doc.Files[i].Messages[j]
			msgMap[m.Name] = m
			msgMap[m.FullName] = m
		}
	}

	var out strings.Builder
	out.WriteString("# API 接口文档\n\n")

	// Collect service info for header
	for _, file := range doc.Files {
		for _, svc := range file.Services {
			desc := svc.Description
			if desc == "" {
				desc = svc.Name
			}
			out.WriteString(fmt.Sprintf("> 服务：**%s** · %s\n\n---\n\n", svc.Name, desc))

			for i, method := range svc.Methods {
				methodDesc := method.Description
				if methodDesc == "" {
					methodDesc = method.Name
				}
				out.WriteString(fmt.Sprintf("## %d. %s\n\n> %s\n\n", i+1, method.Name, methodDesc))

				// Request
				out.WriteString("#### 📥 请求参数")
				reqMsg := lookupMsg(msgMap, method.RequestType, method.RequestFullType)
				if reqMsg != nil && len(reqMsg.Fields) > 0 {
					out.WriteString(fmt.Sprintf("（`%s`）\n\n", reqMsg.Name))
					writeDocFieldTable(&out, reqMsg, msgMap)
					out.WriteString("\n**请求示例：**\n\n```json\n")
					writeDocJSON(&out, reqMsg, msgMap, 0)
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
					writeDocFieldTable(&out, respMsg, msgMap)
					out.WriteString("\n**响应示例：**\n\n```json\n{\n  \"code\": 0,\n  \"msg\": \"\",\n  \"data\": ")
					writeDocJSON(&out, respMsg, msgMap, 1)
					out.WriteString("\n}\n```\n\n")
				} else {
					out.WriteString("\n\n空响应\n\n**响应示例：**\n\n```json\n{\n  \"code\": 0,\n  \"msg\": \"\"\n}\n```\n\n")
				}
				out.WriteString("---\n\n")
			}
		}
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

func writeDocFieldTable(out *strings.Builder, msg *protoMessage, msgMap map[string]*protoMessage) {
	out.WriteString("| 参数名 | 类型 | 说明 | 必填 |\n")
	out.WriteString("|--------|------|------|:----:|\n")
	for _, f := range msg.Fields {
		typ := docMapType(f)
		desc := f.Description
		if desc == "" {
			desc = "—"
		}
		required := docRequired(f)

		nested := lookupMsg(msgMap, f.Type, f.FullType)
		if nested != nil {
			// Parent field: bold
			out.WriteString(fmt.Sprintf("| **%s** | **%s** | **%s** | **%s** |\n", f.Name, typ, desc, required))
			// Child fields: parent.child
			for _, nf := range nested.Fields {
				nTyp := docMapType(nf)
				nDesc := nf.Description
				if nDesc == "" {
					nDesc = "—"
				}
				nReq := docRequired(nf)
				out.WriteString(fmt.Sprintf("| %s.%s | %s | %s | %s |\n", f.Name, nf.Name, nTyp, nDesc, nReq))
			}
		} else {
			out.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", f.Name, typ, desc, required))
		}
	}
}

// ──── JSON example ─────────────────────────────────────────────────────────

func writeDocJSON(out *strings.Builder, msg *protoMessage, msgMap map[string]*protoMessage, indent int) {
	pad := strings.Repeat("  ", indent)
	out.WriteString(pad + "{\n")
	for i, f := range msg.Fields {
		fPad := strings.Repeat("  ", indent+1)
		val := docExampleValue(f, msgMap, indent+1)
		comma := ","
		if i == len(msg.Fields)-1 {
			comma = ""
		}
		out.WriteString(fmt.Sprintf("%s\"%s\": %s%s\n", fPad, f.Name, val, comma))
	}
	out.WriteString(pad + "}")
}

func docExampleValue(f protoField, msgMap map[string]*protoMessage, indent int) string {
	nested := lookupMsg(msgMap, f.Type, f.FullType)
	if f.Label == "repeated" {
		if nested != nil {
			var buf strings.Builder
			buf.WriteString("[\n")
			writeDocJSON(&buf, nested, msgMap, indent+1)
			buf.WriteString("\n" + strings.Repeat("  ", indent) + "]")
			return buf.String()
		}
		return "[" + docScalarExample(f.Type) + "]"
	}
	if nested != nil {
		var buf strings.Builder
		writeDocJSON(&buf, nested, msgMap, indent)
		return buf.String()
	}
	return docScalarExample(f.Type)
}

func docScalarExample(typ string) string {
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
		return "{}"
	}
}

// ──── Helpers ──────────────────────────────────────────────────────────────

func lookupMsg(msgMap map[string]*protoMessage, names ...string) *protoMessage {
	for _, n := range names {
		if m, ok := msgMap[n]; ok {
			return m
		}
	}
	return nil
}

func docMapType(f protoField) string {
	base := f.Type
	switch base {
	case "string":
		base = "string"
	case "uint64", "int64":
		base = "integer"
	case "uint32", "int32":
		base = "integer"
	case "bool":
		base = "boolean"
	case "float", "double":
		base = "number"
	case "bytes":
		base = "string(bytes)"
	default:
		base = "object"
	}
	if f.Label == "repeated" {
		return "array[" + base + "]"
	}
	return base
}

func docRequired(f protoField) string {
	if f.Label == "optional" || f.Label == "repeated" {
		return "选填"
	}
	return "必填"
}
