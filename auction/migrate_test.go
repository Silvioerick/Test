package auction

import (
	"context"
	"database/sql"
	"testing"
)

const migrateTestDB = "auction_migrate_test"

// TestMigrate_DoZero garante que dá para subir o sistema num Postgres
// vazio só com Migrate — antes não existia runner nenhum, apesar de a
// migration 001 dizer que store.go a consumia.
func TestMigrate_DoZero(t *testing.T) {
	admin, err := sql.Open("postgres", "postgresql://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + migrateTestDB); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + migrateTestDB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + migrateTestDB) })

	db, err := sql.Open("postgres",
		"postgresql://postgres:postgres@127.0.0.1:5432/"+migrateTestDB+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("primeira execução: %v", err)
	}
	// Rodar de novo não pode quebrar nem reaplicar nada.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("segunda execução (deveria ser no-op): %v", err)
	}

	// Conta do próprio embed em vez de um número fixo, para não precisar
	// editar este teste a cada migration nova.
	files, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != len(files) {
		t.Fatalf("esperava %d migrations registradas, tem %d", len(files), applied)
	}
	for _, tbl := range []string{
		"participants", "products", "lots", "bids", "lot_defaults", "payment_orders",
		"registration_tokens", "shipping_zones", "login_codes", "sessions",
		"payment_settings", "app_settings",
	} {
		if _, err := db.Exec("SELECT 1 FROM " + tbl + " LIMIT 1"); err != nil {
			t.Errorf("tabela %s ausente: %v", tbl, err)
		}
	}
	for _, col := range []string{"channel_jid", "max_bid"} {
		var name string
		if err := db.QueryRow(`
			SELECT column_name FROM information_schema.columns
			WHERE table_name = 'lots' AND column_name = $1`, col).Scan(&name); err != nil {
			t.Errorf("coluna lots.%s ausente: %v", col, err)
		}
	}
}
