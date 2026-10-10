#!/bin/sh
# Create a sudo user with SSH key login on Keenetic / Entware. Run as root:
#   sh add-ssh-user.sh [USER]
# Entware's dropbear may carry the OpenWrt patch that reads root's keys from
# /opt/etc/dropbear/authorized_keys instead of ~/.ssh; regular users use ~/.ssh.
set -eu
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

KEYS_SRC=/opt/root/.ssh/authorized_keys
SHELL_BIN=/opt/bin/sh
PASSWD=/opt/etc/passwd
GROUP=/opt/etc/group
SHADOW=/opt/etc/shadow
SHELLS=/opt/etc/shells
SUDOERS=/opt/etc/sudoers
SUDOERS_D=/opt/etc/sudoers.d

die() { echo "Ошибка: $*" >&2; exit 1; }

usage() {
    echo "Использование: sh $0 [ПОЛЬЗОВАТЕЛЬ]"
    echo '  Создаёт пользователя (по умолчанию admin) с входом по SSH-ключу и правом sudo.'
    echo "  Ключи копируются из $KEYS_SRC."
}

case "${1:-}" in
    -h|--help|help) usage; exit 0 ;;
esac
[ "$#" -le 1 ] || die 'Слишком много аргументов.'
NEWUSER=${1:-admin}
case "$NEWUSER" in
    root) die 'root не подходит: нужен отдельный пользователь.' ;;
    [a-z_]*) ;;
    *) die "недопустимое имя пользователя: $NEWUSER" ;;
esac
case "$NEWUSER" in
    *[!a-z0-9_-]*) die "недопустимое имя пользователя: $NEWUSER" ;;
esac
HOMEDIR=/opt/home/$NEWUSER
SUDO_LINE="$NEWUSER ALL=(ALL) ALL"

[ "$(id -u)" = 0 ] || die 'запустите от root'
[ -s "$KEYS_SRC" ] || die "нет ключей в $KEYS_SRC"
[ -x "$SHELL_BIN" ] || die "нет $SHELL_BIN"
[ -f "$PASSWD" ] && [ -f "$GROUP" ] || die "нет $PASSWD или $GROUP"

if ! command -v sudo >/dev/null 2>&1; then
    echo 'Устанавливаю sudo...'
    opkg update
    opkg install sudo
fi
[ -f "$SUDOERS" ] || die "нет $SUDOERS после установки sudo"

# User and group
created=0
if grep -q "^$NEWUSER:" "$PASSWD"; then
    echo "Пользователь $NEWUSER уже есть, создание пропускаю"
else
    cp -p "$PASSWD" "$PASSWD.bak-adduser"
    cp -p "$GROUP" "$GROUP.bak-adduser"
    id_=$(awk -F: 'BEGIN{n=1000} $3>=n && $3<60000 {n=$3+1} END{print n}' "$PASSWD" "$GROUP")
    grep -q "^$NEWUSER:" "$GROUP" || echo "$NEWUSER:x:$id_:" >> "$GROUP"
    gid=$(awk -F: -v g="$NEWUSER" '$1==g {print $3}' "$GROUP")
    if [ -f "$SHADOW" ]; then
        cp -p "$SHADOW" "$SHADOW.bak-adduser"
        echo "$NEWUSER:x:$id_:$gid:$NEWUSER:$HOMEDIR:$SHELL_BIN" >> "$PASSWD"
        grep -q "^$NEWUSER:" "$SHADOW" || echo "$NEWUSER:*:0:0:99999:7:::" >> "$SHADOW"
    else
        echo "$NEWUSER:*:$id_:$gid:$NEWUSER:$HOMEDIR:$SHELL_BIN" >> "$PASSWD"
    fi
    created=1
    echo "Создан пользователь $NEWUSER (uid $id_)"
fi
uid=$(awk -F: -v u="$NEWUSER" '$1==u {print $3}' "$PASSWD")
gid=$(awk -F: -v u="$NEWUSER" '$1==u {print $4}' "$PASSWD")
home=$(awk -F: -v u="$NEWUSER" '$1==u {print $6}' "$PASSWD")
[ "$home" = "$HOMEDIR" ] || die "у $NEWUSER домашний каталог $home, а не $HOMEDIR; не трогаю"

# dropbear only accepts shells listed in the shells file
if [ -f "$SHELLS" ] && ! grep -qx "$SHELL_BIN" "$SHELLS"; then
    echo "$SHELL_BIN" >> "$SHELLS"
fi

# Home and keys
[ -L "$HOMEDIR" ] || [ -L "$HOMEDIR/.ssh" ] && die "$HOMEDIR или .ssh — симлинк, отказываюсь"
mkdir -p /opt/home "$HOMEDIR/.ssh"
chmod 755 /opt/home
AK=$HOMEDIR/.ssh/authorized_keys
[ -L "$AK" ] && die "$AK — симлинк, отказываюсь"
touch "$AK"
tr -d '\r' < "$KEYS_SRC" | while IFS= read -r line; do
    case $line in ''|'#'*) continue ;; esac
    grep -qxF "$line" "$AK" || printf '%s\n' "$line" >> "$AK"
done
chown -R "$uid:$gid" "$HOMEDIR"
chmod 755 "$HOMEDIR"
chmod 700 "$HOMEDIR/.ssh"
chmod 600 "$AK"

# sudo rule
if grep -Eq "^[#@]includedir $SUDOERS_D\$" "$SUDOERS"; then
    f=$SUDOERS_D/$NEWUSER
    mkdir -p "$SUDOERS_D"
    printf '%s\n' "$SUDO_LINE" > "$f.tmp"
    chmod 440 "$f.tmp"
    if command -v visudo >/dev/null 2>&1 && ! visudo -c -f "$f.tmp" >/dev/null; then
        rm -f "$f.tmp"
        die 'visudo отклонил правило'
    fi
    mv "$f.tmp" "$f"
elif ! grep -qxF "$SUDO_LINE" "$SUDOERS"; then
    cp -p "$SUDOERS" "$SUDOERS.bak-adduser"
    printf '%s\n' "$SUDO_LINE" >> "$SUDOERS"
    if command -v visudo >/dev/null 2>&1 && ! visudo -c -f "$SUDOERS" >/dev/null; then
        cp -p "$SUDOERS.bak-adduser" "$SUDOERS"
        die 'visudo отклонил sudoers, восстановлен бэкап'
    fi
fi
[ -u "$(command -v sudo)" ] || echo 'Внимание: у sudo нет setuid-бита (раздел /opt смонтирован с nosuid?)'

if [ "$created" = 1 ]; then
    echo "Задайте пароль для $NEWUSER (его спросит sudo):"
    passwd "$NEWUSER"
fi

echo 'Готово. Проверка с компьютера (не закрывайте текущую сессию root):'
echo "  ssh -o PreferredAuthentications=publickey $NEWUSER@<адрес роутера>"
echo '  sudo -i'
