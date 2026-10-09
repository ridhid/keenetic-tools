# keenetic-tools

Утилиты для Keenetic с Entware. Команды выполнять на роутере в shell Entware под root.

## AWG: перезапуск по cron

Каждый час в :01 выполняет `/opt/etc/init.d/S52awg-opkgtun0 restart`.

Если перезапуск не завершился за 120 секунд, он прерывается. Если предыдущий запуск ещё работает, новый пропускается.
Лог последнего запуска: `cat /tmp/awg-restart.log` (хранится в RAM, после перезагрузки роутера очищается).

**Установить / обновить:**

```sh
opkg update && opkg install curl ca-bundle && curl -fSL https://raw.githubusercontent.com/ridhid/keenetic-tools/main/install-awg-cron.sh -o /opt/install-awg-cron.sh.part && mv /opt/install-awg-cron.sh.part /opt/install-awg-cron.sh && sh /opt/install-awg-cron.sh install
```

**Раз в сутки в 04:02 вместо каждого часа:**

```sh
sh /opt/install-awg-cron.sh install daily
```

**Удалить:**

```sh
sh /opt/install-awg-cron.sh uninstall
```

## AWG: мониторинг туннеля

Раз в минуту проверяет туннель и пишет одну строку в лог: потери и RTT ping через `opkgtun0`, HTTP-запрос через
туннель, возраст handshake, трафик, доступность Endpoint мимо туннеля, PID/память/CPU `amneziawg-go`, MTU,
нагрузку роутера. Определяет состояние (`OK` / `DEGRADED` / `DOWN`) и причину сбоя, при переходе в `DOWN`
сохраняет дамп состояния роутера, следит за изменениями конфигурации и хранит её снимки для сравнения.
Ключи (`PrivateKey`, `PresharedKey`) в логи не попадают.

Логи пишутся на внешний диск, каталог задаётся при установке. Каталоги в памяти роутера (`tmpfs`) не принимаются.
Найти путь к диску: `ls /tmp/mnt/` или `df -h`.

**Установить / обновить** (замените путь к диску на свой):

```sh
opkg update && opkg install curl ca-bundle && curl -fSL https://raw.githubusercontent.com/ridhid/keenetic-tools/main/awg-monitor.sh -o /opt/awg-monitor.sh.part && mv /opt/awg-monitor.sh.part /opt/awg-monitor.sh && sh /opt/awg-monitor.sh install --log-dir /tmp/mnt/HDD/awg-monitor
```

Повторный `install` без `--log-dir` оставляет прежний каталог; с `--log-dir` — переключает на новый
(старые логи не трогаются). Настройки (цели ping, пороги, срок хранения) — в `/opt/etc/awg-monitor.conf`.

**Команды:**

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
оборвано или зависло (типично для DPI), `http` — ответа нет, `cert` — чужой сертификат (заглушка).
Если напрямую не открывается, а через туннель открывается, домен нужно направить в туннель.

Чтобы проверять домены постоянно, перечислите их в `/opt/etc/awg-monitor.conf`:
`DOMAINS_WATCH='youtube.com example.org'` (раз в `DOMAINS_EVERY` минут, по умолчанию 5).
Результаты — в `domains/ДАТА.log`, смена доступности — событие `domain_changed` в `events.log`.

**Отладка маршрутизации по доменам.** `route-check` проходит по цепочке: покрыт ли домен записью
`include` в группе с маршрутом `dns-proxy route object-group … OpkgTun0`; какие IP отдаёт DNS роутера;
попали ли они в runtime-список группы; через что идут текущие соединения устройств к этим IP (туннель
или провайдер, есть ли ответ); открывается ли домен напрямую и через туннель. В конце — вывод и
подсказка: добавить домен в группу, сбросить кэш DNS на устройстве, отключить DoH или IPv6.

**Поиск доменов мимо туннеля.** Установите `opkg install tcpdump` и включите `DNS_CAPTURE=1` в
`/opt/etc/awg-monitor.conf`. Каждую минуту `collect` берёт ответы DNS роутера клиентам (через `tcpdump`
на `LAN_IFACE`, только в RAM) и соединения из conntrack, находит соединения устройств без ответа или с
оборванным TLS и определяет их домен. Новые домены, которых нет в списке, проверяются напрямую и через
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

**Удалить** (логи остаются; с `--purge` удаляются все следы: логи, резервные копии crontab,
копия установщика `/opt/awg-monitor.sh`, файлы в `/tmp`):

```sh
awg-monitor uninstall --purge
```

Пакеты Entware (`cron`, `curl`, `ca-bundle`, `tcpdump`) не удаляются — они могут быть нужны другим
программам; `uninstall` перечисляет установленные. Записи в системном журнале роутера остаются до его очистки.
