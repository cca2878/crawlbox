# Manager

A self-hosted collection manager with isolated WASM extensions and immutable Kopia history. Configuration, collection management and public historical data have separate interfaces. Go builds and tests require `CGO_ENABLED=0`; no native Extism or SQLite libraries are used.

## Development

Read [agent instructions](AGENTS.md), [coding conventions](docs/coding.md), and [ABI v1](docs/protocol.md).

```sh
make check test build
make integration    # builds fixed Kopia CLI; runs an authenticated real server locally
make vuln
```

Requires Go 1.26.6, Linux, make, Python 3, Bash, jq, OpenSSL, util-linux and network access for locked module downloads. Integration tests generate their own local repository, server certificate and test credentials; they do not contact an upstream collection service. Tests without `FIXTURE_WASM`/`KOPIA_BIN` skip the explicit integration cases; `make integration` supplies both. CI executes on native amd64 and arm64 runners. The module path is a local project identity; no GitHub remote is needed to build.

## Deploy

For a new NAS deployment, download [deploy/compose.yaml](deploy/compose.yaml), import it into your NAS container manager, and start the project. Open `http://NAS-IP:8080/ui/` to create the first administrator. Choose the shared image release tag with `CRAWLBOX_VERSION`. Credentials and certificates are generated automatically, and the default named-volume layout requires no directory preparation. Initialize the administrator on your trusted LAN before exposing the service elsewhere.

The deployment uses two images: `crawlbox` for the manager and `crawlbox-kopia`, a thin derivative of the official `kopia/kopia:0.23.1` image. Both are published with the same crawlbox release tag. Set `CRAWLBOX_VERSION` to that tag in your Compose environment. Each image embeds an entrypoint that prepares its own environment; no initializer service is needed. The server has no published ports and lives on an internal Docker network. The manager publishes port 8080 for its UI/API and port 8081 for the optional Kopia Web UI proxy, and has outbound network access for collection. The proxy is disabled initially; enable it in the manager UI. Internal communication uses authenticated TLS with a pinned, automatically generated certificate.

Named volumes hold manager configuration/catalog, the Kopia repository and server secrets, the shared connection handoff, and installed extensions. Manager defaults to UID/GID 10001; the Kopia wrapper inherits the official image identity (root in the pinned version). Compose `user:` can override the identity; entrypoints do not change users or existing mount ownership. Named volumes work with the image defaults; bind mounts must grant access to the chosen identity. Keep these volumes when recreating containers. First-start preparation preserves existing credentials and configuration; restarting does not reset the administrator. See [NAS deployment and operations](docs/nas.md).

An existing external Kopia Server can be used with the manager image alone; see [external Kopia deployment](docs/external-kopia.md) and [manager-only Compose](deploy/compose.external.yaml). The Kopia wrapper only provides automatic server initialization for the bundled two-service Compose.

For direct Linux binary or systemd deployment, see [standalone deployment](docs/standalone.md). Runtime does not require container entrypoints or a fixed OS identity.

For an existing manually initialized deployment, retain your existing Compose file or use [deploy/compose.manual.yaml](deploy/compose.manual.yaml). The automatic deployment uses a different volume layout and does not migrate an existing repository automatically.

## Interfaces

- `/ui/`: independent administrator Basic authentication; collection status, progress, failures, schedules, latest revision, trigger forms, per-run cancellation before snapshotting, token creation/list/revocation. Navigation separates sources, runs, revisions, API tokens and service settings using server-rendered pages. Native disclosure elements expand errors and management forms; no JavaScript or frontend dependencies are required. It is not a Kopia file browser or repository maintenance UI.
- `/api/v1`: historical read-only data, using `Authorization: Bearer <token>`. It has no run, scheduler, trigger, configuration or token-management endpoints.

See [public API](docs/api.md) and [operations](docs/operations.md). Tokens authorize explicitly selected source IDs, optionally expire, and display their secret only at creation. New sources are not automatically authorized. Source IDs cannot be reintroduced once retired or changed to another plugin identity.

## Releases

Each version tag builds and verifies both architectures before publishing the manager and thin Kopia wrapper images to GHCR with the same tag. The build does not fetch or reference any external plugin repository. A bundled generic fixture provides protocol and lifecycle integration coverage.

## License

Original project source code, tests, documentation and configuration are licensed under [Apache License 2.0](LICENSE) (`Apache-2.0`). See [NOTICE](NOTICE). Third-party dependencies and bundled components retain their respective licenses and attribution requirements.

The container includes the project LICENSE and NOTICE in `/usr/local/share/licenses/manager/`.
