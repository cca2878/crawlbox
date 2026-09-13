# NAS deployment

Download `deploy/compose.yaml` from the release you deploy. It contains exactly two services. Set `CRAWLBOX_VERSION` in the Compose environment (or an adjacent `.env` file) to a release that publishes both `crawlbox` and `crawlbox-kopia`. Both images always use the same tag; the wrapper does not have an independent release version. Import the project into the NAS container manager and start it, or run `docker compose up -d`. This layout is pending its first release; v0.3.0 and older do not publish the paired images.

Open `http://NAS-IP:8080/ui/` and create the administrator (password: 12–72 bytes). This is a one-time setup page: the first valid submission creates the administrator; subsequent submissions cannot replace it. Finish setup on a trusted LAN before opening access beyond it. Cross-origin POSTs are rejected. The page becomes available after internal storage is connected. Once setup completes, the service switches to Basic-authenticated UI.

Manager publishes port 8080 for its API/UI and port 8081 for the optional Kopia Web UI proxy. Change their host mappings in Compose, or set `CRAWLBOX_PORT` and `CRAWLBOX_KOPIA_UI_PORT`. Kopia uses the private storage network and has no external port. For access beyond the LAN, use your NAS HTTPS reverse proxy, preserving Host, Origin and Sec-Fetch-Site.

The Kopia image is a thin wrapper around official `kopia/kopia:0.23.1`: it adds a Bash entrypoint and standard command-line utilities, retaining the upstream executable and Web UI. The entrypoint creates credentials only for new storage, creates or reconnects the filesystem repository and checks the worker account with `server users list --json`. Kopia itself generates the persistent certificate using `server start --tls-generate-cert`; because this is a server-start option rather than a standalone certificate command, first-start preparation briefly uses an authenticated loopback listener on an ephemeral port before starting the normal server. Existing certificate/key pairs are reused. Partial pairs fail with a recovery message instead of silently changing the server identity.

The manager entrypoint creates default configuration only if absent and first reuses a working persisted client connection. Only the bundled Compose opts into shared-credential initialization with `CRAWLBOX_BOOTSTRAP=/bootstrap`. When needed, it connects using `repository connect server --persist-credentials` and the pinned certificate fingerprint. Kopia CLI operations handle repository format, accounts, TLS generation and client credentials; the Go manager retains application startup and administrator setup only. Both final services use `exec` so container stop signals reach the actual process.

The manager receives only a worker credential and certificate fingerprint through a read-only shared volume, never the repository encryption password or UI password. Initialization fails instead of generating new credentials when a nonempty repository has lost its secrets file. Internal certificates are valid for ten years; preserve their keys along with storage backups.

## Persistent volumes

- `manager-data`: `/data/config.yaml`, `/data/admin.yaml`, catalog, current files, staging, caches and client connection. Administrator credentials are bcrypt hashes. Source configuration is generated with `sources: []`.
- `kopia-data`: encrypted repository, `secrets.json`, server connection, certificate/key and cache. Back up the whole volume; the encryption password is in secrets.json.
- `bootstrap`: generated worker credential and certificate fingerprint, read-only in the manager. The Kopia entrypoint can recreate it.
- `extensions`: installed extension packages, read-only in the manager.

Manager defaults to UID/GID `10001:10001`; the Kopia wrapper inherits the official image identity (root in the pinned version). Fresh Docker named volumes work without a `user` setting: the shared directory is prepared in the images for the manager group. These defaults are image metadata, not an entrypoint policy: Compose `user: "UID:GID"` is honored as supplied, including numeric IDs absent from the container's account database. Neither entrypoint switches identity or changes the owner, mode or ACL of an existing mount directory. Set `user` on **both** services when using a custom identity; `PUID`/`PGID` environment variables are not interpreted.

