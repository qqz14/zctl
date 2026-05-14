build:
	go build -ldflags="-s -w" zctl.go
	$(if $(shell command -v upx || which upx), upx zctl)

mac:
	GOOS=darwin go build -ldflags="-s -w" -o zctl-darwin zctl.go
	$(if $(shell command -v upx || which upx), upx zctl-darwin)

win:
	GOOS=windows go build -ldflags="-s -w" -o zctl.exe zctl.go
	$(if $(shell command -v upx || which upx), upx zctl.exe)

linux:
	GOOS=linux go build -ldflags="-s -w" -o zctl-linux zctl.go
	$(if $(shell command -v upx || which upx), upx zctl-linux)

image:
	docker build --rm --platform linux/amd64 -t kevinwan/zctl:$(version) .
	docker tag kevinwan/zctl:$(version) kevinwan/zctl:latest
	docker push kevinwan/zctl:$(version)
	docker push kevinwan/zctl:latest
	docker build --rm --platform linux/arm64 -t kevinwan/zctl:$(version)-arm64 .
	docker tag kevinwan/zctl:$(version)-arm64 kevinwan/zctl:latest-arm64
	docker push kevinwan/zctl:$(version)-arm64
	docker push kevinwan/zctl:latest-arm64
