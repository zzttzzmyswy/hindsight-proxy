# VERSION is the single source of truth for the release version; everything
# below reads it rather than repeating the number.
VERSION := $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64

.PHONY: build test race fmt vet check image dist release clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/hindsight-proxy ./cmd/hindsight-proxy

test:
	go test ./... -count=1

race:
	go test ./... -count=1 -race

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# What CI runs.
check: vet race
	@size=$$(go run ./cmd/hindsight-proxy -print-tool-surface-size); \
	echo "tool surface: $$size characters"; \
	test "$$size" -lt 3000 || { echo "over the 3000 character budget"; exit 1; }

image:
	docker build --build-arg VERSION=$(VERSION) -t hindsight-proxy:$(VERSION) .

# Static binaries for the architectures this runs on, plus checksums.
dist:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/hindsight-proxy-$$os-$$arch ./cmd/hindsight-proxy || exit 1; \
	done
	cd dist && sha256sum hindsight-proxy-* > SHA256SUMS
	@echo "built $(VERSION); assets in dist/"

# Tag and publish. Refuses to reuse an existing tag, so a version is never
# overwritten once published.
release: dist
	@git rev-parse -q --verify refs/tags/v$(VERSION) >/dev/null && { \
		echo "tag v$(VERSION) already exists; bump VERSION"; exit 1; }
	git tag -a v$(VERSION) -m "hindsight-proxy v$(VERSION)"
	git push origin v$(VERSION)
	gh release create v$(VERSION) --title "hindsight-proxy v$(VERSION)" \
		--notes "See CHANGELOG.md for the changes in this release." \
		dist/hindsight-proxy-* dist/SHA256SUMS

clean:
	rm -rf bin dist
