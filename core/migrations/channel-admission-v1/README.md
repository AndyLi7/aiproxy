# Channel admission v1 — review-only draft

The PostgreSQL and SQLite scripts add independent C admission tables. They do
not activate an epoch or alter existing task/channel rows. They are not wired
to application startup. Production application requires separate review.

Only a deployment with DB and LogDB represented by the same database handle is
currently supported. MySQL and split-store deployments refuse this new C
protocol before writes; existing requests/queues are unchanged because request
integration is not yet enabled.

Keep these tables on rollback. Empty epoch, missing storage, missing deployment
proof or unknown versions must refuse dangerous operations. See
[the deployment contract](../../../docs/b2-c-admission-deployment-contract.md)
for the unresolved control-plane identity, ingress fencing and runtime
integration gates. No production issuer or successful default verifier exists.
