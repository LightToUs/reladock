# Lightsail Script Node Module Design

## Intent

Add an independently bounded Amazon Lightsail module to RelaDock so a platform
administrator can create a generic Linux node, run exactly one administrator-
supplied Bash script on that node over verified SSH, stop the node, and destroy
it. These nodes are not RelaDock relay nodes and do not enter the existing
`nodes`, Agent, forwarding, metering, package, or safe-retirement workflows.

The operator's desired identity is one Lightsail instance, one static public
IPv4 address, and one immutable deployment script. The script is executed at
most once.

## Goals

- Provide a separate platform-admin page and `/api/admin/lightsail/*` API.
- List the allowed Lightsail regions, active Linux blueprints, bundles, and
  availability zones from AWS rather than hard-code catalog identifiers.
- Create one Lightsail instance with a deterministic name and RelaDock-owned
  tags, allocate and attach one static IPv4 address, and limit SSH ingress to a
  configured controller CIDR.
- Obtain temporary SSH access details and authoritative host keys through the
  Lightsail API, verify the remote host key, and run one Bash script with
  non-interactive `sudo`.
- Persist every cloud lifecycle stage, AWS operation ID, bounded sanitized
  execution result, and platform audit entry so process restarts do not turn an
  unknown result into a success or repeat a purchase/destructive action.
- Stop an instance on an explicit administrator action.
- Destroy only a stopped instance on a separate explicit administrator action,
  then detach and release its static IP.
- Keep the integration disabled unless its complete runtime configuration is
  valid.

## Non-goals

- No customer or tenant access, customer-triggered purchase, package/payment
  integration, automatic scaling, spare-pool orchestration, or automatic
  replacement.
- No RelaDock Agent installation, relay-node registration, nftables/tc changes,
  forwarding rule creation, metering, or endpoint-health claim.
- No arbitrary source-code build system. The submitted payload is a Bash script.
- No script editing, replay, or manual retry after remote execution begins.
- No instance start/restart action in the first version. "Close" means
  `StopInstance`; "destroy" means permanent deletion.
- No AWS credentials, SSH private keys, or temporary access material stored in
  PostgreSQL or returned to the browser.
- No real AWS calls, instance purchases, SSH connections, or destruction during
  automated tests.

## Administrator Experience

The platform sidebar gains a separate `Lightsail` page. When the connector is
disabled, the page reports that fact without exposing configuration details.
When enabled, the page contains:

1. A creation dialog with instance name, region, availability zone, active
   Linux blueprint, active compatible bundle, and Bash script.
2. A table of only the Lightsail instances created through this module. It shows
   region, instance name, static IPv4 address, lifecycle state, deployment
   state, script digest, most recent bounded result, timestamps, and available
   actions.
3. A close confirmation for running instances.
4. A destroy confirmation that repeats the exact instance name, region, and IP.

Creation returns a local job immediately. The page polls the local job state; it
does not wait in one HTTP request for AWS or SSH. A failed script does not cause
automatic instance deletion. A clean non-zero exit, timeout after remote start,
or lost SSH connection is terminal because the script may have changed the
machine. The administrator may close or destroy the machine but may not run the
script again.

## Configuration and Credentials

The module uses AWS SDK for Go v2 and the SDK's default credential chain. The
browser never submits AWS access keys. Deployment configuration supplies:

- `LIGHTSAIL_ENABLED=true|false`, default `false`;
- `LIGHTSAIL_ALLOWED_REGIONS`, a non-empty comma-separated allowlist when
  enabled;
- `LIGHTSAIL_SSH_SOURCE_CIDR`, one literal IPv4 `/32` or IPv6 `/128` describing
  the controller's public egress address;
- optional worker, AWS-operation, SSH-connect, and script-execution timeouts
  with conservative bounded defaults.

Partial or invalid enabled configuration fails startup. AWS credentials remain
outside application configuration validation and are resolved only by the SDK;
missing/expired credentials make catalog and jobs fail with a sanitized provider
error. Production should supply a least-privilege workload identity or STS role,
not browser-provided or database-stored long-lived credentials.

## Module Boundaries

`internal/cloud/lightsail` owns cloud catalog and lifecycle calls behind a small
provider interface. It translates SDK models and errors into provider-neutral
types and never imports `internal/platform` or `internal/web`.

