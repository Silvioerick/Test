package auction

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain garante o schema antes de qualquer teste rodar. Sem isto era
// preciso aplicar as migrations à mão (psql arquivo por arquivo) antes de
// `go test` — um clone novo do repositório não passava.
func TestMain(m *testing.M) {
	if err := prepareSchema(); err != nil {
		fmt.Fprintf(os.Stderr, `
não consegui preparar o banco de teste: %v

Suba as dependências primeiro:
    docker compose up -d postgres redis

Os testes esperam Postgres em 127.0.0.1:5432 (banco auction_test) e
Redis em 127.0.0.1:6399.
`, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func prepareSchema() error {
	db, err := sql.Open("postgres", testDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// O Postgres do compose pode ainda estar subindo.
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = db.PingContext(ctx); lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		return lastErr
	}
	return Migrate(ctx, db)
}
