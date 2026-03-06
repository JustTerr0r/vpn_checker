# VPN Checker — Project Documentation

> Последнее обновление: 2026-03-06
> Go module: `vpn_checker` | Go 1.22

---

## Обзор

Система для массовой проверки VPN-конфигов (vless, vmess, shadowsocks, trojan) через xray-core.
Состоит из трёх независимых бинарников + общих внутренних пакетов.

```
┌─────────────────┐    ┌──────────────────┐    ┌───────────────────┐
│  cmd/checker    │    │ cmd/pool-worker  │    │ cmd/redis-checker │
│ (файловый чек)  │    │ (граббер ссылок) │    │ (Redis чек + UI)  │
└────────┬────────┘    └────────┬─────────┘    └────────┬──────────┘
         │                      │                        │
         └──────────────────────┼────────────────────────┘
                                │
              ┌─────────────────┼──────────────────┐
              │                 │                  │
        internal/parser   internal/checker   internal/pool
        internal/xray     internal/web       internal/dashboard
```

---

## Структура проекта

```
vpn_checker/
├── cmd/
│   ├── checker/main.go          # CLI: проверка из файла/stdin
│   ├── pool-worker/main.go      # CLI: граббер ссылок → Redis
│   └── redis-checker/main.go   # CLI: чекер из Redis + веб-дашборд
├── internal/
│   ├── parser/parser.go         # Парсинг URI всех протоколов
│   ├── checker/checker.go       # Логика проверки через xray + ip-api
│   ├── xray/xray.go             # Генерация xray-конфигов, запуск процесса
│   ├── web/server.go            # HTTP-дашборд для cmd/checker (SSE)
│   ├── pool/
│   │   ├── redis.go             # Redis-клиент (pool:raw, pool:checked)
│   │   ├── pool.go              # Оркестратор периодического фетча
│   │   ├── fetcher.go           # HTTP-фетч одного URL → []URI
│   │   └── worker.go            # Параллельный диспетчер фетчеров
│   └── dashboard/server.go      # HTTP-дашборд для redis-checker (SSE)
├── configs.txt                  # Пример входного файла конфигов
├── example_happ_subscription_handler.go  # Справка по заголовкам Happ
├── go.mod
└── DOCS.md                      # Этот файл
```

---

## Бинарники

### 1. `cmd/checker` — файловый чекер

Читает конфиги из файла или stdin, проверяет параллельно, показывает прогресс в терминале.
Опционально поднимает HTTP-дашборд с live SSE.

**Сборка:**
```bash
go build -o checker ./cmd/checker/
```

**Флаги:**
| Флаг | Дефолт | Описание |
|------|--------|----------|
| `-f` | — | Путь к файлу (иначе stdin) |
| `-w` | 5 | Число параллельных воркеров |
| `-t` | 10s | Таймаут на один конфиг |
| `-serve` | — | Адрес HTTP-дашборда, напр. `:8080` |
| `-interval` | 5m | Интервал проверки изменений файла |
| `-recheck` | 10m | Интервал ре-валидации живых конфигов |
| `-json` | false | Вывод результатов JSON в stdout |
| `-no-color` | false | Отключить ANSI-цвета |

**Пример:**
```bash
./checker -f configs.txt -w 10 -t 15s -serve :8080 -interval 1m -recheck 20m
```

**Дашборд:** `http://localhost:8080/`
**Скачать конфиги:** `http://localhost:8080/configs` (plain text)

**Поведение:**
- Сервер поднимается сразу, показывает чек в реальном времени через SSE
- При изменении файла (проверка mtime) новые живые конфиги **добавляются** к существующим (не заменяют)
- `recheckLoop` циклически ре-валидирует живые конфиги от старых к новым, мёртвые удаляет

---

### 2. `cmd/pool-worker` — граббер ссылок

Периодически скачивает списки VPN-конфигов с GitHub/других URL, парсит, дедуплицирует, сохраняет в Redis Set `pool:raw`.

**Сборка:**
```bash
go build -o pool-worker ./cmd/pool-worker/
```

**Флаги:**
| Флаг | Дефолт | Описание |
|------|--------|----------|
| `-urls` | — | URL через запятую |
| `-interval` | 10m | Интервал повторного фетча |
| `-redis` | `$REDIS_URL` | Redis DSN |
| `-workers` | 5 | Параллельные HTTP-фетчеры |

**Пример:**
```bash
./pool-worker \
  -urls "https://raw.githubusercontent.com/.../ss.txt,https://..." \
  -redis "redis://default:pass@host:port" \
  -interval 10m -workers 5
```

**Логика:**
1. `FetchAll` — параллельно скачивает все URL
2. Каждая строка парсится через `parser.ParseLine` — невалидные отбрасываются
3. Валидные оригинальные строки → `SADD pool:raw` (автодедупликация Redis Set)
4. Повтор по тикеру, graceful shutdown по SIGINT/SIGTERM

---

### 3. `cmd/redis-checker` — Redis чекер + дашборд

