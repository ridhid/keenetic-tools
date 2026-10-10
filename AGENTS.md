# AGENTS.md

Инструкции для AI-агентов (Claude Code, Codex и др.), работающих с этим репозиторием.

## Что это

Один статический Go-бинарник `keenetic-tools` для роутеров **Keenetic с Entware**:

- `keenetic-tools awg-cron install [hourly|daily] | uninstall | run` — cron-задание, которое
  перезапускает AmneziaWG-туннель через `/opt/etc/init.d/S52awg-opkgtun0 restart`;
- `keenetic-tools awg-monitor КОМАНДА` (или `awg-monitor КОМАНДА` через симлинк) — мониторинг
  туннеля: замеры, события, дампы при сбоях, снимки конфигурации, проверки доменов, поиск
  соединений мимо туннеля.

Бинарник исполняется **только на роутере** (под root), собирается кросс-компиляцией на ПК.
Пользователь ставит его из **GitHub Releases** по однострочнику из `README.md`; релиз — это тег
`v*` (GoReleaser в `.github/workflows/release.yml`). Push в `main` релизом не является, но
README в `main` должен описывать последний релиз.

## Структура

| Путь | Назначение |
|------|------------|
| `cmd/keenetic-tools/` | диспетчер: подкоманды и `argv[0] == awg-monitor`; встроенный маркер бинарника |
| `internal/sys/` | граница с системой: `Env` (префикс путей `Root`, `Runner` для внешних команд, часы, `Sleep`, `Kill`), атомарная запись, `flock`, `FSType` |
| `internal/sys/systest/` | фейковый роутер для тестов: `t.TempDir()` как корень, сценарные команды, фиксированное время |
| `internal/markers/` | маркеры файлов и строк crontab — **не менять** после релиза |
| `internal/crontab/` | правка `/opt/etc/crontab` по суффиксу-маркеру с бэкапом; установка/перезапуск cron |
| `internal/conf/` | разбор `KEY=value`-конфига без shell-семантики, правка с сохранением комментариев |
| `internal/selfbin/` | установка себя в `/opt/bin/keenetic-tools`, симлинк `/opt/bin/awg-monitor` |
| `internal/awgcron/` | `awg-cron` |
| `internal/monitor/` | `awg-monitor`: замер (`probe.go`), сбор (`collect.go`), снимки и diff, домены, маршрутизация, захват DNS, отчёты, bundle, установка |
| `Makefile` | `build`/`all`/`deploy`/`test`/`verify`/`clean` |
| `.goreleaser.yaml` | релизная сборка; флаги совпадают с `Makefile` (иначе `make verify` не сойдётся) |
| `docs/go-migration.md` | решения и история перехода с shell на Go |
| `scripts/add-ssh-user.sh` | Разовая shell-утилита (не часть бинарника): пользователь со входом по SSH-ключу (ключи из `/opt/root/.ssh/authorized_keys`) и правом `sudo`. Правит `/opt/etc/passwd`, `group`, `sudoers` с бэкапами `*.bak-adduser`, идемпотентна. Скачивается напрямую из `main`, поэтому push в `main` для неё — релиз; только POSIX sh (BusyBox ash). |

## Раскладка на роутере

- `/opt/bin/keenetic-tools` — бинарник; «наш», если содержит строку `# keenetic-tools: keenetic-tools`
  (`markers.Embedded`, используется в `main`, чтобы линкер её не выбросил).
- `/opt/bin/awg-monitor` — симлинк на `keenetic-tools`.
- `/opt/etc/awg-monitor.conf` — конфиг; 1-я строка `# keenetic-tools: awg-monitor`.
- `/opt/etc/crontab`: `* * * * * root /opt/bin/awg-monitor collect >/dev/null 2>&1 # keenetic-tools: awg-monitor`
  и `01 * * * * root /opt/bin/keenetic-tools awg-cron run # keenetic-tools: awg-cron`.
  Бэкапы: `crontab.awg-monitor-backup.XXXXXX`, `crontab.awg-backup.XXXXXX`.
