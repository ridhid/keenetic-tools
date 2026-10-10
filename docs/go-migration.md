# План: переход keenetic-tools с shell на Go

Цель: один статический бинарник `keenetic-tools` вместо `install-awg-cron.sh`,
`awg-monitor.sh` и тела cron-задания, с теми же командами и поведением.

Shell-версия нигде не устанавливалась, поэтому **совместимость со старыми
установками не нужна**: нет миграции, нет отката, нет требования сохранять
маркеры, форматы файлов и блокировки shell-скриптов. Скрипты служили описанием
поведения и удалены вместе с переходом (их можно посмотреть в истории git до
коммита перехода).

**Статус:** перенос сделан целиком (этапы 1–9 в коде), проверен тестами на
фейковом роутере. На железе Go-бинарник ещё не запускался — см. §10.

## 0. Решения по умолчанию (подтвердить)

| Вопрос | Решение |
|--------|---------|
| Архитектуры | `mipsel` (большинство Keenetic), `mips`, `aarch64`, `armv7` (GOARM=5). Имя ассета: `keenetic-tools-<arch>`. Имена берутся из `opkg print-architecture` без суффикса `sf`: бывают и `mipselsf-k3.4`, и `aarch64-3.10_kn` (KN-1811) |
| Доставка | GitHub Releases по тегу `v*`; однострочник берёт `releases/latest/download/...`. **Push в `main` перестаёт быть релизом** |
| Версия Go | 1.26 в `go.mod` (этого требует `x509roots/fallback`; ядра Keenetic ≥ 3.4/3.10, Go требует ≥ 3.2); CGO выключен; версия тулчейна для релизов зафиксирована (`toolchain` в `go.mod`) |
| Сборка | на роутере **не собираем** (нет Go в Entware, сотни МБ RAM/диска, десятки минут). Из исходников — кросс-сборка на ПК через `Makefile` (`make deploy` сам определяет архитектуру роутера по SSH); релизы — GoReleaser в CI. См. §2a |
| Зависимости | только stdlib + `golang.org/x/crypto/x509roots/fallback` (встроенные корневые сертификаты, без `ca-bundle`) |
| Размер | итог на Go 1.26.3: mipsel/mips **7,8 МБ**, armv7 6,9 МБ, aarch64 6,6 МБ (`net/http`+TLS, встроенные корни, tar/gzip, syslog). Бюджет — ≤ 8 МиБ, проверяется в CI; upx не используем (ломает отладку и иногда антивирусы) |
| Внешние утилиты | остаются через `exec`: `awg`, `ndmc`, `opkg`, `ping`, `tcpdump`, `ip` (только в дампах и снимках), `dmesg`, `top`, `free`, init-скрипты. Заменяются Go: `curl`, `nslookup`, `md5sum`, `tar`/`gzip`, `diff`, `find`, `du`, `date -d`, `pgrep`, `logger` (с fallback на exec), все `awk`/`sed` |

## 1. Раскладка на роутере

| Путь | Что | Признак «наш» |
|------|-----|---------------|
| `/opt/bin/keenetic-tools` | бинарник | ELF со встроенной строкой `\n# keenetic-tools: keenetic-tools\n` |
| `/opt/bin/awg-monitor` | симлинк `keenetic-tools`; диспетчеризация по `argv[0]` | симлинк именно на `keenetic-tools` |
| `/opt/etc/awg-monitor.conf` | конфиг | 1-я строка `# keenetic-tools: awg-monitor` |
| crontab, монитор | `* * * * * root /opt/bin/awg-monitor collect >/dev/null 2>&1 # keenetic-tools: awg-monitor` | суффикс |
| crontab, рестарт | `01 * * * * root /opt/bin/keenetic-tools awg-cron run # keenetic-tools: awg-cron` | суффикс |
| `/tmp/awg-restart.lock`, `/tmp/awg-monitor.lock` | `flock`-блокировки с PID внутри | — |

