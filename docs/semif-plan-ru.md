# SemIf (Jev-подобный скоринг) в Gonka — план

**Статус:** черновик для DevNet (Proposal #82) и для PR в прокси.  
**Не деплоить на mainnet MiniMax / DeepSeek / GLM.**

## 1. Короткий вывод

SemIf можно поставить в Gonka, но **не как новую on-chain модель рядом с MiniMax**.

SemIf ([TheoLeeCJ/SemIf](https://github.com/TheoLeeCJ/SemIf), раньше OpenJev) — это **интерфейс решений в стиле Jev**, а не отдельные веса. Официальный код делает один `model.forward()`, читает логиты букв A–P и возвращает вероятности. **Нативных completion-токенов нет.**

Текущий Gonka — обратное: vLLM `serve` + `/v1/chat/completions`, большие MoE, PoC/валидация по **сгенерированному** тексту.

Правильный полигон — **Community DevNet из Proposal #82** ([discussion #1388](https://github.com/gonka-ai/gonka/discussions/1388)): 9–13 машин **16 GB / CUDA 13**, целевой класс моделей — **Qwen3-0.6B**. Это не готовый хост SemIf и не разрешение регистрировать модель в mainnet.

В этом репозитории уже есть продуктовый срез: `POST /v1/systemone` (выключен по умолчанию). Дальше нужен DevNet-vLLM с `logprobs`.

## 2. Что такое SemIf

Jev у TypeSafe — закрытый сервис: на вход неструктурированное состояние + критерий + варианты, на выход типизированные вероятности. SemIf воспроизводит **этот контракт** на открытых моделях. Закрытые веса Jev он не копирует.

Типичный запрос:

```json
{
  "id": "route-1",
  "state": "Клиент не может зайти в аккаунт после сброса пароля.",
  "question": "В какую очередь отправить обращение?",
  "options": [
    {"id": "access", "description": "Поддержка доступа к аккаунту."},
    {"id": "billing", "description": "Биллинг."}
  ]
}
```

Ответ: вероятности по вариантам, выбранный `id`, логиты букв. Softmax только по объявленным слотам — это **условная** оценка, не калиброванная уверенность.

Заявленное качество (BF16, таблица авторов SemIf):

| Система | Authored balanced accuracy | Согласие с TypeSafe subset |
|---|---:|---:|
| Qwen3-0.6B | 0.440 | 0.407 |
| MiniCPM5-2B | 0.686 | 0.637 |
| **Qwen3.5-4B** | **0.813** | **0.845** |
| Published Jev | — | **0.883** |

На RTX 3090, Qwen3.5-4B, 21 бинарный критерий: прямой readout **~1.0 с** и 0 токенов против **~5.3 с** и 111 токенов у компактного JSON. Если просто зарегистрировать 4B как chat-модель и просить JSON — получим медленный путь, ради которого SemIf и писали.

## 3. Совместимость с текущим vLLM / Gonka

Mainnet `models_all` сейчас: MiniMax-M2.7, DeepSeek-V4-Flash, Kimi-K2.6, GLM-5.2-FP8, GLM-5.3-Flash. VRAM **280–1120 GB**. Это не SemIf.

| Кусок | Лежит на текущем стеке? |
|---|---|
| Qwen3-0.6B как обычный chat | Да. DevNet #82 уже целится в этот класс. |
| Qwen3.5-4B как chat | Да, если образ **vLLM ≥ 0.17**. На 16 GB: `--language-model-only`, короткий `--max-model-len`. |
| MiniCPM5-2B | Скорее да на свежем vLLM. В чейне нет. |
| Нативный SemIf (`model.forward()`) | **Нет.** В официальном `pyproject` нет extra для vLLM. |
| Приближение SemIf через vLLM | Да: `max_tokens=1` + `logprobs` / `top_logprobs` / (в новых vLLM) `logprob_token_ids`. Softmax по subset logprobs математически равен softmax по subset сырых логитов. |
| mlnode + биллинг + PoC | Не рассчитаны на score-only / 0 токенов. Обёртка может вырезать `logprobs`. |

Официальный SemIf — HuggingFace Transformers или MLX. Форк fastjev добавил vLLM ScoringBackend и `POST /v1/systemone`. Мы взяли тот же контракт.

## 4. Что даёт Proposal #82

Это **External Test Lab & Community DevNet**, не заявка «поставить SemIf».

- 9–13 постоянно живых inference-машин, **16 GB**, CUDA 13
- Модельный профиль: лёгкие instruct, вроде `Qwen/Qwen3-0.6B`
- Burst-GPU на неделю в месяц — для больших моделей и совместимости
- Доступ на пилоте — **по заявке**, не permissionless

Именно сюда нести фазы 1–4. Mainnet — только после доказанного score-path и отдельного governance.

## 5. Что уже сделано в прокси

Выключено, пока не задан `SEMIF_BASE_URL`.

| Поверхность | Поведение |
|---|---|
| `GET /v1/systemone` | Публичная capability. `enabled: false`, пока скорер не сконфигурирован. |
| `POST /v1/systemone` | SemIf JSON → вероятности. Алиас: `POST /v1/decisions`. |
| `SEMIF_BASE_URL` | Отдельный vLLM (`max_tokens=1`, `logprobs=true`, `top_logprobs=20`). |
| `SEMIF_CHAT_FALLBACK` | Тот же chat-upstream. Если шлюз срезал logprobs — деградация до одной буквы. **На проде не включать.** |

Код: `internal/semif`, `internal/api/semif.go`, проба `scripts/semif_probe.py`.

Проверка без GPU:

```bash
go test ./internal/semif ./internal/api -count=1 -run 'SemIf|SystemOne|Extract|ClientScore'
curl -sS "$PROXY/v1/systemone"
# {"enabled":false,...}
```

## 6. Фазы

### Фаза 0 — срез в репозитории (сделано)

API, тесты, документ, проба. На прод не включали.

### Фаза 1 — chat на DevNet (Qwen3-0.6B)

Попросить у тест-лаба #82 MLNode с `Qwen/Qwen3-0.6B` (или эквивалент) по OpenAI chat.

```bash
vllm serve Qwen/Qwen3-0.6B \
  --dtype bfloat16 \
  --max-model-len 4096 \
  --gpu-memory-utilization 0.85 \
  --port 8000
```

**Проход:** `/v1/models` отдаёт id; короткий chat отвечает; токенизатор держит `A`…`P` как один токен (SemIf иначе отказывается).

**Провал:** старый образ, OOM на 16 GB, chat template сначала пишет think-блок.

### Фаза 2 — зонд logprobs

Тот же процесс, одно поле:

```bash
curl -sS http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"Qwen/Qwen3-0.6B",
    "messages":[
      {"role":"system","content":"Ответь только A или B."},
      {"role":"user","content":"A = да. B = нет. 2+2=4?"}
    ],
    "max_tokens":1,
    "temperature":0,
    "logprobs":true,
    "top_logprobs":16,
    "chat_template_kwargs":{"enable_thinking":false}
  }'
```

**Проход:** в `choices[0].logprobs.content[0].top_logprobs` есть `A` и `B` (или `" A"` / `" B"`).

**Провал:** 400 на `logprobs`, пустые logprobs, только сэмплированный токен. Тогда качество SemIf через этот vLLM не довезём — оставляем sidecar на Transformers.

Если mlnode вырезает logprobs — для эксперимента ставим **голый vLLM** рядом. Mainnet mlnode не трогаем до фазы 5.

### Фаза 3 — прокси на этот vLLM

```
SEMIF_BASE_URL=http://<devnet-vllm>:8000
SEMIF_MODEL=Qwen/Qwen3-0.6B
SEMIF_CHAT_FALLBACK=false
```

```bash
python3 scripts/semif_probe.py --base "$PROXY" --key "$API_KEY"
```

**Проход:**

- `GET /v1/systemone` → `enabled: true`, есть backend и model
- Фикстура access/billing: `chosen_id`, две вероятности в сумме 1, `readout` = native logprobs, `missing_slots` пустой
- `/v1/models` показывает SemIf id с `owned_by: semif`

Записать: латентность, VRAM. Shared-state SemIf (один префилл на 21 критерий) в первом срезе нет.

### Фаза 4 — Qwen3.5-4B (целевое качество)

Нужен **vLLM ≥ 0.17**. На 16 GB:

```bash
vllm serve Qwen/Qwen3.5-4B \
  --language-model-only \
  --max-model-len 8192 \
  --reasoning-parser qwen3 \
  --default-chat-template-kwargs '{"enable_thinking":false}' \
  --port 8000
```

Если образ #82 старше 0.17 — burst-GPU или ждём новый образ. MiniCPM5-2B — необязательная ступень.

**Проход:** та же фикстура + 20 строк из `benchmarks/` SemIf, слоты по-прежнему однотокенные.

### Фаза 5 — первоклассная модель Gonka (не этот PR)

Только после 1–4:

1. On-chain строка: HF repo + commit + `v_ram` ≈ 16 + `--max-model-len` + при необходимости `--language-model-only`.
2. Хосты отдают **score** (`logprobs` или нативный score runner), не только generate.
3. PoC/валидация умеют путь на 0–1 токен. Сейчас ждут сгенерированный текст.
4. Биллинг за prompt/decision, не за completion.
5. Governance: это новый тип инференса, не «ещё один id рядом с MiniMax».

До этого SemIf в Gonka — **sidecar** (`SEMIF_BASE_URL`) плюс этот API.

## 7. Решение после фазы 2

| Logprobs на DevNet vLLM | Дальше |
|---|---|
| Да, буквы A–P на месте | Включаем `/v1/systemone` на этот URL; меряем 4B. |
| Chat живой, logprobs срезаны | API оставляем; на той же GPU — официальный SemIf / Transformers sidecar. |
| Образ не тянет 0.6B | Burst-GPU #82 или новый образ. Стоп. |

## 8. Чего не делать

- Не регистрировать SemIf как mainnet chat-модель и не гонять `/v1/chat/completions` за JSON yes/no.
- Не включать `SEMIF_CHAT_FALLBACK` на проде — это жжёт токены frontier MoE на задачу 4B.
- Не читать финансирование #82 как разрешение листить новую mainnet-модель.
- Не менять gateway auto-settle / PoC ради этого эксперимента.

## 9. Как включить (только DevNet / локально)

```
SEMIF_BASE_URL=http://<devnet-vllm>:8000
SEMIF_API_KEY=
SEMIF_MODEL=Qwen/Qwen3-0.6B
SEMIF_TIMEOUT_SEC=45
SEMIF_CHAT_FALLBACK=false
```

Проба: `python3 scripts/semif_probe.py --base "$PROXY" --key "$API_KEY"`.

Пока скорер выключен, скрипт завершается с кодом 2 — так и задумано.

## 10. Открытые вопросы к тест-лабу #82

1. Есть ли уже живой MLNode с Qwen3-0.6B и какой у него vLLM?
2. Пробрасывает ли mlnode `logprobs`, или нужен голый `vllm serve`?
3. Можно ли получить доступ по заявке на 1–2 машины для фаз 1–3?
4. Образ ≥ 0.17 для фазы 4, или только burst?

Пока нет ответов на 1–2, в mainnet ничего не катим.
