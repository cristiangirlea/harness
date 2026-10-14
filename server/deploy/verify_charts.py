"""Validate the rendered development charts. Requires Helm 4.3.0 and PyYAML 6.0.3."""
import os
import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent / "charts"
HELM = os.environ.get("HELM_BIN", "helm")
TENANT = "t_" + "0" * 32
OTHER_TENANT = "t_" + "1" * 32
PROJECT = "p_" + "2" * 32
NAMESPACE = "hrn-" + TENANT.replace("_", "-")
IMAGE = "example.invalid/hrn@sha256:" + "a" * 64
CONTROL_SETS = {
    "image.ref": IMAGE,
    "secrets.databaseName": "database-dsn",
    "secrets.authName": "control-auth",
    "secrets.tlsName": "control-tls",
}
SETS = {
    "tenant.id": TENANT,
    "project.id": PROJECT,
    "image.ref": IMAGE,
    "secrets.workerName": "worker-token",
    "secrets.scalerName": "scaler-token",
    "secrets.caName": "control-ca",
    "secrets.scalerTLSName": "scaler-tls",
    "storage.className": "standard",
}


def helm(*args, ok=True):
    result = subprocess.run([HELM, *args], text=True, capture_output=True)
    if (result.returncode == 0) != ok:
        raise AssertionError(f"Helm exit {result.returncode}: {result.stderr}")
    return result.stdout


def set_args(values):
    return [part for key, value in values.items() for part in ("--set", f"{key}={value}")]


def render(chart, namespace, values):
    output = helm("template", "fixture", str(ROOT / chart), "--namespace",
                  namespace, *set_args(values))
    return {doc["kind"]: doc for doc in yaml.safe_load_all(output) if doc}


