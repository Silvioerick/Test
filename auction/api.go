package auction

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
)

// APIServer expõe o painel web (cadastro de produto/leilão, relatórios,
// zonas de frete) e os endpoints PÚBLICOS que o vencedor usa pra
// completar o cadastro depois de ganhar. Os lances em si continuam
// entrando pelo WhatsApp (webhook do DigiGO -> Engine.PlaceBid).
type APIServer struct {
	db         *sql.DB
	store      *Store
	engine     *Engine
	orch       *Orchestrator
	payments   *PaymentSettingsStore // pode ser nil se o servidor não usar DBPaymentProvider
	admin      string                // token fixo simples; troque pela auth real do painel.digitalsac.io
	hooks      WebhookAuth           // fallback fixo; o painel tem precedência (ver authFor)
	settings   *SettingsStore        // configuração editável no painel
	waSecret   string                // fallback do segredo do webhook de entrada
	waDefaults int                   // nº de calotes que bloqueia lances. 0 = não bloqueia
	cors       string                // origem permitida no CORS. "" = sem CORS
	loginIPs   *ipLimiter            // trava a varredura de números no pedido de código
	logf       func(string, ...any)
	mux        *http.ServeMux
}

// publicPaths não exigem o token de admin: o webhook do provedor de
// pagamento é autenticado pela própria assinatura dele, e o fluxo de
// cadastro é usado pelo cliente final, que não tem (nem deve ter) o token.
var publicPaths = map[string]bool{
	"/api/webhooks/hubpay":        true,
	"/api/webhooks/asaas":         true,
	"/api/public/registration":    true, // GET com ?token=
	"/api/public/registration/":   true, // POST /api/public/registration/{token}
	"/api/customer/login/request": true,
	"/api/customer/login/verify":  true,
	"/api/webhooks/whatsapp":      true, // autenticado pelo próprio segredo
}

func NewAPIServer(db *sql.DB, engine *Engine, orch *Orchestrator, adminToken string) *APIServer {
	s := &APIServer{
		db: db, store: NewStore(db), engine: engine, orch: orch, admin: adminToken,
		loginIPs: newIPLimiter(loginCodeMaxPerIP, loginCodeWindow),
		logf:     func(string, ...any) {},
	}
	m := http.NewServeMux()
	m.HandleFunc("POST /api/webhooks/whatsapp", s.whatsappWebhook)
	m.HandleFunc("POST /api/customer/logout", s.customerLogout)
	m.HandleFunc("POST /api/products", s.createProduct)
	m.HandleFunc("GET /api/products", s.listProducts)
	m.HandleFunc("POST /api/lots", s.createLot)
	m.HandleFunc("GET /api/lots", s.listLots)
	m.HandleFunc("GET /api/lots/{id}", s.getLot)
	m.HandleFunc("GET /api/reports/summary", s.reportSummary)
	m.HandleFunc("GET /api/customers", s.listCustomers)
	m.HandleFunc("GET /api/shipping-zones", s.listShippingZones)
	m.HandleFunc("POST /api/shipping-zones", s.createShippingZone)
	m.HandleFunc("POST /api/webhooks/hubpay", s.hubpayWebhook)
	m.HandleFunc("POST /api/webhooks/asaas", s.asaasWebhook)
	m.HandleFunc("GET /api/public/registration", s.getRegistration)
	m.HandleFunc("POST /api/public/registration/{token}", s.postRegistration)
	m.HandleFunc("POST /api/customer/login/request", s.customerLoginRequest)
	m.HandleFunc("POST /api/customer/login/verify", s.customerLoginVerify)
	m.HandleFunc("GET /api/customer/me", s.customerMe)
	m.HandleFunc("GET /api/customer/history", s.customerHistory)
	m.HandleFunc("GET /api/settings/payment", s.listPaymentSettings)
	m.HandleFunc("POST /api/settings/payment", s.upsertPaymentSetting)
	m.HandleFunc("POST /api/settings/payment/activate", s.activatePaymentSetting)
	m.HandleFunc("GET /api/settings/app", s.listAppSettings)
	m.HandleFunc("POST /api/settings/app", s.setAppSetting)
	s.mux = m
	return s
}

// WithPaymentSettings liga os endpoints de configuração de pagamento
// (/api/settings/payment) a um PaymentSettingsStore. Sem isso, esses
// endpoints respondem 501 — é opcional porque nem todo deploy usa
// DBPaymentProvider (o servidor de demonstração, por exemplo, não usa).
func (s *APIServer) WithPaymentSettings(ps *PaymentSettingsStore) *APIServer {
	s.payments = ps
	return s
}

