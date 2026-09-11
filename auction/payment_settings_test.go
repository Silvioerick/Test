package auction

import (
	"context"
	"encoding/base64"
	"testing"
)

func testEncryptor(t *testing.T) *Encryptor {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	enc, err := NewEncryptor(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestEncryptor_RoundTrip(t *testing.T) {
	enc := testEncryptor(t)
	ciphertext, err := enc.Encrypt("chave-super-secreta-123")
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == "chave-super-secreta-123" {
		t.Fatal("não deveria guardar em texto puro")
	}
	plain, err := enc.Decrypt(ciphertext)
	if err != nil || plain != "chave-super-secreta-123" {
		t.Fatalf("round-trip falhou: %q, %v", plain, err)
	}
}

func TestEncryptor_WrongMasterKeyFailsToDecrypt(t *testing.T) {
	enc1 := testEncryptor(t)
	ciphertext, _ := enc1.Encrypt("segredo")

	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(255 - i)
	}
	enc2, _ := NewEncryptor(base64.StdEncoding.EncodeToString(otherKey))
	if _, err := enc2.Decrypt(ciphertext); err == nil {
		t.Fatal("deveria falhar ao decifrar com chave mestra diferente")
	}
}

func TestEncryptor_RejectsWrongKeyLength(t *testing.T) {
	if _, err := NewEncryptor(base64.StdEncoding.EncodeToString([]byte("curta-demais"))); err == nil {
		t.Fatal("deveria rejeitar chave mestra que não tem 32 bytes")
	}
}

func TestPaymentSettings_CRUDAndActivation(t *testing.T) {
	db := testDB(t)
	db.Exec(`TRUNCATE payment_settings`)
	enc := testEncryptor(t)
	ps := NewPaymentSettingsStore(db, enc)
	ctx := context.Background()

	// nenhum provedor configurado ainda
	if _, _, _, err := ps.GetActive(ctx); err != ErrNoActivePaymentProvider {
		t.Fatalf("esperava ErrNoActivePaymentProvider, got %v", err)
	}

	if err := ps.Upsert(ctx, "asaas", "chave-asaas-123", "https://api-sandbox.asaas.com"); err != nil {
		t.Fatal(err)
	}
	if err := ps.Upsert(ctx, "hubpay", "chave-hubpay-456", ""); err != nil {
		t.Fatal(err)
	}

	list, err := ps.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("esperava 2 provedores cadastrados, tem %d", len(list))
	}
	for _, p := range list {
		if p.Active {
			t.Fatalf("nenhum deveria estar ativo ainda: %+v", p)
		}
		if p.MaskedKey == "chave-asaas-123" || p.MaskedKey == "chave-hubpay-456" {
			t.Fatal("chave não pode aparecer em texto puro na listagem")
		}
	}

	// ativar um provedor não cadastrado deve falhar
	if err := ps.Activate(ctx, "stripe"); err == nil {
		t.Fatal("deveria falhar ao ativar provedor não cadastrado")
	}

	if err := ps.Activate(ctx, "asaas"); err != nil {
		t.Fatal(err)
	}
	provider, key, baseURL, err := ps.GetActive(ctx)
	if err != nil || provider != "asaas" || key != "chave-asaas-123" || baseURL != "https://api-sandbox.asaas.com" {
		t.Fatalf("GetActive errado: provider=%s key=%s baseURL=%s err=%v", provider, key, baseURL, err)
	}

	// trocar a ativação: só um fica ativo por vez
	if err := ps.Activate(ctx, "hubpay"); err != nil {
		t.Fatal(err)
	}
	provider, key, _, err = ps.GetActive(ctx)
	if err != nil || provider != "hubpay" || key != "chave-hubpay-456" {
		t.Fatalf("ativação não trocou corretamente: provider=%s key=%s err=%v", provider, key, err)
	}
	list, _ = ps.List(ctx)
	activeCount := 0
	for _, p := range list {
		if p.Active {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("deveria ter exatamente 1 ativo, tem %d", activeCount)
	}
}

func TestDBPaymentProvider_UsesActiveProviderLive(t *testing.T) {
	db := testDB(t)
	db.Exec(`TRUNCATE payment_settings`)
	enc := testEncryptor(t)
	ps := NewPaymentSettingsStore(db, enc)
	ctx := context.Background()

	dbPay := NewDBPaymentProvider(ps)

	// sem provedor ativo -> erro claro, não pânico
	if _, err := dbPay.CreateCharge(ctx, "ref-1", 1000, Customer{Name: "X", Document: "123"}); err != ErrNoActivePaymentProvider {
		t.Fatalf("esperava ErrNoActivePaymentProvider, got %v", err)
	}

	fake, values := fakeAsaas(t)
	ps.Upsert(ctx, "asaas", "chave-de-teste", fake.URL)
	ps.Activate(ctx, "asaas")

	res, err := dbPay.CreateCharge(ctx, "ref-2", 5000, Customer{Name: "Fulano", Document: "12345678900"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChargeID == "" || len(*values) != 1 || (*values)[0] != 50.00 {
		t.Fatalf("cobrança não passou pelo provedor configurado no banco: %+v values=%v", res, *values)
	}

	// hubpay ainda não tem implementação real: ativar e cobrar deve falhar
	// com uma mensagem clara, não travar o processo.
	ps.Upsert(ctx, "hubpay", "outra-chave", "")
	ps.Activate(ctx, "hubpay")
	if _, err := dbPay.CreateCharge(ctx, "ref-3", 1000, Customer{Name: "Y", Document: "999"}); err == nil {
		t.Fatal("esperava erro (hubpay sem implementação real)")
	}
}
