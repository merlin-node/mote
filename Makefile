VERSION ?= dev
LDFLAGS = -s -w -X mote/internal/shared.Version=$(VERSION)
BUILD_DIR = dist

.PHONY: all build zk bk linux-amd64 linux-arm64 clean tidy run-zk run-bk

all: build

build: zk bk

zk:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/zk ./cmd/zk

bk:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/bk ./cmd/bk

# 交叉编译
linux-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/zk-linux-amd64 ./cmd/zk
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/bk-linux-amd64 ./cmd/bk

linux-arm64:
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/zk-linux-arm64 ./cmd/zk
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/bk-linux-arm64 ./cmd/bk

release: linux-amd64 linux-arm64

# 本地开发:用 sqlite 数据目录跑主控
run-zk: zk
	mkdir -p ./_data
	./$(BUILD_DIR)/zk run -c ./_data/zk-config.json

# 本地开发:跑被控连本机主控
run-bk: bk
	./$(BUILD_DIR)/bk run -s ws://127.0.0.1:25774 -t YOUR_TOKEN

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR) _data
