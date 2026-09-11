package auction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAsaas imita o suficiente da API real (confirmado via documentação
// oficial) pra validar que o AsaasProvider manda os campos certos: header
// access_token, POST /v3/customers com name+cpfCnpj, POST /v3/payments com
// billingType UNDEFINED e value em reais (não centavos).
func fakeAsaas(t *testing.T) (*httptest.Server, *[]float64) {
	t.Helper()
	var customers []asaasCustomer
	var receivedValues []float64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/customers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("access_token") == "" {
			t.Error("header access_token ausente")
		}
		ref := r.URL.Query().Get("externalReference")
		var found []asaasCustomer
		for _, c := range customers {
			if c.ExternalReference == ref {
				found = append(found, c)
			}
		}
		json.NewEncoder(w).Encode(asaasCustomerList{Data: found})
	})
	mux.HandleFunc("POST /v3/customers", func(w http.ResponseWriter, r *http.Request) {
		var in asaasCustomer
		json.NewDecoder(r.Body).Decode(&in)
		if in.Name == "" || in.CpfCnpj == "" {
			t.Errorf("customer sem name/cpfCnpj: %+v", in)
		}
		in.ID = "cus_000001"
		customers = append(customers, in)
		json.NewEncoder(w).Encode(in)
	})
	mux.HandleFunc("POST /v3/payments", func(w http.ResponseWriter, r *http.Request) {
		var in asaasPaymentRequest
		json.NewDecoder(r.Body).Decode(&in)
		if in.BillingType != "UNDEFINED" {
			t.Errorf("billingType esperado UNDEFINED, veio %q", in.BillingType)
		}
		if in.Customer == "" {
			t.Error("payment sem customer associado")
		}
		receivedValues = append(receivedValues, in.Value)
		json.NewEncoder(w).Encode(asaasPaymentResponse{
			ID: "pay_000001", InvoiceURL: "https://sandbox.asaas.com/i/000001", Status: "PENDING",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &receivedValues
}

func TestAsaasProvider_CreateCharge(t *testing.T) {
	srv, values := fakeAsaas(t)
	p := NewAsaasProvider(srv.URL, "test-key")
	res, err := p.CreateCharge(context.Background(), "lot-1-order-1", 17500, Customer{
		Name: "Fulano de Tal", Document: "12345678900", Phone: "5511999999999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChargeID != "pay_000001" || res.PayURL != "https://sandbox.asaas.com/i/000001" {
		t.Fatalf("resultado inesperado: %+v", res)
	}

	// segunda cobrança do mesmo cliente (mesma reference+"-customer") deve
	// reaproveitar o cliente já criado na Asaas, não duplicar.
	_, err = p.CreateCharge(context.Background(), "lot-1-order-1", 5000, Customer{
		Name: "Fulano de Tal", Document: "12345678900",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*values) != 2 || (*values)[0] != 175.00 || (*values)[1] != 50.00 {
		t.Fatalf("valores em reais (não centavos) errados: %v", *values)
	}
}

func TestAsaasProvider_RequiresDocument(t *testing.T) {
	srv, _ := fakeAsaas(t)
	p := NewAsaasProvider(srv.URL, "test-key")
	_, err := p.CreateCharge(context.Background(), "lot-2-order-1", 1000, Customer{Name: "Sem CPF"})
	if err == nil {
		t.Fatal("esperava erro por falta de CPF/CNPJ")
	}
}

func TestAsaasWebhook_RecognizesPaidEvents(t *testing.T) {
	cases := map[string]bool{
		"PAYMENT_CONFIRMED": true, "PAYMENT_RECEIVED": true,
		"PAYMENT_OVERDUE": false, "PAYMENT_CREATED": false,
	}
	for event, want := range cases {
		ev := AsaasWebhookEvent{Event: event}
		if ev.IsPaidEvent() != want {
			t.Errorf("%s: IsPaidEvent()=%v, want %v", event, ev.IsPaidEvent(), want)
		}
	}
}
