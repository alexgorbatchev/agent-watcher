default:
    @just --list

build:
    cargo build --all-targets --locked

test:
    cargo test --all-targets --locked
    cargo test --doc --locked

fmt:
    cargo fmt --package agent-watcher

lint:
    cargo fmt --package agent-watcher -- --check
    cargo clippy --all-targets --locked -- -D warnings

coverage:
    cargo llvm-cov --all-targets --locked --fail-under-lines 90 --fail-under-functions 90

check: lint test coverage
