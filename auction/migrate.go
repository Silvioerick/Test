package auction

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate aplica as migrations embutidas que ainda não rodaram, em ordem
// de nome, registrando cada uma em schema_migrations.
//
// O comentário da 001 dizia que store.go consumia as migrations, mas esse
// código não existia — subir o sistema exigia rodar psql à mão.
//
// Cada arquivo roda FORA de uma transação envolvente de propósito: as
// migrations 002 e 005 usam ALTER TYPE ... ADD VALUE, que o Postgres não
// aceita num bloco transacional junto com o uso do valor novo. Em troca,
// uma migration que falhe no meio pode ficar parcialmente aplicada — por
// isso todo DDL novo aqui usa IF NOT EXISTS e é reexecutável.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("criar schema_migrations: %w", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var done bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("registrar migration %s: %w", name, err)
		}
	}
	return nil
}
