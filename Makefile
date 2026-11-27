.PHONY: build test test-integration check hooks rules crap lint mutate
build:
	go build -o bin/ambermoth ./cmd/ambermoth
test:
	go test -race -count=1 ./...
# Builds the binary and runs the documented workflow end to end, SIGINT included.
test-integration:
	go test -race -count=1 -tags=integration ./...
check:
	gofmt -l . | (! grep .) && go vet ./... && go vet -tags=integration ./... && $(MAKE) test-integration
hooks:
	git config core.hooksPath .githooks
	@echo "commit-msg hook enabled: commit messages must follow Conventional Commits"
# The [checked] rules in docs/go-rules.md.
rules:
	go run ./tools/rulecheck ./...
# Fails on any function with CRAP >= 10 (docs/go-rules.md §11).
crap:
	go test -count=1 -tags=integration -coverpkg=./internal/...,./cmd/... -coverprofile=cover.out ./internal/... ./cmd/... >/dev/null
	go run ./tools/crap -profile cover.out -limit 10
# Dev tools, run at pinned versions; they never enter go.mod.
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run --max-issues-per-linter=0 --max-same-issues=0 ./...
mutate:
	go run github.com/go-gremlins/gremlins/cmd/gremlins@v0.5.1 unleash ./internal/...
