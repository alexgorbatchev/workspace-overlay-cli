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
    go test -race -count=1 -coverpkg="$(go list ./... | awk '!/\/internal\/scratch$/' | paste -sd, -)" -coverprofile=.tmp/coverage.out ./...
    @go tool cover -func=.tmp/coverage.out | awk '/^total:/ { sub("%", "", $NF); printf "Coverage: %s%%\n", $NF; exit($NF < 90) }'

lint:
    go mod tidy -diff
    go vet ./...
    golangci-lint run ./...
