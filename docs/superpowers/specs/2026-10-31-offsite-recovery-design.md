# RelaDock encrypted offsite recovery design

## Intent

RelaDock needs a reproducible way to move control-plane and node recovery material away from the running host, prove that the copy is intact, and stage a restore without overwriting live state. The feature is for an authorized operator, not an unattended cloud-backup service. It must not turn a database dump alone into a false claim that forwarding, metering or Agent state has recovered.

## Decisions

- Use the external `age` CLI with an operator-supplied recipient. The repository never creates, stores or uploads the recipient private key.
- Produce separate encrypted bundles for the control database and for each node. A control bundle cannot substitute for node PKI, configuration journal, dataplane/shaping state, updater state or usage WAL.
- Every plaintext payload is created in a private temporary directory, has a versioned canonical manifest and SHA-256 entries, is archived as a flat regular-file tar, encrypted, fsynced and then removed. Success is printed only after the final encrypted file is durable.
- Creation is no-overwrite. The destination must be an existing absolute operator-owned directory; output uses a random name and mode 0600. Upload/replication to object storage is deliberately external.
- Control creation uses local PostgreSQL client tools and `DATABASE_URL`. Connection fields are passed through libpq environment variables, never command-line arguments or logs. The dump uses custom format and records schema version plus non-secret row-count/ledger invariants in the manifest.
- Control verification decrypts to a private temporary directory, checks the exact archive whitelist and manifest hashes, restores into a newly generated temporary database, runs schema and wallet/ledger consistency checks, then removes only that temporary database. It never targets the source database.
- Node creation must run as root on Linux with both Agent units and the updater inactive. It acquires the existing control lock non-blockingly and archives only fixed paths under `/etc/reladock`, `/var/lib/reladock-agent`, `/var/lib/reladock-updater` and the control-lock metadata. Missing optional updater state is recorded; symlinks, sockets, devices, hard links and paths outside the whitelist fail closed.
- Node verification decrypts and extracts only into a new absolute staging directory. It verifies the manifest, regular-file/directory types, relative paths, modes and hashes. It never writes `/etc/reladock` or `/var/lib` and never starts a service.
- A real restore remains a two-phase operation: verify/stage first, then an operator-approved maintenance window copies staged state onto an empty replacement host, rechecks interface/namespace/shaping leases and starts validation before apply. The tooling will not automate that promotion in this increment.

## Interfaces

The release includes a small Go binary, `reladock-recovery`, so control and node hosts do not need Node.js. It exposes four modes:

- `control-create --destination /absolute/existing/dir --recipient age1...`
- `control-verify --bundle /absolute/file.age --identity /absolute/age-identity`
- `node-create --destination /absolute/existing/dir --recipient age1...`
- `node-verify --bundle /absolute/file.age --identity /absolute/age-identity --staging /absolute/new/dir`

The binary invokes an operator-selected absolute `age` path (default `/usr/bin/age`) without a shell. Control modes likewise invoke absolute PostgreSQL client paths and pass parsed connection values through libpq environment variables. Node modes are packaged in the Linux release; the target host has no Node.js dependency.

## Security and failure behavior

- Reject relative paths, destination symlinks, existing output/staging targets, unsupported age recipients, plaintext output options and unknown arguments.
- Bound command output, archive entry count, individual file size and total extracted size. Never follow redirects or invoke a shell.
- A failed dump, tar, encryption, fsync, hash check, restore or invariant check removes only task-owned temporary files and temporary databases. Existing bundles and live state remain untouched.
- Secrets may exist inside encrypted node bundles; manifests must not contain certificate bodies, private-key bytes, database passwords, session tokens or Agent lease tokens. Console output contains only bundle path, size, SHA-256 and verification result.
- Verification proves bundle integrity and database invariants, not freshness, offsite replication, RPO/RTO, kernel state, data-plane health or customer protocol health.

## Acceptance

- Tests cover no-overwrite paths, symlink/archive traversal, extra/missing/duplicate entries, hash mismatch, wrong age identity, command failure, cleanup, temporary database naming and cleanup, wallet/ledger mismatch, stopped-service/lock gates and node staging-only behavior.
- An opt-in local test may use installed `age`, PostgreSQL clients and the project test database. Missing external tools skip only the opt-in test; parser/manifest/security tests remain mandatory.
- Documentation records a production drill checklist and explicitly leaves real off-host storage, key custody and measured RPO/RTO unverified until an authorized drill is executed.
