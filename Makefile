# 工程命令在本地与 CI 共用；依赖固定在 go.mod/go.sum 并由 modverify 校验。
GO ?= go
VERSION ?= 0.1.0-dev
TOOL_BIN := $(CURDIR)/.local/bin
STATICCHECK_VERSION := v0.8.1
GOVULNCHECK_VERSION := v1.8.0
GITLEAKS_VERSION := v8.30.1
ACTIONLINT_VERSION := v1.7.12
# 工具按版本装入独立目录，修改版本号后路径随之变化并触发重装，避免本地沿用旧二进制。
STATICCHECK := $(TOOL_BIN)/staticcheck-$(STATICCHECK_VERSION)/staticcheck
GOVULNCHECK := $(TOOL_BIN)/govulncheck-$(GOVULNCHECK_VERSION)/govulncheck
GITLEAKS := $(TOOL_BIN)/gitleaks-$(GITLEAKS_VERSION)/gitleaks
ACTIONLINT := $(TOOL_BIN)/actionlint-$(ACTIONLINT_VERSION)/actionlint

.PHONY: build check test fmt fmt-check vet modverify comments lint security secrets workflows tools

build:
	@mkdir -p dist
	$(GO) build -trimpath -ldflags '-X github.com/chaoRookie/turncourier/internal/cli.Version=$(VERSION)' -o dist/turncourier ./cmd/turncourier

fmt:
	"$$($(GO) env GOROOT)/bin/gofmt" -w cmd internal tests tools

# gofmt 自身失败（找不到工具链、文件不可读、语法错误）时输出为空，需单独检查退出码以免误报通过。
fmt-check:
	@files="$$("$$($(GO) env GOROOT)/bin/gofmt" -l cmd internal tests tools)" || exit 1; test -z "$$files" || { printf '需格式化的文件：\n%s\n' "$$files"; exit 1; }

vet:
	$(GO) vet ./...

comments:
	$(GO) run ./tools/commentcheck .

test:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) run ./tools/covercheck -min 80 coverage.out

# staticcheck 与 govulncheck 会从 PATH 查找 go，把 $(GO) 所属工具链放到最前面，GO 覆盖才对它们生效。
lint: $(STATICCHECK)
	goroot="$$($(GO) env GOROOT)" && PATH="$$goroot/bin:$$PATH" $(STATICCHECK) ./...

# go.sum 记录的依赖哈希须与模块缓存一致，防止被篡改的依赖进入构建。
modverify:
	$(GO) mod verify

check: fmt-check vet modverify comments test lint

security: $(GOVULNCHECK) secrets
	goroot="$$($(GO) env GOROOT)" && PATH="$$goroot/bin:$$PATH" $(GOVULNCHECK) ./...

secrets: $(GITLEAKS)
	GITLEAKS_BIN='$(GITLEAKS)' bash scripts/scan-secrets.sh

workflows: $(ACTIONLINT)
	$(ACTIONLINT) -color

tools: $(STATICCHECK) $(GOVULNCHECK) $(GITLEAKS) $(ACTIONLINT)

$(STATICCHECK):
	@mkdir -p $(@D)
	GOBIN='$(@D)' $(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

$(GOVULNCHECK):
	@mkdir -p $(@D)
	GOBIN='$(@D)' $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

$(GITLEAKS):
	@mkdir -p $(@D)
	GOBIN='$(@D)' $(GO) install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

$(ACTIONLINT):
	@mkdir -p $(@D)
	GOBIN='$(@D)' $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
