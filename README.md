# keenetic-tools

Утилиты для Keenetic с Entware. Команды выполнять на роутере в shell Entware под root.

## AWG: перезапуск по cron

Каждый час в :01 выполняет `/opt/etc/init.d/S52awg-opkgtun0 restart`.

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
