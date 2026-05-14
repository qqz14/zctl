package main

import (
	"github.com/zeromicro/go-zero/core/load"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/qqz14/zctl/cmd"
)

func main() {
	logx.Disable()
	load.Disable()
	cmd.Execute()
}
