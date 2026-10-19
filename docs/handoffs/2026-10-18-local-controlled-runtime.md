# Local controlled runtime, first slice

This is a local, fixed-member supervisor for a newly created private SQLite
namespace. It is **not** a Railway issuer, production installation, all-replica
proof, migration recovery watermark or permission to migrate. No production
schema or automatic migration chain is changed.

## What runs

`go build -o /absolute/gateway ./core` and
`go build -o /absolute/coordinator ./core/cmd/migration-runtime` build the actual
child gateway and launcher. The launcher accepts `-launch /absolute/launch.json`.
The reviewed JSON contains `Directory` (new private directory), `Seed`,
`SeedSHA256`, `Executable`, `ExecutableSHA256`, `Members` (fixed `ServiceID` and
loopback `Listen`), `ValidUntilMs`, and `RevocationFreshUntilMs`. Deadlines are
explicit absolute values from the trusted local operator; there is no new default
business TTL. Stdin accepts `stop` or a strict JSON `renew` command with those two
deadline fields. No public HTTP issuer, registry, tunnel or paid infrastructure.

The seed must already contain all required tables/protocols, a never-activated
control row and no business/admission/migration/task/billing history. Test fixtures
provision synthetic schemas; the runtime never provisions or repairs a seed.
Configuration rows are permitted only in the documented whitelist; the operator
reviews their original bytes through the exact seed hash. File owner and mode,
sidecar absence, regular-file type, exact seed and executable bytes are checked.
The executable and seed are copied into the exclusive private directory.

The actual gateway receives the exact private DB path/hash, issuer public key,
session nonce and artifact hash only through inherited anonymous pipes. It hashes
its executable and uses the existing single `model.ProcessBootIncarnation()`.
The supervisor authenticates that reply on its own inherited spawn session; an
environment variable or an HTTP claimed boot cannot enroll another process.
All members acknowledge the fresh seed before the first signed activation. The
main executable checks this before `AutoMigrate`, `InitOption`, caches, HTTP and
background startup. Controlled DB opening uses SQLite `mode=rw`, not environment
fallbacks which could silently create another database.

The exact signed proof is fsynced outside DB transactions before issuance.
`InstallControlledAdmissionRuntime` and actual `Activate` install it. Each new
admission still uses the existing persistent proof/DB-clock checks. Renewal keeps
the same fixed membership and namespace, increases sequence and uses a fresh
proof ID. Expiry does not refresh permission or close existing uses. Revocation
updates only the exact installed persistent proof ID/fingerprint. A PostgreSQL
row-lock test verifies this primitive; PostgreSQL runtime startup is NOT supplied.

A known revoke/seal refusal is sticky: database-write failure or subsequent source
loss cannot replace the original nonzero proof/conflict with an outage. The
no-anchor frozen-task compatibility path that consults the source therefore
still rejects that refusal. Existing durable execution anchors retain their
original history semantics; this slice does not invent task closure.
A missing/invalid private source stops the local child; the supervisor stops and
joins its entire owned fixed set. An explicit stop records revocation before
sending it, waits for process exits, and retains errors instead of claiming clean
completion. Process exit does not certify task, billing or use closure. No old
session directory can restart. A restored DB prefix cannot enroll in the old
namespace or revive its old proof. An ordinary never-activated legacy process
retains existing behavior; a controlled transaction cannot take that shortcut.

## Deliberately closed

Controlled runtime rejects migration `PrepareMigration`, `SealPrepared`,
`CommitPreparedPins`, forward/reverse atomic Cutover, and legacy `Seal` before
migration authority is obtained. This does not disable ordinary native task or
handoff functions with similar names. Historical reads are unchanged.

`issuance.log` is a fsynced hash-chain audit, not a migration intent/checkpoint
journal. Its verifier is diagnostic only. Complete prefix-truncated lines can
remain a valid diagnostic prefix; this NEVER authorizes restart. Existing
namespace reuse is always refused even with a valid log. Independent migration
intent + exact committed ACK and trusted restore checkpoint are still required
before any migration mode can be opened.

The local trust boundary includes the operator, OS UID, private directory, binary
and provisioned seed. It does not defend against a malicious local root/owner,
assert external processes are fenced, or attest another host/fleet. Restoring the
entire host plus supervisor state is outside this slice. Source loss/unknown exit
is not safe-disable evidence. Runtime privileges cannot be inferred from test
signatures or synthetic fixtures.

## Validation and provenance

The implementation lives in `/private/tmp/b2-runtime-bootstrap-work` while the
original iCloud/Git checkout is unavailable. No Git mutation, commit, push,
deployment, real DB connection, provider authentication or paid generation was
performed. Base source hash inventory and exact overlay manifest are retained
under `/private/tmp`; final evidence/commands are in `/private/tmp/b2-runtime-final`.
The cache implementation is the peer-reviewed unique boot source, not a copy with
a second process identity. Source overlay must be reviewed and committed from a
restored real worktree later; production prerequisites remain separate.
