# SemIf on Gonka — DevNet test plan (draft)

Russian plan: [semif-plan-ru.md](semif-plan-ru.md).

Proposal 82 ([discussion #1388](https://github.com/gonka-ai/gonka/discussions/1388)) is the External Test Lab & Community DevNet: 9–13 always-on **16 GB / CUDA 13** inference boxes, intended for **Qwen3-0.6B-class** models. That is the right sandbox. It is **not** a ready SemIf host and **not** a mainnet model registration.

SemIf ([TheoLeeCJ/SemIf](https://github.com/TheoLeeCJ/SemIf), formerly OpenJev) is a **Jev-style interface**, not a new architecture. Official code does `model.forward()` and softmaxes A–P option logits. **0 generated tokens** in the native path.

This repo now has the product-side slice:

| Surface | Behavior |
|---|---|
| `GET /v1/systemone` | Public capability. `enabled: false` until a scorer is configured. |
| `POST /v1/systemone` | API-key. SemIf JSON in, typed probabilities out. Alias: `POST /v1/decisions`. |
| `SEMIF_BASE_URL` | Dedicated vLLM (`max_tokens=1`, `logprobs=true`, `top_logprobs=20`). |
| `SEMIF_CHAT_FALLBACK` | Reuses current chat upstream. Degraded if the gateway strips logprobs. **Leave off in production.** |

Do not turn this on against mainnet MiniMax / DeepSeek / GLM.

## Phase 0 — already in this repo

```bash
go test ./internal/semif ./internal/api -count=1 -run 'SemIf|SystemOne|Extract|Softmax|ClientScore|ListModelsIncludesSemIf'
```

Probe a running proxy (disabled by default):

```bash
curl -sS "$PROXY/v1/systemone"
# {"enabled":false,...}

python3 scripts/semif_probe.py --base "$PROXY" --key "$API_KEY"
# exits 2 while the scorer is off
```

## Phase 1 — DevNet chat (Qwen3-0.6B)

Ask the #82 test lab for a DevNet MLNode that already serves `Qwen/Qwen3-0.6B` (or equivalent) over OpenAI chat.

On that node (or a burst box with the same CUDA 13 image):

```bash
vllm serve Qwen/Qwen3-0.6B \
  --dtype bfloat16 \
  --max-model-len 4096 \
  --gpu-memory-utilization 0.85 \
  --port 8000
```

Pass:

- `GET /v1/models` lists the id
- `POST /v1/chat/completions` with a 20-token prompt returns a letter or short string
- Tokenizer: `A`…`P` are single tokens (SemIf refuses multi-token slots)

Fail: image too old, OOM at 16 GB, chat template emits a think block before the letter.

## Phase 2 — logprobs probe

Same process, one extra field:

```bash
curl -sS http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"Qwen/Qwen3-0.6B",
    "messages":[
      {"role":"system","content":"Reply with only A or B."},
      {"role":"user","content":"A = yes. B = no. Is 2+2=4?"}
    ],
    "max_tokens":1,
    "temperature":0,
    "logprobs":true,
    "top_logprobs":16,
    "chat_template_kwargs":{"enable_thinking":false}
  }'
```

Pass: `choices[0].logprobs.content[0].top_logprobs` contains `A` and `B` (or `" A"` / `" B"`).

Fail: 400 on `logprobs`, empty `logprobs`, or only the sampled token. That means we cannot deliver SemIf quality through this vLLM; stay on a sidecar `transformers` scorer.

If the Gonka mlnode wrapper strips logprobs, run a **bare vLLM** next to it for this experiment. Do not change mainnet mlnode until Phase 5.

## Phase 3 — proxy `/v1/systemone` against that vLLM

On a proxy (local or a Labs box that can reach the DevNet vLLM):

```
SEMIF_BASE_URL=http://<devnet-vllm>:8000
SEMIF_MODEL=Qwen/Qwen3-0.6B
SEMIF_CHAT_FALLBACK=false
```

Then:

```bash
python3 scripts/semif_probe.py --base "$PROXY" --key "$API_KEY"
```

Pass:

- `GET /v1/systemone` → `enabled: true`, `backend` set, model id present
- `POST /v1/systemone` on the locked-out / access-vs-billing fixture returns `chosen_id`, two probabilities that sum to 1, `readout` = native logprobs, `missing_slots` empty
- `/v1/models` lists the SemIf id with `owned_by: semif`
- Usage log shows prompt tokens + 1 completion (0-token native SemIf is still approximated as 1 token here)

Record: latency, VRAM, whether prefix-cache helps a 21-criterion shared state (optional; official SemIf shared mode is not in this first slice).

## Phase 4 — Qwen3.5-4B (quality target)

Official SemIf quality table uses **Qwen3.5-4B** (0.813 authored balanced accuracy vs published Jev 0.883 on the aligned subset).

Requires **vLLM ≥ 0.17** (Qwen3.5 architecture). On 16 GB:

```bash
vllm serve Qwen/Qwen3.5-4B \
  --language-model-only \
  --max-model-len 8192 \
  --reasoning-parser qwen3 \
  --default-chat-template-kwargs '{"enable_thinking":false}' \
  --port 8000
```

If the #82 image is older than 0.17, use the burst-GPU week or wait for an image bump. MiniCPM5-2B is optional middle rung.

Pass: same `/v1/systemone` fixture + a 20-row subset from SemIf `benchmarks/` with letter slots still single-token.

## Phase 5 — first-class Gonka (not this PR)

Only after Phases 1–4:

1. On-chain model row: HF repo + commit + `v_ram` ≈ 16 + `--max-model-len` + (if needed) `--language-model-only`.
2. Hosts must expose **score** (`logprobs` or a native score runner), not only generate.
3. PoC / validation need a **0-or-1-token** path. Today they assume generated text.
4. Billing is prompt/decision, not completion tokens.
5. Governance: this is a new inference type, not a drop-in next to MiniMax.

Until then, SemIf on Gonka is a **sidecar** (`SEMIF_BASE_URL`) plus this proxy API.

## What not to do

- Do not register SemIf as a mainnet chat model and call `/v1/chat/completions` for JSON yes/no. That is the slow path SemIf exists to avoid.
- Do not enable `SEMIF_CHAT_FALLBACK` on production. It burns frontier MoE tokens for a 4B job.
- Do not treat Proposal 82 funding as permission to list a new mainnet model.

## Decision after Phase 2

| Logprobs on DevNet vLLM | Next step |
|---|---|
| Yes, A–P present | Ship `/v1/systemone` against that URL; measure 4B next. |
| Chat works, logprobs stripped | Keep this API; run official SemIf / a `transformers` sidecar on the same GPU. |
| Image cannot serve 0.6B | Unblock with #82 burst GPU or an image bump; stop. |
