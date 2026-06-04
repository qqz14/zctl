package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/name"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/qqz14/zctl/util/stringx"
	"github.com/zeromicro/go-zero/core/collection"
)

// GenLogicFiles is the SINGLE entry point for generating logic files.
// It parses the root proto, resolves desc/ group/model mapping,
// and generates logic/{group}/{model}/*_logic.go with correct pb signatures.
//
// Called by:
//   - gen-rpc (Generate → GenLogic → here)
//   - gen-rpc-ent-logic (after generating desc proto + merge + protoc)
//   - any future command that needs logic generation
//
// Params:
//   - abs: project root absolute path
//   - style: file naming style (e.g. "go_zero")
//   - overwrite: whether to overwrite existing files
func GenLogicFiles(abs, style string, overwrite bool) error {
	// 1. Find root proto file
	rootProto, err := findRootProto(abs)
	if err != nil {
		return fmt.Errorf("GenLogicFiles: %w", err)
	}

	// 2. Parse proto
	p := parser.NewDefaultProtoParser()
	proto, err := p.Parse(rootProto, false)
	if err != nil {
		return fmt.Errorf("GenLogicFiles: parse proto: %w", err)
	}

	// 3. Resolve imported protos for type references
	srcDir := filepath.Dir(rootProto)
	proto.ImportedProtos, _ = parser.ParseImportedProtos(rootProto, []string{srcDir, abs})

	// 4. Build group/model mapping from desc/
	descDir := filepath.Join(abs, "desc")
	rpcGroupMap := BuildRpcGroupMap(descDir)

	// 5. Resolve project context for import paths
	logicBaseDir := filepath.Join(abs, "internal", "logic")
	svcPkg := detectModulePath(abs) + "/internal/svc"
	pbPkg := detectPbImportPath(abs)
	pbAlias := filepath.Base(pbPkg)

	service := proto.Service[0]
	pkgMap := parser.BuildProtoPackageMap(proto.ImportedProtos)

	for _, rpc := range service.RPC {
		logicName := fmt.Sprintf("%sLogic", stringx.From(rpc.Name).ToCamel())

		logicFilename := name.RpcLogicFileName(rpc.Name)

		// Determine path: logic/{group}/{model}/
		gm := rpcGroupMap[rpc.Name]
		if gm.Group == "" {
			gm = rpcGroupMap[strings.ToLower(rpc.Name)]
		}

		var filename string
		var packageName string
		if gm.Group != "" {
			modelDir := filepath.Join(logicBaseDir, gm.Group, gm.Model)
			pathx.MkdirIfNotExist(modelDir)
			filename = filepath.Join(modelDir, logicFilename+".go")
			packageName = gm.Model
		} else {
			filename = filepath.Join(logicBaseDir, logicFilename+".go")
			packageName = "logic"
		}

		// Skip if exists and not overwrite
		if pathx.FileExists(filename) && !overwrite {
			continue
		}

		// Build function body with correct pb types
		functions, err := buildLogicFunction(service.Service.Name, proto.PbPackage, proto.GoPackage, logicName, rpc, pkgMap)
		if err != nil {
			return err
		}

		// Build imports
		imports := collection.NewSet[string]()
		imports.Add(fmt.Sprintf(`"%s"`, svcPkg))
		addLogicImportsForFile(imports, pbPkg, pbAlias, proto.PbPackage, proto.GoPackage, rpc, pkgMap)

		// Render using logic.tpl
		text, err := pathx.LoadTemplate(category, logicTemplateFileFile, logicTemplate)
		if err != nil {
			return err
		}
		err = util.With("logic").GoFmt(true).Parse(text).SaveTo(map[string]any{
			"logicName":   logicName,
			"functions":   functions,
			"packageName": packageName,
			"imports":     strings.Join(imports.Keys(), pathx.NL),
		}, filename, overwrite)
		if err != nil {
			return err
		}
	}
	return nil
}

