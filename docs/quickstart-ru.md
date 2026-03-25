# Подключение через openGNK (self-hosted прокси)

openGNK — это open-source прокси, который подключается напрямую к децентрализованной сети [Gonka AI](https://gonka.ai) и предоставляет стандартный **OpenAI-совместимый API**. Работает на вашей машине, без сторонних серверов.

### Базовый URL

```
http://localhost:8080/v1
```

### Отличия от proxy.gonka.gg

| | proxy.gonka.gg | openGNK (self-hosted) |
|---|---|---|
| Регистрация | Нужна, даёт `sk-...` ключ | Не нужна — вы создаёте кошелёк сами |
| Где работает | Облако Gonka | Ваша машина (Docker) |
| API-ключ | `sk-...` | Любая строка (авторизация через подпись кошелька) |
| Tool calling | Нативный / эмуляция | Нативный (`NATIVE_TOOL_CALLS=true`) или эмуляция (`SIMULATE_TOOL_CALLS=true`) |
| Приватность | Трафик через прокси Gonka | Запросы подписываются локально, ключи не покидают машину |

---

## Установка

### 1. Скачайте CLI

Скачайте бинарник `inferenced` для вашей системы из [документации Gonka](https://gonka.ai/developer/quickstart/).

На macOS разрешите запуск:

```bash
chmod +x inferenced
```

### 2. Создайте кошелёк

```bash
export ACCOUNT_NAME=my-gonka-account
export NODE_URL=http://node1.gonka.ai:8000

./inferenced create-client $ACCOUNT_NAME --node-address $NODE_URL
```

Сохраните мнемоническую фразу — это единственный способ восстановить аккаунт.

### 3. Экспортируйте приватный ключ

```bash
./inferenced keys export $ACCOUNT_NAME --unarmored-hex --unsafe
```

Запишите 64-символьную hex-строку и адрес `gonka1...` из шага 2.

### 4. Пополните кошелёк

Перейдите на [gonka.gg/faucet](https://gonka.gg/faucet) и запросите тестовые токены на ваш `gonka1...` адрес (0.01 GNK раз в 24 часа).

### 5. Запустите прокси

```bash
git clone https://github.com/gonkalabs/opengnk.git
cd opengnk

cp .env.example .env
```

Отредактируйте `.env`:

```env
GONKA_PRIVATE_KEY=<hex-ключ из шага 3>
GONKA_ADDRESS=gonka1<ваш адрес из шага 2>
GONKA_SOURCE_URL=http://node1.gonka.ai:8000

# Tool calling: выберите один из двух режимов
# Режим 1 — нативный (если ноды поддерживают tools):
NATIVE_TOOL_CALLS=true
SIMULATE_TOOL_CALLS=false
# Режим 2 — эмуляция (если ноды не поддерживают tools):
# NATIVE_TOOL_CALLS=false
# SIMULATE_TOOL_CALLS=true

SANITIZE=false
```

Запустите:

```bash
make run          # или: docker compose up -d
```

Прокси доступен на **http://localhost:8080** (включая веб-чат).

---

## Установка Python-клиента

```bash
pip install openai
```

---

## Пример 1 — Простой чат (без tool calling)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed",  # любая строка, авторизация через кошелёк
)

# Обычный запрос
response = client.chat.completions.create(
    model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
    messages=[
        {"role": "system", "content": "Ты полезный ассистент. Отвечай на русском."},
        {"role": "user", "content": "Объясни в трёх предложениях, что такое децентрализованный инференс."},
    ],
)

print(response.choices[0].message.content)
print(f"\nТокены: {response.usage.total_tokens}")


# Стриминг
print("\n--- Стриминг ---\n")

stream = client.chat.completions.create(
    model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
    messages=[
        {"role": "user", "content": "Напиши хайку про нейросети."},
    ],
    stream=True,
)

for chunk in stream:
    delta = chunk.choices[0].delta
    if delta.content:
        print(delta.content, end="", flush=True)
print()
```

---

## Пример 2 — Tool calling (вызов функций)

openGNK поддерживает два режима tool calling:

- **Нативный** (`NATIVE_TOOL_CALLS=true`) — `tools` и `tool_calls` передаются на ноду как есть. Прокси автоматически нормализует формат `content` (массивы → строки), чтобы ноды Gonka могли его обработать.
- **Эмуляция** (`SIMULATE_TOOL_CALLS=true`) — прокси убирает `tools` из запроса, добавляет системный промпт с описанием функций, а затем парсит JSON-ответ модели обратно в стандартный формат `tool_calls`.

Оба режима полностью прозрачны для клиентского кода — примеры ниже работают одинаково в обоих случаях.

```python
import json
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed",
)

# Описание инструментов
tools = [
    {
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Получить текущую погоду в городе",
            "parameters": {
                "type": "object",
                "properties": {
                    "city": {
                        "type": "string",
                        "description": "Название города, например: Москва",
                    }
                },
                "required": ["city"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "convert_currency",
            "description": "Конвертировать валюту",
            "parameters": {
                "type": "object",
                "properties": {
                    "amount": {"type": "number", "description": "Сумма"},
                    "from_currency": {"type": "string", "description": "Исходная валюта (USD, EUR, RUB)"},
                    "to_currency": {"type": "string", "description": "Целевая валюта (USD, EUR, RUB)"},
                },
                "required": ["amount", "from_currency", "to_currency"],
            },
        },
    },
]


# Заглушки — в реальном приложении здесь будут настоящие API
def get_weather(city: str) -> str:
    fake_data = {
        "москва": "Облачно, +5°C, ветер 3 м/с",
        "берлин": "Ясно, +12°C, ветер 5 м/с",
    }
    return fake_data.get(city.lower(), f"Нет данных для города {city}")


def convert_currency(amount: float, from_currency: str, to_currency: str) -> str:
    rates = {"USD_RUB": 92.5, "EUR_RUB": 100.3, "USD_EUR": 0.92}
    key = f"{from_currency}_{to_currency}"
    reverse_key = f"{to_currency}_{from_currency}"
    if key in rates:
        result = amount * rates[key]
    elif reverse_key in rates:
        result = amount / rates[reverse_key]
    else:
        return f"Курс {from_currency} -> {to_currency} неизвестен"
    return f"{amount} {from_currency} = {result:.2f} {to_currency}"


# Диспетчер вызовов
def call_function(name: str, arguments: dict) -> str:
    if name == "get_weather":
        return get_weather(**arguments)
    elif name == "convert_currency":
        return convert_currency(**arguments)
    return "Неизвестная функция"


# --- Основной цикл ---

messages = [
    {"role": "system", "content": "Ты полезный ассистент. Используй инструменты когда нужно. Отвечай на русском."},
    {"role": "user", "content": "Какая погода в Москве? И сколько будет 100 долларов в рублях?"},
]

print(f"Пользователь: {messages[-1]['content']}\n")

response = client.chat.completions.create(
    model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
    messages=messages,
    tools=tools,
)

assistant_message = response.choices[0].message

# Если модель решила вызвать функции
if assistant_message.tool_calls:
    print(f"Модель вызывает {len(assistant_message.tool_calls)} функций:\n")
    messages.append(assistant_message)

    for tool_call in assistant_message.tool_calls:
        func_name = tool_call.function.name
        func_args = json.loads(tool_call.function.arguments)
        print(f"  -> {func_name}({func_args})")

        result = call_function(func_name, func_args)
        print(f"  <- {result}\n")

        messages.append({
            "role": "tool",
            "tool_call_id": tool_call.id,
            "content": result,
        })

    # Отправляем результаты обратно модели
    final_response = client.chat.completions.create(
        model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
        messages=messages,
        tools=tools,
    )
    print(f"Ассистент: {final_response.choices[0].message.content}")
else:
    print(f"Ассистент: {assistant_message.content}")
```

---

## Ожидаемый вывод примера 2

```
Пользователь: Какая погода в Москве? И сколько будет 100 долларов в рублях?

Модель вызывает 2 функций:

  -> get_weather({'city': 'Москва'})
  <- Облачно, +5°C, ветер 3 м/с

  -> convert_currency({'amount': 100, 'from_currency': 'USD', 'to_currency': 'RUB'})
  <- 100 USD = 9250.00 RUB

Ассистент: В Москве сейчас облачно, температура +5°C, ветер 3 м/с.
100 долларов — это примерно 9 250 рублей.
```

---

## Заметки

- **Модель**: `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` — актуальный список доступен через `GET /v1/models`
- **Tool calling — два режима**:
  - `NATIVE_TOOL_CALLS=true` — нативная передача `tools` на ноду. Работает, когда ноды поддерживают tool calling (прокси нормализует формат `content` автоматически)
  - `SIMULATE_TOOL_CALLS=true` — эмуляция через промпт-инжекцию. Работает всегда, даже если ноды не поддерживают `tools`
  - Если ноды возвращают `"Feature 'tools' is temporarily unavailable"` — переключитесь на эмуляцию
- **Стриминг** поддерживается для обычных запросов (`stream: true`). При эмуляции tool calling используется non-streaming (прокси должен получить полный ответ для парсинга). Нативный режим поддерживает стриминг
- **API-ключ**: любая строка — авторизация происходит через подпись кошелька, а не через ключ
- **Веб-чат**: встроенный UI доступен на `http://localhost:8080`
- **Приватность**: приватные ключи никогда не покидают вашу машину; все запросы подписываются локально

---

## Управление

```bash
make build   # Собрать Docker-образ
make run     # Запустить в фоне
make stop    # Остановить
make logs    # Логи
make dev     # Собрать + запустить в foreground (для разработки)
make clean   # Остановить + удалить образы и тома
```
