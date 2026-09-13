# Standalone Linux deployment

The manager can run directly as a Linux process, including under systemd, without Docker or either container entrypoint. It uses the invoking OS identity; UID/GID 10001 is only a manager image default. The locally verified platform is Linux amd64; CI runs the standalone test on native amd64 and arm64. Windows is not a supported target: the process currently uses Unix file locking and signals. Other operating systems have not been validated.

## Runtime requirements

Provide the manager executable, the pinned Kopia CLI v0.23.1, a prepared Kopia Server connection, administrator credentials and configuration. Build both executables with `CGO_ENABLED=0`; `make build tools` produces them in `bin/`. The manager does not need Go, Bash, jq, OpenSSL, Docker or the `flock` executable at runtime. Ordinary OS facilities such as writable temporary space, CA certificates for public HTTPS and timezone data for configured schedules must be available.

Kopia Server may run locally or remotely. Initialize its repository, TLS and accounts independently using the Kopia CLI or your existing deployment. The manager does not prepare the server environment when launched with `-config`.

## Configuration and administrator

Use a dedicated service user with access to its own data and connection files. For example, keep configuration and state under `/srv/crawlbox`, with executables in `/opt/crawlbox`. Grant that user write access to the data directory and read access to extensions. Kopia's persisted client configuration and cache must be accessible to the same user.

Create an administrator hash using `manager hash-password`, which reads the password from standard input. One possible interactive Bash preparation session is:

```bash
umask 077
read -rsp 'Administrator password: ' CRAWLBOX_ADMIN_PASSWORD
echo
CRAWLBOX_ADMIN_HASH=$(printf '%s' "$CRAWLBOX_ADMIN_PASSWORD" | /opt/crawlbox/manager hash-password)
unset CRAWLBOX_ADMIN_PASSWORD
printf 'username: admin\npassword_hash: "%s"\n' "$CRAWLBOX_ADMIN_HASH" > /srv/crawlbox/admin.yaml
unset CRAWLBOX_ADMIN_HASH
```

For the remote storage connection, obtain a worker account and the server certificate fingerprint from the Kopia administrator. Run the connection command as the intended manager OS user so its files and cache are accessible. Supply the worker password through Kopia's interactive prompt or `KOPIA_PASSWORD`, then remove that environment variable after connecting:

```sh
/opt/crawlbox/kopia --config-file /srv/crawlbox/client.config \
  repository connect server \
  --url https://storage.example:51515 \
  --server-cert-fingerprint REPLACE_WITH_TRUSTED_SHA256 \
  --override-username=worker --override-hostname=manager \
  --cache-directory /srv/crawlbox/kopia-cache --persist-credentials
```

This example uses the repository account `worker@manager`; keep its logical username and hostname stable across restarts. These are Kopia account identifiers, independent of the OS UID/GID. The persisted connection contains sensitive credentials; include it in your protected management backup.

Create `/srv/crawlbox/config.yaml`:

```yaml
listen: 127.0.0.1:8080
data_dir: /srv/crawlbox/data
credentials: /srv/crawlbox/admin.yaml
kopia_binary: /opt/crawlbox/kopia
kopia_config: /srv/crawlbox/client.config
parallel: 1
cache_bytes: 21474836480
sources: []
```

Start with:

```sh
/opt/crawlbox/manager -config /srv/crawlbox/config.yaml
```

This mode requires the administrator file to exist; it does not open the first-account wizard automatically. Add sources using `deploy/config.example.yaml` and restart to load changes. Empty sources are valid but do not collect anything. No proxy port is opened by default.

Relative file paths resolve against the **process working directory**, not the YAML file's directory. This applies to credentials, data, plugin paths, the Kopia connection and certificate-pin files. Use absolute paths for services, or explicitly set a stable working directory. A bare `kopia_binary: kopia` uses PATH; an absolute executable path avoids service PATH differences.

## Service operation

A minimal systemd unit can use the existing account and directories you prepared:

```ini
[Unit]
Description=Crawlbox collection manager
After=network-online.target
Wants=network-online.target

[Service]
User=crawlbox
Group=crawlbox
WorkingDirectory=/srv/crawlbox
ExecStart=/opt/crawlbox/manager -config /srv/crawlbox/config.yaml
Restart=on-failure
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

The process writes logs to stdout/stderr and handles SIGTERM. A second process using the same data directory fails its exclusive lock. Restart after changing configuration or administrator credentials.

For a forgotten password, run the reset command as the service user and supply the password through stdin. **Pass the actual credentials path**:

```sh
/opt/crawlbox/manager reset-password /srv/crawlbox/admin.yaml
```

The command reads until EOF. It preserves the username and only replaces the password hash. The no-argument reset default `/data/admin.yaml` is intended for the automatic container layout. Likewise, `serve-auto` expects a prepared config under `CRAWLBOX_DATA` (default `/data`) and runs the first-account wizard; it does not replace environment preparation. Normal standalone deployment uses `-config`.

## Optional Kopia UI proxy

The proxy is configured independently of `kopia_config` and does not carry collection or recovery operations:

```yaml
kopia_ui_proxy:
  listen: 127.0.0.1:8081
  target: https://storage.example:51515
  fingerprint_file: /srv/crawlbox/kopia-ui-pin.json
```

For a self-signed server certificate, the pin file contains `{"fingerprint":"REPLACE_WITH_TRUSTED_SHA256"}`. It needs no password. For a certificate trusted by the operating system, omit `fingerprint_file` and use standard CA verification. The proxy is initially disabled; enable it in the manager UI and log in using Kopia's own credentials. HTTP targets are also supported when deliberately configured.

The optional `KOPIA_UI_PROXY_LISTEN`, `KOPIA_UI_PROXY_TARGET` and `KOPIA_UI_PROXY_PIN_FILE` environment variables supply defaults for empty YAML fields. Do not carry the Docker image's environment into an unrelated standalone deployment. Remove these variables if you want YAML-only configuration or standard CA verification with an empty fingerprint field.

## Verification

`make integration` includes `TestStandaloneDeployment`, which launches the actual executable under `-config` with container environment variables removed and no command-line utilities in PATH. It exercises a real Kopia Server and a generic WASM plugin, including collection, published-file retrieval, duplicate-process rejection, explicit-path password reset, SIGTERM/restart, current-tree recovery and a YAML-only proxy configuration. No container entrypoint is used.
