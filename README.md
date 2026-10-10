# keenetic-tools

Утилиты для Keenetic с Entware: перезапуск AmneziaWG-туннеля по расписанию и мониторинг его качества.
Один статический бинарник `keenetic-tools` без зависимостей; команды выполнять на роутере в shell Entware под root.

## Установка

Бинарники для `mipsel`, `mips`, `aarch64` и `armv7` публикуются в
[GitHub Releases](https://github.com/ridhid/keenetic-tools/releases) вместе с `SHA256SUMS`.
Архитектура роутера определяется по `opkg print-architecture`.

**1. Скачать** (нужны `curl` и `ca-bundle` — только для этой загрузки):

```sh
opkg update && opkg install curl ca-bundle && A=$(opkg print-architecture | sed -n 's/^arch \([a-z0-9]*\)-k\{0,1\}[0-9].*/\1/p' | sed 's/sf$//' | head -n 1) && echo "Архитектура: $A" && curl -fSL "https://github.com/ridhid/keenetic-tools/releases/latest/download/keenetic-tools-$A" -o /opt/keenetic-tools.part && chmod 755 /opt/keenetic-tools.part && /opt/keenetic-tools.part version
```

Сверить с `SHA256SUMS` релиза (необязательно, если есть `sha256sum`):

```sh
cd /opt && curl -fsSL https://github.com/ridhid/keenetic-tools/releases/latest/download/SHA256SUMS | grep " keenetic-tools-$A\$" | sed "s/keenetic-tools-$A\$/keenetic-tools.part/" | sha256sum -c -
```

**2. Установить нужное** — `install` копирует себя в `/opt/bin/keenetic-tools`, после чего загрузку можно удалить:

```sh
/opt/keenetic-tools.part awg-cron install                                         # перезапуск по cron
/opt/keenetic-tools.part awg-monitor install --log-dir /tmp/mnt/HDD/awg-monitor  # мониторинг
rm -f /opt/keenetic-tools.part
```

**Обновить:** повторить оба шага; настройки, логи и расписание сохраняются. Версия: `keenetic-tools version`.

Установщик никогда не перезаписывает и не удаляет файлы, которые создал не он: `/opt/bin/keenetic-tools`
и `/opt/bin/awg-monitor` должны быть его бинарником и симлинком, в `/opt/etc/crontab` меняются только строки
с маркером `# keenetic-tools: ...`, перед каждым изменением crontab сохраняется резервная копия.

## AWG: перезапуск по cron

Каждый час в :01 выполняет `/opt/etc/init.d/S52awg-opkgtun0 restart`. Если пакета `cron` нет, установщик ставит его.

Если перезапуск не завершился за 120 секунд, init-скрипт прерывается. Если предыдущий запуск ещё работает, новый пропускается.
Лог последнего запуска: `cat /tmp/awg-restart.log` (хранится в RAM, после перезагрузки роутера очищается).
При установке туннель не перезапускается.

```sh
keenetic-tools awg-cron install         # каждый час в :01
keenetic-tools awg-cron install daily   # раз в сутки в 04:02
keenetic-tools awg-cron run             # перезапустить сейчас так же, как это делает cron
keenetic-tools awg-cron uninstall       # удалить задание; пакет cron остаётся
```

## AWG: мониторинг туннеля

Раз в минуту проверяет туннель и пишет одну строку в лог: потери и RTT ping через `opkgtun0`, HTTP-запрос через
туннель, возраст handshake, трафик, доступность Endpoint мимо туннеля, PID/память/CPU `amneziawg-go`, MTU,
нагрузку роутера. Определяет состояние (`OK` / `DEGRADED` / `DOWN`) и причину сбоя, при переходе в `DOWN`
сохраняет дамп состояния роутера, следит за изменениями конфигурации и хранит её снимки для сравнения.
Ключи (`PrivateKey`, `PresharedKey`) в логи не попадают.

Логи пишутся на внешний диск, каталог задаётся при установке. Каталоги в памяти роутера (`tmpfs`) не принимаются.
Найти путь к диску: `ls /tmp/mnt/` или `df -h`.

Повторный `install` без `--log-dir` оставляет прежний каталог; с `--log-dir` — переключает на новый
(старые логи не трогаются). Настройки (цели ping, пороги, срок хранения) — в `/opt/etc/awg-monitor.conf`:
строки `НАСТРОЙКА=значение`, значения с пробелами — в одинарных кавычках. Это не shell-скрипт:
подстановки (`$VAR`, `$(...)`) не выполняются, такая строка — ошибка с номером строки.

**Команды** (`awg-monitor` — то же, что `keenetic-tools awg-monitor`):

| Команда | Что делает |
|---------|------------|
| `awg-monitor check` | замер прямо сейчас, ничего не записывает |
| `awg-monitor status` | что установлено и запущено (cron, tcpdump), сколько данных на диске и в памяти; затем состояние туннеля |
| `awg-monitor enable collect\|capture` | включить сбор по cron / поиск доменов мимо туннеля |
| `awg-monitor disable collect\|capture [--purge]` | выключить и сразу остановить; `capture --purge` удаляет и собранные данные |
| `awg-monitor report [24h\|7d]` | доля времени по состояниям, список сбоев с причинами, перезапуски, RTT, память |
| `awg-monitor snapshot [метка]` | снимок конфигурации (например, `snapshot до-смены-mtu`) |
| `awg-monitor snapshots` | список снимков |
| `awg-monitor diff [A] [B]` | разница снимков, по умолчанию двух последних |
| `awg-monitor bundle [дней]` | архив логов, снимков и дампов для разбора (по умолчанию 3 дня) |
| `awg-monitor check-domain ДОМЕН...` | проверить домены прямо сейчас: напрямую через провайдера и через туннель |
| `awg-monitor domains [24h\|7d]` | сводка проверок доменов из `DOMAINS_WATCH` |
| `awg-monitor route-check ДОМЕН` | почему домен идёт или не идёт через туннель (маршрутизация по доменам Keenetic) |
| `awg-monitor missed [24h\|7d]` | домены и соединения, которые ушли мимо туннеля и не получили ответа (нужен `DNS_CAPTURE=1`) |

Причины `DOWN`: `process_dead` — процесс `amneziawg-go` не запущен; `iface_missing` — нет интерфейса;
`handshake_never` — рукопожатия не было; `endpoint_unreachable` — handshake устарел и сервер не пингуется;
`handshake_stale` — handshake устарел, сервер доступен; `tunnel_no_traffic` — handshake свежий, но трафик
не проходит (DPI, MTU, сервер).

**Проверка доменов.** `check-domain` открывает `https://ДОМЕН/` дважды: через интерфейс провайдера
(из маршрута по умолчанию или `WAN_IFACE`) и через туннель, и показывает, на каком этапе запрос
обрывается: `dns` — имя не разрешилось, `tcp` — сервер не принял соединение, `tls` — рукопожатие
оборвано или зависло (типично для DPI), `http` — ответа нет, `cert` — чужой сертификат (заглушка),
`iface` — интерфейс недоступен. Если напрямую не открывается, а через туннель открывается, домен нужно
направить в туннель. Корневые сертификаты встроены в бинарник, `ca-bundle` для проверок не нужен.

Чтобы проверять домены постоянно, перечислите их в `/opt/etc/awg-monitor.conf`:
`DOMAINS_WATCH='youtube.com example.org'` (раз в `DOMAINS_EVERY` минут, по умолчанию 5).
Результаты — в `domains/ДАТА.log`, смена доступности — событие `domain_changed` в `events.log`.

**Отладка маршрутизации по доменам.** `route-check` проходит по цепочке: покрыт ли домен записью
`include` в группе с маршрутом `dns-proxy route object-group … OpkgTun0`; какие IP отдаёт DNS роутера;
попали ли они в runtime-список группы; через что идут текущие соединения устройств к этим IP (туннель
или провайдер, есть ли ответ); открывается ли домен напрямую и через туннель. В конце — вывод и
подсказка: добавить домен в группу, сбросить кэш DNS на устройстве, отключить DoH или IPv6.

**Поиск доменов мимо туннеля.** Установите `opkg install tcpdump` и выполните `awg-monitor enable capture`
(или `DNS_CAPTURE=1` в `/opt/etc/awg-monitor.conf`). Каждую минуту `collect` берёт ответы DNS роутера клиентам
(через `tcpdump` на `LAN_IFACE`, только в RAM) и соединения из conntrack, находит соединения устройств без ответа
или с оборванным TLS и определяет их домен. Новые домены, которых нет в списке, проверяются напрямую и через
туннель (не больше `MISSED_PROBES` в минуту, повтор через `MISSED_RECHECK` часов). `awg-monitor missed`
показывает: домены, которые нужно добавить в список (с готовыми строками `include`); домены из списка,
соединения к которым всё равно пошли мимо туннеля; соединения без запроса к DNS роутера; устройства со
своим DNS (DoH/DoT). Новый кандидат пишется событием `missed_candidate` в `events.log`.
В `missed/ДАТА.log` попадают IP устройств и домены — в `bundle` эти логи не включаются.

Структура каталога логов: `samples/ДАТА.log` — замеры, `domains/ДАТА.log` — проверки доменов,
`missed/ДАТА.log` — соединения мимо туннеля, `events.log` — события, `dumps/` — дампы при сбоях,
`snapshots/` — снимки конфигурации, `bundles/` — архивы. Замеры, дампы и архивы хранятся 14 дней.

**Разовый сбор без следов** (например, найти домены мимо туннеля):

```sh
opkg install tcpdump
awg-monitor enable capture        # через минуту запустится захват DNS
awg-monitor missed                # смотреть результаты, сколько нужно
awg-monitor disable capture --purge   # остановить tcpdump, удалить данные в памяти и на диске
opkg remove tcpdump
awg-monitor status                # убедиться, что ничего не запущено
```

**Удалить** (логи остаются; с `--purge` удаляются все следы: логи, резервные копии crontab, файлы в `/tmp`):

```sh
awg-monitor uninstall --purge
```

`/opt/bin/keenetic-tools` удаляется вместе с последним компонентом: пока установлен `awg-cron`, бинарник остаётся.
Пакеты Entware (`cron`, `curl`, `ca-bundle`, `tcpdump`) не удаляются — они могут быть нужны другим
программам; `uninstall` перечисляет установленные. Записи в системном журнале роутера остаются до его очистки.

## SSH: пользователь со входом по ключу и sudo

Создаёт пользователя (по умолчанию `admin`) с домашним каталогом `/opt/home/ИМЯ`, копирует ему ключи из
`/opt/root/.ssh/authorized_keys`, ставит `sudo` и разрешает этому пользователю `sudo`. В конце спрашивает
пароль — его будет запрашивать `sudo`. Перед изменением `/opt/etc/passwd`, `group`, `sudoers` делает копии
`*.bak-adduser`. Повторный запуск ничего не дублирует.

Нужен, если dropbear не принимает ключ для root: в сборке Entware ключи root он может искать не в `~/.ssh`,
а в `/opt/etc/dropbear/authorized_keys` (проверка: `strings $(which dropbear) | grep authorized_keys`).

Сначала положите свой публичный ключ в `/opt/root/.ssh/authorized_keys`, затем:

```sh
opkg update && opkg install curl ca-bundle && curl -fSL https://raw.githubusercontent.com/ridhid/keenetic-tools/main/scripts/add-ssh-user.sh -o /opt/add-ssh-user.sh.part && mv /opt/add-ssh-user.sh.part /opt/add-ssh-user.sh && sh /opt/add-ssh-user.sh admin
```

Проверить с компьютера, **не закрывая сессию root**: `ssh admin@192.168.1.1`, затем `sudo -i`.

## Сборка из исходников

Собирается на компьютере (Linux, macOS, WSL; нужны Go и make), не на роутере. Версия Go берётся из `go.mod`
и скачивается автоматически.

```sh
make all                                   # все архитектуры → dist/keenetic-tools-<arch>
make build ARCH=mipsel                     # одна архитектура
make deploy ROUTER=root@192.168.1.1 ARGS='awg-monitor check'   # определить архитектуру по SSH, собрать, загрузить, запустить
make test                                  # go vet + go test
make verify TAG=v1.0.0                     # собрать тег и сверить с SHA256SUMS релиза
```

`make deploy` загружает бинарник в `/opt/keenetic-tools.part`, запускает `ARGS` (по умолчанию `version`)
и удаляет загрузку. SSH Entware часто слушает порт 222: `SSH_PORT=222`.
Сборка воспроизводима: `make verify` пересобирает тег из чистого дерева и проверяет, что бинарники
релиза совпадают байт в байт.
