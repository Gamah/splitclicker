.PHONY: dev build test fmt migrate-up migrate-down up rebuild down logs ps psql

# ── Local dev (no Docker) ───────────────────────────────────────────────────
dev:
	go run ./cmd/server

build:
	go build -ldflags="-s -w" -o bin/splitclicker ./cmd/server

test:
	go test ./... -race

fmt:
	gofmt -w .

# ── Database ────────────────────────────────────────────────────────────────
migrate-up:
	goose -dir migrations postgres "$(DATABASE_URL)" up

migrate-down:
	goose -dir migrations postgres "$(DATABASE_URL)" down

# ── Docker Compose ──────────────────────────────────────────────────────────
# `make up` reviews data/config.json interactively (Enter keeps each current value)
# before building. `make up SKIP=1` skips the review and keeps current/default values
# (seeding data/config.json from the example only if it doesn't exist yet). Pure shell,
# so no Go toolchain is needed on the deploy host.
up:
	sh scripts/configure.sh $(if $(SKIP),--skip)
	GIT_HASH=$$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	docker compose -f docker/docker-compose.yml --env-file .env up -d --build

rebuild:
	GIT_HASH=$$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	docker compose -f docker/docker-compose.yml --env-file .env build --no-cache app
	docker compose -f docker/docker-compose.yml --env-file .env up -d

down:
	docker compose -f docker/docker-compose.yml down

logs:
	docker compose -f docker/docker-compose.yml logs -f app

ps:
	docker compose -f docker/docker-compose.yml ps

psql:
	docker compose -f docker/docker-compose.yml exec postgres psql -U splitclicker -d splitclicker
