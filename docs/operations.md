# Storage and recovery

Authoritative history is the set of complete, pinned business snapshots. Each includes files/, artifacts/ and revision.json. Its metadata contains the parent revision, plugin identity, state, file hashes and changes. Snapshot creation forces hashing, disables repository actions for the upload, and rejects incomplete/error/excluded-file results. The catalog is committed only after successful snapshot creation. No-change and failed collection discard pending state.

A failure or ambiguous result during snapshot creation blocks further runs for that source until restart recovery. This avoids creating a fork when the server stored a snapshot but the acknowledgement was lost. Recovery imports complete tagged snapshots in parent order, is idempotent, and stops on a gap/fork. It never fabricates missing historical bytes. A deleted current tree is restored and verified from the latest snapshot.

Staging and current paths are owned by the application. Candidate construction hardlinks immutable objects on the same filesystem, with copy fallback; published objects are never modified in place. Keep enough free disk for active downloads, restores and archives. HTTP timeouts and staging limits fail the run without advancing state. No automatic deletion of pinned history is implemented.

The management database also holds tokens, source-ID retirement records and recent runs. Back it up separately with configuration, connection credentials and TLS keys. Business snapshot recovery restores revisions and state, not old tokens or full run history. Without the management database, reissue tokens and preserve the source-ID inventory administratively; do not repurpose historical IDs. The Kopia repository itself needs independent backup.

Cache defaults to a 20 GiB soft limit; current state is outside that limit. Cache contents may be removed while the process is stopped. Staging leftovers after a crash are disposable after recovery. Configuration reload is by restart; schedules do not replay missed intervals. Default global collection concurrency is one. The timeout covers the whole run, including queue wait.

The process logs startup/error context, not Authorization headers or full token secrets. UI credentials are bcrypt hashes in an external file. Token secrets are random 256-bit values, stored as SHA-256 digests with a public lookup ID. Token scope and expiry are immutable; rotate by creating a new token and revoking the old one.