// WithWebhookAuth liga a verificação de assinatura dos webhooks de
// pagamento. SEM ISSO os webhooks respondem 503: a política é falhar
// fechada, porque aceitar um webhook não assinado é entregar produto de
// graça a quem souber (ou for dono de) um charge_id.
func (s *APIServer) WithWebhookAuth(a WebhookAuth) *APIServer {
	s.hooks = a
	return s
}

// WithSettings liga a configuração gerenciada pelo painel. Com ela, os
// segredos de webhook e o gateway do WhatsApp passam a vir do Postgres —
// o que estiver salvo no painel ganha do que estiver em WithWebhookAuth
// ou nas variáveis de ambiente.
func (s *APIServer) WithSettings(st *SettingsStore) *APIServer {
	s.settings = st
	return s
}

// authFor resolve os segredos de webhook na hora da requisição: painel
// primeiro, depois o que veio de WithWebhookAuth/env.
func (s *APIServer) authFor(r *http.Request) WebhookAuth {
	a := resolveFromSettings(r.Context(), s.settings)
	if a.HubPaySecret == "" {
		a.HubPaySecret = s.hooks.HubPaySecret
	}
	if a.HubPaySigHeader == "" {
		a.HubPaySigHeader = s.hooks.HubPaySigHeader
	}
	if a.AsaasToken == "" {
		a.AsaasToken = s.hooks.AsaasToken
	}
	if a.AsaasTokenHeader == "" {
		a.AsaasTokenHeader = s.hooks.AsaasTokenHeader
	}
	return a
}

// whatsappSecret resolve o segredo do webhook de entrada: painel, senão
// o que veio de WithWhatsApp/env.
func (s *APIServer) whatsappSecret(r *http.Request) string {
	if s.settings != nil {
		if v := s.settings.Get(r.Context(), SetWhatsAppWebhookSecret); v != "" {
			return v
		}
	}
	return s.waSecret
}

// WithWhatsApp liga o webhook de entrada de mensagens. secret é conferido
// no header X-Webhook-Token. blockAfterDefaults > 0 recusa lances de quem
// já deu essa quantidade de calotes.
func (s *APIServer) WithWhatsApp(secret string, blockAfterDefaults int) *APIServer {
	s.waSecret = secret
	s.waDefaults = blockAfterDefaults
	return s
}

// WithCORS libera o painel hospedado em outra origem. Vazio = sem CORS
// (painel servido pela própria API).
func (s *APIServer) WithCORS(origin string) *APIServer {
	s.cors = origin
	return s
}

// WithLogger define para onde vão os erros internos, que deixaram de ser
// devolvidos ao cliente HTTP.
func (s *APIServer) WithLogger(logf func(string, ...any)) *APIServer {
	if logf != nil {
		s.logf = logf
	}
	return s
}

func (s *APIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cors != "" {
		w.Header().Set("Access-Control-Allow-Origin", s.cors)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// Corpo limitado em toda requisição: sem isso um POST gigante consome
	// memória do processo à vontade.
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)

	// A checagem de rota pública tem de ver o MESMO caminho que o mux vai
	// despachar. Sem normalizar, "/api/customer/../settings/payment" batia
	// no prefixo público e só não vazava porque o ServeMux redireciona.
	if !isPublicPath(path.Clean(r.URL.Path)) {
		if s.admin == "" {
			// Falha fechada: um deploy que esqueceu o ADMIN_TOKEN não pode
			// virar um painel aberto com CPF e endereço de todo mundo.
			writeErr(w, http.StatusServiceUnavailable, "painel sem token de admin configurado")
			return
		}
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.admin
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeErr(w, http.StatusUnauthorized, "não autorizado")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func isPublicPath(p string) bool {
	if publicPaths[p] {
		return true
	}
	// Cadastro público (token na URL) e toda a área do cliente usam auth
	// própria (token de registro / sessão), não o token de admin do painel.
	return strings.HasPrefix(p, "/api/public/registration/") || strings.HasPrefix(p, "/api/customer/")
}

// fail registra o erro real e devolve uma mensagem genérica: detalhe de
// driver e estrutura de tabela não voltam mais no corpo da resposta.
func (s *APIServer) fail(w http.ResponseWriter, where string, err error) {
	s.logf("auction api: %s: %v", where, err)
	writeErr(w, http.StatusInternalServerError, "erro interno")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- Cadastro de produto -----------------------------------------------

type productDTO struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	SKU    string `json:"sku,omitempty"`
	Status string `json:"status"`
	Owner  string `json:"owner_jid,omitempty"`
}

func (s *APIServer) createProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		SKU  string `json:"sku"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name é obrigatório")
		return
	}
	var id int64
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO products (name, sku) VALUES ($1, NULLIF($2, '')) RETURNING id`,
		in.Name, in.SKU).Scan(&id)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	writeJSON(w, http.StatusCreated, productDTO{ID: id, Name: in.Name, SKU: in.SKU, Status: "available"})
}

