#!/bin/bash

# local compare test
# compare zctl between latest and newest version if exists different.

execute_command() {
    local command="$1"

    echo "=> $command"
    eval "$command"
}

has_diff (){
  local command="$1"
  if $command &> /dev/null; then
          return 0
      else
          return 1
      fi
}

echo "=======================env init============================="
WD=$(readlink -f $(dirname $0))/build
BIN=$WD/bin
PROJECT_DIR=$WD/project
OLD_CODE=$PROJECT_DIR/old
NEW_CODE=$PROJECT_DIR/new

if [ -d $WD ]; then
  execute_command "rm -rf $WD"
fi

execute_command "mkdir -p $BIN $PROJECT_DIR $OLD_CODE $NEW_CODE"
execute_command "export GOBIN=$BIN"

echo "=======================install zctl============================="
# install latest zctl
execute_command "go install github.com/zeromicro/go-zero/tools/zctl@master"
execute_command "mv $BIN/zctl $BIN/zctl.old"
execute_command "$BIN/zctl.old env"
execute_command "$BIN/zctl.old env -w GOCTL_EXPERIMENTAL=on"

# install newest zctl
execute_command "cd .."
execute_command "go build -o zctl.new ."
execute_command "mv zctl.new $BIN/zctl.new"
execute_command "cd -"
execute_command "$BIN/zctl.new env"
execute_command "$BIN/zctl.new env -w GOCTL_EXPERIMENTAL=on"

echo "=======================go mod tidy============================="
# go mod init
execute_command "cd $OLD_CODE"
execute_command "go mod init demo"
execute_command "cd -"

execute_command "cd $NEW_CODE"
execute_command "go mod init demo"
execute_command "cd -"

echo "=======================generate api============================="
execute_command "cd api"
# generate api by zctl.old
execute_command "$BIN/zctl.old api go --api test.api --dir $OLD_CODE/api"
# generate api by zctl.new
execute_command "$BIN/zctl.new api go --api test.api --dir $NEW_CODE/api"
execute_command "cd -"

echo "=======================generate rpc============================="
execute_command "cd rpc"
# generate rpc by zctl.old
execute_command "$BIN/zctl.old rpc protoc test.proto --go_out=$OLD_CODE/rpc  --go-grpc_out=$OLD_CODE/rpc  --zrpc_out=$OLD_CODE/rpc"
# generate rpc by zctl.new
execute_command "$BIN/zctl.new rpc protoc test.proto --go_out=$NEW_CODE/rpc  --go-grpc_out=$NEW_CODE/rpc  --zrpc_out=$NEW_CODE/rpc"
execute_command "cd -"

echo "=======================generate model============================="
execute_command "cd model"
# generate model by zctl.old
execute_command "$BIN/zctl.old model mysql ddl --src user.sql --dir $OLD_CODE/cache -c"
execute_command "$BIN/zctl.old model mysql ddl --src user.sql --dir $OLD_CODE/nocache"
# generate model by zctl.new
execute_command "$BIN/zctl.new model mysql ddl --src user.sql --dir $NEW_CODE/cache -c"
execute_command "$BIN/zctl.new model mysql ddl --src user.sql --dir $NEW_CODE/nocache"
execute_command "cd -"

echo "=======================diff compare============================="
# compare and diff
if has_diff "diff -rq $OLD_CODE $NEW_CODE"; then
  echo "no diff"
  exit 0
else
  echo "a diff found"
  execute_command "diff -r $OLD_CODE $NEW_CODE"
  exit 1
fi