`internal/cloud/sshdeploy` owns temporary SSH credential parsing, host-key
verification, Bash execution, output bounding, and control-character removal.
It accepts the AWS-witnessed host keys and temporary key only in memory.

`internal/platform` owns PostgreSQL facts, permissions, leases, state-machine
transitions, immutable script identity, and platform audits. It talks only to
provider and SSH interfaces injected by the runtime worker.

`internal/web` owns strict admin HTTP request/response handling. The static
admin module owns rendering, dialogs, polling, and stale-response isolation.

`cmd/reladock` loads configuration, creates the SDK-backed provider only when
enabled, and starts/stops the worker with the process context.

## Persistent Model

Migration v39 adds two platform-owned tables.

`lightsail_instances` contains a generated UUID, deterministic AWS instance
name, AWS ARN when known, region, availability zone, blueprint, bundle, static
IP name/address, AWS state, local lifecycle state, deployment state, script
SHA-256, script size, AWS operation IDs, SSH host-key SHA-256 fingerprints,
bounded sanitized output, exit code, revision, timestamps, and terminal error
code. The AWS instance name and static IP name are unique.

`lightsail_jobs` contains a generated UUID, instance ID, kind, state, lease
token/expiry, attempt counter, next-attempt time, current stage, provider
operation ID, bounded sanitized failure code, and timestamps. At most one
non-terminal job exists per instance.

The submitted script is stored in a separate nullable encrypted-at-rest-by-
deployment database column only until the worker crosses the execution boundary.
RelaDock does not claim application-layer encryption because the repository has
no established key-management facility. Immediately before remote execution the
worker persists `execution_started_at`, clears the script body in the same
transaction, and retains only its SHA-256 and size. Database backup operators
must therefore treat pending script payloads as sensitive. The script is never
written to platform audit details, application logs, or API responses after
creation.

## Create and Deploy State Machine

The create request validates platform-admin permission, the region allowlist,
catalog identifiers, an AWS-compatible instance name, and a UTF-8 Bash script
between 1 byte and 64 KiB. It creates the local instance/job and
`lightsail.create-requested` audit in one transaction. A request idempotency key
is required; replay returns the same local job and changed content conflicts.

The worker processes these durable stages:

1. `reconcile-instance`: call `GetInstance` by deterministic name. Create only
   when absent, with `reladock-managed=true` and the local UUID tag. Persist the
   returned operation ID before polling.
2. `wait-instance`: poll the exact operation and instance state with bounded
   backoff. AWS `running` means only cloud runtime state.
3. `configure-ssh-firewall`: as soon as the instance API accepts network
   changes, replace its public-port set with TCP/22 ingress only from the
   configured source CIDR. No application ports are guessed or opened.
4. `reconcile-static-ip`: find or allocate the deterministic static-IP name,
   attach it to the exact instance, and persist its address and operation IDs.
5. `wait-ssh-details`: call `GetInstanceAccessDetails(ssh)` until AWS returns a
   temporary private key, username, IP, and at least one valid host key.
6. `execute-script`: acquire the job/instance transaction lock, persist the
   execution boundary, clear the script body, then connect to the recorded
   static IP and verify that the presented key exactly matches an AWS-witnessed
   public host key. Run `/usr/bin/sudo -n /bin/bash -seu` once with the script on
   stdin. Do not log the command or script.
7. `terminal`: persist `succeeded`, `failed`, or `unknown`, exit code when known,
   bounded output, fingerprints, and one platform audit. Nothing in this state
   marks a RelaDock node ready or healthy.

Before stage 6, transient provider/SSH-readiness failures may retry with bounded
backoff. Once `execution_started_at` is persisted, the worker never starts
another SSH command for that instance. If the process dies between persisting
the boundary and starting SSH, the terminal result is conservatively `unknown`.
Lightsail may briefly expose a blueprint's default firewall between instance
creation and the first successful port replacement; the UI and operations
documentation disclose that the API cannot provide a zero-exposure create-and-
firewall transaction.

## Close State Machine