Маркеры — в `internal/markers`; после первого релиза их менять нельзя.
Встроенная строка-маркер бинарника — глобальная `var`, используется в `main`
(иначе линкер её выбросит); тест проверяет её в собранных бинарниках всех архитектур.

Бинарник удаляется только когда ни одного компонента не осталось
(нет строк crontab ни с одним маркером, нет конфига монитора и симлинка `awg-monitor`).

## 2. CLI

```
keenetic-tools awg-cron install [hourly|daily] | uninstall | run
keenetic-tools awg-monitor <команда как сейчас>
awg-monitor <команда как сейчас>              # через argv[0]
keenetic-tools version
```

Команды `awg-monitor` — как в shell-версии: `install [--log-dir DIR]`, `uninstall [--purge]`,
`status`, `enable|disable collect|capture [--purge]`, `check`, `report [24h|7d]`,
`snapshot [метка]`, `snapshots`, `diff [A] [B]`, `bundle [дней]`, `check-domain`,
`domains`, `route-check`, `missed`, `collect`. Сообщения на русском.

Однострочник (README):

```sh
A=$(opkg print-architecture | sed -n 's/^arch \([a-z0-9]*\)-k\{0,1\}[0-9].*/\1/p' | sed 's/sf$//' | head -n 1) && curl -fSL "https://github.com/ridhid/keenetic-tools/releases/latest/download/keenetic-tools-$A" -o /opt/keenetic-tools.part && chmod 755 /opt/keenetic-tools.part && /opt/keenetic-tools.part awg-monitor install --log-dir /tmp/mnt/HDD/awg-monitor; rm -f /opt/keenetic-tools.part
```

`install`, запущенный не из `/opt/bin/keenetic-tools`, копирует себя туда (`/proc/self/exe`,
tmp + rename). `curl` нужен только для этой загрузки; `ca-bundle` — тоже только для неё.
Вариант однострочника со сверкой: дополнительно скачать `SHA256SUMS` и проверить
`sha256sum -c` (BusyBox) перед запуском — в README как «установка с проверкой».

## 2a. Сборка из исходников (`Makefile`)

Собирается на компьютере пользователя (Linux, macOS, WSL; нужны Go и make), не на роутере.

| Цель | Что делает |
|------|------------|
| `make build ARCH=mipsel` | одна архитектура → `dist/keenetic-tools-<arch>` |
| `make all` | все архитектуры из таблицы ниже |
| `make deploy ROUTER=root@192.168.1.1 [ARGS='awg-monitor install --log-dir ...']` | определяет архитектуру по SSH, собирает, `scp` в `/opt/keenetic-tools.part`, запускает `ARGS` (по умолчанию `version`), удаляет `.part` |
| `make test` | `go vet` + `go test ./...` |
| `make verify TAG=v1.0.0` | собирает из тега в чистом worktree и сравнивает хэши с `SHA256SUMS` релиза |
| `make clean` | удалить `dist/` |

Определение архитектуры:
`ssh $(ROUTER) /opt/bin/opkg print-architecture | sed -n 's/^arch \([a-z0-9]*\)-k\{0,1\}[0-9].*/\1/p' | sed 's/sf$//' | head -n 1`
(порт SSH — `SSH_PORT`, по умолчанию 22; у многих Keenetic Entware слушает 222)

| Entware | `GOARCH` | доп. переменные |
|---------|----------|-----------------|
| `mipsel` (`mipselsf`) | `mipsle` | `GOMIPS=softfloat` |
| `mips` (`mipssf`) | `mips` | `GOMIPS=softfloat` |
| `aarch64` | `arm64` | — |
| `armv7` (`armv7sf`) | `arm` | `GOARM=5` |

Неизвестная архитектура → ошибка с выводом `opkg print-architecture`.

Общие флаги для `make` и GoReleaser (должны совпадать, иначе `verify` не сойдётся):
`CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=$(VERSION)"`,
`VERSION` = `git describe --tags --always`. Воспроизводимость проверяется в CI:
двойная сборка одного коммита → одинаковые хэши.

