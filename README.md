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
| `awg-monitor status` | текущее состояние, последний замер и события |
| `awg-monitor report [24h\|7d]` | доля времени по состояниям, список сбоев с причинами, перезапуски, RTT, память |
| `awg-monitor snapshot [метка]` | снимок конфигурации (например, `snapshot до-смены-mtu`) |
| `awg-monitor snapshots` | список снимков |
| `awg-monitor diff [A] [B]` | разница снимков, по умолчанию двух последних |
| `awg-monitor bundle [дней]` | архив логов, снимков и дампов для разбора (по умолчанию 3 дня) |

Причины `DOWN`: `process_dead` — процесс `amneziawg-go` не запущен; `iface_missing` — нет интерфейса;
`handshake_never` — рукопожатия не было; `endpoint_unreachable` — handshake устарел и сервер не пингуется;
`handshake_stale` — handshake устарел, сервер доступен; `tunnel_no_traffic` — handshake свежий, но трафик
не проходит (DPI, MTU, сервер).

Структура каталога логов: `samples/ДАТА.log` — замеры, `events.log` — события, `dumps/` — дампы при сбоях,
`snapshots/` — снимки конфигурации, `bundles/` — архивы. Замеры, дампы и архивы хранятся 14 дней.

**Удалить** (логи остаются; с `--purge` — удаляются):

```sh
awg-monitor uninstall
```

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
