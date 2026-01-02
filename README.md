# Turnip Podcast Bot

Personal YouTube-to-Podcast service with Telegram bot integration. Send a YouTube link to your Telegram bot and get audio in your podcast app.

Based on [feed-master](https://github.com/umputun/feed-master) by umputun.

## How it works

1. Send YouTube video link to your Telegram bot
2. Bot downloads video and extracts audio (via yt-dlp)
3. Audio file is added to your personal RSS feed
4. Subscribe to the RSS feed in any podcast app

## Quick Start

### Prerequisites

- Docker (recommended) or Go 1.21+
- [yt-dlp](https://github.com/yt-dlp/yt-dlp) installed
- Telegram bot token (get from [@BotFather](https://t.me/BotFather))
- Your Telegram user ID (get from [@userinfobot](https://t.me/userinfobot))

### Configuration

Create `etc/fm.yml`:

```yaml
telegram_bot:
  enabled: true
  allowed_user_id: 123456789  # your Telegram user ID
  feed_name: "manual"
  feed_title: "My YouTube Podcast"
  max_items: 100

system:
  base_url: http://your-server:8080

youtube:
  files_location: ./var/yt
  rss_location: ./var/rss
```

### Run with Docker

```bash
docker run -d \
  -p 8080:8080 \
  -e TELEGRAM_TOKEN=your_bot_token \
  -e FM_CONF=/srv/etc/fm.yml \
  -v $(pwd)/etc:/srv/etc \
  -v $(pwd)/var:/srv/var \
  umputun/feed-master:master
```

### Run locally

```bash
cd app
go build -o turnip
./turnip --conf ../etc/fm.yml --telegram_token YOUR_BOT_TOKEN
```

## Usage

1. Start a chat with your bot in Telegram
2. Send `/help` to see available commands
3. Send any YouTube link:
   - `https://youtube.com/watch?v=VIDEO_ID`
   - `https://youtu.be/VIDEO_ID`
4. Bot will download audio and confirm when ready
5. Subscribe to RSS feed: `http://your-server:8080/yt/rss/manual`

### Bot Commands

| Command | Description |
|---------|-------------|
| `/help` | Show help message |
| `/list` | Show recent additions |
| (YouTube URL) | Add video to feed |

## Configuration Reference

### telegram_bot section

| Field | Description | Default |
|-------|-------------|---------|
| `enabled` | Enable Telegram bot | `false` |
| `allowed_user_id` | Your Telegram user ID (required) | - |
| `feed_name` | RSS feed name | `manual` |
| `feed_title` | RSS feed title | `My YouTube Podcast` |
| `max_items` | Max items in feed | `100` |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `TELEGRAM_TOKEN` | Telegram bot token (required) |
| `FM_CONF` | Config file path |
| `FM_DB` | Database file path |

## RSS Feed

Your podcast feed will be available at:
```
http://your-server:8080/yt/rss/{feed_name}
```

Default: `http://your-server:8080/yt/rss/manual`

Add this URL to your podcast app (Apple Podcasts, Pocket Casts, Overcast, etc.)

## Credits

Fork of [feed-master](https://github.com/umputun/feed-master) by [umputun](https://github.com/umputun).

## License

MIT License - see [LICENSE](LICENSE)
