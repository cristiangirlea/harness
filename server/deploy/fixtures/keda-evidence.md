# Disposable KEDA smoke evidence, 2026-09-27

This was run in a new `k3d` cluster named `hrn-fixture-927812` with a temporary
`KUBECONFIG`. It did not use the machine's saved Kubernetes context or deploy
to a remote cluster. Versions observed: k3d 5.9.0, K3s/Kubernetes
1.35.8+k3s1, KEDA chart/application 2.21.0, Helm 4.3.0, and PostgreSQL 16
Alpine. The image was built locally from `server/Dockerfile`, imported into
the single node, and aliased by its immutable local digest in containerd.
No registry push or provider API call occurred.

The test installed the control chart behind in-cluster HTTPS, with external
PostgreSQL in `harness-system` and explicit schema migration. An HTTPS health
request validated the temporary CA. The tenant chart created one restricted
namespace with quota and default-deny policy. The project chart created a
zero-replica StatefulSet, headless Service, authenticated metrics-api
ScaledObject, TriggerAuthentication, and retained workspace claim template.
KEDA reported the ScaledObject Ready. A bearer-authenticated metric returned
`{"value":0}` before admission.

A tenant-authenticated, reasoning-only API fixture job was submitted. The
control admitted it, KEDA changed the StatefulSet from 0 to 1, the workspace
PVC bound, and the project worker completed the job with three checkpoints.
After the configured 300-second cooldown, the StatefulSet returned to 0 while
the PVC stayed Bound. A separate restricted Pod wrote
`retained-after-scale-zero` to the PVC before scale-down; a second Pod mounted
the same PVC after scale-down and read the exact marker. A second job submitted
after zero triggered another scale-up and completed with three checkpoints.

Live API admission caught two chart mistakes missed by the first render-only
test: an absent `caKey` default, then missing client certificate/key for
KEDA's combined bearer+TLS authentication. Both were fixed and the verifier
now asserts the required keys. The first worker Pod restarted once; its prior error was not captured.
The fixture worker now waits for the local fake model health endpoint to
remove that possible startup race. The second Pod restarted
once after a transient control Service connection refusal before any claim.
The worker now has a bounded retry for pre-claim transport errors, covered by
a focused unit test. The retry change has not yet been rerun in Kubernetes.

This smoke test does not prove isolation against hostile code, multi-node
failover, automated recovery after pod/node death, scaler-outage behavior,
resource exhaustion behavior, or backup/restore. The test PostgreSQL Pod used
ephemeral storage and the fake model had no external side effects.


A short control Deployment outage while the project was idle changed the
ScaledObject Ready condition to False. Restoring the Deployment changed it
back to True, with the StatefulSet still at zero and the PVC still bound.
This proves that this scaler path surfaces an unavailable metric in the
idle fixture; it does not prove safe behavior during an active job outage.
A later worker change adds bounded retry for transport errors before claim
and passed Windows/Linux tests. The rebuilt image has not been exercised in
this Kubernetes fixture.
