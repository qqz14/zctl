package config

import "github.com/zeromicro/go-zero/zrpc"

type Config struct {
	zrpc.RpcServerConf
	Env          string `json:",default=dev,options=dev|stage|uat|prod"`
	DatabaseConf DatabaseConf
	RedisConf    RedisConf
}

type DatabaseConf struct {
	Type        string `json:",default=mysql"`
	Host        string
	Port        int    `json:",default=2883"`
	DBName      string
	Username    string
	Password    string
	MaxOpenConn int    `json:",default=50"`
	SSLMode     string `json:",default=disable"`
	CacheTime   int    `json:",default=5"`
}

type RedisConf struct {
	Host string
	Db   int `json:",default=0"`
}
