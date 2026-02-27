# Gonka Proxy

A lightweight, Docker-based proxy that exposes the [Gonka AI](https://gonka.ai) decentralised inference network as a standard **OpenAI-compatible API**. Point any app that speaks the OpenAI protocol at this proxy and it just works - no SDK changes required.

(with Gonka network's Transfer Agent feature (v0.2.9+) applied!)

## Features

- **OpenAI-compatible REST API** - `/v1/models`, `/v1/chat/completions` (streaming and non-streaming)
- **Multi-wallet support** - configure multiple wallets and the proxy round-robins requests across them, spreading rate limits and increasing throughput
- **Automatic endpoint discovery** - fetches the active participant list from the Gonka network and routes requests to healthy, whitelisted nodes
- **Transparent request signing** - ECDSA / secp256k1 signatures are added to every upstream request; your private keys never leave the proxy
- **Tool / function-call simulation** - rewrites tool-call requests into plain prompts and converts the model's JSON back into proper `tool_calls` responses, so tool calling works even though upstream nodes don't support it natively
- **Privacy sanitization** - strips sensitive data (names, emails, API keys, credentials) from messages before forwarding and restores them in the response; the upstream LLM never sees your real data ([details](docs/sanitization.md))
- **Zero host dependencies** - runs entirely in Docker; no local Go installation needed
- **Built-in web chat UI** at `http://localhost:8080`

## Quick start

```bash
# 1. Clone
git clone https://github.com/gonkalabs/gonka-proxy-go.git
cd gonka-proxy-go

# 2. Configure
cp .env.example .env
# Edit .env with your credentials (see "Obtaining a key" below)

# 3. Run
make run          # or: docker compose up -d

# 4. Try it
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

The web UI is available at **http://localhost:8080**.

## Obtaining a key

You need a Gonka account (private key + address) to sign inference requests.

### 1. Download the CLI

Download the latest `inferenced` binary for your system from the [Gonka docs](https://gonka.ai/developer/quickstart/).

On macOS, grant execution permission and allow it in System Settings > Privacy & Security:

```bash
chmod +x inferenced
```

### 2. Create an account

Pick any genesis node as the `NODE_URL`:

```
http://node1.gonka.ai:8000
http://node2.gonka.ai:8000
http://node3.gonka.ai:8000
```

Then run:

```bash
export ACCOUNT_NAME=my-gonka-account
export NODE_URL=http://node1.gonka.ai:8000

./inferenced create-client $ACCOUNT_NAME --node-address $NODE_URL
```

This generates a keypair, saves it locally, and **registers the account on-chain**. You'll see output like:

```
- address: gonka1abc123...
  name: my-gonka-account
  pubkey: '{"@type":"...","key":"..."}'
  type: local
```

**Save the mnemonic phrase securely** - it's the only way to recover the account.

### 3. Export the private key

```bash
./inferenced keys export $ACCOUNT_NAME --unarmored-hex --unsafe
```

This prints a 64-character hex string. Add it to your `.env`:

```env
GONKA_PRIVATE_KEY=<hex key from above>
GONKA_ADDRESS=gonka1abc123...
```

### 4. Fund the account

You need GNK tokens to pay for inference. Transfer tokens to your `GONKA_ADDRESS` via the [Gonka.gg Faucet](https://gonka.gg/faucet) (0.01GNK per 24H) 

...or fund your wallet from another funded account:

```bash
./inferenced tx bank send <funded-account-name> <your-address> 5000000ngonka \
  --node $NODE_URL/chain-rpc/ \
  --chain-id gonka-mainnet \
  --fees 500ngonka \
  --keyring-backend os -y
```

## Configuration

All configuration is via environment variables (loaded from `.env`):

| Variable | Required | Default | Description |
|---|---|---|---|
| `GONKA_WALLETS` | No* | - | Comma-separated `privkey:address` pairs for multiple wallets (see below) |
| `GONKA_PRIVATE_KEY` | No* | - | Hex-encoded secp256k1 private key (single wallet) |
| `GONKA_ADDRESS` | No | Derived from key | Your bech32 account address (single wallet) |
| `GONKA_SOURCE_URL` | No | `http://node2.gonka.ai:8000` | Genesis node for endpoint discovery |
| `SIMULATE_TOOL_CALLS` | No | `false` | Enable tool/function-call simulation |
| `PORT` | No | `8080` | HTTP server port |

\* Either `GONKA_WALLETS` or `GONKA_PRIVATE_KEY` must be set. If both are set, `GONKA_WALLETS` takes priority.

### Multiple wallets

You can configure multiple wallets to spread requests across them in round-robin order. This helps avoid per-wallet rate limits and increases overall throughput.

Set `GONKA_WALLETS` as a comma-separated list of `private_key:address` pairs:

```env
GONKA_WALLETS=privkey1:gonka1addr1,privkey2:gonka1addr2,privkey3:gonka1addr3
```

The address part is optional - if omitted, just list the private keys:

```env
GONKA_WALLETS=privkey1,privkey2,privkey3
```

Each incoming request cycles to the next wallet. The proxy logs which wallet was used for every upstream request so you can verify the distribution.

For a single wallet, you can use either format:

```env
# Option A: multi-wallet format (one entry)
GONKA_WALLETS=your_private_key:gonka1youraddress

# Option B: legacy format (backward compatible)
GONKA_PRIVATE_KEY=your_private_key
GONKA_ADDRESS=gonka1youraddress
```

## NOTE about TransferAgent (Whitelisted inference nodes)

The Gonka network's Transfer Agent feature (v0.2.9+) restricts which nodes can process proxied inference requests. The proxy automatically discovers active participants and filters them to this whitelist:

```
gonka1y2a9p56kv044327uycmqdexl7zs82fs5ryv5le
gonka1dkl4mah5erqggvhqkpc8j3qs5tyuetgdy552cp
gonka1kx9mca3xm8u8ypzfuhmxey66u0ufxhs7nm6wc5
gonka1ddswmmmn38esxegjf6qw36mt4aqyw6etvysy5x
gonka10fynmy2npvdvew0vj2288gz8ljfvmjs35lat8n
gonka1v8gk5z7gcv72447yfcd2y8g78qk05yc4f3nk4w
gonka1gndhek2h2y5849wf6tmw6gnw9qn4vysgljed0u
```

Requests sent to non-whitelisted nodes will be rejected with `Transfer Agent not allowed`. The proxy handles this automatically - you don't need to pick nodes manually. If the whitelist changes in a future Gonka update, edit the `allowedTransferAgents` map in `internal/upstream/client.go`.

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
    model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
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
  model: "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(response.choices[0].message.content);
```

### curl

```bash
# Non-streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8","messages":[{"role":"user","content":"Hello!"}]}'

# Streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8","messages":[{"role":"user","content":"Hello!"}],"stream":true}'
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

### How it works

1. Your message arrives at the proxy
2. Classifiers scan the text and replace sensitive values with stable placeholders
3. The redacted message is forwarded to the upstream LLM
4. When the response comes back, placeholders are swapped back to the originals
5. The web UI shows you a side-by-side diff of what you typed vs. what was sent

### Setup

Sanitization runs as an optional Docker profile. Start it alongside the proxy:

```bash
docker compose --profile sanitize up -d
```

This starts three extra containers:
- `sanitize-ner` - NER model (Natasha for Russian, spaCy for English), handles names and organisations
- `ollama` - runs the local LLM classifier
- `ollama-init` - pulls the model on first run, then exits

The first start downloads the LLM model (~2.6 GB). Subsequent starts are instant since the model is cached in a Docker volume.

### Configuration

Set these in your `.env`:

```env
# Enable sanitization
SANITIZE=true

# NER sidecar - catches names, orgs, locations
SANITIZE_NER=true
SANITIZE_NER_URL=http://sanitize-ner:8001

# Local LLM - catches API keys, passwords, and other credentials
SANITIZE_LLM=true
SANITIZE_LLM_URL=http://ollama:11434
SANITIZE_LLM_MODEL=qwen3:4b-instruct-2507-q4_K_M
```

### Web UI

The built-in chat UI at `http://localhost:8080` shows exactly what happened to each message:

- A yellow shield badge shows how many values were redacted
- Click it to expand a side-by-side view: your original message on the left, what was sent to the LLM on the right
- Sensitive values are highlighted in the original; placeholders are shown in the sent version
- A green badge under the assistant response shows which placeholders were restored

### Hardware requirements

The LLM classifier runs on CPU. Inference takes roughly 5-20 seconds per message depending on message length and hardware. The NER sidecar is much faster (under 100ms). Both run in parallel so total latency is dominated by whichever takes longer.

If latency is a concern, you can disable the LLM layer and rely only on NER:

```env
SANITIZE_NER=true
SANITIZE_LLM=false
```

## Tool / function calling

Not all Gonka inference nodes currently support the OpenAI tool-calling protocol natively (`--enable-auto-tool-choice` is not set for them during node deployment!). 

The proxy can simulate it.

### Enable simulation

Set `SIMULATE_TOOL_CALLS=true` in your `.env` and restart:

```bash
make stop && make run
```

### How it works

1. Your app sends a standard OpenAI request with `tools` and `tool_choice`
2. The proxy strips those fields (which upstream would reject) and injects a system prompt that describes the available tools and asks the model to respond with structured JSON
3. The model returns a JSON array of tool calls
4. The proxy parses the JSON and converts it back into the standard OpenAI `tool_calls` response format (`finish_reason: "tool_calls"`, `content: null`, structured `tool_calls` array)
5. Your app sees a perfectly standard response and handles the tool-call round-trip as usual

### Example

```python
response = client.chat.completions.create(
    model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
    messages=[{"role": "user", "content": "What's the weather in Berlin?"}],
    tools=[{
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Get the current weather for a location",
            "parameters": {
                "type": "object",
                "properties": {
                    "location": {"type": "string"}
                },
                "required": ["location"]
            }
        }
    }],
)

# Works exactly like OpenAI:
tool_call = response.choices[0].message.tool_calls[0]
print(tool_call.function.name)       # "get_weather"
print(tool_call.function.arguments)  # '{"location": "Berlin"}'
```

The full round-trip (ask -> tool call -> tool result -> final answer) works exactly as it does with OpenAI.

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check (`{"status":"ok"}`) |
| `GET` | `/v1/models` | List available models |
| `POST` | `/v1/chat/completions` | Chat completions (streaming & non-streaming) |
| `GET` | `/` | Web chat UI |

## Make commands

```bash
make build   # Build Docker image
make run     # Start in background
make stop    # Stop
make logs    # Tail logs
make dev     # Build + run in foreground (for development)
make clean   # Stop + remove images and volumes
```

## Project structure

```
opengnk/
  cmd/proxy/main.go                       # entry point, server setup, graceful shutdown
  internal/
    api/handler.go                        # HTTP handlers for all endpoints
    config/config.go                      # environment variable loading
    signer/signer.go                      # ECDSA secp256k1 request signing
    toolsim/toolsim.go                    # tool-call simulation
    upstream/client.go                    # upstream HTTP client, endpoint discovery
    wallet/pool.go                        # multi-wallet pool with round-robin routing
    sanitize/
      sanitize.go                         # redaction and restoration core
      classifier.go                       # Classifier interface
      ner/ner.go                          # NER sidecar client (Natasha + spaCy)
      llmclassifier/llmclassifier.go      # local LLM classifier (Ollama)
  web/index.html                          # chat UI with redaction diff panel
  sanitize-ner/                           # Python NER sidecar (Docker)
  Dockerfile
  docker-compose.yml
  Makefile
```

## License

MIT
