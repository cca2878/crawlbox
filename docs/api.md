# Public API v1

Every endpoint requires a Bearer token in the Authorization header. Missing, invalid, expired and revoked tokens return 401; resources outside the authorized source set return 404. UI credentials and query-string tokens are not accepted. Responses use private/no-store client caching. Server-side byte caches never bypass authorization.

| GET path under /api/v1 | Result |
| --- | --- |
| /sources | Authorized sources with published data: id and name |
| /sources/{source}/revisions | Revision summaries |
| /sources/{source}/tags | Nonunique tag to revision mappings |
| /sources/{source}/revisions/{revision}/metadata | ID, parent, source, created_at, plugin ID/version, tags and opaque metadata |
| /sources/{source}/revisions/{revision}/changes | files and artifacts change arrays; added/modified/deleted with before/after SHA-256 |
| /sources/{source}/revisions/{revision}/files | Paginated complete file entries |
| /sources/{source}/revisions/{revision}/files/{path} | Original file bytes; GET/HEAD, ETag and Range |
| /sources/{source}/revisions/{revision}/artifacts | Paginated artifact entries |
| /sources/{source}/revisions/{revision}/artifacts/{path} | Original artifact bytes; GET/HEAD, ETag and Range |
| /sources/{source}/revisions/{revision}/archive | tar.gz containing files/ and artifacts/ |

`revision` may be `latest` or an immutable revision ID. File-list pagination uses zero-based offset and limit (1..1000, default 1000); response fields are entries, total and next_offset. Entry fields are path, size and sha256. First revision changes are additions. Version tags are plugin-provided opaque strings and can repeat.

Snapshot IDs, opaque Plugin State, deployment configuration and token records are not included in public summaries. No SQL query API is exposed for database artifacts: clients download their bytes.

Archive generation is request-driven and cached per immutable revision, with concurrent generation serialized. Failure mid-stream terminates the gzip stream and never publishes a cache entry. Clients must treat interrupted/truncated archives as failures and retry. Cache limits are soft: an active restore or archive can temporarily exceed the configured size; completed entries are evicted between requests. A revoked token cannot begin another request, but an already authorized stream may finish.