Основной продакшн-бинарник. Берёт URI из `pool:raw`, проверяет, мёртвые удаляет (`SREM pool:raw`), живые кладёт в `pool:checked` (Sorted Set, score = latency ms). Поднимает веб-дашборд с live SSE и встроенным управлением граббером.

**Сборка:**
```bash
go build -o redis-checker ./cmd/redis-checker/
```

**Флаги:**
| Флаг | Дефолт | Описание |
|------|--------|----------|
| `-redis` | `$REDIS_URL` | Redis DSN |
| `-workers` | 10 | Параллельные воркеры чека |
| `-timeout` | 15s | Таймаут на один конфиг |
| `-serve` | `:8081` | Адрес дашборда |
| `-recheck` | false | Зациклить прогоны (loop forever) |

**Пример:**
```bash
./redis-checker -redis "redis://default:pass@host:port" -workers 10 -recheck
```

**Дашборд:** `http://localhost:8081/`

**Логика чека:**
1. `SMEMBERS pool:raw` → список URI
2. Параллельные воркеры вызывают `processURI`
3. `ParseLine` ошибка → `SREM pool:raw` + PublishDead
4. `CheckConfig` мёртвый → `SREM pool:raw` + PublishDead
5. `CheckConfig` живой → `ZADD pool:checked score=latencyMs` + PublishAlive (из `pool:raw` НЕ удаляется)
6. После прогона: `SetDone`, если `-recheck` → следующий прогон

---

## Redis-структура

| Ключ | Тип | Описание |
|------|-----|----------|
| `pool:raw` | Set | Все собранные URI. Граббер добавляет (SADD). Чекер удаляет мёртвые (SREM). |
| `pool:checked` | Sorted Set | Живые URI. Score = latency ms. Отдаётся через `/configs`. |

**Redis DSN формат:** `redis://default:PASSWORD@HOST:PORT`

---

## Внутренние пакеты

### `internal/parser`

Парсинг URI в типизированные конфиги.

**Поддерживаемые протоколы:** `vless://`, `vmess://`, `ss://` (shadowsocks), `trojan://`

**Ключевые функции:**
```go
parser.ParseLine(line string) (ProxyConfig, error)
parser.RenameURI(rawURI, name string) string
```

`RenameURI` — переписывает display name внутри URI:
- `vless://`, `ss://`, `trojan://` → заменяет `#fragment`
- `vmess://` → декодирует base64 JSON, меняет поле `ps`, перекодирует

**Интерфейс ProxyConfig:**
```go
GetName() string
GetProtocol() string  // "vless" | "vmess" | "shadowsocks" | "trojan"
GetServer() string
GetPort() int
```

---

### `internal/checker`

Проверка одного конфига через xray + ip-api.com.

**Алгоритм `CheckConfig`:**
1. Найти свободный локальный порт
2. Сгенерировать xray JSON конфиг (`GenerateConfig`)
3. Запустить `xray run -config stdin:`
4. Ждать готовности SOCKS5 (до 3s)
5. HTTP GET `http://ip-api.com/json` через SOCKS5
6. Измерить latency, получить ExitIP + Country
7. Убить xray процесс

**`CheckAll`** — параллельный запуск через `jobs chan + WaitGroup + N goroutines`

---

### `internal/xray`

Генерация xray JSON конфигов для всех протоколов.

- `GenerateConfig(cfg ProxyConfig, socksPort int) ([]byte, error)` — диспетчер по типу
- `Start(configJSON []byte) (*exec.Cmd, error)` — запуск `xray` процесса, stdin = конфиг
- `Stop(cmd *exec.Cmd)` — kill + wait

**Требование:** бинарник `xray` должен быть в `$PATH`.

**Поддерживаемые транспорты в streamSettings:** ws, grpc, http/h2, httpupgrade, xhttp/splithttp, tcp
**Поддерживаемые security:** tls, reality (с publicKey/shortId)

---

### `internal/pool`

Граббер и Redis-клиент.

**`RedisClient` методы:**
```go
AddURIs(ctx, uris []string) (int64, error)           // SADD pool:raw
GetRawURIs(ctx) ([]string, error)                    // SMEMBERS pool:raw
RemoveRawURI(ctx, uri string) error                  // SREM pool:raw
AddCheckedURI(ctx, uri string, latencyMs float64)    // ZADD pool:checked
GetCheckedURIs(ctx) ([]string, error)                // ZRANGE pool:checked 0 -1 (asc latency)
RawCount(ctx) (int64, error)                         // SCARD pool:raw
CheckedCount(ctx) (int64, error)                     // ZCARD pool:checked
ClearRaw(ctx) error                                  // DEL pool:raw
ClearChecked(ctx) error                              // DEL pool:checked
```

**`Pool.RunOnce`:** `FetchAll → SADD → log "+N new URIs"`
**`Pool.Run`:** RunOnce сразу + ticker

---

### `internal/dashboard`

