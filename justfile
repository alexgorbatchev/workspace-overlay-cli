build:
    go build -o bin/workspace-overlay ./cmd/workspace-overlay

dev project="alpha":
    go run ./cmd/workspace-overlay overlay mount --replace --project {{quote(project)}}

stop project="alpha":
    go run ./cmd/workspace-overlay overlay unmount --project {{quote(project)}}

status project="alpha":
    go run ./cmd/workspace-overlay overlay status --project {{quote(project)}}

run *args:
    go run ./cmd/workspace-overlay {{args}}

run-ai *args:
    AGENT=1 go run ./cmd/workspace-overlay {{args}}

lint:
    go vet ./...
    go mod tidy -diff