def main():
    helm("lint", str(ROOT / "tenant-namespace"), *set_args({"tenant.id": TENANT}))
    helm("lint", str(ROOT / "project-worker"), "--namespace",
         NAMESPACE, *set_args(SETS))
    helm("lint", str(ROOT / "control"), "--namespace", "harness-system",
         *set_args(CONTROL_SETS))
    tenant = render("tenant-namespace", "default", {"tenant.id": TENANT})
    other = render("tenant-namespace", "default", {"tenant.id": OTHER_TENANT})
    assert set(tenant) == {"Namespace", "ResourceQuota", "NetworkPolicy"}
    assert tenant["Namespace"]["metadata"]["name"] == NAMESPACE
    assert tenant["Namespace"]["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] == "restricted"
    assert tenant["ResourceQuota"]["metadata"]["namespace"] == NAMESPACE
    assert other["Namespace"]["metadata"]["name"] != NAMESPACE
    assert tenant["NetworkPolicy"]["spec"]["policyTypes"] == ["Ingress", "Egress"]
    assert "ingress" not in tenant["NetworkPolicy"]["spec"]
    assert "egress" not in tenant["NetworkPolicy"]["spec"]

    control = render("control", "harness-system", CONTROL_SETS)
    assert set(control) == {"Service", "Deployment", "NetworkPolicy"}
    deployment = control["Deployment"]["spec"]
    control_pod = deployment["template"]["spec"]
    assert deployment["replicas"] == 1
    assert deployment["strategy"]["type"] == "Recreate"
    assert control_pod["automountServiceAccountToken"] is False
    assert control_pod["securityContext"]["runAsNonRoot"] is True
    assert control["Service"]["spec"]["ports"][0]["port"] == 8443
    control_env = {item["name"]: item for item in control_pod["containers"][0]["env"]}
    for name in ("HRN_DATABASE_URL", "HRN_OPERATOR_TOKEN", "HRN_SCALER_TOKEN"):
        assert "valueFrom" in control_env[name]
    assert control_env["HRN_TLS_CERT_FILE"]["value"] == "/etc/hrn-tls/tls.crt"
    assert control_env["HRN_TLS_KEY_FILE"]["value"] == "/etc/hrn-tls/tls.key"
    assert control["NetworkPolicy"]["spec"]["policyTypes"] == ["Ingress", "Egress"]
    assert len(control["NetworkPolicy"]["spec"]["ingress"]) == 1
    assert len(control["NetworkPolicy"]["spec"]["egress"]) == 2
    docs = render("project-worker", NAMESPACE, SETS)
    assert set(docs) == {"StatefulSet", "Service", "ScaledObject",
                         "TriggerAuthentication", "NetworkPolicy"}
    stateful = docs["StatefulSet"]
    spec = stateful["spec"]
    pod = spec["template"]["spec"]
    assert stateful["metadata"]["namespace"] == NAMESPACE
    assert spec["replicas"] == 0
    assert spec["updateStrategy"]["type"] == "OnDelete"
    assert spec["persistentVolumeClaimRetentionPolicy"] == {
        "whenDeleted": "Retain", "whenScaled": "Retain"}
    assert spec["volumeClaimTemplates"][0]["metadata"]["name"] == "workspace"
    assert pod["terminationGracePeriodSeconds"] == 120
    assert pod["automountServiceAccountToken"] is False
    assert pod["securityContext"]["runAsNonRoot"] is True
    assert pod["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault"
    assert [c["name"] for c in pod["containers"]] == ["worker", "fake-model"]
    for container in pod["containers"]:
        security = container["securityContext"]
        assert security["allowPrivilegeEscalation"] is False
        assert security["readOnlyRootFilesystem"] is True
        assert security["capabilities"]["drop"] == ["ALL"]
        assert container["image"] == IMAGE
        assert container["resources"]["requests"]
        assert container["resources"]["limits"]
    fixture = pod["containers"][1]
    assert "volumeMounts" not in fixture
    assert [item["name"] for item in fixture["env"]] == ["HRN_FIXTURE_LISTEN"]
    env = {item["name"]: item for item in pod["containers"][0]["env"]}
    assert env["HRN_WORKER_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "worker-token"
    assert env["SSL_CERT_FILE"]["value"] == "/etc/hrn-control-ca/ca.crt"
    assert not any("DATABASE" in name or "SCALER" in name for name in env)
    assert pod["volumes"][1]["secret"]["secretName"] == "control-ca"
    assert pod["volumes"][1]["secret"]["items"][0]["key"] == "ca.crt"
    assert docs["Service"]["spec"]["clusterIP"] == "None"
    scaled = docs["ScaledObject"]["spec"]
    assert (scaled["minReplicaCount"], scaled["maxReplicaCount"]) == (0, 1)
    assert (scaled["pollingInterval"], scaled["cooldownPeriod"]) == (10, 300)
    assert scaled["scaleTargetRef"]["name"] == stateful["metadata"]["name"]
    trigger = scaled["triggers"][0]
    assert trigger["type"] == "metrics-api"
    assert trigger["metadata"]["authMode"] == "bearer,tls"
    assert trigger["metadata"]["valueLocation"] == "value"
    assert trigger["metadata"]["url"].endswith("/internal/scaling/projects/" + PROJECT)
    auth = docs["TriggerAuthentication"]["spec"]["secretTargetRef"]
    assert auth[0]["name"] == "scaler-token" and auth[0]["parameter"] == "token"
    assert auth[1]["name"] == "control-ca" and auth[1]["parameter"] == "ca"
    assert auth[1]["key"] == "ca.crt"
    assert auth[2]["parameter"] == "cert" and auth[2]["name"] == "scaler-tls"
    assert auth[2]["key"] == "tls.crt"
    assert auth[3]["parameter"] == "key" and auth[3]["name"] == "scaler-tls"
    assert auth[3]["key"] == "tls.key"
    net = docs["NetworkPolicy"]["spec"]
    assert "ingress" not in net
    assert len(net["egress"]) == 2

    bad = [
        ("default", SETS),
        (NAMESPACE, {**SETS, "image.ref": "hrn:latest"}),
        (NAMESPACE, {**SETS, "control.url": "http://hrn-control:8088"}),
        (NAMESPACE, {**SETS, "secrets.workerName": ""}),
    ]
    for namespace, values in bad:
        helm("template", "fixture", str(ROOT / "project-worker"),
             "--namespace", namespace, *set_args(values), ok=False)
    helm("template", "fixture", str(ROOT / "control"), "--namespace",
         "default", *set_args(CONTROL_SETS), ok=False)
    print("chart rendering, isolation, scaling and negative checks passed")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(exc, file=sys.stderr)
        raise
