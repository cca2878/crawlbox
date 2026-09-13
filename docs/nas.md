# NAS deployment

Download `deploy/compose.yaml` from the release you deploy. It contains exactly two services. Set `CRAWLBOX_VERSION` in the Compose environment (or an adjacent `.env` file) to a release that publishes both `crawlbox` and `crawlbox-kopia`. Both images always use the same tag; the wrapper does not have an independent release version. Import the project into the NAS container manager and start it, or run `docker compose up -d`. This layout is pending its first release; v0.3.0 and older do not publish the paired images.

Open `http://NAS-IP:8080/ui/` and create the administrator (password: 12–72 bytes). This is a one-time setup page: the first valid submission creates the administrator; subsequent submissions cannot replace it. Finish setup on a trusted LAN before opening access beyond it. Cross-origin POSTs are rejected. The page becomes available after internal storage is connected. Once setup completes, the service switches to Basic-authenticated UI.

Only port 8080 is published. Change the host port in Compose, or set CRAWLBOX_PORT. Both API and UI use this port. Kopia uses the private storage network and has no external port. For access beyond the LAN, use your NAS HTTPS reverse proxy, preserving Host, Origin and Sec-Fetch-Site.

The Kopia image is a thin wrapper around official `kopia/kopia:0.23.1`: it adds a Bash entrypoint and standard command-line utilities, retaining the upstream executable and Web UI. The entrypoint creates credentials only for new storage, creates or reconnects the filesystem repository and checks the worker account with `server users list --json`. Kopia itself generates the persistent certificate using `server start --tls-generate-cert`; because this is a server-start option rather than a standalone certificate command, first-start preparation briefly uses an authenticated loopback listener on an ephemeral port before starting the normal server. Existing certificate/key pairs are reused. Partial pairs fail with a recovery message instead of silently changing the server identity.

The manager entrypoint creates default configuration only if absent, waits for the shared worker credential, and reuses a working persisted client connection. When needed, it connects using `repository connect server --persist-credentials` and the pinned certificate fingerprint. Kopia CLI operations handle repository format, accounts, TLS generation and client credentials; the Go manager retains application startup and administrator setup only. Both final services use `exec` so container stop signals reach the actual process.

The manager receives only a worker credential and certificate fingerprint through a read-only shared volume, never the repository encryption password or UI password. Initialization fails instead of generating new credentials when a nonempty repository has lost its secrets file. Internal certificates are valid for ten years; preserve their keys along with storage backups.

## Persistent volumes

- `manager-data`: `/data/config.yaml`, `/data/admin.yaml`, catalog, current files, staging, caches and client connection. Administrator credentials are bcrypt hashes. Source configuration is generated with `sources: []`.
- `kopia-data`: encrypted repository, `secrets.json`, server connection, certificate/key and cache. Back up the whole volume; the encryption password is in secrets.json.
- `bootstrap`: generated worker credential and certificate fingerprint, read-only in the manager. The Kopia entrypoint can recreate it.
- `extensions`: installed extension packages, read-only in the manager.

No `user` setting is required. The manager entrypoint starts as root only to prepare the manager volume root, then drops to UID/GID 10001; it does not recursively change existing data ownership. Kopia retains its official default user. NAS ACLs must allow the container users to access their bind mounts. Extension files must be readable by UID 10001. Keep manager and Kopia data directories separate. Do not run `docker compose down --volumes` unless intentionally destroying this deployment.

For Synology SSD/HDD placement, keep `compose.yaml`, `.env` and `extensions/` in your SSD Docker project directory. Replace the manager volume mount with `/volume2/crawlbox/manager:/data`, the Kopia data mount with `/volume2/crawlbox/kopia:/data`, and both bootstrap mounts with `./bootstrap:/bootstrap` (retain `:ro` in the manager). Replace the extensions mount with `./extensions:/extensions:ro`. Adjust `/volume2/crawlbox` to your actual large-array shared-folder path. Create the four directories before starting. The manager's full current trees and archive cache also consume space, so placing only the Kopia repository on HDD would leave substantial data on SSD.

## Add an extension

An empty installation starts successfully but does not collect anything. Install compatible packages and configure sources separately; no business-specific extension is bundled.

For convenient NAS file management, replace the `extensions` named-volume mount with `./extensions:/extensions:ro`, create that directory in the Compose project directory and unpack packages there. Allow UID 10001 to read/traverse the files. Copy the generated configuration out:

```sh
docker compose cp manager:/data/config.yaml ./config.yaml
```

Edit its `sources` using `deploy/config.example.yaml` as the schema example. Pin the actual WASM SHA-256, use `/extensions/.../plugin.wasm`, grant exact network hosts and choose a schedule. Source IDs must remain stable. To install the edited config without modifying the generated volume permissions, the image contains a shell:

```sh
docker compose exec --user 10001:10001 -T manager sh -c 'cat > /data/config.yaml' < config.yaml
docker compose restart manager
```

Use the UI to trigger a run and inspect errors. Create an API token scoped to the source after configuring it. Administrator credentials do not authenticate to the public API.

## Diagnostics and upgrades

```sh
docker compose ps
docker compose logs --tail=100 manager kopia
```

Upgrade by changing `CRAWLBOX_VERSION` once for both images. When upgrading from v0.2.0 or v0.3.0, stop the old project first with `docker compose down` (without `--volumes`) before applying the new Compose; remove the old initializer container with `--remove-orphans` when bringing up the new project. Retain all volume names and mount paths. The scripts reuse existing secrets, certificates and history, and remove the obsolete generated `/data/start-server.sh`. Back up before upgrading; downgrading to the old non-root server may require restoring file ownership. Configuration is generated only when absent, so custom sources remain intact. Back up the management database consistently (stop the manager for a filesystem copy), server secrets/configuration and the Kopia repository. The shared handoff is sensitive even though it is reconstructible.

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

The wrapper preserves the official Web UI. It is not exposed by the default Compose. If needed, temporarily publish Kopia port 51515 on a trusted interface and visit `https://NAS-IP:51515`; the certificate is self-signed. The username is `admin`; retrieve its password locally with `docker compose exec -T kopia jq -r .server /data/secrets.json`. This displays a secret: do not include the output in logs or support messages. Remove the port mapping when finished. The worker password and the crawlbox administrator account do not log in to this UI.