The simplest custom arrangement uses the same UID/GID for both services. Different UIDs can use a common group: the shared `connection.json` is created with mode `0640` and the writer's group (or the shared directory's setgid group). When Kopia runs as root, it assigns only the newly generated handoff file to the existing shared directory's group, allowing the non-root manager to read it without modifying the directory. Grant the manager group read/traverse access to the shared directory, using ownership, groups or your NAS ACL tools. Repository encryption and Kopia administrator secrets stay in the separate Kopia data directory with private file modes. Scripts do not require a particular directory owner or reject permissive ACLs; access granted by the deployment environment is sufficient. They do not recursively apply `chmod` or `chown`.

For bind mounts, grant the chosen users access to their data directories and grant the manager read access to extensions and the shared handoff. Existing files also need appropriate access; setting the parent directory's owner alone does not migrate them. Keep manager and Kopia data directories separate. Do not run `docker compose down --volumes` unless intentionally destroying this deployment.

For example, set the following on both services, substituting your actual numeric IDs:

```yaml
user: "1026:100"
```

Default named volumes and custom bind mounts use the same scripts. Direct binary deployments continue to use `manager -config /path/to/config.yaml` under the invoking OS user and do not use these entrypoints or any container UID convention.

For Synology SSD/HDD placement, keep `compose.yaml`, `.env` and `extensions/` in your SSD Docker project directory. Replace the manager volume mount with `/volume2/crawlbox/manager:/data`, the Kopia data mount with `/volume2/crawlbox/kopia:/data`, and both bootstrap mounts with `./bootstrap:/bootstrap` (retain `:ro` in the manager). Replace the extensions mount with `./extensions:/extensions:ro`. Adjust `/volume2/crawlbox` to your actual large-array shared-folder path. Create the four directories before starting. The manager's full current trees and archive cache also consume space, so placing only the Kopia repository on HDD would leave substantial data on SSD.

## Add an extension

An empty installation starts successfully but does not collect anything. Install compatible packages and configure sources separately; no business-specific extension is bundled.

For convenient NAS file management, replace the `extensions` named-volume mount with `./extensions:/extensions:ro`, create that directory in the Compose project directory and unpack packages there. Allow the configured manager identity to read/traverse the files. Copy the generated configuration out:

```sh
docker compose cp manager:/data/config.yaml ./config.yaml
```

Edit its `sources` using `deploy/config.example.yaml` as the schema example. Pin the actual WASM SHA-256, use `/extensions/.../plugin.wasm`, grant exact network hosts and choose a schedule. Source IDs must remain stable. To install the edited config without modifying the generated volume permissions, the image contains a shell:

```sh
docker compose exec -T manager sh -c 'cat > /data/config.yaml' < config.yaml
docker compose restart manager
```

Use the UI to trigger a run and inspect errors. Create an API token scoped to the source after configuring it. Administrator credentials do not authenticate to the public API.

## Diagnostics and upgrades

```sh
docker compose ps
docker compose logs --tail=100 manager kopia
```

Upgrade by changing `CRAWLBOX_VERSION` once for both images. When upgrading from v0.2.0 or v0.3.0, stop the old project first with `docker compose down` (without `--volumes`) before applying the new Compose; remove the old initializer container with `--remove-orphans` when bringing up the new project. Retain all volume names and mount paths. The scripts reuse existing secrets, certificates and history, and remove the obsolete generated `/data/start-server.sh`. Back up before upgrading. If you change the configured identity, grant it access to existing repository files, `secrets.json`, the private key, connection files and shared directory before starting; the entrypoint does not take ownership automatically. Configuration is generated only when absent, so custom sources remain intact. Back up the management database consistently (stop the manager for a filesystem copy), server secrets/configuration and the Kopia repository. The shared handoff is sensitive even though it is reconstructible.

Existing manually initialized deployments should retain their original Compose layout; `compose.manual.yaml` is provided for them. Automatic initialization is intended for new volumes, not in-place migration of existing manual paths.

## Forgotten administrator password

