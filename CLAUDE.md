# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build, Test, Lint Commands
```bash
# Run tests
go test -race -v ./...                            # Run all tests from root
go test -race -v ./app/...                        # Run app tests
go test -race -v ./app/proc                       # Test specific package
go test -race -v ./app/proc -run TestStore       # Run specific test

# Lint code
golangci-lint run ./...                           # Lint entire codebase from root
golangci-lint run ./app/...                       # Lint app directory

# Build application
cd app && go build -o feed-master                 # Build binary
docker build -t feed-master .                      # Build Docker image

# Format and normalize
gofmt -s -w $(find . -type f -name "*.go" -not -path "./vendor/*")
goimports -w $(find . -type f -name "*.go" -not -path "./vendor/*")
```

## High-Level Architecture

Feed Master is a Go service that aggregates RSS feeds and YouTube content into unified feeds:

- **app/main.go**: Entry point with CLI flags, initializes Processor and Server
- **app/proc**: Core feed processing logic
  - `Processor`: Orchestrates feed fetching, filtering, and notifications
  - `Store`: BoltDB persistence layer for feed items
  - `Telegram`/`Twitter`: Notification handlers
- **app/feed**: RSS feed parsing and generation utilities
- **app/youtube**: YouTube channel/playlist processing
  - `Service`: Downloads videos as audio, manages channel RSS generation
  - `feed.Downloader`: Handles yt-dlp interactions
  - `store.BoltDB`: Persists YouTube metadata
- **app/api**: HTTP endpoints for RSS feeds and admin operations
  - Public: `/rss/{name}`, `/list`, `/yt/rss/{channel}`
  - Admin: `/yt/rss/generate`, `/yt/entry/{channel}/{video}` (DELETE)
- **app/config**: YAML configuration loading and validation

## Key Design Patterns

- **Feed Aggregation**: Multiple source feeds → normalized → single output feed
- **YouTube Integration**: Uses yt-dlp for audio extraction, serves files via HTTP
- **Storage**: BoltDB for both feed items and YouTube metadata
- **Notifications**: Template-based messages to Telegram/Twitter on new items
- **Concurrent Processing**: Uses go-pkgz/syncs for controlled parallelism
- **Error Handling**: pkg/errors for wrapping, lgr for structured logging

## Testing Patterns

Tests use testify with table-driven patterns:
```go
func TestFeature(t *testing.T) {
    tests := []struct {
        name    string
        input   Type
        want    Result
        wantErr bool
    }{
        {"case 1", input1, expected1, false},
        {"error case", badInput, nil, true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // test implementation
        })
    }
}
```

## Configuration Structure

Config loaded from YAML (see _example/etc/):
- `feeds`: Named feed configurations with sources, filters, notifications
- `youtube`: Channel definitions, download settings, file locations
- `system`: Update intervals, limits, base URL

## Dependencies

- **Web**: chi/v5 router with go-pkgz/rest middlewares
- **Storage**: etcd.io/bbolt
- **Testing**: stretchr/testify
- **YouTube**: External yt-dlp binary
- **Notifications**: tucnak/telebot.v2, ChimeraCoder/anaconda

## Current Usage: Personal Podcast via Telegram Bot

Этот форк (turnip) используется для личного подкаста через Telegram бота, а не через автоматические YouTube каналы.

### Два режима работы YouTube:

**1. Автоматические каналы (НЕ используется):**
```yaml
youtube:
  channels:
    - {id: UCxxx, name: "Channel", keep: 10}
```
- Автоматически скачивает все новые видео с канала/плейлиста
- Нет контроля что попадает в ленту
- Есть автоочистка старых файлов (removeOld)

**2. Telegram бот (ИСПОЛЬЗУЕТСЯ):**
```yaml
telegram_bot:
  enabled: true
  allowed_user_id: 123456789
  feed_name: "manual"
  feed_title: "Offthplant 🪴"
  feed_description: "פֿון פּערזענלעכע מחלוקות און פּרינציפּן, קיין דערקלערונגען"
  feed_image: "./var/images/offthplant.png"
  max_items: 100
```
- Отправляешь ссылку на YouTube видео боту
- Бот скачивает аудио, добавляет в RSS ленту
- Полный контроль — только выбранные видео
- RSS: `{base_url}/yt/rss/{feed_name}`
- Слушать в Overcast или другом подкаст-приложении