`Makefile` использует только POSIX make + `ssh`/`scp`; Taskfile/just не нужны.

## 2b. Факты с роутера (KN-1811, проверено только чтением)

Проверено 2026-10-10 по SSH под `admin` (не root), без изменений на роутере.

| Что | Результат | Вывод для Go-версии |
|-----|-----------|---------------------|
| Модель, прошивка | Keenetic Ultra KN-1811, KeeneticOS 5.1.5, ядро 4.9-ndm-5, aarch64, 490 МБ RAM | Go ≥ 1.26 работает на ядрах ≥ 3.2 |
| `opkg print-architecture` | `aarch64-3.10`, `aarch64-3.10_kn` — **без `k`** | регулярное выражение исправлено (§2, §2a) |
| `/opt` | ext4 на USB-диске, 423 ГБ свободно | размер бинарника здесь не важен |
| Корневые сертификаты | `/etc/ssl/certs` есть (каталог прошивки), Entware — `/opt/etc/ssl/certs/ca-certificates.crt` | встроенные корни (`x509roots/fallback`) остаются страховкой |
| DNS | `/etc/resolv.conf`: `nameserver 127.0.0.1`; `nslookup` выдаёт `Address 1: IP имя` | резолвер Go читает тот же файл |
| `/dev/log` | есть (сокет) | `log/syslog` без внешнего `logger` |
| `awg show`, `/proc/net/nf_conntrack` | без root недоступны | формат `awg show dump` проверить под root (этап 4); при неожиданном формате — три отдельных вызова, как в shell |
| curl `--interface opkgtun0` | работает (301 от youtube.com), напрямую через `eth3` — таймаут (rc 28) | хороший живой сценарий для `check-domain`. Без root curl, видимо, привязывается к адресу интерфейса, а не через `SO_BINDTODEVICE`; Go под root использует `SO_BINDTODEVICE` |
| `pgrep -f "amneziawg-go opkgtun0"` | находит и сам процесс, и shell, в чьей командной строке есть эта строка | в Go сверять `argv[0]` (basename `amneziawg-go`) и `argv[1]`, а не подстроку |
| вывод `ndmc` | начинается с управляющей последовательности `ESC[K` | парсеры вывода `ndmc` должны её отбрасывать |
| `ndmc -c 'show object-group fqdn ГРУППА'` | блоки `fqdn:` / `address:` / `ttl:`, есть `parent:` | формат совпадает с тем, что читает `route-check` |
| `dns-proxy` | `route object-group domain-list0 OpkgTun0 auto reject` | 4-е поле — интерфейс |
| cron | не установлен (`/opt/etc/crontab`, `S10cron` нет) | первый `awg-cron install` поставит пакет `cron` |
| Не проверено | запуск Go-бинарника на роутере (нужна загрузка во `/tmp` и root), `SO_BINDTODEVICE` из Go, стадии `tls`/`cert` против curl | `awg-monitor check` / `check-domain` под root — первый шаг §10 |

## 3. Структура репозитория

