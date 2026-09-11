// Command server sobe a API do leilão, o motor de lances e os workers de
// varredura.
//
// Segredos e configuração (toda variável aceita também <NOME>_FILE
// apontando para um secret do Swarm):
//
//	DATABASE_URL          Postgres
//	REDIS_ADDR            Redis >= 7
//	ADMIN_USER            usuário do primeiro operador (padrão "admin")
//	ADMIN_PASSWORD        opcional: sem ela, a senha inicial é sorteada e
//	                      mostrada uma vez no log — melhor que deixá-la
//	                      em texto puro num arquivo
//	ADMIN_TOKEN           opcional: token para script/automação
//	ENCRYPTION_KEY        chave mestra AES-256 (base64, 32 bytes)
//	HUBPAY_WEBHOOK_SECRET segredo HMAC do webhook da HubPay
//	ASAAS_WEBHOOK_TOKEN   authToken configurado no webhook da Asaas
//	WHATSAPP_WEBHOOK_SECRET segredo do webhook de entrada de mensagens
//	WHATSAPP_API_URL      endpoint de envio do gateway (DigiGO)
//	WHATSAPP_API_TOKEN    token do gateway
//	REGISTER_URL          prefixo do link de cadastro
//	PANEL_DIR             pasta do painel estático (padrão ./panel)
//	CORS_ORIGIN           origem liberada, se o painel for hospedado à parte
//	LISTEN_ADDR           padrão :4000
//
// A chave do gateway de pagamento fica CIFRADA no Postgres e é gerenciada
// pelo painel em /api/settings/payment. O único segredo que continua fora
// do banco é a ENCRYPTION_KEY. Gere uma vez com:
//
//	openssl rand -base64 32
//
// Trocar a ENCRYPTION_KEY invalida toda chave já cifrada (recadastre no
// painel).
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	auction "example.com/auction"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

func mustEnv(key string) string {
	if v := envOr(key, ""); v != "" {
		return v
	}
	log.Fatalf("variável de ambiente obrigatória não definida: %s (ou %s_FILE apontando pro secret)", key, key)
	return ""
}

// randomPassword sorteia uma senha forte e legível para o primeiro
// acesso. Sem ambiguidade visual (nada de 0/O, 1/l/I), porque ela vai ser
// lida de um log e digitada à mão.
func randomPassword() (string, error) {
	const alfabeto = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alfabeto[int(b)%len(alfabeto)]
	}
	// Grupos de 5 para dar para ler e digitar sem errar.
	return string(out[0:5]) + "-" + string(out[5:10]) + "-" + string(out[10:15]) + "-" + string(out[15:20]), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if path := os.Getenv(key + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("lendo secret %s (%s): %v", key, path, err)
		}
		return string(trimTrailingSpace(data))
	}
	return fallback
}

func trimTrailingSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func main() {
	// Saída para o esquecimento de senha. Rode no servidor:
	//   docker compose exec -e ADMIN_PASSWORD='nova-senha' api \
	//     /app/server -reset-admin=usuario
	resetAdmin := flag.String("reset-admin", "",
		"redefine a senha deste operador usando ADMIN_PASSWORD e sai")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	// Falhar agora, com mensagem clara, em vez de na primeira requisição.
	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		log.Fatalf("postgres indisponível: %v", err)
	}
	cancelPing()
	if err := auction.Migrate(ctx, db); err != nil {
		log.Fatalf("aplicar migrations: %v", err)
	}

	if *resetAdmin != "" {
		senha := envOr("ADMIN_PASSWORD", "")
		if senha == "" {
			log.Fatal("defina ADMIN_PASSWORD com a nova senha, ex.: docker compose exec -e ADMIN_PASSWORD='nova-senha' api /app/server -reset-admin=usuario")
		}
		if err := auction.NewStore(db).ResetAdminPassword(ctx, *resetAdmin, senha); err != nil {
			log.Fatalf("redefinir senha: %v", err)
		}
		log.Printf("senha de %q redefinida. As sessões dele foram encerradas e a troca é obrigatória no próximo acesso.", *resetAdmin)
		return
	}

	rdb := redis.NewClient(&redis.Options{Addr: mustEnv("REDIS_ADDR")})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis indisponível: %v", err)
	}

	enc, err := auction.NewEncryptor(mustEnv("ENCRYPTION_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	paymentSettings := auction.NewPaymentSettingsStore(db, enc)
	pay := auction.NewDBPaymentProvider(paymentSettings)

	// Configuração editável no painel (gateway do WhatsApp, segredos de
	// webhook, link de cadastro). Enquanto ninguém salvar nada na tela, os
	// valores continuam sendo herdados das variáveis de ambiente de
	// sempre, então um deploy existente não quebra.
	settings := auction.NewSettingsStore(db, enc)
	notifier := auction.NewDBNotifier(settings).WithLogger(log.Printf)

	orch := auction.NewOrchestrator(auction.NewStore(db), pay, notifier, auction.NewZoneShipping(db),
		auction.OrchestratorOptions{
			PaymentWindow: 15 * time.Minute,
			ProviderName:  "db", // o provedor de fato é resolvido em tempo real pelo DBPaymentProvider
			RegisterURL:   envOr("REGISTER_URL", "https://leiloes.digitalsac.io/cadastro/"),
			Settings:      settings,
			OnNotice:      logNotice,
			Logf:          log.Printf,
		})
	eng := auction.New(rdb, auction.Options{
		OnEvent:  orch.HandleEvent,
		Warnings: []time.Duration{30 * time.Second, 10 * time.Second},
		Logf:     log.Printf,
	})
	go eng.Run(ctx)
	go orch.Run(ctx)
	go orch.RunExpiryWorker(ctx, eng, 5*time.Second)

	// Primeiro operador do painel, criado só quando ainda não existe
	// nenhum — depois disso as contas saem do próprio painel.
	//
	// A senha NÃO precisa vir do .env: sem ADMIN_PASSWORD, sorteamos uma e
	// mostramos uma única vez no log. Senha em texto puro num arquivo fica
	// em disco, em backup e em `docker inspect`; no log ela aparece uma
	// vez e perde a validade assim que a pessoa troca.
	//
	// De qualquer forma o operador nasce com troca obrigatória: o que vale
	// no banco é o hash PBKDF2, nunca a senha.
	store := auction.NewStore(db)
	if n, err := store.CountAdmins(ctx); err != nil {
		log.Fatalf("contar operadores do painel: %v", err)
	} else if n == 0 {
		user := envOr("ADMIN_USER", "admin")
		pass := envOr("ADMIN_PASSWORD", "")
		gerada := false
		if pass == "" {
			// Sem senha configurada, sorteamos uma e mostramos UMA VEZ no
			// log. É melhor que pedir a senha no .env: nada em texto puro
			// fica em disco, em backup ou em `docker inspect`.
			if pass, err = randomPassword(); err != nil {
				log.Fatalf("sortear senha do primeiro operador: %v", err)
			}
			gerada = true
		}
		if _, err := store.CreateAdmin(ctx, user, pass, true); err != nil {
			log.Fatalf("criar o primeiro operador do painel: %v", err)
		}
		if gerada {
			log.Printf("\n"+
				"┌──────────────────────────────────────────────────────────┐\n"+
				"│ PRIMEIRO ACESSO AO PAINEL                                │\n"+
				"│ usuário: %-47s │\n"+
				"│ senha:   %-47s │\n"+
				"│                                                          │\n"+
				"│ Esta senha aparece só agora e só aqui. Entre, troque, e   │\n"+
				"│ ela deixa de valer. Não precisa guardá-la em lugar algum. │\n"+
				"└──────────────────────────────────────────────────────────┘", user, pass)
		} else {
			log.Printf("operador %q criado a partir de ADMIN_PASSWORD — troque a senha no primeiro acesso e APAGUE ADMIN_PASSWORD do .env", user)
		}
	} else if envOr("ADMIN_PASSWORD", "") != "" {
		// Já existe operador: a variável não faz mais nada e só serve para
		// vazar uma senha em disco.
		log.Println("AVISO: ADMIN_PASSWORD continua definida mas é ignorada (o painel já tem operador). Apague do .env — senha em texto puro em arquivo é exposição à toa.")
	}

	// ADMIN_TOKEN agora é opcional: serve para script e automação. As
	// pessoas entram com usuário e senha.
	api := auction.NewAPIServer(db, eng, orch, envOr("ADMIN_TOKEN", "")).
		WithShipping(auction.NewZoneShipping(db)).
		WithPaymentSettings(paymentSettings).
		WithSettings(settings).
		WithWebhookAuth(auction.WebhookAuth{
			HubPaySecret:     envOr("HUBPAY_WEBHOOK_SECRET", ""),
			HubPaySigHeader:  envOr("HUBPAY_SIGNATURE_HEADER", ""),
			AsaasToken:       envOr("ASAAS_WEBHOOK_TOKEN", ""),
			AsaasTokenHeader: envOr("ASAAS_TOKEN_HEADER", ""),
		}).
		WithWhatsApp(envOr("WHATSAPP_WEBHOOK_SECRET", ""), 1).
		WithCORS(envOr("CORS_ORIGIN", "")).
		WithLogger(log.Printf)

	// O painel existia no repositório mas nunca era servido: sem isto ele
	// só funcionaria em outra origem, e não havia CORS.
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	panelDir := envOr("PANEL_DIR", "panel")
	if _, err := os.Stat(panelDir); err == nil {
		// O link mandado ao vencedor é <REGISTER_URL><token>, ou seja
		// /cadastro/<token>. Um FileServer procuraria um arquivo com o
		// nome do token e devolveria 404 — a mesma página atende
		// qualquer token, que o JavaScript lê da URL.
		registerPage := filepath.Join(panelDir, "cadastro.html")
		if _, err := os.Stat(registerPage); err == nil {
			mux.HandleFunc("/cadastro/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				http.ServeFile(w, r, registerPage)
			})
		} else {
			log.Printf("AVISO: %s não encontrado — o link de cadastro do vencedor vai dar 404", registerPage)
		}
		mux.Handle("/", http.FileServer(http.Dir(panelDir)))
		log.Printf("painel servido de %s", panelDir)
	} else {
		log.Printf("AVISO: painel não encontrado em %s — só a API está no ar", panelDir)
	}

	srv := &http.Server{
		Addr:              envOr("LISTEN_ADDR", ":4000"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if settings.Get(ctx, auction.SetWhatsAppAPIURL) == "" {
		log.Println("AVISO: gateway de WhatsApp não configurado — nenhuma mensagem será entregue de verdade. Configure no painel, em Configurações.")
	}
	go func() {
		log.Printf("ouvindo em %s — configure gateway e segredos no painel, em Configurações", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("encerrando...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func logNotice(_ context.Context, n auction.Notice) {
	log.Printf("[aviso] %s lote=%s jid=%s valor=%d %s", n.Kind, n.LotID, n.JID, n.Amount, n.Detail)
}
