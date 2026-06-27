# OpenGNK — Gonka AI Proxy

A lightweight, Docker-based proxy that exposes the [Gonka AI](https://gonka.ai) decentralised inference network as a standard **OpenAI-compatible API**. Point any app that speaks the OpenAI protocol at this proxy and it just works — no SDK changes required.

## How it works

The Gonka network uses **DevShards** — on-chain escrows that fund inference against AI models. The proxy connects to a devshard gateway (either an external one or a self-hosted one) and routes your OpenAI-format requests through it. The gateway manages escrow creation, capacity routing, and proof-of-compute settlement internally.

**Direct escrow operations** (creating and managing your own escrows) require a **whitelisted wallet**. If your wallet is not whitelisted, you can connect to an external devshard operator instead — the proxy supports both modes.

## Quick start (devshard mode)

### Option A — Connect to an external gateway (simplest)

```bash
# 1. Clone
git clone https://github.com/gonkalabs/opengnk.git
cd opengnk

# 2. Configure
cp .env.example .env
```

Edit `.env`:
```env
GONKA_UPSTREAM_MODE=devshard
GATEWAY_URL=https://your-gateway.example.com/v1
GATEWAY_API_KEY=sk-your-api-key
```

```bash
# 3. Run
make run          # or: docker compose up -d
```

### Option B — Self-hosted gateway (fully self-contained)

Requires a funded Gonka wallet for escrow creation.

```env
GONKA_UPSTREAM_MODE=devshard-embedded
GATEWAY_API_KEY=sk-your-api-key
DEVSHARD_PRIVATE_KEY=your_hex_private_key
DEVSHARD_TARGETS=MiniMaxAI/MiniMax-M2.7=6
```

```bash
docker compose -f docker-compose.yml -f docker-compose.devshard.yml up -d
```

This launches a devshard-gateway container alongside the proxy. The gateway creates and manages its own escrows on-chain.

### Try it

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MiniMaxAI/MiniMax-M2.7",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

The web UI is available at **http://localhost:8080**.

## Upstream modes

The proxy supports three modes, selected via `GONKA_UPSTREAM_MODE` in your `.env`:

| Mode | Description | Use when |
|---|---|---|
| **`devshard`** | Bearer-token client to an external devshard gateway | You have a gateway API key (simplest setup) |
| **`devshard-embedded`** | Bundled devshard-gateway container with own escrows | You have a whitelisted wallet and want full autonomy |
| **`node`** | Legacy ECDSA-signed requests directly to Gonka nodes | ⚠️ **Deprecated.** The old direct-node flow is being phased out. Use devshard modes instead. |

### `devshard` — connect to an external gateway

No wallets, no signing, no escrow management. Just an API key against a gateway that handles everything.

```env
GONKA_UPSTREAM_MODE=devshard
GATEWAY_URL=http://your-devshard-gateway:8080/v1
GATEWAY_API_KEY=sk-your-gateway-key
```

### `devshard-embedded` — self-hosted gateway

Bundles a devshard-gateway container. Requires a Gonka private key for escrow creation and a funded wallet.

```env
GONKA_UPSTREAM_MODE=devshard-embedded
GATEWAY_API_KEY=sk-your-gateway-key
DEVSHARD_PRIVATE_KEY=your_hex_key
DEVSHARD_TARGETS=MiniMaxAI/MiniMax-M2.7=6
DEVSHARD_ESCROW_AMOUNT_NGONKA=1500000000
```

```bash
docker compose -f docker-compose.yml -f docker-compose.devshard.yml up -d
```

Config options for embedded mode:

| Variable | Default | Description |
|---|---|---|
| `DEVSHARD_IMAGE` | `ghcr.io/gonka-ai/devshard-gateway:mainnet-v0.2.13-latest` | Gateway container image |
| `DEVSHARD_PRIVATE_KEY` | — | Hex private key for escrow creation (required) |
| `DEVSHARD_TARGETS` | `MiniMaxAI/MiniMax-M2.7=6` | Comma-separated `model_id=count` pairs |
| `DEVSHARD_ESCROW_AMOUNT_NGONKA` | `1500000000` (1.5 GNK) | Escrow deposit per devshard |
| `DEVSHARD_CHAIN_REST` | `http://204.12.168.157:1317` | Chain REST API URL |
| `DEVSHARD_PUBLIC_API` | `https://node3.gonka.ai` | Public API for epoch/PoC phase checks |

### `node` — legacy ECDSA mode (deprecated)

> ⚠️ The direct ECDSA node-signing flow is being phased out in favor of devshards. New deployments should use `devshard` or `devshard-embedded` mode.

Signs each request with ECDSA and sends it to Gonka network nodes. Requires a Gonka wallet.

```env
GONKA_UPSTREAM_MODE=node
GONKA_PRIVATE_KEY=your_hex_key
GONKA_SOURCE_URL=http://node1.gonka.ai:8000
```

See the [legacy setup guide](#legacy-node-mode-setup) below for wallet creation instructions.

## Features

- **OpenAI-compatible REST API** — `/v1/models`, `/v1/chat/completions` (streaming and non-streaming)
- **DevShard support** — connect to a devshard gateway via API key or run your own embedded gateway
- **Tool / function-call simulation** — rewrites tool-call requests into plain prompts and converts responses back, so tool calling works even when upstream doesn't support it natively
- **Native tool calling** — forward `tool_calls` to nodes that support it
- **Privacy sanitization** — strips sensitive data (names, emails, API keys, credentials) from messages before forwarding and restores them in the response; the upstream LLM never sees your real data ([details](docs/sanitization.md))
- **Zero host dependencies** — runs entirely in Docker; no local Go installation needed
- **Built-in web chat UI** at `http://localhost:8080`

## Using as an OpenAI drop-in

The proxy exposes the same API as OpenAI. Any library or application that supports a custom `base_url` will work.

### Python (openai SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed",  # any string works
)

response = client.chat.completions.create(
    model="MiniMaxAI/MiniMax-M2.7",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)
```

### TypeScript / Node.js

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "not-needed",
});

const response = await client.chat.completions.create({
  model: "MiniMaxAI/MiniMax-M2.7",
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(response.choices[0].message.content);
```

### curl

```bash
# Non-streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"MiniMaxAI/MiniMax-M2.7","messages":[{"role":"user","content":"Hello!"}]}'

# Streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"MiniMaxAI/MiniMax-M2.7","messages":[{"role":"user","content":"Hello!"}],"stream":true}'
```

## Privacy sanitization

The proxy can automatically strip sensitive data from messages before they leave your machine and restore the original values in the response. The upstream LLM only ever sees placeholder tokens like `«TOKEN_000001»`, never your real data.

### What gets redacted

- API keys and tokens (anything starting with `sk-`, `pk-`, `ghp_`, `Bearer`, etc.)
- Email addresses
- Phone numbers
- Full person names (English and Russian)
- Credit card numbers and IBANs
- Anything else a local LLM classifier flags as sensitive

### Setup

Sanitization runs as an optional Docker profile:

```bash
docker compose --profile sanitize up -d
```

Configure in `.env`:

```env
SANITIZE=true
SANITIZE_NER=true
SANITIZE_LLM=true
```

See [docs/sanitization.md](docs/sanitization.md) for full details.

## Tool / function calling

### Native tool calls (recommended when supported)

```env
NATIVE_TOOL_CALLS=true
SIMULATE_TOOL_CALLS=false
```

### Simulated tool calls (fallback)

```env
SIMULATE_TOOL_CALLS=true
```

The proxy strips `tools`/`tool_choice`, injects a system prompt instructing the model to emit JSON, and converts the response back into proper `tool_calls` format. Your app sees a standard OpenAI response.

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check (`{"status":"ok"}`) |
| `GET` | `/v1/models` | List available models |
| `POST` | `/v1/chat/completions` | Chat completions (streaming & non-streaming) |
| `GET` | `/` | Web chat UI |
| `GET` | `/quality/stats` | Request quality metrics |

## Make commands

```bash
make build   # Build Docker image
make run     # Start in background
make stop    # Stop
make logs    # Tail logs
make dev     # Build + run in foreground (for development)
make clean   # Stop + remove images and volumes
```

## Legacy node-mode setup

> ⚠️ This section is for the deprecated ECDSA `node` mode only. New users should use `devshard` or `devshard-embedded` mode.

### 1. Download the CLI

Download the latest `inferenced` binary from the [Gonka docs](https://gonka.ai/developer/quickstart/).

```bash
chmod +x inferenced
```

### 2. Create an account

```bash
export ACCOUNT_NAME=my-gonka-account
export NODE_URL=http://node1.gonka.ai:8000

./inferenced create-client $ACCOUNT_NAME --node-address $NODE_URL
```

### 3. Export the private key

```bash
./inferenced keys export $ACCOUNT_NAME --unarmored-hex --unsafe
```

Add to `.env`:

```env
GONKA_UPSTREAM_MODE=node
GONKA_PRIVATE_KEY=<hex key from above>
GONKA_ADDRESS=gonka1abc123...
```

### 4. Fund the account

Transfer GNK tokens to your address via the [Gonka.gg Faucet](https://gonka.gg/faucet) or from another funded account.

### Multi-wallet support (node mode only)

Configure multiple wallets for round-robin load distribution:

```env
GONKA_WALLETS=privkey1:gonka1addr1,privkey2:gonka1addr2,privkey3
```

## Configuration reference

| Variable | Required | Default | Description |
|---|---|---|---|
| `GONKA_UPSTREAM_MODE` | No | `node` | `node`, `devshard`, or `devshard-embedded` |
| `GATEWAY_URL` | devshard mode | — | External devshard gateway URL |
| `GATEWAY_API_KEY` | devshard mode | — | Gateway API key for Bearer auth |
| `DEVSHARD_PRIVATE_KEY` | embedded mode | — | Hex key for escrow creation |
| `DEVSHARD_TARGETS` | No | `MiniMaxAI/MiniMax-M2.7=6` | Target models and escrow counts |
| `DEVSHARD_ESCROW_AMOUNT_NGONKA` | No | `1500000000` | Escrow deposit (1.5 GNK) |
| `SIMULATE_TOOL_CALLS` | No | `false` | Enable tool-call simulation |
| `NATIVE_TOOL_CALLS` | No | `false` | Forward tool calls natively |
| `SANITIZE` | No | `false` | Enable privacy sanitization |
| `PORT` | No | `8080` | HTTP server port |

## Project structure

```
opengnk/
  cmd/proxy/main.go                       # entry point, mode switch, server setup
  internal/
    api/handler.go                        # HTTP handlers (OpenAI-compatible)
    config/config.go                      # env loading + mode config
    upstream/
      iface.go                            # Upstream interface
      client.go                           # node-mode ECDSA client (legacy)
      devshard.go                         # devshard gateway client (Bearer auth)
    signer/signer.go                      # ECDSA secp256k1 signing (legacy)
    wallet/pool.go                        # multi-wallet pool (legacy, node mode)
    toolsim/toolsim.go                    # tool-call simulation
    sanitize/                             # privacy redaction pipeline
  web/index.html                          # chat UI
  docker-compose.yml                      # base compose
  docker-compose.devshard.yml            # embedded devshard overlay
  Dockerfile
  Makefile
```

## License

MIT