```
go.mod                          module github.com/ridhid/keenetic-tools; toolchain закреплён
Makefile                        build / all / deploy / test / verify / clean (§2a)
.goreleaser.yaml                релизная сборка 4 арх + SHA256SUMS, те же флаги, что в Makefile
cmd/keenetic-tools/main.go      диспетчер по argv[0] и подкомандам
internal/markers/               маркеры файлов и строк crontab
internal/sys/                   Env: Root (префикс путей), Runner (exec, фоновый Start), Now, Sleep, Kill;
                                Owned(), атомарная запись, flock, fs_type по /proc/mounts
internal/sys/systest/           фейковый роутер для тестов
internal/selfbin/               установка бинарника в /opt/bin, симлинк awg-monitor
internal/crontab/               удаление строк по суффиксу, добавление, бэкап *.XXXXXX; установка cron
internal/conf/                  разбор/запись KEY=value-конфига (§5)
internal/awgcron/               install/uninstall/run
internal/monitor/               один пакет, файлы по темам:
  monitor.go config.go          диспетчер команд, настройки и текст конфига по умолчанию
  probe.go collect.go           замер, классификация, collect, события, дамп, check
  snapshot.go diff.go           config_stable/info, снимки, unified diff
  domains.go net.go             нормализация, параллельные пробы, HTTPS с SO_BINDTODEVICE + httptrace, syslog
  routing.go capture.go         running-config, conntrack, route-check; tcpdump, harvest, missed_collect
  report.go bundle.go           report / domains / missed, tar.gz с проверкой на ключи
  install.go                    install/uninstall/enable/disable/status
.github/workflows/ci.yml        gofmt, vet, staticcheck, test -race, кросс-сборка, размер, воспроизводимость, goreleaser check
.github/workflows/release.yml   по тегу v*: тесты → GoReleaser → сверка релиза с make all
```

Подпакеты `internal/monitor/*` из первоначального плана не понадобились: части монитора делят
конфиг, `Env` и форматы логов, а один пакет проще читать и тестировать.

Каждый путь (`/opt/...`, `/tmp/awg-*`, `/proc`, `/sys/class/net`) берётся через `Env.Root`,
поэтому тесты гоняют полные сценарии в `t.TempDir()` с фейковым `Runner` — без `sed` по копиям.

## 4. Перенос по частям

### awg-cron (готово)
- `install`: root; исполняемый `$SERVICE`; `opkg install cron` при отсутствии `S10cron`;
  бинарник в `/opt/bin` (чужой файл → отказ); crontab с бэкапом; рестарт `S10cron`;
  туннель не трогается.
- `run`: `flock` на `/tmp/awg-restart.lock`; занят → строка
  `... skipped: previous run (PID N) is still active` в конец лога; лог `/tmp/awg-restart.log`
  усекается; stdout/stderr init-скрипта — в лог (файл, а не pipe: демон, оставшийся
  с открытым логом, не держит задание); таймаут 120 с → `SIGKILL` только init-скрипту,
  код 137 и строка `restart timed out after 120s and was killed`; `restart exit code: N`.
- `uninstall`: строка crontab; бинарник — если `awg-monitor` не установлен; пакет cron остаётся.

### awg-monitor: форматы
Совместимость со старыми файлами не нужна, но форматы shell-версии разумные
(`key=value` в строке замера, `events.log`, `state`, снимки с `## stable` / `## info`),
поэтому берём их как есть и меняем, только если есть причина. Строка замера —
контракт между `collect` и `report`/`status`: новые ключи добавлять, смысл
существующих не менять. `cpu` — в тиках USER_HZ=100, счётчики rx/tx — `uint64`.
Временные файлы в `/tmp` — по необходимости Go-реализации (lock-каталог с файлами
внутри не нужен: параллельные пробы — горутины).

### awg-monitor: что меняется внутри
- `awg show IFACE latest-handshakes|transfer|endpoints` — три вызова, как в shell: формат проверен,
  а `dump` выводит приватный ключ интерфейса, которого монитору лучше не видеть вовсе.
- HTTP-проверка и `dom_probe` — `net/http` c `net.Dialer.Control` → `syscall.BindToDevice`,
  `httptrace` даёт время connect/TLS. Таблица соответствия кодам curl:
  `*net.DNSError` → `dns`; ошибка/таймаут до `ConnectDone` → `tcp`; после connect до
  `TLSHandshakeDone` → `tls`; `x509.*Error`/`tls.CertificateVerificationError` → `cert`;
  ошибка bind (`ENODEV`/`EPERM`) → `iface`; после TLS без ответа → `http`; таймаут во время
  резолва (`DNSStart` без `DNSDone`) → `dns`. Стадия `ca` исчезает (корни встроены).