### Команды бота:
- `/list` — что в ленте (название + длительность)
- `/history` — история с ссылками на YouTube
- `/del` — удалить последнее (из ленты + файл с диска)
- `/del N` — удалить N-ый из списка
- `/help` — справка

### Особенности:
- Сообщение со ссылкой удаляется через 5 сек после добавления
- Статус бота остаётся в чате (✅ Title (12:34))
- Картинки эпизодов берутся из YouTube thumbnails
- Длительность в формате MM:SS или H:MM:SS
- `/del` удаляет и запись из базы, и файл с диска

### Хранение файлов:
- Аудиофайлы: `./var/yt/`
- RSS файлы: `./var/rss/`
- Картинка подкаста: `./var/images/`
- Автоочистка: когда записей > `max_items`, старые удаляются автоматически (и из базы, и файлы)

## Server Deployment (Google Cloud)

### Как всё работает вместе

```
┌─────────────────┐     ┌──────────────────────────────────────────────┐
│   Telegram      │     │           Google Cloud VM                    │
│                 │     │                                              │
│  Ты отправляешь │────▶│  Docker контейнер "turnip"                   │
│  ссылку боту    │     │    ├── feed-master (Go binary)               │
│                 │◀────│    ├── слушает Telegram (long polling)       │
│  Бот отвечает   │     │    ├── скачивает через yt-dlp                │
│  ✅ Title (12:34)│     │    └── HTTP сервер :8080                     │
└─────────────────┘     │                                              │
                        │  /srv/var/yt/*.mp3      ← аудиофайлы         │
┌─────────────────┐     │  /srv/var/feed-master.bdb ← база данных      │
│   Overcast      │     │                                              │
│                 │────▶│  GET /yt/rss/manual     ← RSS лента          │
│  Подписка на    │     │  GET /yt/media/xxx.mp3  ← аудио              │
│  подкаст        │◀────│  GET /yt/image/manual   ← картинка подкаста  │
└─────────────────┘     └──────────────────────────────────────────────┘
```

**RSS лента:** `http://35.238.12.191:8080/yt/rss/manual`

### Структура на сервере

```
/srv/
├── etc/
│   ├── feed-master.yml    # конфиг приложения
│   └── secrets.env        # TELEGRAM_TOKEN (не в git!)
└── var/
    ├── yt/                # скачанные mp3 файлы (хэш от feed_name+video_id)
    ├── images/
    │   └── offthplant.png # картинка подкаста
    └── feed-master.bdb    # BoltDB база данных (ВСЕ эпизоды тут!)

/usr/local/bin/yt-dlp      # бинарник yt-dlp (маунтится в контейнер)
~/turnip/                  # git репозиторий для сборки образа
```

### Конфиг на сервере (/srv/etc/feed-master.yml)

```yaml
system:
  update: 5m
  max_per_feed: 5
  max_total: 100
  max_keep: 5000
  concurrent: 8
  base_url: "http://35.238.12.191:8080"

youtube:
  files_location: /srv/var/yt
  rss_location: /srv/var/rss

telegram_bot:
  enabled: true
  allowed_user_id: 5504926420        # твой Telegram user ID
  feed_name: "manual"
  feed_title: "Offthplant 🪴"
  feed_description: "פֿון פּערזענלעכע מחלוקות און פּרינציפּן, קיין דערקלערונגען"
  feed_image: "/srv/var/images/offthplant.png"
  max_items: 100
```

### Секреты (TELEGRAM_TOKEN)

Токен бота НЕ в конфиге, а в отдельном файле (не попадает в git):

```bash
# Получить токен: Telegram → @BotFather → /mybots → выбрать бота → API Token
# Формат файла: без кавычек вокруг значения!
echo 'TELEGRAM_TOKEN=123456789:ABCdefGHI-jklMNOpqrSTUvwxYZ' > /srv/etc/secrets.env
chmod 600 /srv/etc/secrets.env
```

### Запуск контейнера

