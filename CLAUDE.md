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

## Important Testing Note

The processor tests in `app/proc` may fail if test data contains dates older than 1 year. The processor skips RSS items older than 1 year (see `processor.go:83`). If tests fail with "no bucket for feed1" errors:

1. Check the dates in `app/proc/testdata/rss1.xml` and `app/proc/testdata/rss2.xml`
2. Update the year in `<pubDate>` tags to be within the last year
3. Example: Change `<pubDate>Sat, 19 Mar 2024 19:35:46 EST</pubDate>` to `<pubDate>Sat, 19 Mar 2025 19:35:46 EST</pubDate>`