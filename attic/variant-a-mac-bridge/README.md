# Attic: variant A — the Mac bridge (retired 2026-10, fully working)

Архив мак-стороны «варианта A» издательской библиотеки. Снято с вооружения в
пользу веб-заноса (drag-and-drop страница → presigned PUT прямо в R2), но
сохранено целиком: схема рабочая, доказана вживую, и к ней можно вернуться.

Полный рабочий срез репозитория — git-тег **`variant-a-final`**
(последний коммит, где Go-сторона ещё создаёт очередь заявок).

## Какой была схема

```
iCloud «orchestra and choral»  =  мастер-архив (только на маках)
        │
        │ manifest-publish.sh        (занос: 0-байтные стабы имён → VM originals/)
        ▼
VM: каталог = скан стабов; бот/веб «▶ Слушать» → ActivationQueue
        │  requests/pending/{id}.json
        ▼
Мак: activate-agent.sh  (launchd com.turnip.activate-agent, тик 60с)
        │  claim → материализация iCloud → rclone → R2 a/{secret}/{cat}/{work}/…
        ▼  done
VM: finalize-watcher → Finalize (size из R2 HEAD, duration стримом) → state+feed
```

## Что лежит здесь

- `activate-agent.sh` — агент-дренер очереди (санирован: `VM=` задаётся через
  env/launchd, адрес VM — это origin-IP за Cloudflare, в публичный репо не
  попадает). Деплой-копия жила в `~/.turnip/activate-agent.sh`.
- `com.turnip.activate-agent.plist` — launchd-шаблон (USER/VM_IP подставить).
- `manifest-publish.sh` — занос скелета: 0-байтные стабы + *.yaml → VM
  `originals/`, аддитивно (`--size-only`, `PRUNE=1` для удаления). `VM=` из
  env или соседнего гитигноренного `env.local`.
- `publish.env.example` — шаблон мак-секретов (`~/.turnip/publish.env`).

## Что НЕ здесь (живёт в основном дереве и после пенсии)

Go-сторона не удалялась: `app/publisher/activation_queue.go`,
`finalize_watcher.go`, `Finalize` и проводка в боте/Mini App. `Finalize`
активно используется новой схемой; очередь просто не создаётся, когда занос
идёт через веб.

## Как вернуть (чеклист)

1. `git log`/diff от тега `variant-a-final` — убедиться, что проводка очереди
   в `app/main.go` жива (ActQueue создаётся при `remote_activation: true`).
2. VM `/srv/etc/feed-master.yml`: `audio: {remote_activation: true}` + рестарт.
3. Мак: `brew install rclone`; `~/.turnip/publish.env` по `publish.env.example`
   (значения с VM `/srv/etc/secrets.env`, те же bucket и FEED_SECRET).
4. `cp activate-agent.sh ~/.turnip/` + plist в `~/Library/LaunchAgents/`
   (подставить USER, VM) + `launchctl bootstrap gui/$(id -u) …plist`.
5. Занос новых работ: `./manifest-publish.sh --push` (стабы), аудио льёт агент
   при активации.

## Уроки эксплуатации (почему ушли)

- Мак обязан быть не спящим в момент «▶ Слушать» — ночью на батарее агент
  умирает молча, заявка сиротеет в `claimed/` (reclaim по возрасту так и не
  построили).
- TUN-прокси (Shadowrocket) шейпил путь к R2 до ~12 КБ/с; нужен DIRECT для
  `r2.cloudflarestorage.com`.
- `ProcessType: Background` в plist давал сокетам агента scavenger-QoS
  (ledbat) — в шаблоне выше уже исправлено на Interactive.
- Веб-занос убирает дежурную машину целиком: байты идут браузер → R2 в момент,
  когда человек и так онлайн.
