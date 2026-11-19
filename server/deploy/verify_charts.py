"""Validate development image layering and charts. Requires Helm 4.3.0 and PyYAML 6.0.3."""
import hashlib
import json
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
METRICS_SECRET = "control-metrics"
SCRAPE_SETS = {
    r"metrics.scrapeFrom.namespaceLabels.app\.kubernetes\.io/part-of": "observability",
    r"metrics.scrapeFrom.podLabels.app\.kubernetes\.io/name": "collector",
}
SCRAPE_FROM = {
    "namespaceLabels": {"app.kubernetes.io/part-of": "observability"},
    "podLabels": {"app.kubernetes.io/name": "collector"},
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


def helm(*args, ok=True, error=None):
    result = subprocess.run([HELM, *args], text=True, capture_output=True)
    if (result.returncode == 0) != ok:
        raise AssertionError(f"Helm exit {result.returncode}: {result.stderr}")
    if error is not None:
        assert error in result.stderr, f"Expected Helm error {error!r}: {result.stderr}"
    return result.stdout


def set_args(values):
    return [part for key, value in values.items() for part in ("--set", f"{key}={value}")]


def render(chart, namespace, values):
    output = helm("template", "fixture", str(ROOT / chart), "--namespace",
                  namespace, *set_args(values))
    docs = [doc for doc in yaml.safe_load_all(output) if doc]
    kinds = {doc["kind"]: doc for doc in docs}
    assert len(docs) == len(kinds), f"{chart}: repeated document kind"
    return kinds


def has_runtime_class(value):
    if isinstance(value, dict):
        return "runtimeClassName" in value or any(has_runtime_class(item) for item in value.values())
    if isinstance(value, list):
        return any(has_runtime_class(item) for item in value)
    return False


def check_control_metrics(control):
    pod = control["Deployment"]["spec"]["template"]["spec"]
    env = pod["containers"][0]["env"]
    assert not {"HRN_METRICS_TOKEN", "HRN_METRICS_TOKEN_FILE"}.intersection(
        item["name"] for item in env), "default control must omit metrics env vars"
    assert all(item["name"] != "metrics-token" for item in pod["volumes"]), (
        "default control must omit the metrics volume")
    # Pin all default document fields to the pre-metrics render.
    assert hashlib.sha256(json.dumps(control, sort_keys=True).encode()).hexdigest() == (
        "3ff95692c0b4bac4d621c4ba6799728716eadbc6359eaaa3b3da08b47ca4d8bf"
    ), "default control documents must match the pre-metrics render"

    configured = render("control", "harness-system", {
        **CONTROL_SETS, "secrets.metricsName": METRICS_SECRET,
    })
    pod = configured["Deployment"]["spec"]["template"]["spec"]
    container = pod["containers"][0]
    assert container["name"] == "control", "metrics token must be on the control container"
    assert all(item["name"] != "HRN_METRICS_TOKEN" for item in container["env"]), (
        "configured control must omit the plain metrics env var")
    assert pod["volumes"].pop() == {"name": "metrics-token", "secret": {
        "secretName": METRICS_SECRET, "items": [{"key": "token", "path": "token"}],
    }}, "metrics volume must project the named Secret's token key"
    mount = container["volumeMounts"].pop()
    assert mount == {"name": "metrics-token", "mountPath": "/etc/hrn-metrics", "readOnly": True}, (
        "metrics volume must have a read-only mount")
    assert container["env"].pop() == {
        "name": "HRN_METRICS_TOKEN_FILE", "value": mount["mountPath"] + "/token",
    }, "metrics env var must point to the mounted token file"
    assert configured == control, "metrics Secret must only add the volume, mount and env var"

    configured = render("control", "harness-system", {**CONTROL_SETS, **SCRAPE_SETS})
    ingress = configured["NetworkPolicy"]["spec"]["ingress"]
    assert len(ingress) == len(control["NetworkPolicy"]["spec"]["ingress"]) + 1, (
        "scrape source must add exactly one ingress rule")
    assert ingress.pop() == {
        "from": [{"namespaceSelector": {"matchLabels": SCRAPE_FROM["namespaceLabels"]},
                  "podSelector": {"matchLabels": SCRAPE_FROM["podLabels"]}}],
        "ports": [{"protocol": "TCP", "port": control["Service"]["spec"]["ports"][0]["port"]}],
    }, "scrape ingress must intersect namespace and pod selectors on the TLS port only"
    assert configured == control, "scrape source must only add the ingress rule"


def check_invalid_control_metrics():
    cases = [
        *(("secrets.metricsName", name, "secrets.metricsName must be a DNS subdomain of at most 253 characters")
          for name in ("Metrics", "metrics-", "a" * 254)),
        ("secrets.metricsName", "0", "secrets.metricsName must be a string"),
        ("secrets.metricsName", CONTROL_SETS["secrets.authName"],
         "secrets.metricsName must name a Secret of its own"),
        *((f"metrics.scrapeFrom.{selector}.role", "collector",
           "metrics.scrapeFrom needs both namespaceLabels and podLabels")
          for selector in SCRAPE_FROM),
        ("metrics", "false", "metrics must be a map"),
        ("metrics.scrapeFrom", "false", "metrics.scrapeFrom must be a map"),
        *((f"metrics.scrapeFrom.{selector}", "false",
           f"metrics.scrapeFrom.{selector} must be a map") for selector in SCRAPE_FROM),
        *((f"metrics.scrapeFrom.{selector}.role", "true",
           f"metrics.scrapeFrom.{selector} must have non-empty keys and string values")
          for selector in SCRAPE_FROM),
    ]
    for key, value, error in cases:
        helm("template", "fixture", str(ROOT / "control"), "--namespace",
             "harness-system", *set_args({**CONTROL_SETS, key: value}), ok=False, error=error)
    for selector in SCRAPE_FROM:
        helm("template", "fixture", str(ROOT / "control"), "--namespace",
             "harness-system", *set_args(CONTROL_SETS), "--set-json",
             f'metrics.scrapeFrom.{selector}={{"":"collector"}}', ok=False,
             error=f"metrics.scrapeFrom.{selector} must have non-empty keys and string values")


def main():
    # Consume every document so syntax errors after a separator also fail.
    list(yaml.safe_load_all((ROOT.parent / "fixtures/postgres.yaml").read_text()))
    worker_template = (ROOT / "project-worker/templates/resources.yaml").read_text()
    assert "if .Values.secrets.caName" not in worker_template, "required CA must be unconditional"
    dockerfile = (ROOT.parent.parent / "Dockerfile").read_text().splitlines()
    assert dockerfile.index("COPY server/go.mod server/go.sum ./server/") < dockerfile.index(
        "RUN cd server && go mod download") < dockerfile.index("COPY server/ ./server/"), (
        "download server modules before copying server sources")
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
    assert control_pod["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault"
    control_container = control_pod["containers"][0]
    security = control_container["securityContext"]
    assert security["readOnlyRootFilesystem"] is True
    assert security["allowPrivilegeEscalation"] is False
    assert security["capabilities"]["drop"] == ["ALL"]
    assert "add" not in security["capabilities"]
    assert security.get("seccompProfile", control_pod["securityContext"][
        "seccompProfile"])["type"] == "RuntimeDefault"
    assert control_container["image"] == IMAGE
    assert control["Service"]["spec"]["ports"][0]["port"] == 8443
    control_env = {item["name"]: item for item in control_pod["containers"][0]["env"]}
    for name in ("HRN_DATABASE_URL", "HRN_OPERATOR_TOKEN", "HRN_SCALER_TOKEN"):
        assert "valueFrom" in control_env[name]
    assert control_env["HRN_TLS_CERT_FILE"]["value"] == "/etc/hrn-tls/tls.crt"
    assert control_env["HRN_TLS_KEY_FILE"]["value"] == "/etc/hrn-tls/tls.key"
    assert control["NetworkPolicy"]["spec"]["policyTypes"] == ["Ingress", "Egress"]
    assert control["NetworkPolicy"]["spec"]["ingress"] == [{
        "from": [
            {"namespaceSelector": {"matchLabels": {"app.kubernetes.io/part-of": "harness"}},
             "podSelector": {"matchLabels": {"app.kubernetes.io/name": "hrn-project-worker"}}},
            {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "keda"}}},
            {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "hrn-admin"}}},
        ],
        "ports": [{"protocol": "TCP", "port": 8443}],
    }]
    assert len(control["NetworkPolicy"]["spec"]["egress"]) == 2
    dns_egress = {
        "to": [{
            "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
            "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}},
        }],
        "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}],
    }
    assert control["NetworkPolicy"]["spec"]["egress"] == [dns_egress, {
        "to": [{
            "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "harness-system"}},
            "podSelector": {"matchLabels": {"app.kubernetes.io/name": "hrn-postgres"}},
        }],
        "ports": [{"protocol": "TCP", "port": 5432}],
    }]
    check_control_metrics(control)
    docs = render("project-worker", NAMESPACE, SETS)
    assert not has_runtime_class(docs), "default worker render must omit runtimeClassName"
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
        assert "envFrom" not in container
        security = container["securityContext"]
        assert security["allowPrivilegeEscalation"] is False
        assert security["readOnlyRootFilesystem"] is True
        assert security["capabilities"]["drop"] == ["ALL"]
        assert "add" not in security["capabilities"]
        assert container["image"] == IMAGE
        assert container["resources"]["requests"]
        assert container["resources"]["limits"]
    fixture = pod["containers"][1]
    assert "volumeMounts" not in fixture
    assert [item["name"] for item in fixture["env"]] == ["HRN_FIXTURE_LISTEN"]
    env = {item["name"]: item for item in pod["containers"][0]["env"]}
    assert env["HRN_WORKER_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "worker-token"
    assert env["SSL_CERT_FILE"]["value"] == "/etc/hrn-control-ca/ca.crt"
    mounts = {item["name"]: item for item in pod["containers"][0]["volumeMounts"]}
    assert mounts["control-ca"]["mountPath"] == "/etc/hrn-control-ca"
    assert mounts["control-ca"]["readOnly"] is True
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
    assert len(auth) == 4
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
    assert net["egress"] == [dns_egress, {
        "to": [{
            "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "harness-system"}},
            "podSelector": {"matchLabels": {"app.kubernetes.io/name": "hrn-control"}},
        }],
        "ports": [{"protocol": "TCP", "port": 8443}],
    }]

    for runtime_class, requirement in (("sandboxed.worker-v1", "false"), ("a" * 253, "true")):
        configured = render("project-worker", NAMESPACE, {
            **SETS, "worker.runtimeClassName": runtime_class,
            "isolation.requireRuntimeClass": requirement,
        })
        configured_pod = configured["StatefulSet"]["spec"]["template"]["spec"]
        assert configured_pod.pop("runtimeClassName", None) == runtime_class, (
            "configured runtimeClassName must appear on the worker pod spec")
        assert configured == docs, "runtimeClassName must be the only change to the worker render"

    bad = [
        ("default", SETS),
        (NAMESPACE, {**SETS, "image.ref": "hrn:latest"}),
        (NAMESPACE, {**SETS, "control.url": "http://hrn-control:8088"}),
        (NAMESPACE, {**SETS, "control.url": "https://host:8443/x"}),
        (NAMESPACE, {**SETS, "secrets.workerName": ""}),
        (NAMESPACE, {**SETS, "secrets.caName": ""}),
        (NAMESPACE, {**SETS, "project.id": "p_ABC"}),
        (NAMESPACE, {**SETS, "project.id": "p_" + "a" * 31}),
    ]
    for tenant_id in ("t_ABC", "t_" + "a" * 31):
        helm("template", "fixture", str(ROOT / "tenant-namespace"),
             *set_args({"tenant.id": tenant_id}), ok=False)
        # Match the derived namespace so its guard cannot mask a bad ID.
        bad.append(("hrn-" + tenant_id.replace("_", "-"), {**SETS, "tenant.id": tenant_id}))
    for namespace, values in bad:
        helm("template", "fixture", str(ROOT / "project-worker"),
             "--namespace", namespace, *set_args(values), ok=False)
    bad_runtime_classes = [
        *[({"worker.runtimeClassName": name},
           "worker.runtimeClassName must be a DNS subdomain of at most 253 characters")
          for name in ("Sandbox", "-sandbox", "sandbox-", "a" * 254)],
        ({"worker.runtimeClassName": "true"}, "worker.runtimeClassName must be a string"),
        ({"isolation.requireRuntimeClass": "true"},
         "isolation.requireRuntimeClass is set: worker.runtimeClassName is required"),
        ({"isolation.requireRuntimeClass": "yes"},
         "isolation.requireRuntimeClass must be a boolean"),
    ]
    for values, error in bad_runtime_classes:
        helm("template", "fixture", str(ROOT / "project-worker"), "--namespace",
             NAMESPACE, *set_args({**SETS, **values}), ok=False, error=error)
    helm("template", "fixture", str(ROOT / "control"), "--namespace",
         "default", *set_args(CONTROL_SETS), ok=False)
    for key, value in (("image.ref", "hrn:latest"), ("secrets.databaseName", ""),
                       ("secrets.authName", ""), ("secrets.tlsName", "")):
        helm("template", "fixture", str(ROOT / "control"), "--namespace",
             "harness-system", *set_args({**CONTROL_SETS, key: value}), ok=False)
    check_invalid_control_metrics()
    print("chart rendering, isolation, scaling and negative checks passed")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(exc, file=sys.stderr)
        raise
