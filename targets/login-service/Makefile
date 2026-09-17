BIN := bin
DATA ?= data

.PHONY: all build test vet run reindex clean

all: build

build:
	go build -o $(BIN)/login-service .

test:
	go test ./...

# 把每日汇总从原始记录重算一遍。换了算口径的代码之后必须跑这个，
# 否则代码是新的、页面上的数字还是老口径算出来的。
reindex: build
	$(BIN)/login-service -data $(DATA) -reindex

vet:
	go vet ./...

# 起一个实例。灰度期会有两个同时在跑，所以端口和数据目录都要能换：
#   make run PORT=:8081 DATA=data-canary VERSION=v2
PORT ?= :8080
VERSION ?= dev
run: build
	$(BIN)/login-service -addr $(PORT) -data $(DATA) -version $(VERSION)

clean:
	rm -rf $(BIN)