func (s *APIServer) listProducts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT p.id, p.name, COALESCE(p.sku, ''), p.status, COALESCE(o.whatsapp_jid, '')
		FROM products p LEFT JOIN participants o ON o.id = p.owner_id
		ORDER BY p.id DESC`)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	defer rows.Close()
	out := []productDTO{}
	for rows.Next() {
		var p productDTO
		if err := rows.Scan(&p.ID, &p.Name, &p.SKU, &p.Status, &p.Owner); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Abertura de leilão ---------------------------------------------------

func (s *APIServer) createLot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID       int64  `json:"product_id"`
		StartPriceCents int64  `json:"start_price_cents"` // valor inicial do lance
		MinIncrentCents int64  `json:"min_increment_cents"`
		DurationSeconds int    `json:"duration_seconds"` // máx 180 (3 min)
		ExtendSeconds   int    `json:"extend_seconds"`   // padrão 15
		PaymentMinutes  int    `json:"payment_minutes"`  // padrão do orquestrador se 0
		MaxBidCents     int64  `json:"max_bid_cents"`    // teto do lote; 0 = sem teto
		ChannelJID      string `json:"channel_jid"`      // grupo/número onde o leilão acontece
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if in.ProductID <= 0 {
		writeErr(w, http.StatusBadRequest, "product_id é obrigatório")
		return
	}
	cfg := Config{
		StartPrice:   in.StartPriceCents,
		MinIncrement: in.MinIncrentCents,
		Duration:     time.Duration(in.DurationSeconds) * time.Second,
		MaxBid:       in.MaxBidCents,
	}
	if in.ExtendSeconds > 0 {
		cfg.ExtendTo = time.Duration(in.ExtendSeconds) * time.Second
	}
	lotID := fmt.Sprintf("lot-%d-%d", in.ProductID, time.Now().UnixNano())
	paymentWindow := time.Duration(in.PaymentMinutes) * time.Minute
	endsAt, err := s.orch.CreateAuction(r.Context(), s.engine, lotID, in.ProductID, cfg, paymentWindow, in.ChannelJID)
	switch {
	case errors.Is(err, ErrExists):
		writeErr(w, http.StatusConflict, "lote já existe")
		return
	case errors.Is(err, ErrProductUnavailable):
		// Antes dava para abrir dois leilões simultâneos do mesmo item
		// físico: dois vencedores, um produto.
		writeErr(w, http.StatusConflict, "produto não está disponível (já em leilão ou vendido)")
		return
	case err != nil:
		// Erro de validação de Config é culpa de quem chamou.
		if strings.HasPrefix(err.Error(), "auction: ") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.fail(w, "criar lote", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"lot_id": lotID, "ends_at": endsAt})
}

// --- Relatórios ------------------------------------------------------------

type lotDTO struct {
	ID            string     `json:"id"`
	ProductName   string     `json:"product_name"`
	Status        string     `json:"status"`
	StartPrice    int64      `json:"start_price_cents"`
	CurrentAmount int64      `json:"current_amount_cents"`
	ShippingCents int64      `json:"shipping_cents"`
	DiscountCents int64      `json:"discount_cents"`
	BidCount      int        `json:"bid_count"`
	ClaimantJID   string     `json:"claimant_jid,omitempty"`
	OpenedAt      time.Time  `json:"opened_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
}

