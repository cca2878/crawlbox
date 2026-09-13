# NAS deployment

Download `deploy/compose.yaml` from the release you deploy. It is a complete two-service Compose project. Import it into the NAS container manager and start it, or run `docker compose up -d`. The initial version supporting this deployment is v0.2.0; v0.1.0 does not have automatic initialization.

Open `http://NAS-IP:8080/ui/` and create the administrator (password: 12–72 bytes). This is a one-time setup page: the first valid submission creates the administrator; subsequent submissions cannot replace it. Finish setup on a trusted LAN before opening access beyond it. Cross-origin POSTs are rejected. Once setup completes, the service reconnects to internal storage and switches to Basic-authenticated UI. If the browser refreshes before storage is ready, refresh again after a few seconds.

Only port 8080 is published. Change the host port in Compose, or set CRAWLBOX_PORT. Both API and UI use this port. Kopia uses the private storage network and has no external port. For access beyond the LAN, use your NAS HTTPS reverse proxy, preserving Host, Origin and Sec-Fetch-Site.

The Kopia container uses the same crawlbox image with `kopia-server` as its command. This runs the version-pinned, CGO-disabled Kopia CLI as the server after initialization; it does not run another manager. It generates independent repository, worker and server passwords and a TLS certificate. The manager receives only a worker credential and certificate fingerprint through a read-only shared volume, never the repository encryption password. Initialization fails instead of generating new credentials when a nonempty repository has lost its secrets file. Internal certificates are valid for ten years; preserve their keys along with storage backups.

## Persistent volumes

- `manager-data`: `/data/config.yaml`, `/data/admin.yaml`, catalog, current files, staging, caches and client connection. Administrator credentials are bcrypt hashes. Source configuration is generated with `sources: []`.
- `kopia-data`: encrypted repository, `secrets.json`, server connection, certificate/key and cache. Back up the whole volume; the encryption password is in secrets.json.
- `bootstrap`: generated worker credential and certificate fingerprint, read-only in the manager. The server can recreate it.
- `extensions`: installed extension packages, read-only in the manager.

Docker initializes volume ownership from the image. No manual chown is required for named volumes. If replacing these with NAS bind mounts, ensure UID/GID 10001 can write the data directories and can read extension files. Keep the two containers' data directories separate. Container restart preserves both administrator and repository credentials. Do not run `docker compose down --volumes` unless intentionally destroying this deployment.

## Add an extension

An empty installation starts successfully but does not collect anything. Install compatible packages and configure sources separately; no business-specific extension is bundled.

For convenient NAS file management, replace the `extensions` named-volume mount with `./extensions:/extensions:ro`, create that directory in the Compose project directory and unpack packages there. Allow UID 10001 to read/traverse the files. Copy the generated configuration out:

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

Upgrade by changing the image version for both services and recreating them while retaining all volumes. Back up data before upgrading. Bootstrap configuration is generated only when absent; custom source configuration is preserved. Back up the management database consistently (stop the manager for a filesystem copy), server secrets/configuration and the Kopia repository. The shared bootstrap is also sensitive, even though it is reconstructible.

Existing manually initialized deployments should retain their original Compose layout; `compose.manual.yaml` is provided for them. Automatic initialization is intended for new volumes, not in-place migration of existing manual paths.

## Forgotten administrator password

Use access to the NAS container terminal/SSH to reset the existing account. In Bash, read the new password without echoing it or putting it in command arguments:

```sh
read -rsp 'New administrator password: ' CRAWLBOX_NEW_PASSWORD
echo
printf '%s' "$CRAWLBOX_NEW_PASSWORD" | docker compose exec -T manager manager reset-password
unset CRAWLBOX_NEW_PASSWORD
docker compose restart manager
```

The password must be 12–72 bytes. The command preserves the existing username and replaces only its bcrypt hash. Restart is required because the running web server holds the old credentials in memory. It does not delete API tokens or reopen first-start setup. Do not delete admin.yaml to reset a password. For manual deployments, an optional credentials-file argument is available; the file must be writable for this operation.