An administrator submits the current revision. The transaction requires no
active lifecycle job, a known AWS resource, and a non-destroyed instance; it
creates one `stop` job and audit request. The worker reconciles the actual AWS
state, calls `StopInstance` only when needed, records the operation ID, and waits
for `stopped`. Replayed HTTP requests return the existing job. Stopping preserves
the static IP, script digest, deployment result, and instance record.

## Destroy State Machine

An administrator submits the current revision and the exact instance name as a
confirmation string. The transaction requires the reconciled AWS state to be
`stopped` and no active job. The worker:

1. reconciles the exact instance and static IP ownership;
2. detaches the static IP if attached;
3. deletes the exact instance and waits for terminal absence;
4. releases the exact static IP;
5. marks the local record `destroyed` and preserves metadata/audits.

Each stage is restart-safe and verifies tag/name/local-ID ownership before a
destructive provider call. A mismatch stops in `manual-review`; there is no
force-delete path. Provider absence is treated as success only when the stored
ownership evidence and the stage make that absence expected.

## Error, Logging, and Security Rules

- Provider error bodies, AWS credentials, temporary SSH keys, script contents,
  and raw request payloads never enter logs, audits, or browser responses.
- Script stdout/stderr is combined, limited to the final 32 KiB, normalized to
  UTF-8, stripped of terminal control sequences, and masked for common token,
  password, secret, and authorization assignment forms. Because arbitrary
  output cannot be proven secret-free, the page warns administrators not to echo
  credentials and only platform admins can read the result.
- SSH accepts only AWS-witnessed host public keys from the exact instance access
  response. No insecure host-key callback or TOFU fallback exists.
- HTTP bodies use strict JSON, existing Host/origin/CSRF/session/rate-limit
  protections, a 96 KiB request cap for creation, and no tenant API-key access.
- All state-changing requests require an idempotency key, current revision, or
  exact confirmation as appropriate. Platform audit failure rolls back the local
  state change before any provider call.
- AWS API throttling is handled with SDK retry plus durable bounded backoff; jobs
  never busy-poll.
- The AWS SDK HTTP client has explicit connect/request timeouts, does not honor
  untrusted browser proxy settings, and never follows provider-supplied URLs.

## Testing and Verification

Tests use deterministic provider and SSH substitutes; they never use live AWS
credentials or network access. Coverage includes:

- disabled and partial configuration;
- platform-admin authorization and strict HTTP input;
- create idempotency and changed-payload conflict;
- catalog allowlisting and active Linux compatibility;
- crash/retry before and after every provider operation;
- no duplicate instance or static IP after uncertain responses;
- authoritative SSH host-key match and mismatch;
- script size, one-time execution boundary, process loss, timeout, non-zero exit,
  output bounding and masking;
- stop replay, already-stopped reconciliation, and failure recovery;
- destroy refusal while running, confirmation mismatch, ownership mismatch,
  staged detach/delete/release restart recovery, and no force-delete path;
- audit rollback before provider calls and secret-absence assertions;
- migration v39 preservation and idempotent rerun;
- admin UI dialog, polling, stale-response, action, and sensitive-data rendering
  contracts.

After focused tests, run `npm test`, `npm run vet`, and `git diff --check`. No
live Lightsail creation, SSH deployment, stop, or destroy is part of automated
verification. A later explicitly authorized sandbox acceptance must use a
dedicated AWS account/region, a non-sensitive script, a cost ceiling, and manual
confirmation before destruction.

## Documentation and Status

Update README, requirements, architecture, roadmap, deployment/operations, and
implementation status to distinguish:

- code implemented with provider substitutes;
- connector disabled by default;
- no live AWS account, purchase, SSH, billing, or destroy acceptance;
- cloud `running`, script exit zero, and local job success are not RelaDock
  forwarding or end-to-end health.

## Builder Calls

The following choices are implementation calls rather than user-confirmed
product requirements:

- The first version includes a platform-admin UI because script paste requires a
  protected operator surface.
- Every created instance receives a static IPv4 address so the requested
  IP-to-script identity remains stable across stop/start.
- Temporary SSH credentials and host keys come from
  `GetInstanceAccessDetails`; no permanent SSH private key is persisted.
- Script bodies are cleared at the one-time execution boundary and cannot be
  replayed.
- The module supports active Linux/Unix Lightsail blueprints only.
- Destruction requires the instance to be stopped first and has no force path.
