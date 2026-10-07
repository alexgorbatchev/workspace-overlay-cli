build:
    go build -trimpath -o bin/workspace-overlay ./cmd/workspace-overlay

dev *args:
    go run ./cmd/workspace-overlay fixture create
    go run ./cmd/workspace-overlay overlay mount --config .tmp/dev-workspace/workspace-overlay.toml --replace {{args}}

stop *args:
    go run ./cmd/workspace-overlay overlay unmount --config .tmp/dev-workspace/workspace-overlay.toml {{args}}

status *args:
    go run ./cmd/workspace-overlay overlay status --config .tmp/dev-workspace/workspace-overlay.toml {{args}}

run *args:
    go run ./cmd/workspace-overlay {{args}}

run-ai *args:
    AGENT=1 go run ./cmd/workspace-overlay {{args}}

test:
    @mkdir -p .tmp
    go test -race -coverprofile=.tmp/coverage.out ./...
    @awk 'NR > 1 { total += $(NF-1); if ($NF > 0) covered += $(NF-1) } END { coverage = 100 * covered / total; printf "Coverage: %.2f%%\n", coverage; exit(coverage < 90) }' .tmp/coverage.out

lint:
    go vet ./...
    go mod tidy -diff
