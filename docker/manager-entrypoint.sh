#!/bin/bash
set -Eeuo pipefail
umask 077
log() { printf 'level=INFO component=manager-entrypoint message="%s"\n' "$*"; }
fail() { printf 'level=ERROR component=manager-entrypoint message="%s"\n' "$*" >&2; exit 1; }
trap 'fail "Manager preparation failed at line $LINENO"' ERR
export CRAWLBOX_DATA=${CRAWLBOX_DATA:-/data}
manager=${MANAGER_BINARY:-manager}
if [[ $(id -u) == 0 ]]; then
    if [[ ${1:-serve-auto} == serve-auto ]]; then
        mkdir -p "$CRAWLBOX_DATA"
        # Never recursively chown a potentially large existing data tree.
        chown 10001:10001 "$CRAWLBOX_DATA"
    fi
    exec setpriv --reuid=10001 --regid=10001 --init-groups "$0" "$@"
fi
if [[ ${1:-serve-auto} != serve-auto ]]; then exec "$manager" "$@"; fi
storage=$CRAWLBOX_DATA
shared=${CRAWLBOX_BOOTSTRAP:-/bootstrap}
kopia=${KOPIA_BINARY:-kopia}
mkdir -p "$storage/connection" "$storage/kopia-cache"
exec 9>"$storage/bootstrap.lock"
flock -n 9 || fail 'Manager environment is already in use'
if [[ ! -e $storage/config.yaml ]]; then
    log 'Creating default manager configuration'
    temporary=$(mktemp "$storage/.config.XXXXXX")
    # JSON is accepted by the application's YAML parser. Existing YAML is preserved.
    jq -n --arg data "$storage" --arg kopia "$kopia" '{listen:":8080",data_dir:$data,credentials:($data+"/admin.yaml"),kopia_binary:$kopia,kopia_config:($data+"/connection/repository.config"),parallel:1,cache_bytes:21474836480,sources:[]}' > "$temporary"
    sync -f "$temporary"
    mv "$temporary" "$storage/config.yaml"
    sync -f "$storage"
fi
export KOPIA_CHECK_FOR_UPDATES=false
# Bounded retries, including a timeout on each network operation.
for ((attempt=1;attempt<=120;attempt++)); do
    if (( attempt==1 || attempt%15==0 )); then log 'Waiting for internal Kopia connection'; fi
    if [[ -f $shared/connection.json ]]; then
        jq -e 'all(.password,.fingerprint; type=="string" and test("^[0-9a-f]{64}$"))' "$shared/connection.json" >/dev/null || fail 'Invalid connection handoff'
        export KOPIA_PASSWORD
        KOPIA_PASSWORD=$(jq -r .password "$shared/connection.json")
        fingerprint=$(jq -r .fingerprint "$shared/connection.json")
        if { [[ -f $storage/connection/repository.config ]] &&
            timeout 10 "$kopia" --config-file "$storage/connection/repository.config" --disable-file-logging --no-progress snapshot list --json >/dev/null 2>&1; } ||
            timeout 10 "$kopia" --config-file "$storage/connection/repository.config" --disable-file-logging --no-progress \
            repository connect server --url "${KOPIA_URL:-https://kopia:51515}" \
            --server-cert-fingerprint "$fingerprint" --override-username=worker --override-hostname=manager \
            --cache-directory "$storage/kopia-cache" --persist-credentials >/dev/null 2>&1; then
            unset KOPIA_PASSWORD fingerprint
            log 'Internal Kopia connection ready; starting manager'
            exec "$manager" serve-auto
        fi
    fi
    sleep 1
done
fail 'Kopia did not become ready; check kopia container logs and connection credentials'
