# Crawler ABI v1

A package contains plugin.wasm and plugin.json (id, version, abi=1, hosts, config_schema). The deployment pins the WASM SHA-256. Host imports are `extism:host/user.call`, taking and returning pointers to Extism JSON memory. Exports `describe`, `validate_config`, `run` return Extism status 0 or error 1.

Run input: {abi:1, source, run, config, state, previous_revision}. Exports return {status:"no_change"} or {status:"candidate", state, metadata}. The host buffers candidate operations until snapshot commit. State is opaque JSON, default maximum 1 MiB. No-change and failure discard pending state.

Host request: {op, path?, handle?, data?, offset?, limit?, url?, method?, headers?, body?, size?, sha256?}. Host response: {error?, handle?, data?, size?, sha256?, status?, headers?, entries?}. Byte fields use base64 JSON encoding. Each data chunk is at most 1 MiB. Error means the operation failed; plugins must propagate errors.

Operations: http (response body staged, handle returned); create; read; write (append only); close; stat; previous_file; previous_artifact; list_files; publish_file; publish_artifact; delete_file; progress. Previous reads are immutable. publish operations use an existing staged handle. Paths are relative slash paths, never empty/absolute, and cannot contain dot segments, backslashes, NUL or symlinks. list_files uses offset/limit and entries {path,size,sha256}. Artifacts are replaced as a complete set by each candidate; files inherit the previous complete view. No filesystem paths, SQL or storage credentials cross this boundary.

HTTP targets must satisfy both descriptor hosts and source hosts. Redirects are reauthorized. Downloads and writes share a cumulative staging quota. Host cancellation applies to network and WASM calls. Plugins cannot directly use Extism HTTP or filesystem access. Describe/config validation are read-only and have no host capabilities.

Revision snapshots contain files/, artifacts/, revision.json. Revision records include id, source, parent, created_at, plugin identity, state, metadata, files, artifacts, changes. The snapshot ID is stored in the catalog, not recursively in revision.json. Only complete pinned snapshots are publishable. Authorization never depends on artifact contents.
