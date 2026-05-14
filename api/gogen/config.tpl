// Code scaffolded by zctl. Safe to edit.
// zctl {{.version}}

package config

import {{.authImport}}

type Config struct {
	rest.RestConf
	{{.auth}}
	{{.jwtTrans}}
}