- `nslookup` → `net.Resolver` с `Dial` на `DNS_SERVER:53`.
- адреса интерфейсов — `net.InterfaceByName().Addrs()`; маршрут по умолчанию — `/proc/net/route`.
- lock `collect` — `flock` на `/tmp/awg-monitor.lock` вместо каталога с pid; `uninstall` удаляет файл.
- `curl` больше не нужен ни монитору, ни установщику — только однострочнику для загрузки.
- `dom_run` — горутины вместо фоновых процессов.
- поиск `amneziawg-go`: `argv[0]`/`argv[1]` из `/proc/*/cmdline`, а не подстрока (§2b).
- `tcpdump` остаётся отдельным фоновым процессом (`Setsid`, вывод в файл с `O_APPEND`),
  `cap_running` по-прежнему проверяет `/proc/PID/cmdline`. Замена tcpdump на
  `AF_PACKET` + `dnsmessage` — отдельная задача после релиза.
- `diff` — свой unified diff (LCS по строкам, снимки маленькие).
- `bundle` — `archive/tar` + `compress/gzip` во временный `.part`, затем проверка на
  ключи из `AWG_CONF` и `rename`.
- `debug.SetMemoryLimit(16 MiB)` для команд монитора, `GOMAXPROCS` не трогаем.

## 5. Конфиг `/opt/etc/awg-monitor.conf`

`KEY=value`, `KEY='value'`, `KEY="value"` (без подстановок), комментарии `#`, пустые строки.
Всё, что похоже на shell (`$VAR`, `$(...)`, `` `...` ``, `export`, несколько слов без
кавычек), → ошибка с номером строки, а не тихое неверное чтение. Неизвестные ключи —
предупреждение. Правка сохраняет комментарии и порядок строк. Реализовано в `internal/conf`.

## 6. Совместимость

Не нужна (см. начало). На роутере владельца ничего из shell-версии не стоит (§2b).

## 7. Тесты

**Юнит/сценарные (`go test`, без роутера)** — фейковый `Runner` отдаёт фикстуры,
`Env.Root` = `t.TempDir()`, часы фиксированы:
- awg-cron (готово): чистая установка; установка пакета cron; повторная (идемпотентность,
  бэкап только при изменении); hourly→daily; отказы (нет root, нет сервиса, чужой бинарник)
  без изменений; uninstall с бинарником и без (если нужен монитору); `run`: успех,
  ненулевой код, сервис не найден (127), пропуск при занятом lock, ничейный lock-файл;
  на реальных процессах — таймаут → 137 и демон с открытым логом.
- collect: OK; потери; высокий RTT; http_fail; DOWN по каждой причине
  (`process_dead`, `iface_missing`, `handshake_never`, `endpoint_unreachable`,
  `handshake_stale`, `tunnel_no_traffic`) + дамп; возврат из DOWN с `down_for`;
  `process_restarted`; `endpoint_changed`; смена конфига → снимок; отключённый диск
  (нет маркера → ничего не пишется, одна запись в syslog, tcpdump остановлен); занятый lock;
  уборка старше `KEEP_DAYS`; ротация events.log.
- report/domains/missed/status на фикстурных логах.
- domain-проба: `httptest` + искусственные сбои на каждой стадии.
- route-check: фикстуры `show running-config`, `show object-group fqdn`, `/proc/net/nf_conntrack`
  (форматы сняты с KN-1811, §2b; реальные данные в репо не кладём).
- bundle: архив без ключей; ключ в логах → архив удалён, ошибка.
- install/uninstall `--purge`/enable/disable.

Поведение shell-скриптов — спецификация: тесты пишутся по ним, отдельного
паритетного прогона через busybox не делаем.

**CI**: `gofmt`, `go vet`, `staticcheck`, `go test -race` (amd64), кросс-сборка 4 архитектур,
позже — `qemu-user-static`: `keenetic-tools version` и `awg-monitor report` на фикстуре,
проверка размера бинарника (≤ 8 МБ), воспроизводимость (две сборки → один хэш;
хэши `make all` совпадают с `goreleaser build --snapshot`), `git ls-files --eol`.

