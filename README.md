# Splitpayments

A self-hosted [Splitwise](https://www.splitwise.com) alternative.
Track shared expenses with groups and friends, split them and 
settle up.

## Features

- **Groups** with per-member balances.
- **Split payments** even across multiple payers.
- **Settle-up payments** recorded as first-class entries.
- **Receipt scanning** - Upload a photo (or use a camera) to save manual data 
   entry.
- **Invite links** instead of open sign-up; 
- **Guest members** for people without accounts.
- **Installable PWA** — add it to your home screen on Android or iOS and it
  runs full-screen like a native app.
- **CSV export** per group in a Splitwise-like shape.

## Quick start

Open the site (defaults to :8080), and create the first account. 
Everyone else joins through invite links you create when adding 
members to a group.

## Configuration

Configuration is handled via cli flags or env flags. Each flag has an
equivalent environment variable (upper-case, dashes and dots replaced with
underscores); a flag given on the command line takes precedence over the
environment variable.

| Flag | Environment variable | Default | Purpose |
|---|---|---|---|
| `-listen` | `LISTEN` | `:8080` | HTTP listen address |
| `-db-driver` | `DB_DRIVER` | `sqlite` | Database backend: `sqlite` (also `sqlite3`) or `postgres` (also `pg`/`postgresql`) |
| `-database` | `DATABASE` | `data/splitpayments.db` | For SQLite, the database file path (receipt images are stored in it too); for Postgres, a libpq-style connection string |
| `-static-dir` | `STATIC_DIR` | *(embedded)* | Serve assets from disk instead (dev) |
| `-currency` | `CURRENCY` | `GBP` | Install-wide ISO 4217 currency code |
| `-currency-symbol` | `CURRENCY_SYMBOL` | `£` | Symbol used in templates and the JS formatter |
| `-receipt-api-url` | `RECEIPT_API_URL` | *(disabled)* | API base URL, e.g. `https://api.openai.com/v1` or `https://api.anthropic.com` |
| `-receipt-api-key` | `RECEIPT_API_KEY` | *(empty)* | API key sent as `Authorization: Bearer` (and `x-api-key` for Anthropic) |
| `-receipt-model` | `RECEIPT_MODEL` | *(empty)* | Vision model name, e.g. `gpt-4o-mini` or `claude-sonnet-4-5` |
| `-receipt-api-format` | `RECEIPT_API_FORMAT` | *(auto)* | `openai`, `anthropic`, `gwtype1`, `gwtype2`, or `gwtype3`; auto-detects Anthropic from the URL |
| `-receipt-api-workflow` | `RECEIPT_API_WORKFLOW` | *(empty)* | Workflow executed on the gwtype2 or gwtype3 gateway |
| `-receipt-api-args` | `RECEIPT_API_ARGS` | `{}` | JSON workflow args for the gwtype2/gwtype3 gateway |
| `-session-lifetime` | `SESSION_LIFETIME` | `720h` | Login session duration |
| `-invite-lifetime` | `INVITE_LIFETIME` | `72h` | Invite link validity |
| `-trusted-proxy` | `TRUSTED_PROXY` | private ranges | Repeatable; extra CIDRs trusted for `X-Forwarded-For` |
| `-cookie-secure` | `COOKIE_SECURE` | `true` | Set `false` for plain-HTTP local dev |
| `--log.level` | `LOG_LEVEL` | `` | Lowest log level that should be output |
| `--log.format` | `LOG_FORMAT` | `text` | Log format to output |

### Database

Data lives in SQLite by default (no external services needed). To run
against Postgres instead, set the driver and a libpq-style connection
string; the schema is created automatically on first start, and the
database itself must already exist:

```sh
splitpayments -db-driver postgres \
  -database 'postgres://user:password@db:5432/splitpayments?sslmode=disable'
```

### Receipt scanning

Receipt scanning is enabled when `-receipt-api-url` is set along with
`-receipt-model` — or, for the `gwtype2`/`gwtype3` workflow formats, along
with `-receipt-api-workflow`. The protocol is picked by
`-receipt-api-format`: `openai` (the default), `anthropic`, `gwtype1`,
`gwtype2`, `gwtype3`, or empty to auto-detect Anthropic from an
`api.anthropic.com` base URL. When disabled the scan UI simply disappears.

- **OpenAI** (`openai`) — works with any OpenAI-compatible vision endpoint,
  including local ones such as Ollama (`http://localhost:11434/v1`).
  Z.ai's OCR-only models (e.g. `-receipt-model glm-ocr`) are also supported:
  they are served through the layout-parsing endpoint rather than
  `/chat/completions`, so the client calls that and extracts the receipt
  fields from the OCR text.
- **Anthropic** (`anthropic`) — uses Anthropic's Messages API (e.g.
  `-receipt-api-url https://api.anthropic.com -receipt-model
  claude-sonnet-4-5`): requests authenticate with `x-api-key` and call
  `/v1/messages`; a base URL ending in `/v1` also works.
- **Gateway Type 1** (`gwtype1`) — the AI Gateway chat API is used (base URL
  ending in `/api/v1`): the plain Bearer key authenticates, the receipt
  schema is sent along for structured output, and the gateway's pre-parsed
  `json` answer is preferred over the raw text.
- **Gateway Type 2** (`gwtype2`) — a workflow runs instead of a chat: the
  receipt is POSTed as a `files` part of a multipart form to
  `{base}/api/execute` alongside the `-receipt-api-workflow` name and
  optional `-receipt-api-args` JSON, and the receipt is recovered from the
  bare reply, a common envelope key (`result`, `json`, `text`, …), or JSON
  embedded in a text answer.
- **Gateway Type 3** (`gwtype3`) — a named workflow is called at
  `{base}/api/v1/workflows/{workflow}` with the receipt base64 in a JSON
  `files` list (the gateway refuses calls without one) and the optional
  `-receipt-api-args` JSON. The workflow returns expense-claim fields rather
  than a receipt: the stated total and VAT become the receipt total and tax,
  each line item's gross amount becomes an item, and the workflow's
  free-text description and exceptions are logged (it answers no merchant
  name).
