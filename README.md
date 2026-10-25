# zap

Gateway de envio de mensagens WhatsApp. Um processo Go expõe uma API HTTP local que outros sistemas (o Assistente Acadêmico, por exemplo) usam para enviar mensagens de texto. Não recebe nem encaminha mensagens.

Usa [whatsmeow](https://github.com/tulir/whatsmeow), que é um cliente **não oficial** do WhatsApp Web. Use um número dedicado ao serviço, não o seu número pessoal. Envios em volume ou sem consentimento do destinatário podem levar ao bloqueio da conta.

## Requisitos

- Go 1.25.5 ou mais recente (definido no `go.mod`).
- Compilador C, porque o driver SQLite (`go-sqlite3`) usa cgo.

## Build

```sh
go build -o zap .
go test ./...
```

## Configuração

Tudo é lido de variáveis de ambiente. Nenhuma senha fica em arquivo versionado.

| Variável | Padrão | Função |
|---|---|---|
| `ZAP_DATA_DIR` | `./data` | Diretório de `whatsapp.db` (sessão), `gateway.db` (idempotência) e `zap.lock` |
| `ZAP_LISTEN` | `127.0.0.1:8787` | Endereço da API. Mantenha em loopback ou na rede interna do Docker |
| `ZAP_TOKEN_FILE` | — (obrigatória em `serve`) | Arquivo com o token Bearer, com 32 caracteres ou mais |
| `ZAP_MIN_INTERVAL` | `20s` | Intervalo mínimo entre envios (mínimo 1s) |
| `ZAP_JITTER` | `10s` | Folga aleatória somada ao intervalo |
| `ZAP_MAX_WAIT` | `60s` | Se o próximo envio exigir mais espera que isso, a API responde 429 |
| `ZAP_DAILY_LIMIT` | `50` | Envios por dia, contados na hora local do servidor |

Crie o token e restrinja o arquivo:

```sh
openssl rand -base64 48 > /etc/zap/token
chmod 600 /etc/zap/token
```

## Pareamento

Rode uma vez, com o celular do número dedicado em mãos:

```sh
ZAP_DATA_DIR=/var/lib/zap ./zap pair
```

Escaneie o QR code pelo WhatsApp (Aparelhos conectados). A sessão fica em `whatsapp.db`.

## Execução

```sh
ZAP_DATA_DIR=/var/lib/zap ZAP_TOKEN_FILE=/etc/zap/token ./zap serve
```

Só uma instância pode usar o mesmo diretório de dados; o `zap.lock` impede a segunda.

## API

Todas as rotas exigem `Authorization: Bearer <token>`.

### `POST /v1/messages`

```json
{
  "to": "5598999999999",
  "text": "Lembrete: a entrega do debate é amanhã.",
  "idempotency_key": "entrega-42-debate-2026-10-26"
}
```

- `to`: DDI, DDD e número, só dígitos, sem `+`.
- `text`: até 1000 caracteres.
- `idempotency_key`: de 8 a 128 caracteres, letras, números, `_` ou `-`. Use uma chave estável para cada mensagem lógica. Repetir a mesma chave **não reenvia**.

Respostas:

| Status | `code` | Significado |
|---|---|---|
| 200 | — | Enviado. `{"message_id": "...", "state": "enviado", "duplicate": false}`. Com `duplicate: true`, a chave já tinha sido enviada e o ID anterior é devolvido |
| 400 | `validacao`, `requisicao_invalida` | Corpo ou campos inválidos |
| 401 | `nao_autorizado` | Token ausente ou errado |
| 409 | `estado_incerto` | Já existe envio com esta chave sem confirmação. **Não reenvie automaticamente**; verifique antes |
| 429 | `limite_de_ritmo` | Aguarde o tempo do header `Retry-After` |
| 429 | `limite_diario` | Limite diário atingido. Tente no dia seguinte |
| 502 | `falha_envio` | O WhatsApp não confirmou. A chave fica como incerta |
| 503 | `desconectado` | Sem conexão com o WhatsApp no momento. A chave não foi reservada |

### `GET /v1/health`

```json
{"connected": true, "paired": true, "sent_today": 3, "daily_limit": 50}
```

## Semântica de reenvio

Cada chave passa por estes estados em `gateway.db`:

- `pendente`: reservada, envio em andamento. Se o processo cair aqui, a chave fica pendente e a API responde 409.
- `enviado`: confirmado, com o ID da mensagem.
- `falhou`: recusada antes de chegar ao WhatsApp (por ritmo, limite ou cancelamento). Pode ser tentada de novo com a mesma chave.
- `incerto`: houve tentativa de envio e não houve confirmação. Pode ter sido entregue. Não é reaproveitada automaticamente.

Para o cliente, a regra é: **repetir a requisição com a mesma chave é seguro** quando a resposta foi 200 ou 429 com `limite_de_ritmo`. Após 409 ou 502, verifique antes de tentar de novo.

## Logs e dados

- Logs em stdout, em texto. Não contêm o conteúdo das mensagens; o destino aparece só com os quatro últimos dígitos.
- `gateway.db` guarda chave, estado, ID da mensagem e data. Não guarda telefone nem texto. Registros com mais de 90 dias são apagados na inicialização.
- `whatsapp.db` contém as chaves da sessão. Trate como segredo: permissão restrita e backup privado.

## Estrutura

- `main.go`: comandos `pair` e `serve`.
- `config.go`: variáveis de ambiente e leitura do token.
- `whatsapp.go`: cliente whatsmeow, pareamento, reconexão e lock.
- `gateway.go`: idempotência em SQLite, pacing e limite diário.
- `server.go`: rotas HTTP, autenticação e validação.
- `gateway_test.go`: testes de validação, pacing, idempotência e contagem.
