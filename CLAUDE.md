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
  - `TelegramBot`: Manual podcast additions via Telegram
  - `ArticleExtractor`: Text extraction from web pages
  - `EdgeTTS`: Text-to-speech via Microsoft Edge TTS
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
- **TTS**: gorilla/websocket (Edge TTS), go-shiori/go-readability (article extraction)

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
  tts_enabled: true                    # озвучка статей
  tts_voice: "ru-RU-DmitryNeural"      # голос Edge TTS
```

**Что умеет бот:**
- YouTube видео → скачивает аудио через yt-dlp
- Статья/веб-страница → извлекает текст, озвучивает через Edge TTS
- Полный контроль — только то, что отправишь
- RSS: `{base_url}/yt/rss/{feed_name}`
- Слушать в Overcast или другом подкаст-приложении

### Команды бота:
- `/list` — что в ленте (название + длительность)
- `/history` — история с ссылками на YouTube
- `/del` — удалить последнее (из ленты + файл с диска)
- `/del N` — удалить N-ый из списка
- `/help` — справка

### Озвучка статей (TTS):

Если `tts_enabled: true`, бот может озвучивать статьи:

1. Отправляешь ссылку на статью (не YouTube)
2. Бот извлекает текст через go-readability (аналог Mozilla Readability)
3. Озвучивает через Edge TTS (бесплатный сервис Microsoft)
4. Сохраняет mp3 и добавляет в RSS ленту

**Поддерживаемые голоса Edge TTS:**
- `ru-RU-DmitryNeural` — мужской русский (по умолчанию)
- `ru-RU-SvetlanaNeural` — женский русский
- `en-US-GuyNeural` — мужской английский
- `en-US-JennyNeural` — женский английский

**Как работает:**
```
Telegram: URL статьи (habr.com, medium.com, любой блог)
    ↓
Извлечение текста (заголовок, контент)
    ↓
Edge TTS (WebSocket API)
    ↓
MP3 файл в /srv/var/yt/
    ↓
Запись в BoltDB → появляется в RSS
```

**Ограничения и rate limiting:**
- Edge TTS обрабатывает ~3000 символов за раз, длинные статьи разбиваются на чанки
- Между чанками задержка 2 сек (чтобы не забанили)
- Retry с exponential backoff при ошибках (5с → 10с → 20с, 3 попытки)
- Некоторые сайты могут блокировать парсинг (403/Cloudflare)
- Для статей не скачивается картинка-обложка
- Большие статьи (40К+ символов) могут занять 5-10 минут

**Реализация:**
- `app/proc/article.go` — извлечение текста из URL
- `app/proc/tts.go` — обёртка над библиотекой `edge-tts-go`
- `app/proc/translate.go` — перевод через Yandex Translate API
- Используется библиотека [github.com/wujunwei928/edge-tts-go](https://github.com/wujunwei928/edge-tts-go) для работы с Microsoft Edge TTS

### Перевод статей (Yandex Translate)

Если статья на английском — автоматически переводится на русский перед озвучкой.

**Как работает:**
1. Определяется язык текста (кириллица vs латиница)
2. Если не русский → перевод через Yandex Translate API
3. Затем озвучка переведённого текста

**Настройка:**
- Нужен API ключ Yandex Cloud (бесплатно 10 млн символов/месяц)
- Добавить в `/srv/etc/secrets.env`:
  ```
  YANDEX_TRANSLATE_KEY=ваш_api_ключ
  YANDEX_FOLDER_ID=ваш_folder_id
  ```

**Как получить ключ:**
1. Зарегистрироваться на [Yandex Cloud](https://cloud.yandex.ru/)
2. Создать платёжный аккаунт (получишь грант ~4000₽)
3. IAM → Сервисные аккаунты → Создать (роль: `ai.translate.user`)
4. В сервисном аккаунте → Создать API-ключ
5. Folder ID — в URL консоли или в шапке

**Статус:** работает! Тестировано на статьях New Yorker

### Особенности:
- Сообщение со ссылкой удаляется через 5 сек после добавления
- Статус бота остаётся в чате (✅ Title (12:34))
- Картинки эпизодов берутся из YouTube thumbnails
- Для статей — без картинки эпизода
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
  tts_enabled: true                  # озвучка статей
  tts_voice: "ru-RU-DmitryNeural"    # голос Edge TTS
```

### Секреты (/srv/etc/secrets.env)

Секреты НЕ в конфиге, а в отдельном файле (не попадает в git):

```bash
# Формат файла: без кавычек вокруг значений!
TELEGRAM_TOKEN=123456789:ABCdefGHI-jklMNOpqrSTUvwxYZ
YANDEX_TRANSLATE_KEY=AQVN...ваш_ключ
YANDEX_FOLDER_ID=b1gxxxxxxxxx
```