**На роутере** (вручную, чек-лист в PR): `check`, `status`, `check-domain youtube.com`,
`route-check`, 10 минут `collect` по cron, `report`, `bundle`, `enable/disable capture`,
`awg-cron install daily` + ручной `awg-cron run`, `uninstall --purge`.

## 8. Этапы

| # | Что | Статус |
|---|-----|--------|
| 0 | Проверка подхода на железе | ⚠️ сделано только чтением (§2b); запуск Go-бинарника — после ревью PR, §10 |
| 1 | Каркас: `go.mod`, `cmd/`, `internal/sys`, `crontab`, `conf`, маркеры, `Makefile`, CI | ✅ |
| 2 | `awg-cron` целиком + тесты | ✅ |
| 3 | `.goreleaser.yaml`, релизный workflow, `make verify`, однострочник | ✅ в коде; сборки GoReleaser и `make all` совпадают побайтно (проверено локально); релиз — тег после мержа |
| 4 | Монитор: probe/classify/collect/state/events/dump/snapshot/diff, `check`, `status`, `snapshot(s)` | ✅ |
| 5 | `report`, `domains`, `bundle` | ✅ |
| 6 | Домены: `check-domain`, `route-check`, `DOMAINS_WATCH` | ✅ |
| 7 | Захват: tcpdump, harvest, `missed_collect`, `missed`, `enable/disable capture` | ✅ |
| 8 | Монитор: install/uninstall/enable/disable | ✅ |
| 9 | Удалить `*.sh`, `awg-restart`, пробник; README и AGENTS.md | ✅ |

## 9. Риски

- **Место во флеше** — 7,8 МБ на mips, если Entware стоит во внутренней памяти.
  `-s -w` уже включён; дальше только upx или отказ от `net/http` (нежелательно).
- **Поведение HTTPS-проб** отличается от curl в пограничных случаях (порядок резолва
  A/AAAA, таймауты, SNI) — проверяется на первом шаге §10 (`check-domain` против браузера/`curl --interface`).
- **Конфиг с shell-синтаксисом** (если пользователь по привычке напишет `$VAR`) —
  явная ошибка с номером строки.
- **Доверие к бинарнику** вместо читаемого скрипта: сборка только в CI из тега,
  `SHA256SUMS` в релизе, воспроизводимая сборка; любой может собрать сам
  (`make deploy`) или сверить релиз со своей сборкой (`make verify`).

## 10. Проверка на роутере (после ревью, вручную)

Порядок — от команд, которые ничего не меняют, к установке. Каждый шаг можно остановить.

1. **Только чтение, без установки.** Загрузить бинарник во `/tmp` (RAM, исчезнет при перезагрузке)
   и запустить под root: `version`, `awg-monitor check`, `awg-monitor check-domain youtube.com`,
   `awg-monitor route-check youtube.com`, `awg-monitor status`. Эти команды ничего не пишут
   (кроме DNS-запроса к роутеру в `route-check`). Сверить: PID и RSS `amneziawg-go`, handshake,
   стадии проверок доменов против ожидаемых (через туннель — `ok`, напрямую — как в браузере).
   С ПК: `make deploy ROUTER=root@192.168.1.1 ARGS='awg-monitor check'` (загрузка в `/opt/*.part`,
   запуск, удаление).
2. **Монитор.** `awg-monitor install --log-dir /tmp/mnt/<диск>/awg-monitor`; через 10 минут
   `status`, `report`; затем `snapshot`, `diff`, `bundle`.
3. **Захват** (по желанию): `opkg install tcpdump`, `enable capture`, `missed`, `disable capture --purge`.
4. **Рестарт.** `awg-cron install daily`, ручной `awg-cron run` в удобный момент (рвёт туннель
   на время рестарта), `cat /tmp/awg-restart.log`.
5. **Удаление.** `awg-monitor uninstall --purge`, `awg-cron uninstall`; `status` показывает, что
   ничего не осталось.

