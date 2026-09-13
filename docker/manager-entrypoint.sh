#!/bin/bash
set -Eeuo pipefail
umask 077
log() { printf 'level=INFO component=manager-entrypoint message="%s"\n' "$*"; }
fail() { printf 'level=ERROR component=manager-entrypoint message="%s"\n' "$*" >&2; exit 1; }
trap 'fail "Manager preparation failed at line $LINENO"' ERR
export CRAWLBOX_DATA=${CRAWLBOX_DATA:-/data}
manager=${MANAGER_BINARY:-manager}
if [[ ${1:-serve-auto} != serve-auto ]]; then exec "$manager" "$@"; fi
storage=$CRAWLBOX_DATA
shared=${CRAWLBOX_BOOTSTRAP:-}
client=${KOPIA_CONFIG_PATH:-$storage/connection/repository.config}
kopia=${KOPIA_BINARY:-kopia}
mkdir -p "$storage"
[[ -w $storage && -x $storage ]] || fail "Data directory is not accessible to UID $(id -u), GID $(id -g); check mount permissions"
mkdir -p "$storage/connection" "$storage/kopia-cache"
exec 9>"$storage/bootstrap.lock"
flock -n 9 || fail 'Manager environment is already in use'
if [[ ! -e $storage/config.yaml ]]; then
    log 'Creating default manager configuration'
    temporary=$(mktemp "$storage/.config.XXXXXX")
    # Emit a block-style YAML mapping; JSON-quoted values safely escape paths.
    jq -nr --arg data "$storage" --arg kopia "$kopia" --arg client "$client" '{listen:":8080",data_dir:$data,credentials:($data+"/admin.yaml"),kopia_binary:$kopia,kopia_config:$client,parallel:1,cache_bytes:21474836480,sources:[]} | to_entries[] | "\(.key): \(.value | tojson)"' > "$temporary"
    sync -f "$temporary"
    mv "$temporary" "$storage/config.yaml"
    sync -f "$storage"
fi
export KOPIA_CHECK_FOR_UPDATES=false
if [[ -z $shared && ! -f $client ]]; then
    fail 'No Kopia client configuration; prepare KOPIA_CONFIG_PATH or use the bundled bootstrap Compose'
fi
ready() {
    log 'Kopia connection ready; starting manager'
    exec "$manager" serve-auto
}
# Bounded retries, including a timeout on each network operation.
for ((attempt=1;attempt<=120;attempt++)); do
    if (( attempt==1 || attempt%15==0 )); then log 'Waiting for Kopia connection'; fi
    # A standard persisted connection works without any shared handoff or wrapper.
    if [[ -f $client ]] && timeout 10 "$kopia" --config-file "$client" --disable-file-logging --no-progress snapshot list --json >/dev/null 2>&1; then ready; fi
    if [[ -n $shared && -d $shared && ! -x $shared ]]; then fail "Shared directory is not accessible to UID $(id -u), GID $(id -g); check mount permissions"; fi
    if [[ -n $shared && -f $shared/connection.json ]]; then
        [[ -r $shared/connection.json ]] || fail "Connection credentials are not readable by UID $(id -u), GID $(id -g); check shared mount permissions"
        jq -e 'all(.password,.fingerprint; type=="string" and test("^[0-9a-f]{64}$"))' "$shared/connection.json" >/dev/null || fail 'Invalid connection handoff'
        export KOPIA_PASSWORD
        KOPIA_PASSWORD=$(jq -r .password "$shared/connection.json")
        fingerprint=$(jq -r .fingerprint "$shared/connection.json")
        if timeout 10 "$kopia" --config-file "$client" --disable-file-logging --no-progress \
            repository connect server --url "${KOPIA_URL:-https://kopia:51515}" \
            --server-cert-fingerprint "$fingerprint" --override-username=worker --override-hostname=manager \
            --cache-directory "$storage/kopia-cache" --persist-credentials >/dev/null 2>&1; then
            unset KOPIA_PASSWORD fingerprint
            ready
        fi
    fi
    sleep 1
done
fail 'Kopia did not become ready; check the server, network and client credentials'