// buildLogicFunction renders the function body for a single rpc method.
// Reuses the same template as genLogicFunction but as a standalone function.
func buildLogicFunction(serviceName, goPackage, mainGoPackage, logicName string,
	rpc *parser.RPC, pkgMap map[string]parser.ImportedProto) (string, error) {
	text, err := pathx.LoadTemplate(category, logicFuncTemplateFileFile, logicFunctionTemplate)
	if err != nil {
		return "", err
	}

	comment := parser.GetComment(rpc.Doc())
	streamServer := fmt.Sprintf("%s.%s_%s%s", goPackage, parser.CamelCase(serviceName),
		parser.CamelCase(rpc.Name), "Server")

	reqRef := resolveRPCTypeRef(rpc.RequestType, goPackage, mainGoPackage, pkgMap)
	respRef := resolveRPCTypeRef(rpc.ReturnsType, goPackage, mainGoPackage, pkgMap)

	buffer, err := util.With("fun").Parse(text).Execute(map[string]any{
		"logicName":    logicName,
		"method":       parser.CamelCase(rpc.Name),
		"hasReq":       !rpc.StreamsRequest,
		"request":      "*" + reqRef.GoRef,
		"hasReply":     !rpc.StreamsRequest && !rpc.StreamsReturns,
		"response":     "*" + respRef.GoRef,
		"responseType": respRef.GoRef,
		"stream":       rpc.StreamsRequest || rpc.StreamsReturns,
		"streamBody":   streamServer,
		"hasComment":   len(comment) > 0,
		"comment":      comment,
	})
	if err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// addLogicImportsForFile adds pb imports for a logic file.
func addLogicImportsForFile(imports *collection.Set[string], pbPkg, pbAlias, goPackage, mainGoPackage string,
	rpc *parser.RPC, pkgMap map[string]parser.ImportedProto) {
	if rpc.StreamsRequest || rpc.StreamsReturns {
		imports.Add(fmt.Sprintf(`"%s"`, pbPkg))
		return
	}
	reqRef := resolveRPCTypeRef(rpc.RequestType, goPackage, mainGoPackage, pkgMap)
	respRef := resolveRPCTypeRef(rpc.ReturnsType, goPackage, mainGoPackage, pkgMap)
	if reqRef.ImportPath == "" || respRef.ImportPath == "" {
		imports.Add(fmt.Sprintf(`"%s"`, pbPkg))
	}
	if reqRef.ImportPath != "" {
		imports.Add(fmt.Sprintf(`"%s"`, reqRef.ImportPath))
	}
	if respRef.ImportPath != "" {
		imports.Add(fmt.Sprintf(`"%s"`, respRef.ImportPath))
	}
}

// ──── helpers ────

// findRootProto finds the root .proto file in the project directory.
func findRootProto(abs string) (string, error) {
	// Try SERVICE_STYLE from Makefile
	serviceName := filepath.Base(abs)
	if data, err := os.ReadFile(filepath.Join(abs, "Makefile")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "SERVICE_STYLE=") {
				if v := strings.TrimSpace(strings.TrimPrefix(line, "SERVICE_STYLE=")); v != "" {
					serviceName = v
					break
				}
			}
		}
	}
	protoPath := filepath.Join(abs, serviceName+".proto")
	if pathx.FileExists(protoPath) {
		return protoPath, nil
	}
	return "", fmt.Errorf("root proto not found: %s", protoPath)
}

// detectModulePath reads go.mod to find the module path.
func detectModulePath(abs string) string {
	data, err := os.ReadFile(filepath.Join(abs, "go.mod"))
	if err != nil {
		return filepath.Base(abs)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return filepath.Base(abs)
}

// detectPbImportPath finds the pb package import path by scanning types/ directory.
// It skips directories that contain '.' (e.g. "buf.build") since those are
// external module artifacts, not the project's own generated pb package.
func detectPbImportPath(abs string) string {
	modulePath := detectModulePath(abs)
	typesDir := filepath.Join(abs, "types")
	entries, _ := os.ReadDir(typesDir)
	for _, e := range entries {
		if e.IsDir() && !strings.Contains(e.Name(), ".") {
			return modulePath + "/types/" + e.Name()
		}
	}
	return modulePath + "/types/" + strings.ToLower(filepath.Base(abs))
}
