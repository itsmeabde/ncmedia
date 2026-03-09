server:
	go run ./cmd/ncmedia/main.go

build:
	CGO_ENABLED=0 go build -pgo=auto -o ncmedia -ldflags="-w -s" ./cmd/ncmedia

restart:
	sudo systemctl restart ncmedia.service

status:
	sudo systemctl status ncmedia.service

.PHONY: server build restart status