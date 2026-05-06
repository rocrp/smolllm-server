.PHONY: build test vet tidy install reload uninstall logs status

BIN := $(HOME)/.local/bin/smolllm-server

build:
	go build -o $(BIN) ./cmd/server

test:
	go test ./... -race -count=1

vet:
	go vet ./...

tidy:
	go mod tidy

install:
	bash launch/service.sh install

reload:
	bash launch/service.sh reload

uninstall:
	bash launch/service.sh uninstall

logs:
	bash launch/service.sh logs

status:
	bash launch/service.sh status