**Как получить:**
- `TELEGRAM_TOKEN`: @BotFather → /mybots → выбрать бота → API Token
- `YANDEX_TRANSLATE_KEY`: Yandex Cloud → IAM → Сервисные аккаунты → API-ключ
- `YANDEX_FOLDER_ID`: Yandex Cloud → ID каталога (в URL или в шапке консоли)

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

## CI/CD: Автоматический деплой

### Как работает

```
git push main → GitHub Actions → ghcr.io → Watchtower → контейнер перезапущен
```

1. **GitHub Actions** (`.github/workflows/deploy.yml`):
   - При push в main собирает Docker образ
   - Пушит в GitHub Container Registry (`ghcr.io/ryepollen/turnip:latest`)
   - Никаких секретов настраивать не нужно (использует `GITHUB_TOKEN`)

2. **Watchtower** на сервере:
   - Каждые 5 минут проверяет новые версии образов
   - Автоматически скачивает и перезапускает контейнер

### Первоначальная настройка на сервере

**1. Сделать репозиторий публичным** (или настроить auth для ghcr.io):
- GitHub → Settings → Change visibility → Public
- Или: создать Personal Access Token и настроить docker login

**2. Остановить старый контейнер:**
```bash
docker stop turnip && docker rm turnip
```

**3. Запустить turnip с образом из ghcr.io:**
```bash
docker run -d \
  --name turnip \
  -p 8080:8080 \
  --env-file /srv/etc/secrets.env \
  -v /srv/etc:/srv/etc \
  -v /srv/var:/srv/var \
  -v /usr/local/bin/yt-dlp:/usr/local/bin/yt-dlp \
  ghcr.io/ryepollen/turnip:latest /srv/feed-master -f /srv/etc/feed-master.yml
```

**Однострочная версия** (для SSH-in-browser, где многострочные команды не работают):
```bash
docker run -d --name turnip -p 8080:8080 --env-file /srv/etc/secrets.env -v /srv/etc:/srv/etc -v /srv/var:/srv/var -v /usr/local/bin/yt-dlp:/usr/local/bin/yt-dlp ghcr.io/ryepollen/turnip:latest /srv/feed-master -f /srv/etc/feed-master.yml
```

**4. Запустить Watchtower:**
```bash
docker run -d \
  --name watchtower \
  -e DOCKER_API_VERSION=1.44 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  containrrr/watchtower \
  --interval 300 \
  --cleanup
```

**Что делают флаги Watchtower:**
- `-e DOCKER_API_VERSION=1.44` — версия Docker API (важно для совместимости!)
- `--interval 300` — проверять каждые 5 минут (300 сек)
- `--cleanup` — удалять старые образы после обновления

### Проверка работы CI/CD

```bash
# Посмотреть версию текущего образа (коммит в имени)
docker logs turnip | head -5

# Логи Watchtower (видно когда обновляет)
docker logs watchtower

# Проверить что Watchtower работает без ошибок
docker logs watchtower | grep -i error
```

### Проблемы с Watchtower

**"client version X is too old. Minimum supported API version is Y":**
```bash
docker stop watchtower && docker rm watchtower
docker run -d --name watchtower -e DOCKER_API_VERSION=1.44 -v /var/run/docker.sock:/var/run/docker.sock containrrr/watchtower --interval 300 --cleanup
```

**Watchtower не обновляет контейнер:**
1. Проверь логи: `docker logs watchtower`
2. Проверь что образ в ghcr.io обновился: GitHub → Actions → должен быть зелёный
3. Подожди 5 минут (интервал проверки)
4. Если срочно — обнови вручную (см. ниже)

### Ручное обновление (если нужно срочно)

```bash
docker pull ghcr.io/ryepollen/turnip:latest
docker stop turnip && docker rm turnip
docker run -d --name turnip -p 8080:8080 --env-file /srv/etc/secrets.env -v /srv/etc:/srv/etc -v /srv/var:/srv/var -v /usr/local/bin/yt-dlp:/usr/local/bin/yt-dlp ghcr.io/ryepollen/turnip:latest /srv/feed-master -f /srv/etc/feed-master.yml
```

### Workflow dispatch (ручной запуск сборки)

GitHub → Actions → "Build and Push Docker Image" → Run workflow

## Important Testing Note

The processor tests in `app/proc` may fail if test data contains dates older than 1 year. The processor skips RSS items older than 1 year (see `processor.go:83`). If tests fail with "no bucket for feed1" errors:

1. Check the dates in `app/proc/testdata/rss1.xml` and `app/proc/testdata/rss2.xml`
2. Update the year in `<pubDate>` tags to be within the last year
3. Example: Change `<pubDate>Sat, 19 Mar 2024 19:35:46 EST</pubDate>` to `<pubDate>Sat, 19 Mar 2025 19:35:46 EST</pubDate>`