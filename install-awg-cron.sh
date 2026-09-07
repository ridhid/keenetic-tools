#!/bin/sh
# Standalone installer for Keenetic / Entware. Run as root.
set -eu
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

JOB=/opt/sbin/awg-scheduled-restart
CRONTAB=/opt/etc/crontab
CRON_INIT=/opt/etc/init.d/S10cron
SERVICE=/opt/etc/init.d/s52awg-opgktun0
MARKER='# keenetic-awg-restart: managed by install-awg-cron.sh'
WORK=

die() { echo "Ошибка: $*" >&2; exit 1; }
cleanup() { [ -z "$WORK" ] || rm -rf "$WORK"; }
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM

usage() {
    echo "Использование: sh $0 install [hourly|daily] | uninstall"
    echo '  install         — каждый час в :01 (по умолчанию)'
    echo '  install daily   — раз в сутки в 04:02 по времени cron'
    echo '  uninstall       — удалить задание и скрипт; оставить пакет cron'
}

ACTION=${1:-install}
FREQUENCY=${2:-hourly}
case "$ACTION" in
    -h|--help|help) usage; exit 0 ;;
    install)
        [ "$#" -le 2 ] || die 'Слишком много аргументов.'
        case "$FREQUENCY" in
            hourly) SCHEDULE='01 * * * *' ;;
            daily) SCHEDULE='02 4 * * *' ;;
            *) usage; exit 1 ;;
        esac ;;
    uninstall) [ "$#" -eq 1 ] || die 'uninstall не принимает дополнительных аргументов.' ;;
    *) usage; exit 1 ;;
esac
[ "$(id -u)" = 0 ] || die 'Запустите скрипт под root в shell Entware.'
WORK=$(mktemp -d /tmp/awg-cron-install.XXXXXX)

# Earlier script, used only for safe migration (ignore comments/blank lines).
cat > "$WORK/legacy" <<'EOF'
#!/bin/sh
# Run from /opt/etc/cron.hourly/awg-restart.
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

# Keep only the latest run's output in RAM.
exec > /tmp/awg-restart.log 2>&1
date '+%Y-%m-%d %H:%M:%S %Z'
/opt/etc/init.d/s52awg-opgktun0 restart
status=$?
echo "restart exit code: $status"
exit "$status"
EOF
sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$WORK/legacy" > "$WORK/legacy-code"
is_legacy() {
    [ ! -L "$1" ] && [ -f "$1" ] || return 1
    sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$1" > "$WORK/candidate-code"
    cmp -s "$WORK/candidate-code" "$WORK/legacy-code"
}

if [ -e "$JOB" ] || [ -L "$JOB" ]; then
    [ ! -L "$JOB" ] && [ -f "$JOB" ] && grep -Fqx "$MARKER" "$JOB" ||
        die "$JOB уже существует и не принадлежит установщику. Файл не изменён."
fi

if [ "$ACTION" = install ]; then
    [ -x "$SERVICE" ] || die "Не найден исполняемый $SERVICE. Проверьте имя и регистр букв."
    for old in /opt/etc/cron.hourly/awg-restart /opt/etc/cron.daily/awg-restart; do
        if [ -e "$old" ] || [ -L "$old" ]; then
            is_legacy "$old" ||
                die "Найден прежний $old с другим содержимым. Перенесите его за пределы cron.* и повторите установку."
        fi
    done
    if [ ! -x "$CRON_INIT" ]; then
        command -v opkg >/dev/null 2>&1 || die 'opkg не найден. Нужен shell Entware.'
        opkg update
        opkg install cron
    fi
    [ -x "$CRON_INIT" ] || die "После установки не найден $CRON_INIT."
    [ -f "$CRONTAB" ] || die "Не найден $CRONTAB. Проверьте установку cron."
    mkdir -p /opt/sbin
    {
        echo '#!/bin/sh'
        echo "$MARKER"
        cat <<'EOF'
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
exec > /tmp/awg-restart.log 2>&1
date '+%Y-%m-%d %H:%M:%S %Z'
/opt/etc/init.d/s52awg-opgktun0 restart
status=$?
echo "restart exit code: $status"
exit "$status"
EOF
    } > "$WORK/job"
    cp "$WORK/job" "$JOB"
    chmod 755 "$JOB"
fi

if [ -f "$CRONTAB" ]; then
    # Preserve unrelated entries; remove only lines carrying our exact suffix.
    awk -v marker="$MARKER" '
        length($0) < length(marker) ||
        substr($0, length($0) - length(marker) + 1) != marker { print }
    ' "$CRONTAB" > "$WORK/crontab"
    if [ "$ACTION" = install ]; then
        printf '%s root %s %s\n' "$SCHEDULE" "$JOB" "$MARKER" >> "$WORK/crontab"
    fi
    if ! cmp -s "$CRONTAB" "$WORK/crontab"; then
        BACKUP=$(mktemp "${CRONTAB}.awg-backup.XXXXXX")
        cp -p "$CRONTAB" "$BACKUP"
        cat "$WORK/crontab" > "$CRONTAB"
        echo "Резервная копия расписания: $BACKUP"
    fi
fi

if [ "$ACTION" = install ]; then
    for old in /opt/etc/cron.hourly/awg-restart /opt/etc/cron.daily/awg-restart; do
        if is_legacy "$old"; then
            rm -f "$old"
            echo "Прежний скрипт $old заменён новой установкой."
        fi
    done
    "$CRON_INIT" restart
    echo "Установлено: $SCHEDULE — $SERVICE restart"
    echo 'Лог последнего запуска: /tmp/awg-restart.log'
    echo 'Туннель сейчас не перезапускался. Первый запуск — по расписанию.'
else
    rm -f "$JOB"
    if [ -x "$CRON_INIT" ]; then "$CRON_INIT" restart; fi
    echo 'Задание и установленный скрипт удалены. Пакет cron и другие задания сохранены.'
fi