HTTP-дашборд для `redis-checker`. Не зависит напрямую от `pool`.

**SSE события:**
| Тип | Когда |
|-----|-------|
| `alive` | Новый живой конфиг |
| `dead` | Мёртвый конфиг (без добавления в таблицу) |
| `stats` | Обновление статистики |
| `done` | Прогон завершён |
| `grabber` | Обновление статуса граббера |

**HTTP endpoints:**
| Метод | Путь | Описание |
|-------|------|----------|
| GET | `/` | HTML дашборд |
| GET | `/events` | SSE stream |
| GET | `/configs` | Plain text URI (с ренеймом + Happ заголовки) |
| POST | `/grabber/start` | Запустить граббер `{"urls":["..."],"interval":"10m"}` |
| POST | `/grabber/stop` | Остановить граббер |
| GET | `/grabber/status` | Текущий статус граббера JSON |
| POST | `/pool/clear-raw` | `DEL pool:raw` |
| POST | `/pool/clear-checked` | `DEL pool:checked` + сброс таблицы |

**`/configs` — Happ subscription endpoint:**

Отдаёт plain text URI с заголовками:
```
profile-title: Babyl0n Free
profile-update-interval: 12
support-url: https://t.me/vabes
subscription-userinfo: upload=0; download=0; total=0; expire=<+10лет>
announce: base64:<кириллица>
content-disposition: attachment; filename="Babyl0n Free"
hide-settings: 1
```

> **Важно:** Go канонизирует имена заголовков (`Profile-Title`), Happ читает только lowercase.
> Заголовки пишутся напрямую в `w.Header()` map, минуя `Header.Set()`.

**Ренейм конфигов:** каждый URI при отдаче переименовывается через `parser.RenameURI`:
`<ISO country code> t.me/vpn0y - всегда рабочий VPN`
Country берётся из `uriCountry` map, заполняемой при `PublishAlive`.

**Happ подписка работает только через URL** (не через буфер) — нужен HTTPS (ngrok / VPS).

---

### `internal/web`

HTTP-дашборд для `cmd/checker` (файловый чекер). Аналогичная структура с SSE.

**Ключевые методы:**
```go
NewServer(entries []AliveEntry) *Server
SetChecking(total int)
PublishResult(e AliveEntry, done, total int)
SetDone()
AppendEntries(newEntries []AliveEntry, nextCheckIn string)  // merge с дедупликацией
RemoveEntry(key string)                                      // SSE "remove" event
UpdateNextCheckIn(s string)
Entries() []AliveEntry
```

---

## Запуск всего вместе

**Только `redis-checker`** (граббер управляется из дашборда):
```bash
./redis-checker -redis "redis://default:PASS@HOST:PORT" -workers 10 -recheck
# Открыть http://localhost:8081/
# Вставить URL в панель Grabber → Start Grabber
# Ждать заполнения pool:raw, чек запустится автоматически
```

**С отдельным граббером:**
```bash
# Терминал 1 — граббер
./pool-worker -urls "url1,url2" -redis "redis://..." -interval 10m

# Терминал 2 — чекер + дашборд
./redis-checker -redis "redis://..." -workers 10 -recheck
```

---

## Зависимости

| Пакет | Версия | Использование |
|-------|--------|---------------|
| `golang.org/x/net` | v0.24.0 | SOCKS5 proxy dialer |
| `github.com/redis/go-redis/v9` | v9.18.0 | Redis клиент |

**Внешние зависимости:**
- `xray` — должен быть в `$PATH` (проект xtls/Xray-core)
- Redis — `pool:raw` и `pool:checked`
- `http://ip-api.com/json` — определение ExitIP и страны

---

## Ключевые паттерны кода

**Worker pool:**
```go
jobs := make(chan T, total)
var wg sync.WaitGroup
for i := 0; i < workers; i++ {
    wg.Add(1)
    go func() { defer wg.Done(); for item := range jobs { process(item) } }()
}
for _, item := range items { jobs <- item }
close(jobs)
wg.Wait()
```

**SSE late-joiner:** при подключении к `/events` сервер сразу шлёт весь текущий snapshot (entries + stats + done если применимо), затем переходит в режим реального времени.

**Lowercase HTTP заголовки для Happ:**
```go
hdr := w.Header()
hdr["profile-title"] = []string{"Babyl0n Free"}  // НЕ w.Header().Set()
```

---

## Известные ограничения и заметки

- `uriCountry` map в dashboard живёт только в памяти — после перезапуска country для старых URI в Redis неизвестен (подставляется `VPN t.me/...`)
- `pool:raw` содержит URI для чека, мёртвые удаляются. Живые **остаются** в `pool:raw` для следующего прогона
- Happ требует HTTPS — для dev использовать ngrok: `ngrok http 8081`
- `hide-settings: 1` требует Provider ID `KKvkWFbv` в настройках Happ Provider
- `cmd/checker` и `cmd/redis-checker` — независимые инструменты, не связаны между собой