func (s *APIServer) listLots(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	q := `
		SELECT l.id, pr.name, l.status, l.start_price, l.current_amount,
		       l.shipping_cents, l.discount_cents, l.bid_count,
		       COALESCE(cl.whatsapp_jid, ''), l.opened_at, l.closed_at
		FROM lots l
		JOIN products pr ON pr.id = l.product_id
		LEFT JOIN participants cl ON cl.id = l.claimant_id`
	args := []any{}
	if statusFilter != "" {
		q += ` WHERE l.status = $1`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY l.opened_at DESC LIMIT 200`
	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	defer rows.Close()
	out := []lotDTO{}
	for rows.Next() {
		var l lotDTO
		var closedAt sql.NullTime
		if err := rows.Scan(&l.ID, &l.ProductName, &l.Status, &l.StartPrice, &l.CurrentAmount,
			&l.ShippingCents, &l.DiscountCents, &l.BidCount, &l.ClaimantJID, &l.OpenedAt, &closedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if closedAt.Valid {
			l.ClosedAt = &closedAt.Time
		}
		out = append(out, l)
	}
	writeJSON(w, http.StatusOK, out)
}

type bidDTO struct {
	JID       string    `json:"jid"`
	Amount    int64     `json:"amount_cents"`
	CreatedAt time.Time `json:"created_at"`
}

type orderDTO struct {
	JID       string     `json:"jid"`
	Provider  string     `json:"provider"`
	Amount    int64      `json:"amount_cents"`
	Status    string     `json:"status"`
	PayURL    string     `json:"pay_url,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	PaidAt    *time.Time `json:"paid_at,omitempty"`
}

func (s *APIServer) getLot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var l lotDTO
	var closedAt sql.NullTime
	err := s.db.QueryRowContext(r.Context(), `
		SELECT l.id, pr.name, l.status, l.start_price, l.current_amount,
		       l.shipping_cents, l.discount_cents, l.bid_count,
		       COALESCE(cl.whatsapp_jid, ''), l.opened_at, l.closed_at
		FROM lots l JOIN products pr ON pr.id = l.product_id
		LEFT JOIN participants cl ON cl.id = l.claimant_id
		WHERE l.id = $1`, id).Scan(&l.ID, &l.ProductName, &l.Status, &l.StartPrice,
		&l.CurrentAmount, &l.ShippingCents, &l.DiscountCents, &l.BidCount, &l.ClaimantJID, &l.OpenedAt, &closedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "lote não encontrado")
		return
	}
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	if closedAt.Valid {
		l.ClosedAt = &closedAt.Time
	}

	bidRows, err := s.db.QueryContext(r.Context(), `
		SELECT p.whatsapp_jid, b.amount, b.created_at FROM bids b
		JOIN participants p ON p.id = b.participant_id
		WHERE b.lot_id = $1 ORDER BY b.amount DESC`, id)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	defer bidRows.Close()
	bids := []bidDTO{}
	for bidRows.Next() {
		var b bidDTO
		if err := bidRows.Scan(&b.JID, &b.Amount, &b.CreatedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		bids = append(bids, b)
	}

	orderRows, err := s.db.QueryContext(r.Context(), `
		SELECT p.whatsapp_jid, po.provider, po.amount, po.status, COALESCE(po.pay_url, ''), po.expires_at, po.paid_at
		FROM payment_orders po
		JOIN participants p ON p.id = po.participant_id
		WHERE po.lot_id = $1 ORDER BY po.created_at`, id)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	defer orderRows.Close()
	orders := []orderDTO{}
	for orderRows.Next() {
		var o orderDTO
		var paidAt sql.NullTime
		if err := orderRows.Scan(&o.JID, &o.Provider, &o.Amount, &o.Status, &o.PayURL, &o.ExpiresAt, &paidAt); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if paidAt.Valid {
			o.PaidAt = &paidAt.Time
		}
		orders = append(orders, o)
	}

	writeJSON(w, http.StatusOK, map[string]any{"lot": l, "bids": bids, "payment_orders": orders})
}

func (s *APIServer) reportSummary(w http.ResponseWriter, r *http.Request) {
	var totalLots, sold, unsold, open, awaitingReg, awaitingPayment, defaults int
	var revenue, shippingCollected sql.NullInt64
	row := s.db.QueryRowContext(r.Context(), `
		SELECT
			(SELECT count(*) FROM lots),
			(SELECT count(*) FROM lots WHERE status = 'paid'),
			(SELECT count(*) FROM lots WHERE status = 'unsold'),
			(SELECT count(*) FROM lots WHERE status = 'open'),
			(SELECT count(*) FROM lots WHERE status = 'awaiting_registration'),
			(SELECT count(*) FROM lots WHERE status = 'awaiting_payment'),
			(SELECT count(*) FROM lot_defaults),
			(SELECT COALESCE(SUM(amount), 0) FROM payment_orders WHERE status = 'paid'),
			(SELECT COALESCE(SUM(shipping_cents - discount_cents), 0) FROM lots WHERE status = 'paid')`)
	if err := row.Scan(&totalLots, &sold, &unsold, &open, &awaitingReg, &awaitingPayment, &defaults,
		&revenue, &shippingCollected); err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_lots":            totalLots,
		"sold":                  sold,
		"unsold_no_payment":     unsold,
		"open":                  open,
		"awaiting_registration": awaitingReg,
		"awaiting_payment":      awaitingPayment,
		"total_defaults":        defaults,
		"revenue_cents":         revenue.Int64,
		"net_shipping_cents":    shippingCollected.Int64,
	})
}

