GO  ?= go
BIN := switchboard

# Release targets. Release builds are pure Go (CGO off); the race detector in
# `test` needs CGO, so tests run natively on each OS instead.
DIST_TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: check fmt-check vet test hygiene dist

check: fmt-check vet test hygiene ## Everything CI runs on one machine

fmt-check: ## Fail when a tracked Go file needs gofmt (never rewrites)
	@out=$$(git ls-files -z -- '*.go' | xargs -0 gofmt -l); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race -count=1 ./...

# Fail if anything to be published carries a private detail. A maintainer keeps
# the private names in the untracked .agents/hygiene-patterns; set
# HYGIENE_FLAGS=-require-private to make its absence an error.
hygiene:
	$(GO) run ./tools/hygiene $(HYGIENE_FLAGS)

dist: ## Cross builds into dist/, plus the console-less Windows daemon
	@rm -rf dist && mkdir -p dist
	@for t in $(DIST_TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -o dist/$(BIN)-$$os-$$arch$$ext ./cmd/switchboard || exit 1; \
	  if [ "$$os" = windows ]; then \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags=-H=windowsgui -o dist/$(BIN)w-$$os-$$arch.exe ./cmd/switchboard || exit 1; \
	  fi; \
	done
	@ls dist
