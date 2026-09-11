# Leilão relâmpago por WhatsApp

Motor de leilão de 1–3 minutos com *soft close*, alimentado por mensagens
de WhatsApp. O Redis é a autoridade sobre a corrida de lances (todas as
decisões de tempo saem do `TIME` do próprio Redis, dentro de scripts Lua,
então réplicas com relógios diferentes nunca discordam de quem venceu). O
Postgres guarda o registro durável: quem deu lance, cadastro, cobranças e
dono do produto.

## Como roda

```bash
docker compose up -d postgres redis
go test ./...           # o schema é aplicado sozinho (TestMain -> Migrate)
```

Os testes esperam Postgres em `127.0.0.1:5432` (banco `auction_test`) e
Redis em `127.0.0.1:6399` — é o que o `docker-compose.yml` sobe. Não é
preciso rodar migration à mão.

Para subir a aplicação:

```bash
export ENCRYPTION_KEY=$(openssl rand -base64 32)
export ADMIN_TOKEN=$(openssl rand -hex 32)
docker compose --profile app up --build
```

O servidor aplica as migrations sozinho na subida (`auction.Migrate`) e
serve o painel em `/` e a API em `/api/`.

## Fluxo

1. O painel abre um lote para um produto **disponível**, amarrado a um
   canal de WhatsApp (`channel_jid`).
2. Mensagens naquele canal viram lances: `POST /api/webhooks/whatsapp` →
   `ParseBid` → `Engine.PlaceBid`. Aceita `150`, `R$ 150`, `1.500,00`,
   `lance 200` e `+` (cobre o mínimo seguinte).
3. Cada lance nos últimos `ExtendTo` segundos **reseta** o relógio para
   `ExtendTo` (não soma), respeitando o `HardCap`.
4. No fechamento: se o vencedor não tem cadastro, recebe um link de uso
   único; com cadastro, o frete é calculado pelo CEP, o desconto de
   múltiplos itens é aplicado e a cobrança é gerada e enviada.
5. Não pagou no prazo → calote registrado, produto volta ao estoque. Não
   existe segundo colocado.

## Configuração

Toda variável aceita também `<NOME>_FILE` apontando para um secret.

| Variável | Obrigatória | Para quê |
|---|---|---|
| `DATABASE_URL` | sim | Postgres |
| `REDIS_ADDR` | sim | Redis >= 7 |
| `ADMIN_TOKEN` | sim | painel. **Sem ele o painel responde 503** |
| `ENCRYPTION_KEY` | sim | chave mestra AES-256 (base64, 32 bytes) |
| `HUBPAY_WEBHOOK_SECRET` | para usar HubPay | segredo HMAC do webhook |
| `ASAAS_WEBHOOK_TOKEN` | para usar Asaas | `authToken` do webhook |
| `WHATSAPP_WEBHOOK_SECRET` | para receber lances | segredo do webhook de entrada |
| `WHATSAPP_API_URL` / `_TOKEN` | para enviar mensagens | gateway (DigiGO) |
| `REGISTER_URL` | não | prefixo do link de cadastro |
| `CORS_ORIGIN` | não | se hospedar o painel em outra origem |
| `PANEL_DIR`, `LISTEN_ADDR` | não | padrões `panel` e `:8080` |

A chave do gateway de pagamento **não** é variável de ambiente: fica
cifrada (AES-256-GCM) no Postgres e é gerenciada pelo painel em
`/api/settings/payment`. A `ENCRYPTION_KEY` é o único segredo que continua
fora do banco — perdê-la significa recadastrar as chaves de pagamento.

## Segurança — leia antes de ir ao ar

- **Os webhooks de pagamento falham fechados.** Sem segredo configurado
  eles respondem 503, e sem assinatura válida respondem 401. Isso é
  deliberado: quem conhece um `charge_id` conseguiria marcar um pedido
  como pago — e o próprio comprador conhece o dele, porque vem na URL do
  checkout.
- **Confirme o header do webhook da Asaas.** `WebhookAuth` tenta os nomes
  conhecidos (`asaas-access-token` e variantes). Assim que você confirmar
  o seu no painel da Asaas, fixe em `ASAAS_TOKEN_HEADER` para a checagem
  ficar estrita.
- **Confirme o formato da assinatura da HubPay.** O código espera
  HMAC-SHA256 do corpo cru em hex, no header `X-Hubpay-Signature`
  (configurável por `HUBPAY_SIGNATURE_HEADER`). Ajuste `VerifyHubPay` se a
  HubPay usar outro esquema.
- **Fixe o Subresource Integrity do Vue no painel.** Hoje ele vem de um
  CDN de terceiros sem hash. Gere com:
  ```bash
  curl -s https://unpkg.com/vue@3.4.21/dist/vue.global.prod.js \
    | openssl dgst -sha384 -binary | openssl base64 -A
  ```
  e acrescente `integrity="sha384-..." crossorigin="anonymous"`, ou baixe
  o arquivo para `panel/vendor/`.
- **O token de admin é um segredo compartilhado**, guardado em
  `sessionStorage` no navegador. Troque por login de verdade com cookie
  `httpOnly` quando o painel crescer.
- **O gateway de WhatsApp precisa mesmo do `WHATSAPP_WEBHOOK_SECRET`:**
  quem consegue postar nesse endpoint dá lance no lugar de terceiros.

## Decisões que valem conhecer

- **Quem decide o valor cobrado é o Redis.** O evento de fechamento
  carrega o lance vencedor e é ele que vale, não o `lots.current_amount`
  do Postgres — que pode estar atrasado se a fila do orquestrador
  descartar um evento de lance sob pressão.
- **Fechamento nunca é descartado.** Lances e avisos podem ser (o Redis
  continua sendo a verdade); o fechamento espera na fila, e se ainda
  assim se perder, o `Reconcile` acha o lote e conclui o fluxo.
- **Pagamento que chega antes do `charge_id` ser gravado devolve 409**,
  para o provedor reenviar. Responder 200 fazia o pagamento sumir.
- **Provedor fora do ar não gera caloteiro.** O pedido é retentado
  (`RetryFailedCharges`), o prazo do cliente só começa quando a cobrança
  existe, e se desistirmos o pedido vira `failed` — não entra em
  `lot_defaults`.
- **Desconto de frete** assume "ganhou mais de um lote no mesmo dia"
  (frete combinado), não "deu vários lances". Trocar é só mudar
  `ShippingDiscountRule`.

## Testes

`regression_test.go` cobre um defeito por teste, todos encontrados numa
revisão de segurança/consistência: webhook sem assinatura, evento de
lance descartado baixando o valor cobrado, lote travado por fechamento
perdido, reentrega inflando contador, pagamento perdido por corrida com o
`AttachCharge`, caloteiro fabricado por provedor fora do ar, cliente
duplicado na Asaas, produto em dois leilões, painel aberto sem token,
lance troll sem teto e a ponta do WhatsApp que não existia.