// --- Clientes (somente leitura) ---------------------------------------------

type customerDTO struct {
	JID        string `json:"jid"`
	Name       string `json:"name,omitempty"`
	Document   string `json:"document,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	Registered bool   `json:"registered"`
}

func (s *APIServer) listCustomers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT whatsapp_jid, COALESCE(full_name, name, ''), COALESCE(document, ''),
		       COALESCE(address_city, ''), COALESCE(address_state, ''), registered_at IS NOT NULL
		FROM participants ORDER BY id DESC LIMIT 500`)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	defer rows.Close()
	out := []customerDTO{}
	for rows.Next() {
		var c customerDTO
		if err := rows.Scan(&c.JID, &c.Name, &c.Document, &c.City, &c.State, &c.Registered); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Zonas de frete ----------------------------------------------------------

type shippingZoneDTO struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CEPPrefix  string `json:"cep_prefix"`
	PriceCents int64  `json:"price_cents"`
}

func (s *APIServer) listShippingZones(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, cep_prefix, price_cents FROM shipping_zones ORDER BY length(cep_prefix) DESC`)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	defer rows.Close()
	out := []shippingZoneDTO{}
	for rows.Next() {
		var z shippingZoneDTO
		if err := rows.Scan(&z.ID, &z.Name, &z.CEPPrefix, &z.PriceCents); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, z)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *APIServer) createShippingZone(w http.ResponseWriter, r *http.Request) {
	var in shippingZoneDTO
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if in.CEPPrefix == "" || in.PriceCents < 0 {
		writeErr(w, http.StatusBadRequest, "cep_prefix e price_cents são obrigatórios")
		return
	}
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO shipping_zones (name, cep_prefix, price_cents) VALUES ($1, $2, $3)
		ON CONFLICT (cep_prefix) DO UPDATE SET name = $1, price_cents = $3
		RETURNING id`, in.Name, in.CEPPrefix, in.PriceCents).Scan(&in.ID)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

// --- Cadastro público (o vencedor preenche depois de ganhar) ---------------

func (s *APIServer) getRegistration(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	rc, err := s.store.GetRegistrationContext(r.Context(), token)
	if errors.Is(err, ErrTokenInvalid) {
		writeErr(w, http.StatusGone, "link inválido, expirado ou já usado")
		return
	}
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"product_name":     rc.ProductName,
		"bid_amount_cents": rc.BidAmount,
	})
}

func (s *APIServer) postRegistration(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	var form CustomerForm
	if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if form.Name == "" || form.Document == "" || form.CEP == "" || form.Street == "" ||
		form.Number == "" || form.City == "" || form.State == "" {
		writeErr(w, http.StatusBadRequest, "nome, documento e endereço completo são obrigatórios")
		return
	}
	err := s.orch.CompleteRegistration(r.Context(), token, form)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, ErrTokenInvalid):
		writeErr(w, http.StatusGone, "link inválido, expirado ou já usado")
	case errors.Is(err, ErrNoShippingZone):
		// O link continua válido: a pessoa corrige o CEP e reenvia.
		writeErr(w, http.StatusBadRequest,
			"ainda não entregamos nesse CEP. Confira o número ou fale com a gente no WhatsApp.")
	case errors.Is(err, ErrChargePending):
		// Cadastro salvo; a cobrança falhou por nossa conta e vai para o
		// retry. Para o cliente isso é sucesso — ele não tem o que fazer.
		s.logf("auction api: cadastro salvo com cobrança pendente de retry: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "charge_pending": true})
	default:
		s.fail(w, r.URL.Path, err)
	}
}

// --- Webhooks de pagamento ---------------------------------------------------