- `/tmp/awg-restart.lock`, `/tmp/awg-monitor.lock` — `flock` с PID внутри; `/tmp/awg-restart.log` —
  лог последнего рестарта; `/tmp/awg-monitor.*` — состояние захвата DNS (RAM).
- Бинарник удаляется только последним компонентом: `awg-cron uninstall` оставляет его, если
  установлен монитор, и наоборот.

## Как работает awg-monitor

- Каталог логов задаётся `--log-dir`, хранится в конфиге (`LOG_DIR`). Установщик отказывается от
  каталогов на `tmpfs`/`ramfs`/внутренней флеш-ФС. В каталоге лежит файл-маркер `.awg-monitor`: без
  него `collect` ничего не пишет (диск отключён — не писать в пустую точку монтирования в RAM).
- `collect` (cron, раз в минуту): `flock`, замер, строка `key=value` в `samples/ДАТА.log`, события в
  `events.log` + syslog, дамп при переходе в `DOWN`, хэш конфигурации раз в `CONFIG_EVERY` минут →
  снимок при изменении, раз в сутки удаление старше `KEEP_DAYS`, состояние в `state`.
- Секреты (`PrivateKey`, `PresharedKey`) вырезаются из снимков; `bundle` проверяет архив на ключи из
  конфига AWG и удаляет его при совпадении.
- Формат строки замера читают `report` и `status`: при добавлении полей не менять смысл
  существующих ключей. Так же — `domains/*.log`, `missed/*.log`, `events.log`.
- Сетевые пробы — `net/http` с `SO_BINDTODEVICE` (`internal/monitor/net.go`), стадия сбоя
  `dns`/`tcp`/`tls`/`http`/`cert`/`iface` по `httptrace`. Внешние команды: `awg`, `ping`, `ndmc`,
  `opkg`, `ip`, `tcpdump`, `dmesg`, `top`, `free`, init-скрипты; вывод `ndmc` очищается от `ESC[K`.

## Правила для изменений

1. **Безопасность прежде всего**: бинарник работает под root на роутере. Не удалять и не
   перезаписывать то, что установщик не создавал; проверять маркер, симлинки, существование
   файлов. Новая логика должна быть идемпотентной (повторный `install` не плодит дубликаты).
2. Все пути роутера — через `env.P(...)`/методы `Env`, все внешние команды — через `env.Run`,
   время — через `env.Now`. Иначе код нельзя проверить на фейковом роутере.
3. Только stdlib и `golang.org/x/crypto/x509roots/fallback`. `CGO_ENABLED=0`.
4. Сообщения пользователю — **на русском**; комментарии в коде — на английском, коротко.
5. Не менять маркеры (`internal/markers`) и строки crontab — по ним находятся установленные копии.
6. При изменении CLI/поведения обновить `usage()` и `README.md`.
7. Флаги сборки в `Makefile` и `.goreleaser.yaml` должны совпадать; версия Go — строка `toolchain`
   в `go.mod`.
8. Окончания строк — **LF** (`.gitattributes`). Репо лежит на Windows-ФС — не допускать CRLF.

## Проверка

Роутера в окружении нет; **не запускать** `install`/`uninstall`/`collect`/`enable`/`disable`/`awg-cron`
на машине разработчика — они пишут в `/opt`, вызывают `opkg` и init-скрипты. Всё проверяется тестами
на фейковом роутере (`internal/sys/systest`):

```sh
gofmt -l .                     # пусто
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go test ./...                  # в CI ещё -race
make all                       # 4 архитектуры, размер ≤ 8 МиБ
sh -n scripts/*.sh             # синтаксис shell-утилит
git ls-files --eol             # везде lf
```

Новое поведение — новый сценарий в `*_test.go`: фикстуры вывода команд через `Runner.On`/`OnFunc`,
файлы `/proc`, `/sys`, конфиги — через `Router.Write`, сеть — `fakeNet` в тестах монитора.

## Git

- Ветка по умолчанию — `main`; GitHub: `ridhid/keenetic-tools`.
- Релиз: тег `vX.Y.Z` → GoReleaser публикует бинарники и `SHA256SUMS`, затем workflow сверяет их с `make all`.
- Не коммитить секреты, бэкапы и логи роутера (см. `.gitignore`).
