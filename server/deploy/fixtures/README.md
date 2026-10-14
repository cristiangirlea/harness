# Disposable single-node k3s fixture

This is a local test recipe, not a deployment. The verified run used k3d
5.9.0 (Windows amd64 checksum
`49d0b9c796f5b8ba6f58a1dd5469e8b7b34bf9b6ce9078633eedb2789680a034`),
K3s `v1.35.8-k3s1`, KEDA chart `2.21.0`, Helm `4.3.0`, and
PostgreSQL `16-alpine` pinned by the digest in [postgres.yaml](postgres.yaml).
KEDA documents Kubernetes 1.34–1.36 as tested with 2.21.

Set `KUBECONFIG` to a new file under a disposable temp directory before any
kubectl or Helm call. Do not use a shared Kubernetes context. On Windows,
with k3d.exe, helm.exe, kubectl, Go, Docker and OpenSSL on PATH:

    $env:KUBECONFIG = Join-Path $env:TEMP 'hrn-k3d-fixture\kubeconfig'
    k3d cluster create hrn-fixture --image rancher/k3s:v1.35.8-k3s1 --servers 1 --agents 0 --api-port 127.0.0.1:62579 --wait --timeout 4m --kubeconfig-update-default=false --kubeconfig-switch-context=false
    k3d kubeconfig get hrn-fixture | Set-Content -Encoding utf8 $env:KUBECONFIG
    kubectl get nodes
    helm repo add kedacore https://kedacore.github.io/charts
    helm repo update kedacore
    helm install keda kedacore/keda --version 2.21.0 --namespace keda --create-namespace --wait

Use temp `HELM_REPOSITORY_CONFIG` and `HELM_REPOSITORY_CACHE` paths if you
also want to isolate Helm's repository state. Create and label
`harness-system` with restricted Pod Security. Create the fixture-only
`hrn-postgres-auth` Secret with password `fixture-only`, then apply
[postgres.yaml](postgres.yaml) and wait for its Pod to become Ready. Port-forward the
PostgreSQL Service to a local port and run `hrn-server migrate` with a
fixture-only DSN. The fixture DB uses `emptyDir`; deleting its Pod loses data.

Build the image from the repository root with `docker build -f
server/Dockerfile -t hrn-durable-fixture:local .`. Import it with
`k3d image import hrn-durable-fixture:local -c hrn-fixture`. For a local-only
immutable image reference, inspect its digest with
`docker image inspect hrn-durable-fixture:local --format
'{{json .RepoDigests}}'`, then alias the imported tag to
`docker.io/library/hrn-durable-fixture@sha256:<digest>` using
`docker exec k3d-hrn-fixture-server-0 ctr -n k8s.io images tag`. This
node-local alias is a test convenience, not a registry publication.

Generate independent operator/scaler bearer tokens and a temporary TLS
certificate covering `hrn-control.harness-system.svc.cluster.local`.
Create the control DSN, auth and TLS Secrets in `harness-system`. Install
[the control chart](../charts/control) with immutable image digest and those Secret names.
Use a CA-validated HTTPS client through a local port-forward to register a
tenant and project. Retain the issued tenant and project worker tokens outside
source control. The registered project should use backend `api`, a full Git
commit hash, and a harmless fixture repository URL. Install
[the tenant chart](../charts/tenant-namespace) once for that tenant. In the resulting namespace,
create separate worker, scaler bearer, CA, and scaler TLS client identity
Secrets. Install [the project chart](../charts/project-worker) with the registered IDs, matching
Secret names, immutable digest, and `local-path` StorageClass.

Before submission, check the ScaledObject is Ready and the StatefulSet is
`0/0`. Submit a tenant-authenticated read-only API fixture job with a positive
`max_cost_usd` and an idempotency key. Observe the admitted demand,
StatefulSet `1/1`, job `succeeded`, and then `0/0` after the 300-second
cooldown. The bound workspace PVC must remain. The evidence file records
the separate write/read Pods used to confirm bytes survive scale-down.

After testing, stop port-forwards, delete only the named fixture cluster with
`k3d cluster delete hrn-fixture`, and remove the fixture temp directory
containing tokens and private keys. Never delete a user-saved cluster or
namespace to clean this test.