Use access to the NAS container terminal/SSH to reset the existing account. In Bash, read the new password without echoing it or putting it in command arguments:

```sh
read -rsp 'New administrator password: ' CRAWLBOX_NEW_PASSWORD
echo
printf '%s' "$CRAWLBOX_NEW_PASSWORD" | docker compose exec -T manager manager-entrypoint reset-password
unset CRAWLBOX_NEW_PASSWORD
docker compose restart manager
```

The password must be 12–72 bytes. The command preserves the existing username and replaces only its bcrypt hash. Restart is required because the running web server holds the old credentials in memory. It does not delete API tokens or reopen first-start setup. Do not delete admin.yaml to reset a password. For manual deployments, an optional credentials-file argument is available; the file must be writable for this operation.

## Container logs

The manager writes structured text logs to stdout, visible in Container Manager's log viewer. The default `CRAWLBOX_LOG_LEVEL=info` includes startup configuration path, history recovery, loaded sources, scheduled times, queued runs, phase transitions, committed revision IDs, final status and elapsed time. Long runs emit an activity record once a minute. A fresh installation logs explicitly that no sources are configured. Kopia readiness retries are logged periodically. Use `debug` temporarily for additional progress-event notifications.

Logs do not include passwords, tokens, deployment business configuration or raw plugin progress/error text. Plugin errors may contain upstream secrets, so detailed collection errors remain in the authenticated UI; container logs correlate them by source and run ID. Storage preparation errors appear under kopia; manager preparation and collection errors appear under manager. Compose bounds logs to three 10 MiB files per long-running service.

## Kopia Web UI

The manager exposes a separate HTTP proxy port, default `8081`, forwarding over the container network to `https://kopia:51515`. Open the manager UI on port 8080 and use **Kopia Web UI proxy → Enable proxy**, then visit `http://NAS-IP:8081/`. The proxy starts disabled; changing the switch takes effect immediately and survives manager restarts. When disabled, the port returns HTTP 503 and does not forward new requests. Requests/streams already being served may finish.

This is a transparent reverse proxy using Go's [standard ReverseProxy](https://pkg.go.dev/net/http/httputil#ReverseProxy). Kopia handles its own login, cookies, UI APIs and permissions. The manager does not inject credentials or add its own authentication on this port. The Kopia username is `admin`; retrieve its password locally with `docker compose exec -T kopia jq -r .server /data/secrets.json`. Do not include that secret in logs or support messages. Manager administrator credentials and API tokens do not log in to Kopia. Existing collection, snapshot and recovery calls continue to use the original direct Kopia CLI/server channel regardless of the proxy switch.

The additional port serves Kopia at the URL root, without a path prefix or response rewriting. If external HTTPS is needed, configure it at your NAS reverse proxy. The container-network connection verifies the server certificate using the existing handoff fingerprint. Kopia itself still has no published port. The toggle is an authenticated, CSRF-protected POST under `/ui/`; there is no public `/api/v1` control endpoint. Its value is a management database setting, excluded from business snapshots. Restoring business snapshots alone leaves it disabled.

The bundled Compose provides `KOPIA_UI_PROXY_LISTEN=:8081`, `KOPIA_UI_PROXY_TARGET=https://kopia:51515` and `KOPIA_UI_PROXY_PIN_FILE=/bootstrap/connection.json`. These are independent of the business `KOPIA_URL` connection. Change the proxy target separately if you rename the storage service. YAML `kopia_ui_proxy` fields in `deploy/config.example.yaml` take precedence over these defaults. For direct binary deployments no proxy listener is created unless configured; an empty fingerprint file setting uses standard system CA verification for HTTPS. The manager image itself has no proxy target or bootstrap-directory default. The manual and external Compose layouts leave the proxy unconfigured; add the optional listener and a host port mapping when desired. See [external Kopia deployment](external-kopia.md) for using an already prepared server without the wrapper.