func (s *APIServer) hubpayWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := readSignedBody(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if err := s.authFor(r).VerifyHubPay(r, body); err != nil {
		s.rejectWebhook(w, "hubpay", err)
		return
	}
	var in struct {
		ChargeID string `json:"charge_id"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if in.Status != "paid" {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": in.Status})
		return
	}
	s.finishPaymentWebhook(w, r, in.ChargeID)
}

// rejectWebhook responde a um webhook que não provou vir do provedor.
// Segredo ausente é 503 (erro NOSSO de configuração, e o provedor reenvia);
// assinatura errada é 401.
func (s *APIServer) rejectWebhook(w http.ResponseWriter, provider string, err error) {
	if errors.Is(err, ErrWebhookUnconfigured) {
		s.logf("auction api: webhook %s recusado: segredo não configurado — configure antes de ir ao ar", provider)
		writeErr(w, http.StatusServiceUnavailable, "webhook sem segredo configurado")
		return
	}
	s.logf("auction api: webhook %s com assinatura inválida", provider)
	writeErr(w, http.StatusUnauthorized, "assinatura inválida")
}

// finishPaymentWebhook traduz o resultado em código HTTP. ErrNoOrder vira
// 409 DE PROPÓSITO: é o webhook correndo na frente do AttachCharge, e
// precisamos que o provedor REENVIE. Respondendo 200, como antes, o
// pagamento sumia em silêncio e o cliente ainda virava caloteiro.
func (s *APIServer) finishPaymentWebhook(w http.ResponseWriter, r *http.Request, chargeID string) {
	err := s.orch.OnPaymentWebhook(r.Context(), chargeID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, ErrNoOrder):
		writeErr(w, http.StatusConflict, "cobrança ainda não registrada, reenvie")
	default:
		s.fail(w, "webhook de pagamento", err)
	}
}

// asaasWebhook espera os eventos PAYMENT_CONFIRMED / PAYMENT_RECEIVED
// configurados na Asaas (painel ou POST /v3/webhooks). Confirme no painel
// da Asaas o header exato que carrega o authToken do webhook antes de
// validar assinatura em produção — não hardcodei porque não confirmei esse
// detalhe na documentação consultada.
func (s *APIServer) asaasWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.authFor(r).VerifyAsaas(r); err != nil {
		s.rejectWebhook(w, "asaas", err)
		return
	}
	var ev AsaasWebhookEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if !ev.IsPaidEvent() {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": ev.Event})
		return
	}
	s.finishPaymentWebhook(w, r, ev.Payment.ID)
}

// --- Área do cliente: login por código via WhatsApp -------------------------
//
// Sem senha: a pessoa informa o telefone, recebe um código de 6 dígitos
// pelo próprio WhatsApp (prova de que o número é dela) e troca o código
// por uma sessão. Nenhum endpoint aqui usa o token de admin do painel.

const (
	loginCodeWindow   = 10 * time.Minute
	loginCodeMaxTries = 3 // no máximo 3 códigos pedidos a cada 10 min por número
	loginCodeTTL      = 5 * time.Minute
	loginMaxAttempts  = 5 // tentativas erradas de digitar o código antes de precisar pedir outro
	// loginCodeMaxPerIP limita a MESMA origem, independente do telefone: o
	// limite por número não impedia varrer milhares de números usando o
	// seu WhatsApp comercial como máquina de spam.
	loginCodeMaxPerIP = 20
	sessionTTL        = 24 * time.Hour
)

func (s *APIServer) customerLoginRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	jid, err := NormalizePhone(in.Phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "telefone inválido")
		return
	}
	if !s.loginIPs.allow(clientIP(r)) {
		s.logf("auction api: pedido de código bloqueado por limite de origem (%s)", clientIP(r))
		writeErr(w, http.StatusTooManyRequests, "muitos pedidos, tente de novo em alguns minutos")
		return
	}
	code, err := s.store.CreateLoginCode(r.Context(), jid, loginCodeWindow, loginCodeMaxTries, loginCodeTTL)
	if errors.Is(err, ErrTooManyCodes) {
		writeErr(w, http.StatusTooManyRequests, "muitos pedidos de código, tente de novo em alguns minutos")
		return
	}
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	minutes := int(loginCodeTTL.Minutes())
	text := fmt.Sprintf("Seu código de acesso à área do cliente: %s. Válido por %d min.", code, minutes)
	if err := s.orch.SendText(r.Context(), jid, text); err != nil {
		writeErr(w, http.StatusInternalServerError, "não consegui enviar o código pelo WhatsApp")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (s *APIServer) customerLoginVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	jid, err := NormalizePhone(in.Phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "telefone inválido")
		return
	}
	participantID, err := s.store.VerifyLoginCode(r.Context(), jid, in.Code, loginMaxAttempts)
	if errors.Is(err, ErrCodeInvalid) {
		writeErr(w, http.StatusUnauthorized, "código inválido ou expirado")
		return
	}
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	token, err := s.store.CreateSession(r.Context(), participantID, sessionTTL)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_token":      token,
		"expires_in_seconds": int(sessionTTL.Seconds()),
	})
}

// requireCustomerSession lê "Authorization: Bearer <session_token>" — uma
// sessão de cliente, nunca o token de admin do painel.
func (s *APIServer) requireCustomerSession(r *http.Request) (int64, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		return 0, ErrSessionInvalid
	}
	return s.store.GetSession(r.Context(), token)
}

type customerProfileDTO struct {
	JID        string `json:"jid"`
	Name       string `json:"name,omitempty"`
	Document   string `json:"document,omitempty"`
	Email      string `json:"email,omitempty"`
	CEP        string `json:"cep,omitempty"`
	Street     string `json:"street,omitempty"`
	Number     string `json:"number,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	Registered bool   `json:"registered"`
}

func (s *APIServer) customerMe(w http.ResponseWriter, r *http.Request) {
	pid, err := s.requireCustomerSession(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "sessão inválida ou expirada, faça login de novo")
		return
	}
	p, err := s.store.GetCustomerProfile(r.Context(), pid)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	writeJSON(w, http.StatusOK, customerProfileDTO{
		JID: p.JID, Name: p.Name, Document: p.Document, Email: p.Email,
		CEP: p.CEP, Street: p.Street, Number: p.Number, City: p.City, State: p.State,
		Registered: p.Registered,
	})
}

