PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
LDFLAGS := -s -w

# 交叉编译：make build GOOS=linux GOARCH=amd64
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

.PHONY: all build install uninstall test vet fmt fmt-check clean

all: build

build:
	cd server && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../bin/iris .

install: build
	install -d $(BINDIR)
	install -m 0755 bin/iris $(BINDIR)/iris

uninstall:
	rm -f $(BINDIR)/iris

test:
	cd server && go test -count=1 -timeout 60s ./...

vet:
	cd server && go vet ./...

fmt:
	cd server && gofmt -w .
	cd server && goimports -w . 2>/dev/null || true

fmt-check:
	@cd server && diff=$$(gofmt -l .); if [ -n "$$diff" ]; then \
		echo "gofmt found unformatted files:"; echo "$$diff"; exit 1; fi

clean:
	rm -rf bin/
