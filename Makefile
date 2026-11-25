.PHONY: test run migrate sqlc

test:
	go test ./...

run:
	go run ./cmd/api

migrate:
	go run ./cmd/migrate

sqlc:
	sqlc generate
lint-openapi:
	npx --yes @redocly/cli@latest lint api/openapi.yaml
