.PHONY: up down test race vet fmt

up:        ## sobe Postgres e Redis para os testes
	docker compose up -d postgres redis

down:
	docker compose down -v

test: ## exige `make up`
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .
