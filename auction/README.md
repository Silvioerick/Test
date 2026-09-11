# Leilão relâmpago por WhatsApp

Motor de leilão de 1–3 minutos com *soft close*, alimentado por mensagens
de WhatsApp. O Redis é a autoridade sobre a corrida de lances (todas as
decisões de tempo saem do `TIME` do próprio Redis, dentro de scripts Lua,
então réplicas com relógios diferentes nunca discordam de quem venceu). O
Postgres guarda o registro durável: quem deu lance, cadastro, cobranças e
dono do produto.

## Instalação

Tudo em Docker, sim. Você precisa só de `docker` e `docker compose`.

```bash
cd auction
cp .env.example .env

# Gere os dois segredos obrigatórios (sem eles o processo recusa subir):
sed -i "s|^ENCRYPTION_KEY=.*|ENCRYPTION_KEY=$(openssl rand -base64 32)|" .env
sed -i "s|^ADMIN_TOKEN=.*|ADMIN_TOKEN=$(openssl rand -hex 32)|" .env

docker compose --profile app up --build
```

Pronto: painel em <http://localhost:8080/>, API em `/api/`. O servidor
aplica as migrations sozinho na subida. Cole o `ADMIN_TOKEN` do `.env` no
campo "Token de admin" do painel e cadastre a chave do gateway em
Configurações — ela vai cifrada para o Postgres, não para o `.env`.

Os dados ficam no volume `pgdata`; `docker compose down` preserva,
`docker compose down -v` apaga tudo.

Para desenvolver (dependências no Docker, Go na máquina):

```bash
docker compose up -d postgres redis
go test ./...     # o schema é aplicado sozinho (TestMain -> Migrate)
```

O compose cria dois bancos: `auction` (aplicação) e `auction_test`
(suíte, que dá `TRUNCATE` nas tabelas e por isso não pode encostar no
primeiro). Redis fica na 6399 para não colidir com uma instância local.

### Por que estas versões

- **Postgres 18** é o padrão do compose. O schema não usa nada acima de
  PG 12 — o recurso mais novo é `ALTER TYPE ... ADD VALUE IF NOT EXISTS`
  —, então subir de major é livre. A CI roda a suíte em **16 e 18** para
  que isso continue sendo verdade e não uma suposição. Fixe outra com
  `POSTGRES_VERSION` no `.env`.
- **Redis 7 é requisito duro, não preferência.** Todo o desempate de
  lance sai de `redis.call('TIME')` **dentro do script Lua** — é isso que
  faz réplicas com relógios diferentes nunca discordarem de quem venceu.
  Redis só permite comandos não-determinísticos em script a partir do
  7.0; em 6.x o motor não funciona.

### Trocar Redis por Dragonfly

É plausível: o motor usa uma superfície pequena e comum — `EXISTS`,
`EXPIRE`, `GET`, `SET`, `HSET`, `HMGET`, `TTL`, `XADD` no Lua, e
`HGETALL`, `SADD`, `SREM`, `SMEMBERS`, `DEL`, `TIME` no cliente.

Mas **a peça que decide tudo é `TIME` dentro do Lua**, e isso precisa ser
verificado antes de trocar, não depois. Teste assim:

```bash
docker run --rm -p 6399:6379 -d --name df   docker.dragonflydb.io/dragonflydb/dragonfly
redis-cli -p 6399 EVAL "return redis.call('TIME')[1]" 0   # tem que devolver o epoch
go test ./...                                              # a suíte inteira
```

Se os dois passarem, é drop-in: só trocar a imagem no compose. Se o
`EVAL` falhar, **não troque** — o leilão perde a garantia de quem venceu.

Dito isso: num leilão de WhatsApp o pico é de dezenas de lances por
segundo, e o Redis single-thread resolve isso com folga enorme. O ganho
multi-core do Dragonfly só aparece em ordens de grandeza acima disso.

### Vault

Vault **não substitui o Redis** — ele é cofre de segredos, não banco de
dados; não tem Lua atômico nem relógio para arbitrar lance.

Onde ele encaixa bem é na `ENCRYPTION_KEY`, e o código já está pronto
para isso: toda variável aceita `<NOME>_FILE` apontando para um arquivo.
Monte o segredo do Vault (via Agent Injector ou CSI) e aponte:

```yaml
environment:
  ENCRYPTION_KEY_FILE: /vault/secrets/encryption-key
  ADMIN_TOKEN_FILE: /vault/secrets/admin-token
```

O mesmo vale para secret do Docker Swarm ou Kubernetes. Assim nenhum
segredo fica em variável de ambiente nem no `.env`.

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
| `HUBPAY_WEBHOOK_SECRET` | não* | segredo HMAC do webhook |
| `ASAAS_WEBHOOK_TOKEN` | não* | `authToken` do webhook |
| `WHATSAPP_WEBHOOK_SECRET` | não* | segredo do webhook de entrada |
| `WHATSAPP_API_URL` / `_TOKEN` | não* | gateway (DigiGO) |
| `REGISTER_URL` | não* | prefixo do link de cadastro |

\* Configure pelo painel, em Configurações — estas variáveis existem só
como fallback para deploys anteriores.
| `CORS_ORIGIN` | não | se hospedar o painel em outra origem |
| `PANEL_DIR`, `LISTEN_ADDR` | não | padrões `panel` e `:8080` |

### Quase nada precisa de variável de ambiente

Só quatro variáveis são obrigatórias: `DATABASE_URL`, `REDIS_ADDR`,
`ENCRYPTION_KEY` e `ADMIN_TOKEN`. **Todo o resto se configura no painel**,
em Configurações — o gateway do DigiGO (URL e token), os segredos dos
webhooks de pagamento, o segredo do webhook de entrada e o prefixo do
link de cadastro. Valores sensíveis vão cifrados (AES-256-GCM) para o
Postgres; os demais ficam em claro, para dar para auditar no banco.

A tela mostra, campo a campo, de onde o valor efetivo veio: **salvo no
painel**, **herdado do ambiente** ou **não configurado**. O que estiver
salvo no painel sempre ganha da variável de ambiente, e apagar o campo
devolve o controle ao ambiente — então um deploy que já existia continua
funcionando sem ninguém abrir o painel, e a migração pode ser feita campo
a campo. Uma alteração vale na própria réplica na hora e nas demais em
até 5s (cache curto, porque o webhook consulta isso a cada requisição).

Quatro coisas não cabem no painel, de propósito:

| | Por quê |
|---|---|
| `DATABASE_URL`, `REDIS_ADDR` | São necessários para chegar até a tabela de configuração. |
| `ENCRYPTION_KEY` | É a chave que decifra os segredos da tela. Guardá-la ali não protegeria nada. |
| `ADMIN_TOKEN` | É o que protege o painel. Se morasse nele, seria preciso autenticar para poder configurar a autenticação. |

A chave do gateway de **pagamento** segue no seu próprio lugar
(`/api/settings/payment`), também cifrada. A `ENCRYPTION_KEY` é o único
segredo que continua fora do banco — perdê-la significa recadastrar
chaves de pagamento e segredos de webhook.

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
- **O painel não depende de CDN.** O Vue é servido de
  `panel/vendor/vue-3.4.21.prod.js` pelo próprio servidor, com
  `integrity` fixado. Antes ele vinha do unpkg sem hash — o que, além do
  risco de integridade, deixava o painel em branco em qualquer rede que
  bloqueasse o CDN. Para atualizar a versão:
  ```bash
  npm pack vue@3.4.21 && tar xzf vue-3.4.21.tgz
  cp package/dist/vue.global.prod.js panel/vendor/vue-3.4.21.prod.js
  echo "sha384-$(openssl dgst -sha384 -binary panel/vendor/vue-3.4.21.prod.js \
    | openssl base64 -A)"   # atualize o integrity nos dois HTML
  ```
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
