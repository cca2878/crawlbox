# Manager

A self-hosted collection manager with isolated WASM extensions and immutable Kopia history. Configuration, collection management and public historical data have separate interfaces. Go builds and tests require `CGO_ENABLED=0`; no native Extism or SQLite libraries are used.

## Development

Read [agent instructions](AGENTS.md), [coding conventions](docs/coding.md), and [ABI v1](docs/protocol.md).

```sh
make check test build
make integration    # builds fixed Kopia CLI; runs an authenticated real server locally
make vuln
```

Requires Go 1.26.6, Linux, make and network access for locked module downloads. Integration tests generate their own local repository, server certificate and test credentials; they do not contact an upstream collection service. Tests without `FIXTURE_WASM`/`KOPIA_BIN` skip the explicit integration cases; `make integration` supplies both. CI executes on native amd64 and arm64 runners. The module path is a local project identity; no GitHub remote is needed to build.

## Deploy

The image includes `manager` and Kopia CLI v0.23.1. `deploy/compose.yaml` uses the matching official Kopia Server image. Set `MANAGER_IMAGE` to a fixed versioned GHCR image after publishing your repository; do not use `latest`.

1. Prepare the bind directories in `deploy/compose.yaml`. The manager runs as UID/GID 10001; its data directory must be writable and its configuration, credentials and client connection readable by that user.
2. Initialize the server repository using the official CLI with `repository create filesystem --path /repository`. Keep its connection file in `/app/config`, and create a repository-server user such as `worker@manager` with `server user add`. Repository password and UI password are distinct from API tokens.
3. Supply a TLS certificate and key at the Compose paths, start Kopia, and connect the manager CLI with `repository connect server --url https://kopia:51515 --server-cert-fingerprint <SHA256> --override-username worker --override-hostname manager`. Run this using the manager image's `kopia` entrypoint override, mounting the client connection directory writable during setup. Supply the repository-server user's password through `KOPIA_PASSWORD`; the resulting connection file is a secret. The steady-state manager mounts it read-only.
4. Copy `deploy/config.example.yaml` to the mounted config directory. Install compatible plugin packages into the read-only extension mount; register source entries with their actual WASM digests and exact authorized hosts. Plugin-specific setup belongs to each plugin package.
5. Generate a bcrypt password hash with `manager hash-password`, reading the password from stdin. Put `username` and `password_hash` in `/config/admin.yaml`; do not put the password in command arguments.
6. Start the manager. It verifies plugin digests, descriptors and configuration, recovers the snapshot catalog, then enables schedules. Visit `/ui/` using the administrator credential to trigger collection and create API tokens.

The supplied port mappings bind to loopback. Use an HTTPS reverse proxy for browser/client access across machines; preserve Host, Origin and Sec-Fetch-Site for the standard cross-origin protection middleware. No write operation uses GET. Configuration changes require restart. An OS file lock prevents two managers sharing one data directory.

## Interfaces

- `/ui/`: independent administrator Basic authentication; collection status, progress, failures, schedules, latest revision, trigger forms, token creation/list/revocation. It is not a Kopia file browser or repository maintenance UI.
- `/api/v1`: historical read-only data, using `Authorization: Bearer <token>`. It has no run, scheduler, trigger, configuration or token-management endpoints.

See [public API](docs/api.md) and [operations](docs/operations.md). Tokens authorize explicitly selected source IDs, optionally expire, and display their secret only at creation. New sources are not automatically authorized. Source IDs cannot be reintroduced once retired or changed to another plugin identity.

## Releases

Each version tag builds and verifies both architectures before publishing the application image to GHCR. The build does not fetch or reference any external plugin repository. A bundled generic fixture provides protocol and lifecycle integration coverage.

## License

Original project source code, tests, documentation and configuration are licensed under [Apache License 2.0](LICENSE) (`Apache-2.0`). See [NOTICE](NOTICE). Third-party dependencies and bundled components retain their respective licenses and attribution requirements.

The container includes the project LICENSE and NOTICE in `/usr/local/share/licenses/manager/`.
