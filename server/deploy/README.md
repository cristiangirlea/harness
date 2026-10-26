# Development KEDA deployment slice

These charts render one Kubernetes namespace per tenant and one zero-or-one
StatefulSet per project. KEDA 2.21 reads a project-scoped demand value from the
always-running control service through the Metrics API scaler. The checked
values are polling every 10 seconds, a 300-second idle cooldown, and a
120-second pod termination grace. The StatefulSet retains its workspace PVC
when scaled or deleted. The tenant chart applies restricted Pod Security labels,
a quota, and default-deny ingress/egress. The project chart allows only DNS and
the labeled control pod for egress. A compatible network-policy implementation
must actually enforce those rules.

This is a development fixture, not a hosted multi-tenant deployment. The worker
only runs API reasoning against the local fake-model sidecar; it has no shell,
file, or MCP execution. The fake-model container receives no PVC mount or
worker token, but it shares the Pod network namespace and is not an
isolation boundary for untrusted code. The control chart installs a TLS-enabled service.
PostgreSQL, TLS certificates,
a real model provider, KEDA, a StorageClass, and network-policy support are
separate prerequisites. Its syntactically valid dummy image value in
the verifier is not a deployable registry reference. Do not use the fake model
as a substitute for a paid model or advertise this as production isolation.

The operator must register the tenant and project in the control service,
check that the project belongs to that tenant, and provision separate Secrets
in that tenant's namespace:

- a 64-character project worker token in the worker Secret key `token`;
- the independent scaler bearer token in the scaler Secret key `token`;
- a CA certificate in the CA Secret key `ca.crt`, signed for the HTTPS control
  service name used in `control.url`;
- a separate TLS client certificate and key in the scaler TLS Secret keys
  `tls.crt` and `tls.key`. KEDA 2.21 requires this pair when its metrics-api
  scaler combines bearer and TLS authentication. Do not copy the control
  server private key into a tenant namespace.

`secrets.caName` is required. The worker always mounts that Secret at
`/etc/hrn-control-ca` and sets `SSL_CERT_FILE` to its `ca.crt` file.

Use distinct tokens for operator, tenant, project worker, and scaler. Never
place token values in Helm values or source control. The control service
requires both `HRN_TLS_CERT_FILE` and `HRN_TLS_KEY_FILE` unless `HRN_LISTEN`
uses a loopback IP literal (`127.0.0.0/8` or `::1`). Host names (including
`localhost`) and wildcard addresses require TLS. The control chart binds
`0.0.0.0:8443` with both TLS files set.
The included control chart installs it in `harness-system` with Pod label
`app.kubernetes.io/name=hrn-control`, a Service reachable on the configured
TLS port, and network ingress from tenant workers and KEDA. The project chart
matches that label and port in its egress policy. The chart cannot itself
attest tenant/project ownership or verify that the Secret copied into a
namespace is scoped to the named project.

Build the development image from the repository root:

    docker build -f server/Dockerfile -t hrn-durable-fixture:local .

The image downloads server modules before copying the remaining server sources,
so server source changes can reuse the dependency download layer.

The chart requires an immutable image digest. In a disposable single-node
k3d cluster, the local image can be imported and aliased by digest in
containerd. A shared or hosted cluster requires a registry digest. Publish
to a private registry only after separate
authorization, then use its immutable `repository@sha256:...` reference in a
local values file. No image was published as part of this work.

After installing KEDA 2.21 and creating the control/TLS/Secret prerequisites,
install the tenant chart once per tenant and the project chart once per
project. For a server-issued tenant ID such as `t_<32 lowercase hex digits>`,
the namespace is `hrn-t-<same hex digits>`. The project chart refuses any
other namespace. Example render validation:

    python -m pip install PyYAML==6.0.3
    python server/deploy/verify_charts.py
    python -m unittest discover -s server/deploy -p test_verify_charts.py

`HELM_BIN` can point to a Helm 4.3.0 binary. The verifier checks the module
download layer and unconditional CA wiring, runs Helm lint,
renders all three charts, checks scaling, storage, isolation, credential references,
and rejects unsafe values: unpinned images, missing required Secret names,
malformed tenant/project IDs, and control URLs with HTTP or a path. It also
rejects repeated document kinds, checks the control container's image and
security settings (including inherited seccomp), forbids `capabilities.add` on
the control, worker and fake-model containers, verifies DNS/control/PostgreSQL
egress selectors and ports and the control ingress sources (project workers,
KEDA, hrn-admin) and port 8443, forbids `envFrom` on both worker containers, and
requires exactly four scaler Secret references. It parses every YAML document
in `fixtures/postgres.yaml`. The regression tests exercise broken rendered
documents and simulate Helm accepting forbidden values, without editing charts.
These checks do not query a Kubernetes API or prove KEDA
activation. A live disposable-cluster test must observe 0 -> 1 -> 0 replicas,
persisted workspace bytes, admission during scale-down, scaler outages, and
worker/pod termination before this gate can be called complete. Never use an
existing shared cluster context for that test.

StatefulSet PVC retention preserves a claim on the same cluster; it is not a
backup. A single-node local StorageClass cannot provide node-failure
continuity. Until backup and clean restore are tested, the cluster deployment
must remain disposable. Tenant namespace deletion also deletes its PVCs, even
when StatefulSet retention is `Retain`, so namespace deletion is not part of a
routine scale-down.

## Control chart prerequisites

Create the `harness-system` namespace with restricted Pod Security labels.
Provide an external PostgreSQL 16 service in that namespace with backing Pod
label `app.kubernetes.io/name=hrn-postgres` (or override the chart selectors),
then apply the explicit schema migration before starting control:

    HRN_DATABASE_URL='<private DSN>' hrn-server migrate

Create the database-DSN Secret, the auth Secret with separate
`operator-token` and `scaler-token` keys, and the TLS Secret with `tls.crt` and
`tls.key`. The certificate must cover
`hrn-control.harness-system.svc.cluster.local`, and the corresponding CA must
be copied to each tenant's CA Secret. The control chart runs one always-on
replica, serves TLS on port 8443, probes database-backed health, and restricts
network ingress to tenant worker Pods, the configured KEDA namespace, and
an administrative Pod labeled `app.kubernetes.io/name=hrn-admin` in
`harness-system`. It allows egress only to DNS and the configured PostgreSQL
Pod label and port. The administrative Pod must still present the operator
token for registration and recovery. No untrusted tenant Pod receives a
database credential or the operator token.

The tenant chart owns its Namespace, so install it as one Helm release per
tenant without `--create-namespace` for that target. Install the control chart
in a separately precreated `harness-system` namespace and the project chart in
the tenant namespace. Values contain only Secret names, image digests, tenant
and project IDs, storage class, and endpoint configuration. Do not use Helm
upgrade to replace an active worker without an explicit drain and checkpoint
compatibility check.


## Disposable cluster evidence

A local k3d 5.9.0 / K3s 1.35.8+k3s1 cluster with KEDA 2.21.0 and Helm
4.3.0 exercised the charts on 2026-09-27. The one-node development cluster
observed 0-to-1-to-0 scaling, a completed checkpointed job, and PVC bytes
surviving scale-down. The pinned [PostgreSQL fixture](fixtures/postgres.yaml)
database Pod used `emptyDir` and was deliberately disposable. See
[KEDA evidence](fixtures/keda-evidence.md) for versions, observations and remaining gates.