```bash
docker run -d \
  --name turnip \
  -p 8080:8080 \
  --env-file /srv/etc/secrets.env \
  -v /srv/etc:/srv/etc \
  -v /srv/var:/srv/var \
  -v /usr/local/bin/yt-dlp:/usr/local/bin/yt-dlp \
  turnip /srv/feed-master -f /srv/etc/feed-master.yml
```

**Что делают флаги:**
- `-d` — в фоне (detached)
- `--name turnip` — имя контейнера
- `-p 8080:8080` — проброс порта
- `--env-file` — передаёт TELEGRAM_TOKEN в контейнер
- `-v /srv/etc:/srv/etc` — маунт конфига
- `-v /srv/var:/srv/var` — маунт данных (mp3, база)
- `-v /usr/local/bin/yt-dlp:...` — маунт yt-dlp бинарника
- `turnip` — имя образа
- `/srv/feed-master -f ...` — команда запуска с указанием конфига

### Обновление кода

```bash
cd ~/turnip
git pull
docker build -t turnip .
docker stop turnip && docker rm turnip
# запустить docker run заново (см. выше)
```

### Обновление только конфига

```bash
# Редактировать конфиг (стрелками листать, Ctrl+O сохранить, Ctrl+X выйти)
sudo nano /srv/etc/feed-master.yml

# Перезапустить (без пересборки)
docker restart turnip
```

### Загрузка файлов на сервер

В SSH-in-browser: иконка ⚙️ (или ⋮) в правом верхнем углу → "Upload file"
Файл попадает в `~/`, потом переместить:
```bash
sudo mv ~/filename /srv/var/images/
```

### Проверка и диагностика

```bash
docker ps                              # контейнер запущен?
docker logs turnip                     # все логи
docker logs turnip | tail -20          # последние 20 строк
docker logs -f turnip                  # следить за логами (Ctrl+C выйти)
curl localhost:8080/ping               # health check

# Проверить что бот запустился:
docker logs turnip | grep "starting telegram bot"
# Должно быть: [INFO] starting telegram bot for user 5504926420, feed: manual

# Проверить что конфиг подхватился:
docker logs turnip | grep "TelegramBot"

# Проверить что токен передался в контейнер:
docker exec turnip env | grep TELEGRAM
```

### Типичные проблемы

**Бот не запускается (нет "starting telegram bot" в логах):**
- Проверь что токен передаётся: `docker exec turnip env | grep TELEGRAM`
- Проверь формат secrets.env: без кавычек! `TELEGRAM_TOKEN=xxx` а не `TELEGRAM_TOKEN="xxx"`

**Контейнер сразу падает:**
- Смотри логи: `docker logs turnip`
- Часто: неверный токен → "telegram: Not Found (404)"

**"container name already in use":**
- `docker stop turnip && docker rm turnip` перед новым запуском

**Файлы не скачиваются:**
- Проверь что yt-dlp работает: `docker exec turnip yt-dlp --version`
- Предупреждения про SABR/pot:bgutil — не критичны, скачивание работает

### SSH доступ

Google Cloud Console → Compute Engine → VM instances → SSH (кнопка)
Откроется SSH-in-browser в новом окне.

### Бэкап и восстановление базы

База данных `/srv/var/feed-master.bdb` содержит все эпизоды. MP3 файлы в `/srv/var/yt/`.

```bash
# Бэкап базы
docker stop turnip
cp /srv/var/feed-master.bdb /srv/var/feed-master.bdb.backup
docker start turnip

# Восстановление из бэкапа
docker stop turnip
cp /srv/var/feed-master.bdb.backup /srv/var/feed-master.bdb
docker start turnip
```

**Важно:** база и mp3 файлы связаны. Если удалить mp3 без удаления из базы — в RSS будут битые ссылки. Используй `/del` в боте для правильного удаления.

## Important Testing Note

The processor tests in `app/proc` may fail if test data contains dates older than 1 year. The processor skips RSS items older than 1 year (see `processor.go:83`). If tests fail with "no bucket for feed1" errors:

1. Check the dates in `app/proc/testdata/rss1.xml` and `app/proc/testdata/rss2.xml`
2. Update the year in `<pubDate>` tags to be within the last year
3. Example: Change `<pubDate>Sat, 19 Mar 2024 19:35:46 EST</pubDate>` to `<pubDate>Sat, 19 Mar 2025 19:35:46 EST</pubDate>`