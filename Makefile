GO      ?= go
BIN     := switchboard
# VERSION reaches recipes only through the environment, never pasted into
# their text, so a tag name with shell syntax in it cannot run anything.
VERSION ?= dev
export VERSION

# Release targets. Release builds are pure Go (CGO off); the race detector in
# `test` needs CGO, so tests run natively on each OS instead.
DIST_TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: check fmt-check vet test hygiene release

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

# An archive per target in dist/: the binary (and on Windows the console-less
# switchboardw.exe), LICENSE, README.md and THIRD_PARTY_NOTICES, then
# SHA256SUMS over them all. VERSION is stamped into the binaries.
release: ## Release archives in dist/ (VERSION=vX.Y.Z)
	@case "$$VERSION" in ''|*[!A-Za-z0-9._+-]*) echo "VERSION may use only letters, digits and ._+-" >&2; exit 1;; esac
	@rm -rf dist && mkdir -p dist/stage
	@$(GO) run ./tools/notices > dist/stage/THIRD_PARTY_NOTICES
	@for t in $(DIST_TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; name=$(BIN)-$$VERSION-$$os-$$arch; dir=dist/stage/$$name; ext=; \
	  [ "$$os" = windows ] && ext=.exe; \
	  mkdir -p $$dir && cp LICENSE README.md dist/stage/THIRD_PARTY_NOTICES $$dir/ || exit 1; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -X main.version=$$VERSION" -o $$dir/$(BIN)$$ext ./cmd/switchboard || exit 1; \
	  if [ "$$os" = windows ]; then \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$$VERSION" -o $$dir/$(BIN)w.exe ./cmd/switchboard || exit 1; \
	    (cd dist/stage && zip -qr ../$$name.zip $$name) || exit 1; \
	  else \
	    tar -C dist/stage -czf dist/$$name.tar.gz $$name || exit 1; \
	  fi; \
	done
	@rm -rf dist/stage
	@cd dist && if command -v sha256sum >/dev/null; then sha256sum -- *.tar.gz *.zip; else shasum -a 256 -- *.tar.gz *.zip; fi > SHA256SUMS
	@ls dist
