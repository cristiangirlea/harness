# Worker subprocess protocol, version 1

This contract specifies the future worker subprocess boundary; the worker still embeds the API
backend. Framing, output sequence numbers, events, errors and exit codes follow
[exec protocol v1](exec-protocol.md). The worker starts `hrn exec --protocol v1 --worker` for
one job attempt.

After `hello`, the worker sends `start` with the attempt ID and task and, on recovery, the last
accepted checkpoint. The child accepts only the `api` backend and an HTTP base URL whose host is
`localhost` or a loopback IP, with no user information, using the same test as `RunOnce`. It
offers no tools regardless of the start frame. Plain exec retains its existing rules. Credentials
enter the child only through its environment, filtered by `envpolicy`; checkpoints carry none.
The worker validates the endpoint before spawning as well.

For each durable boundary the child sends a numbered `checkpoint` frame and waits for an `ack`
or `nack` for that number. The worker revalidates the workspace, posts the checkpoint with the
job generation, and acknowledges only after the service accepts it. A failed validation, fenced
generation or failed post is a `nack`. A `nack`, timeout or closed input before the answer ends
the run with `ErrRecoveryRequired` and a result with `recovery_required: true`; the child never
continues or resends it. A resumed checkpoint with `Pending` also requires recovery.

The start frame places the optional checkpoint in a top-level `checkpoint` object. A checkpoint
output uses the usual output envelope, plus `checkpoint` containing the complete durable snapshot.
Its `sequence` identifies the save within the attempt; it is distinct from the output envelope's
`seq`. The response is `{"type":"ack","attempt_id":"...","sequence":1}` or the same object
with `type` set to `nack`. The attempt and sequence must match the outstanding checkpoint. Only
one checkpoint can be outstanding. The response timeout is 15 seconds from sending the frame;
an invalid or unmatched response does not acknowledge it.

A `cancel` frame requests cancellation. The worker reports `canceled` only when no operation is
in flight; otherwise the result requires recovery. If the worker kills the child after its grace
period, there is no result: the service reaches recovery through expiry of the job lease.

The worker validates the workspace before start and handles `finishWorkspaceRecovery`. Only the
worker calls the control service, always with the job generation. A fenced or canceled heartbeat
causes it to send `cancel`. These responsibilities remain in the worker when execution moves to
the child; the service's behavior does not change in this step.
