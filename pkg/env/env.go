package env

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qqz14/zctl/internal/version"
	sortedmap "github.com/qqz14/zctl/pkg/collection"
	"github.com/qqz14/zctl/pkg/protoc"
	"github.com/qqz14/zctl/pkg/protocgengo"
	"github.com/qqz14/zctl/pkg/protocgengogrpc"
	"github.com/qqz14/zctl/util/pathx"
)

var zctlEnv *sortedmap.SortedMap

const (
	GoctlOS                = "GOCTL_OS"
	GoctlArch              = "GOCTL_ARCH"
	GoctlHome              = "GOCTL_HOME"
	GoctlDebug             = "GOCTL_DEBUG"
	GoctlCache             = "GOCTL_CACHE"
	GoctlVersion           = "GOCTL_VERSION"
	GoctlExperimental      = "GOCTL_EXPERIMENTAL"
	ProtocVersion          = "PROTOC_VERSION"
	ProtocGenGoVersion     = "PROTOC_GEN_GO_VERSION"
	ProtocGenGoGRPCVersion = "PROTO_GEN_GO_GRPC_VERSION"

	envFileDir      = "env"
	ExperimentalOn  = "on"
	ExperimentalOff = "off"
)

// init initializes the zctl environment variables, the environment variables of the function are set in order,
// please do not change the logic order of the code.
func init() {
	defaultGoctlHome, err := pathx.GetDefaultGoctlHome()
	if err != nil {
		log.Fatalln(err)
	}
	zctlEnv = sortedmap.New()
	zctlEnv.SetKV(GoctlOS, runtime.GOOS)
	zctlEnv.SetKV(GoctlArch, runtime.GOARCH)
	existsEnv := readEnv(defaultGoctlHome)
	if existsEnv != nil {
		zctlHome, ok := existsEnv.GetString(GoctlHome)
		if ok && len(zctlHome) > 0 {
			zctlEnv.SetKV(GoctlHome, zctlHome)
		}
		if debug := existsEnv.GetOr(GoctlDebug, "").(string); debug != "" {
			if strings.EqualFold(debug, "true") || strings.EqualFold(debug, "false") {
				zctlEnv.SetKV(GoctlDebug, debug)
			}
		}
		if value := existsEnv.GetStringOr(GoctlCache, ""); value != "" {
			zctlEnv.SetKV(GoctlCache, value)
		}
		experimental := existsEnv.GetOr(GoctlExperimental, ExperimentalOn)
		zctlEnv.SetKV(GoctlExperimental, experimental)
	}

	if !zctlEnv.HasKey(GoctlHome) {
		zctlEnv.SetKV(GoctlHome, defaultGoctlHome)
	}
	if !zctlEnv.HasKey(GoctlDebug) {
		zctlEnv.SetKV(GoctlDebug, "False")
	}

	if !zctlEnv.HasKey(GoctlCache) {
		cacheDir, _ := pathx.GetCacheDir()
		zctlEnv.SetKV(GoctlCache, cacheDir)
	}

	if !zctlEnv.HasKey(GoctlExperimental) {
		zctlEnv.SetKV(GoctlExperimental, ExperimentalOn)
	}

	zctlEnv.SetKV(GoctlVersion, version.BuildVersion)

	protocVer, _ := protoc.Version()
	zctlEnv.SetKV(ProtocVersion, protocVer)

	protocGenGoVer, _ := protocgengo.Version()
	zctlEnv.SetKV(ProtocGenGoVersion, protocGenGoVer)

	protocGenGoGrpcVer, _ := protocgengogrpc.Version()
	zctlEnv.SetKV(ProtocGenGoGRPCVersion, protocGenGoGrpcVer)
}

func Print(args ...string) string {
	if len(args) == 0 {
		return strings.Join(zctlEnv.Format(), "\n")
	}

	var values []string
	for _, key := range args {
		value, ok := zctlEnv.GetString(key)
		if !ok {
			value = fmt.Sprintf("%s=%%not found%%", key)
		}
		values = append(values, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(values, "\n")
}

func Get(key string) string {
	return GetOr(key, "")
}

// Set sets the environment variable for testing
func Set(t *testing.T, key, value string) {
	zctlEnv.SetKV(key, value)
	t.Cleanup(func() {
		zctlEnv.Remove(key)
	})
}

func GetOr(key, def string) string {
	return zctlEnv.GetStringOr(key, def)
}

func UseExperimental() bool {
	return GetOr(GoctlExperimental, ExperimentalOff) == ExperimentalOn
}

func readEnv(zctlHome string) *sortedmap.SortedMap {
	envFile := filepath.Join(zctlHome, envFileDir)
	data, err := os.ReadFile(envFile)
	if err != nil {
		return nil
	}
	dataStr := string(data)
	lines := strings.Split(dataStr, "\n")
	sm := sortedmap.New()
	for _, line := range lines {
		_, _, err = sm.SetExpression(line)
		if err != nil {
			continue
		}
	}
	return sm
}

func WriteEnv(kv []string) error {
	defaultGoctlHome, err := pathx.GetDefaultGoctlHome()
	if err != nil {
		log.Fatalln(err)
	}
	data := sortedmap.New()
	for _, e := range kv {
		_, _, err := data.SetExpression(e)
		if err != nil {
			return err
		}
	}
	data.RangeIf(func(key, value any) bool {
		switch key.(string) {
		case GoctlHome, GoctlCache:
			path := value.(string)
			if !pathx.FileExists(path) {
				err = fmt.Errorf("[writeEnv]: path %q is not exists", path)
				return false
			}
		}
		if zctlEnv.HasKey(key) {
			zctlEnv.SetKV(key, value)
			return true
		} else {
			err = fmt.Errorf("[writeEnv]: invalid key: %v", key)
			return false
		}
	})
	if err != nil {
		return err
	}
	envFile := filepath.Join(defaultGoctlHome, envFileDir)
	return os.WriteFile(envFile, []byte(strings.Join(zctlEnv.Format(), "\n")), 0o777)
}
