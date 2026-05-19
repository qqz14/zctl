Name: {{.serviceName}}.rpc
ListenOn: 0.0.0.0:{{.port}}
Env: dev

DatabaseConf:
  Type: mysql
  Host: 127.0.0.1
  Port: 3306
  DBName: {{.serviceName}}
  Username: root
  Password: "password"
  MaxOpenConn: 50
  SSLMode: disable
  CacheTime: 5

Log:
  ServiceName: {{.serviceName}}Logger
  Mode: console
  Level: debug
  Encoding: plain
  StackCoolDownMillis: 100

RedisConf:
  Host: 127.0.0.1:6379
  Db: 0

Prometheus:
  Host: 0.0.0.0
  Port: 4000
  Path: /metrics