type customerLotDTO struct {
	LotID         string     `json:"lot_id"`
	ProductName   string     `json:"product_name"`
	LotStatus     string     `json:"lot_status"`
	YourBidCents  int64      `json:"your_bid_cents"`
	Won           bool       `json:"won"`
	ShippingCents int64      `json:"shipping_cents,omitempty"`
	DiscountCents int64      `json:"discount_cents,omitempty"`
	ChargeCents   *int64     `json:"charge_cents,omitempty"`
	PaymentStatus string     `json:"payment_status,omitempty"` // pending, paid, expired — vazio se nunca chegou a ser cobrado
	PaidAt        *time.Time `json:"paid_at,omitempty"`
	OpenedAt      time.Time  `json:"opened_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
}

// customerHistory é "tudo que a pessoa tem direito de ver": cada lote em
// que deu lance, se ganhou, e se pagou ou não — sempre da própria sessão,
// nunca de um id que alguém possa forjar na URL.
func (s *APIServer) customerHistory(w http.ResponseWriter, r *http.Request) {
	pid, err := s.requireCustomerSession(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "sessão inválida ou expirada, faça login de novo")
		return
	}
	hist, err := s.store.GetCustomerHistory(r.Context(), pid)
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	out := make([]customerLotDTO, 0, len(hist))
	for _, h := range hist {
		d := customerLotDTO{
			LotID: h.LotID, ProductName: h.ProductName, LotStatus: h.LotStatus,
			YourBidCents: h.YourBidCents, Won: h.Won,
			ShippingCents: h.ShippingCents, DiscountCents: h.DiscountCents,
			OpenedAt: h.OpenedAt,
		}
		if h.ClosedAt.Valid {
			d.ClosedAt = &h.ClosedAt.Time
		}
		if h.ChargeAmount.Valid {
			d.ChargeCents = &h.ChargeAmount.Int64
		}
		if h.PaymentStatus.Valid {
			d.PaymentStatus = h.PaymentStatus.String
		}
		if h.PaidAt.Valid {
			d.PaidAt = &h.PaidAt.Time
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Configuração de pagamento (painel admin) -------------------------------
//
// A chave de API do gateway fica cifrada no Postgres, não em variável de
// ambiente — só a chave mestra de cifragem (ENCRYPTION_KEY) mora fora do
// banco. Ver payment_settings.go e crypto.go.

type paymentSettingDTO struct {
	Provider  string    `json:"provider"`
	BaseURL   string    `json:"base_url,omitempty"`
	Active    bool      `json:"active"`
	MaskedKey string    `json:"masked_key"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *APIServer) listPaymentSettings(w http.ResponseWriter, r *http.Request) {
	if s.payments == nil {
		writeErr(w, http.StatusNotImplemented, "este servidor não usa configuração de pagamento pelo banco")
		return
	}
	list, err := s.payments.List(r.Context())
	if err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	out := make([]paymentSettingDTO, 0, len(list))
	for _, p := range list {
		out = append(out, paymentSettingDTO{
			Provider: p.Provider, BaseURL: p.BaseURL, Active: p.Active,
			MaskedKey: p.MaskedKey, UpdatedAt: p.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *APIServer) upsertPaymentSetting(w http.ResponseWriter, r *http.Request) {
	if s.payments == nil {
		writeErr(w, http.StatusNotImplemented, "este servidor não usa configuração de pagamento pelo banco")
		return
	}
	var in struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
		Activate bool   `json:"activate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if in.Provider == "" || in.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "provider e api_key são obrigatórios")
		return
	}
	if err := s.payments.Upsert(r.Context(), in.Provider, in.APIKey, in.BaseURL); err != nil {
		s.fail(w, r.URL.Path, err)
		return
	}
	if in.Activate {
		if err := s.payments.Activate(r.Context(), in.Provider); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *APIServer) activatePaymentSetting(w http.ResponseWriter, r *http.Request) {
	if s.payments == nil {
		writeErr(w, http.StatusNotImplemented, "este servidor não usa configuração de pagamento pelo banco")
		return
	}
	var in struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if err := s.payments.Activate(r.Context(), in.Provider); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Webhook de entrada do WhatsApp ----------------------------------------
//
// Esta é a ponta que faltava: sem ela, PlaceBid/ParseBid não eram chamados
// por nenhum código de produção e o sistema não tinha por onde receber um
// lance. Autenticada pelo próprio segredo (X-Webhook-Token), porque quem
// posta aqui consegue dar lance no lugar de terceiros.

func (s *APIServer) whatsappWebhook(w http.ResponseWriter, r *http.Request) {
	secret := s.whatsappSecret(r)
	if secret == "" {
		s.logf("auction api: webhook do whatsapp recusado: segredo não configurado")
		writeErr(w, http.StatusServiceUnavailable, "webhook sem segredo configurado")
		return
	}
	got := r.Header.Get("X-Webhook-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		writeErr(w, http.StatusUnauthorized, "não autorizado")
		return
	}
	body, err := readSignedBody(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	msg, err := ParseInbound(body)
	if err != nil {
		// Não é erro do gateway: mensagem que não interessa (status,
		// recibo de leitura). Responde 200 para não gerar reentrega.
		writeJSON(w, http.StatusOK, map[string]string{"ignored": err.Error()})
		return
	}
	out, err := s.orch.HandleInbound(r.Context(), s.engine, msg, s.waDefaults)
	if err != nil {
		s.fail(w, "processar mensagem do whatsapp", err)
		return
	}
	if !out.Handled {
		writeJSON(w, http.StatusOK, map[string]bool{"ignored": true})
		return
	}
	if out.Reply != "" {
		if err := s.orch.SendText(r.Context(), msg.From, out.Reply); err != nil {
			s.logf("auction api: responder lance de %s: %v", msg.From, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": string(out.Result.Status),
		"reply":  out.Reply,
	})
}

// customerLogout revoga a sessão da área do cliente. A coluna
// sessions.revoked_at existia e nunca era escrita — não havia logout.
func (s *APIServer) customerLogout(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "sessão inválida")
		return
	}
	if err := s.store.RevokeSession(r.Context(), token); err != nil {
		s.fail(w, "encerrar sessão", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Configuração geral (painel admin) --------------------------------------
//
// Tudo que o admin consegue digitar na tela mora aqui, cifrado quando for
// segredo — em vez de variável de ambiente. Enquanto ninguém salvar nada,
// o valor continua sendo herdado do env, e a tela mostra de onde ele veio.

func (s *APIServer) listAppSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeErr(w, http.StatusNotImplemented, "este servidor não usa configuração pelo banco")
		return
	}
	list, err := s.settings.List(r.Context())
	if err != nil {
		s.fail(w, "listar configuração", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *APIServer) setAppSetting(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeErr(w, http.StatusNotImplemented, "este servidor não usa configuração pelo banco")
		return
	}
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	if in.Key == "" {
		writeErr(w, http.StatusBadRequest, "key é obrigatória")
		return
	}
	if err := s.settings.Set(r.Context(), in.Key, in.Value); err != nil {
		// Chave desconhecida é erro de quem chamou, não do servidor.
		if strings.HasPrefix(err.Error(), "auction: chave de configuração desconhecida") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.fail(w, "gravar configuração", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
