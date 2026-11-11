#!/bin/sh
set -eu

# Defaults target only the local Docker Compose stand. Every integration suite
# gets its own database schema, Redis topic, and temporary S3 object keys.
export JACKO_TEST_DATABASE_URL="${JACKO_TEST_DATABASE_URL:-postgres://jacko:jacko@127.0.0.1:5432/jacko?sslmode=disable}"
export JACKO_TEST_REDIS_URL="${JACKO_TEST_REDIS_URL:-redis://127.0.0.1:6379/0}"
export JACKO_TEST_S3_URL="${JACKO_TEST_S3_URL:-http://127.0.0.1:9000}"
export JACKO_TEST_S3_REGION="${JACKO_TEST_S3_REGION:-us-east-1}"
export JACKO_TEST_S3_BUCKET="${JACKO_TEST_S3_BUCKET:-jacko}"
export JACKO_TEST_S3_ACCESS_KEY="${JACKO_TEST_S3_ACCESS_KEY:-jacko-local}"
export JACKO_TEST_S3_SECRET_KEY="${JACKO_TEST_S3_SECRET_KEY:-jacko-local-storage}"
export JACKO_TEST_LOAD=1

cd "$(dirname "$0")/.."
go test -race -count=1 -timeout=2m -v ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
