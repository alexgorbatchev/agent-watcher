# agent-watcher justfile

# Default recipe: list available recipes
default:
    @just --list

# Run all unit tests with race detector and coverage
test *args:
    scripts/check-coverage.sh -race -coverprofile=coverage.out {{ if args == "" { "./..." } else { args } }}

# Run static code analysis
vet:
    go vet ./...

# Check formatting, module hygiene, vet and static analysis
lint:
    scripts/check-format.sh
    go mod tidy -diff
    go vet ./...
    golangci-lint run

# Format Go code
fmt:
    go fmt ./...

# Run module hygiene check
tidy:
    go mod tidy -diff

# Run static analysis and test suite in sequence
check: lint test

# Clean temporary and test artifacts
clean:
    rm -rf bin dist coverage.out .tmp

# Download project dependencies for local development
setup:
    go mod download
