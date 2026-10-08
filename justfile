build:
    go build -trimpath -o bin/workspace-overlay ./cmd/workspace-overlay

# Build and replace the workspace-overlay payload used by the dotfiles shim
dev-bootstrap $dotfiles_dir=(env_var("HOME") / ".dotfiles"):
    revision="$(git rev-parse --short HEAD 2>/dev/null || echo dev)" && go build -trimpath -ldflags="-X main.version=999.0.0-dev.$revision" -o bin/workspace-overlay ./cmd/workspace-overlay
    bun run ./scripts/devBootstrap.ts

dev *args:
    go run ./cmd/workspace-overlay fixture create
    go run ./cmd/workspace-overlay overlay mount --config dev-workspace/workspace-overlay.toml --replace {{args}}

stop *args:
    go run ./cmd/workspace-overlay overlay unmount --config dev-workspace/workspace-overlay.toml {{args}}

status *args:
    go run ./cmd/workspace-overlay overlay status --config dev-workspace/workspace-overlay.toml {{args}}

test-local *args:
    go test -race -v -run TestE2ELocalWorkspace ./cmd/workspace-overlay {{args}}

run *args:
    go run ./cmd/workspace-overlay {{args}}

run-ai *args:
    AGENT=1 go run ./cmd/workspace-overlay {{args}}

test:
    @mkdir -p .tmp
    go test -race -count=1 -coverpkg="$(go list ./... | awk '!/\/internal\/scratch$/' | paste -sd, -)" -coverprofile=.tmp/coverage.out ./...
    @go tool cover -func=.tmp/coverage.out | awk '/^total:/ { sub("%", "", $NF); printf "Coverage: %s%%\n", $NF; exit($NF < 90) }'

lint:
    go mod tidy -diff
    go vet ./...
    golangci-lint run ./...
