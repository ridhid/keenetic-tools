# keenetic-tools

Скрипты и инструкции для моего роутера Keenetic с Entware (`opkg`).
Сейчас доступен автоматический перезапуск AWG по cron; сюда же можно добавлять
другие утилиты для обслуживания роутера.

## Установка AWG cron одной командой

Выполните в shell Entware под root, где работает `opkg`.
AWG уже должен быть установлен: ожидается исполняемый файл
`/opt/etc/init.d/s52awg-opgktun0`.

```sh
opkg update && opkg install curl ca-bundle && curl -fSL https://raw.githubusercontent.com/ridhid/keenetic-tools/main/install-awg-cron.sh -o /opt/install-awg-cron.sh.part && mv /opt/install-awg-cron.sh.part /opt/install-awg-cron.sh && sh /opt/install-awg-cron.sh install
```

Команда скачивает установщик, сохраняет его в `/opt/install-awg-cron.sh`
и включает перезапуск каждый час в :01. Git и авторизация GitHub на роутере
не нужны. При ошибке скачивания установщик не запускается.

Удаление одной командой:

```sh
sh /opt/install-awg-cron.sh uninstall
```

## Ручная установка и настройка

Скопируйте один файл `install-awg-cron.sh` на роутер, например в `/opt/`.
Команды выполняются под root в shell Entware, где работает `opkg`.

Установить перезапуск каждый час в :01:

```sh
sh /opt/install-awg-cron.sh install
```

Переключить на раз в сутки в 04:02 по времени cron:

```sh
sh /opt/install-awg-cron.sh install daily
```

Вернуться к почасовому режиму: `sh /opt/install-awg-cron.sh install hourly`.
Без аргументов установщик выбирает `install hourly`. Повторная установка
обновляет расписание, не добавляя дубликат.

Удалить установленное задание и его скрипт:

```sh
sh /opt/install-awg-cron.sh uninstall
```

Сам установщик, лог последнего запуска, резервные копии crontab и пакет cron
остаются. Другие задания не удаляются. Повторное удаление допустимо.

Установщик проверяет `/opt/etc/init.d/s52awg-opgktun0`, при необходимости
устанавливает пакет `cron`, создаёт `/opt/sbin/awg-scheduled-restart` и добавляет
помеченную строку в `/opt/etc/crontab`. Перед изменением расписания создаётся
уникальная резервная копия `/opt/etc/crontab.awg-backup.*`.
После изменения cron перезапускается. Стандартный init Entware запускает cron
при загрузке роутера, после подключения `/opt`.

Прежний файл `awg-restart` из этого проекта в `cron.hourly` или `cron.daily`
автоматически заменяется новой установкой, если команды совпадают с исходным
файлом или примером из чата (комментарии и пустые строки не учитываются).
Если команды изменены, установщик останавливается с пояснением: перенесите его за пределы
каталогов cron.* и повторите установку. Прямые задания перезапуска, ранее
добавленные вручную в crontab, нужно удалить самостоятельно.

Проверка расписания и лога после первого запуска:

```sh
grep 'keenetic-awg-restart:' /opt/etc/crontab
cat /tmp/awg-restart.log
```

Принудительная проверка с немедленным перезапуском туннеля:

```sh
/opt/sbin/awg-scheduled-restart
cat /tmp/awg-restart.log
```

Лог в `/tmp` перезаписывается при каждом запуске и исчезает после перезагрузки.
`restart exit code: 0` означает успешное завершение init-скрипта, но не проверку
связи через туннель. При рестарте возможен краткий обрыв соединений через него.
Установка и удаление сами туннель не перезапускают.

Источник: [документация Entware по cron](https://github.com/Entware/Entware/wiki/Using-Cron).
