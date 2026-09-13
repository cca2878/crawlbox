#!/bin/bash
set -Eeuo pipefail
umask 077
log() { printf 'level=INFO component=kopia-entrypoint message="%s"\n' "$*"; }
fail() { printf 'level=ERROR component=kopia-entrypoint message="%s"\n' "$*" >&2; exit 1; }
trap 'fail "Storage preparation failed at line $LINENO; inspect persistent state before retrying"' ERR
if [[ $# -gt 1 || ${1:-server} != server ]]; then exec "${KOPIA_BINARY:-/bin/kopia}" "$@"; fi
storage=${CRAWLBOX_DATA:-/data}
shared=${CRAWLBOX_BOOTSTRAP:-/bootstrap}
kopia=${KOPIA_BINARY:-/bin/kopia}
mkdir -p "$storage"
# Set a mode only when creating the directory; preserve existing mount permissions.
# shellcheck disable=SC2174 # Only the shared directory itself gets this creation mode.
mkdir -p -m 0750 "$shared"
[[ -w $storage && -x $storage ]] || fail "Data directory is not accessible to UID $(id -u), GID $(id -g); check mount permissions"
[[ -w $shared && -x $shared ]] || fail "Shared directory is not writable by UID $(id -u), GID $(id -g); check mount permissions"
exec 9>"$storage/bootstrap.lock"
flock -n 9 || fail 'Storage is already in use'
secrets="$storage/secrets.json"
if [[ ! -e $secrets ]]; then
    if [[ -d $storage/repository ]] && [[ -n $(find "$storage/repository" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
        fail 'Existing repository has no secrets.json; restore the credentials backup'
    fi
    log 'Creating storage credentials'
    temporary=$(mktemp "$storage/.secrets.XXXXXX")
    # Feed secrets through stdin; do not put them in jq arguments or logs.
    { openssl rand -hex 32; openssl rand -hex 32; openssl rand -hex 32; } |
        jq -Rn '[inputs] | {repository:.[0], worker:.[1], server:.[2]}' > "$temporary"
    sync -f "$temporary"
    mv "$temporary" "$secrets"
    sync -f "$storage"
fi
jq -e 'all(.repository,.worker,.server; type=="string" and test("^[0-9a-f]{64}$"))' "$secrets" >/dev/null || fail 'Invalid secrets.json'
export KOPIA_PASSWORD KOPIA_SERVER_PASSWORD
KOPIA_PASSWORD=$(jq -r .repository "$secrets")
KOPIA_SERVER_PASSWORD=$(jq -r .server "$secrets")
worker=$(jq -r .worker "$secrets")
export KOPIA_CHECK_FOR_UPDATES=false KOPIA_CACHE_DIRECTORY="$storage/cache"
cli() { "$kopia" --config-file "$storage/repository.config" --disable-file-logging --no-progress "$@"; }
if [[ ! -e $storage/repository.config ]]; then
    verb=create
    if [[ -d $storage/repository ]] && [[ -n $(find "$storage/repository" -mindepth 1 -maxdepth 1 -print -quit) ]]; then verb=connect; fi
    log "Preparing repository ($verb)"
    cli repository "$verb" filesystem --path "$storage/repository" --cache-directory "$storage/cache" >/dev/null
fi
# Validate existing configuration before publishing readiness or skipping steps.
cli repository status >/dev/null
if ! cli server users list --json | jq -e 'any(.[]; .username == "worker@manager")' >/dev/null; then
    log 'Creating worker account'
    cli server users add worker@manager --user-password "$worker" >/dev/null
fi
cert="$storage/server.crt"
key="$storage/server.key"
if [[ ! -e $cert && ! -e $key ]]; then
    log 'Generating internal TLS certificate with Kopia'
    # Kopia generates persistent certificates only through server start. Use a
    # loopback-only ephemeral listener once, then start the normal server below.
    "$kopia" --config-file "$storage/repository.config" --disable-file-logging --no-progress \
        server start --address=https://127.0.0.1:0 --server-username=admin \
        --tls-generate-cert --tls-generate-cert-name=kopia --tls-generate-cert-valid-days=3650 \
        --tls-cert-file "$cert" --tls-key-file "$key" &
    generator=$!
    cleanup() { kill "$generator" 2>/dev/null || true; wait "$generator" 2>/dev/null || true; }
    trap 'cleanup; exit 143' TERM INT
    trap cleanup EXIT
    for ((attempt=1;attempt<=120;attempt++)); do
        if [[ -s $key ]] && openssl pkey -in "$key" -noout >/dev/null 2>&1; then break; fi
        kill -0 "$generator" 2>/dev/null || fail 'Kopia certificate generation stopped'
        sleep 0.25
    done
    cleanup
    trap - EXIT TERM INT
fi
[[ -s $cert ]] || fail 'TLS certificate is missing; restore it from backup'
[[ -f $key ]] || fail 'TLS private key is missing; restore it from backup'
[[ $(openssl x509 -in "$cert" -pubkey -noout) == $(openssl pkey -in "$key" -pubout 2>/dev/null) ]] || fail 'TLS certificate and key do not match'
fingerprint=$(openssl x509 -in "$cert" -outform DER | sha256sum | cut -d ' ' -f1)
temporary=$(mktemp "$shared/.connection.XXXXXX")
printf '%s\n%s\n' "$worker" "$fingerprint" | jq -Rn '[inputs] | {password:.[0],fingerprint:.[1]}' > "$temporary"
# A root publisher can use the shared directory's group without changing the
# directory itself. Non-root writers retain their primary or inherited setgid group.
if [[ $(id -u) == 0 ]]; then chgrp --reference="$shared" "$temporary"; fi
# Share only the worker credential with the writer's group. Repository and
# administrator secrets remain private. A setgid directory can select the group.
chmod 0640 "$temporary"
sync -f "$temporary"
mv "$temporary" "$shared/connection.json"
sync -f "$shared"
unset worker fingerprint
# Remove only the obsolete generated executable, never repository data.
rm -f "$storage/start-server.sh"
log 'Storage ready; starting official Kopia Server'
exec "$kopia" --config-file "$storage/repository.config" --disable-file-logging --no-progress server start \
    --address "${KOPIA_LISTEN:-https://0.0.0.0:51515}" \
    --tls-cert-file "$cert" --tls-key-file "$key" --server-username=admin
