# Use an existing Kopia Server

The manager image does not require `crawlbox-kopia`. Its business backend uses the standard Kopia CLI/server protocol; the wrapper adds no special server API. The wrapper exists to initialize a repository, accounts, TLS and a shared client handoff for the bundled two-container Compose. An independently prepared Kopia Server can instead be used with just the manager container. The tested server/CLI pair is v0.23.1; compatibility with arbitrary server versions is not implied.

## Manager-only Compose

Use `deploy/compose.external.yaml`. It declares only `manager`: no Kopia service, `depends_on`, shared bootstrap mount or internal service hostname. Set a published `CRAWLBOX_VERSION` (or `CRAWLBOX_IMAGE`), create adjacent `connection/` and `extensions/` directories and grant access to the configured manager identity. The image defaults to UID/GID 10001; you may add `user: "UID:GID"` to the service to match your host permissions.

The external server must be reachable from the manager container. Its worker account must be able to create/read the snapshots used by the manager. `localhost` inside a normal Docker network refers to the manager container; use the external server's reachable address or attach the manager to an existing Docker network containing it.

Prepare a normal Kopia client profile once using the CLI bundled in the manager image. From the directory containing the Compose file, use:

```sh
docker compose -f compose.external.yaml run --rm --entrypoint kopia manager \
  --config-file /connection/repository.config --disable-file-logging \
  repository connect server \
  --url https://storage.example:51515 \
  --server-cert-fingerprint REPLACE_WITH_TRUSTED_SHA256 \
  --override-username=worker --override-hostname=manager \
  --cache-directory /data/kopia-cache --persist-credentials
```

Enter the worker password at Kopia's prompt. Match the logical `username@hostname` to the account provisioned on your server; `worker@manager` is an example, not a manager requirement. For a system-trusted HTTPS certificate, the explicit fingerprint flag may be omitted. This operation connects to the prepared server; it does not create repositories, accounts or certificates there. The saved client profile and cache use paths valid inside the manager container. Keep the connection directory writable because Kopia may update its own configuration and credential files.

Then start the manager and create its administrator in the browser:

```sh
docker compose -f compose.external.yaml up -d
```

Open `http://NAS-IP:8080/ui/`. This manager administrator is independent of the external Kopia account. Configure sources in the generated `/data/config.yaml` as usual. No `connection.json` handoff file, repository encryption password or server data mount is needed by the manager.

## Existing client profiles or application configuration

The automatic manager entrypoint uses `KOPIA_CONFIG_PATH` when provided, otherwise `/data/connection/repository.config`. It validates and reuses that persisted profile **before** considering any bootstrap handoff. The generated manager YAML uses the selected path. No profile is copied or rewritten by our script; the Kopia CLI owns its own profile format and credentials.

Existing manager YAML is preserved. If changing a deployment's connection path, update its `kopia_config` field to agree with `KOPIA_CONFIG_PATH`. Alternatively, supply a complete application configuration and administrator file, set `command: [-config, /config/config.yaml]`, and mount the paths referenced there. That command bypasses automatic environment preparation entirely and uses the YAML-selected backend directly.

Only an explicitly configured `CRAWLBOX_BOOTSTRAP` enables the bundled handoff convention. Without it, an unavailable external server is retried using its profile, never replaced with an automatically initialized local repository or a connection to `kopia:51515`.

## Optional Web UI proxy

External collection does not require the Web UI proxy. The manager image does not set a proxy listener, upstream URL or fingerprint file. To expose an external server's Web UI through the manager, add the port mapping and configure it separately:

```yaml
ports:
  - "8080:8080"
  - "8081:8081"
environment:
  KOPIA_CONFIG_PATH: /connection/repository.config
  KOPIA_UI_PROXY_LISTEN: :8081
  KOPIA_UI_PROXY_TARGET: https://storage.example:51515
  KOPIA_UI_PROXY_PIN_FILE: /connection/ui-pin.json
```

For a self-signed certificate, create `connection/ui-pin.json` containing `{"fingerprint":"REPLACE_WITH_TRUSTED_SHA256"}`. No password is required in that file. Omit `KOPIA_UI_PROXY_PIN_FILE` when using a system-trusted HTTPS certificate. Enable the proxy in the manager UI; authenticate using the external Kopia UI account. The proxy's URL is independent of the business client profile and does not change its connection.

## Verification

`TestExternalManagerEntrypoint` starts an external server using only the standard Kopia CLI, then launches the manager entrypoint without a handoff or proxy environment. It verifies initial startup and restart. CI repeats it against the actual manager Docker image before building the Kopia wrapper image; only the manager container runs, connecting to the separately prepared server. The regular two-container Compose and standalone binary tests remain in the same CI suite.
