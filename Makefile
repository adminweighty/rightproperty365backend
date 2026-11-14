# Convenience targets. On Windows without make, run the underlying go commands
# directly (see README.md).

.PHONY: run worker migrate-up migrate-down migrate-status seed mail-preview fmt fmt-check vet test build check docker-api docker-worker docker-migrate

run:
	go run ./cmd/api

worker:
	go run ./cmd/worker

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

migrate-status:
	go run ./cmd/migrate status

# Development sample data into Postgres. Refuses to run unless
# APP_ENV=development and DEV_SEED_PASSWORD is set. Idempotent.
seed:
	go run ./cmd/seed

# Renders every email template with sample data into tmp/mail-preview/
# (git-ignored). Sends nothing and needs no API key.
mail-preview:
	go run ./cmd/mailpreview

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

vet:
	go vet ./...

test:
	go test ./...

build:
	go build -trimpath -o bin/ ./cmd/...

check: fmt-check vet test build

docker-api:
	docker build -f deploy/docker/Dockerfile --target api -t rp365-api .

docker-worker:
	docker build -f deploy/docker/Dockerfile --target worker -t rp365-worker .

docker-migrate:
	docker build -f deploy/docker/Dockerfile --target migrate -t rp365-migrate .
