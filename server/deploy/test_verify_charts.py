"""Regression checks for the verifier; Helm renders are cached, never installed."""
import copy
import io
import subprocess
import unittest
from contextlib import redirect_stdout
from functools import lru_cache
from pathlib import Path
from unittest.mock import patch

import verify_charts as verify

RUN = subprocess.run


@lru_cache(maxsize=None)
def helm_result(args):
    return RUN(args, text=True, capture_output=True)


class VerifierTests(unittest.TestCase):
    def setUp(self):
        self.render = verify.render
        self.output = redirect_stdout(io.StringIO())
        self.output.__enter__()
        self.addCleanup(self.output.__exit__, None, None, None)
        # Keep the real exit-status check, but reuse read-only Helm results.
        self.process = patch.object(
            verify.subprocess, "run",
            side_effect=lambda args, **kwargs: copy.deepcopy(helm_result(tuple(args))),
        )
        self.process.start()
        self.addCleanup(self.process.stop)

    def check_mutation(self, chart, mutate, *, overrides=None, error=None):
        def render(name, namespace, values):
            docs = self.render(name, namespace, values)
            if name == chart and (overrides is None or values == {**verify.CONTROL_SETS, **overrides}):
                mutate(docs)
            return docs

        with patch.object(verify, "render", side_effect=render):
            with self.assertRaisesRegex(AssertionError, error or ""):
                verify.main()

    def control_pod(self, docs):
        return docs["Deployment"]["spec"]["template"]["spec"]

    def test_valid_charts_and_fixture_pass(self):
        verify.main()

    def test_invalid_values_must_be_refused(self):
        cases = [
            ("control", "image.ref", "hrn:latest"),
            *[("control", "secrets." + name, "")
              for name in ("databaseName", "authName", "tlsName")],
            *[(chart, "tenant.id", value)
              for chart in ("tenant-namespace", "project-worker")
              for value in ("t_ABC", "t_" + "a" * 31)],
            *[("project-worker", "project.id", value)
              for value in ("p_ABC", "p_" + "a" * 31)],
            ("project-worker", "control.url", "https://host:8443/x"),
        ]
        for chart, key, value in cases:
            with self.subTest(chart=chart, key=key, value=value):
                def run(args, **kwargs):
                    if (args[1] == "template" and Path(args[3]).name == chart
                            and f"{key}={value}" in args):
                        return subprocess.CompletedProcess(args, 0, "", "")
                    return copy.deepcopy(helm_result(tuple(args)))

                with patch.object(verify.subprocess, "run", side_effect=run):
                    with self.assertRaises(AssertionError):
                        verify.main()

    def test_invalid_runtime_class_values_must_be_refused_with_expected_error(self):
        cases = [
            *(("worker.runtimeClassName", name) for name in ("Sandbox", "-sandbox", "sandbox-", "a" * 254, "true")),
            ("isolation.requireRuntimeClass", "true"),
            ("isolation.requireRuntimeClass", "yes"),
        ]
        for key, value in cases:
            options = ["--namespace", verify.NAMESPACE, *verify.set_args({**verify.SETS, key: value})]
            for exit_code in (0, 1):
                with self.subTest(key=key, value=value, exit_code=exit_code):
                    def run(args, **kwargs):
                        if (args[1] == "template" and Path(args[3]).name == "project-worker"
                                and args[4:] == options):
                            return subprocess.CompletedProcess(args, exit_code, "", "unrelated error")
                        return copy.deepcopy(helm_result(tuple(args)))

                    with patch.object(verify.subprocess, "run", side_effect=run):
                        with self.assertRaisesRegex(AssertionError, "unrelated error"):
                            verify.main()

    def test_default_worker_render_must_omit_runtime_class_everywhere(self):
        def mutate(docs):
            docs["Service"]["metadata"]["annotations"] = {"runtimeClassName": "unexpected"}

        self.check_mutation("project-worker", mutate)

    def test_default_control_must_match_pre_metrics_render(self):
        def mutate(docs):
            docs["Deployment"]["spec"]["template"]["spec"]["terminationGracePeriodSeconds"] = 31

        self.check_mutation("control", mutate, overrides={}, error="match the pre-metrics render")

    def test_default_control_must_omit_metrics_env_vars_and_volume(self):
        for name in ("HRN_METRICS_TOKEN", "HRN_METRICS_TOKEN_FILE", "metrics-token"):
            with self.subTest(name=name):
                def mutate(docs):
                    pod = self.control_pod(docs)
                    if name == "metrics-token":
                        pod["volumes"].append({"name": name})
                    else:
                        pod["containers"][0]["env"].append({"name": name})

                error = "omit the metrics volume" if name == "metrics-token" else "omit metrics env vars"
                self.check_mutation("control", mutate, overrides={}, error=error)

    def test_metrics_secret_must_only_add_volume_mount_and_file_env(self):
        def container(docs):
            return self.control_pod(docs)["containers"][0]

        cases = [
            ("on the control container", lambda docs: container(docs).update(name="other")),
            ("omit the plain metrics env var", lambda docs: container(docs)["env"].append(
                {"name": "HRN_METRICS_TOKEN", "value": "unexpected"})),
            ("project the named Secret", lambda docs: self.control_pod(docs)["volumes"].pop()),
            ("project the named Secret", lambda docs: self.control_pod(docs)["volumes"][-1][
                "secret"]["items"][0].update(key="other")),
            ("read-only mount", lambda docs: container(docs)["volumeMounts"].pop()),
            ("read-only mount", lambda docs: container(docs)["volumeMounts"][-1].update(readOnly=False)),
            ("mounted token file", lambda docs: container(docs)["env"].pop()),
            ("mounted token file", lambda docs: container(docs)["env"][-1].update(value="/other/token")),
            ("only add the volume", lambda docs: container(docs)["env"][0].update(value="unexpected")),
            ("only add the volume", lambda docs: docs["Service"]["metadata"].update(name="other")),
        ]
        for error, mutate in cases:
            with self.subTest(check=error):
                self.check_mutation("control", mutate,
                                    overrides={"secrets.metricsName": verify.METRICS_SECRET}, error=error)

    def test_scrape_source_must_only_add_one_intersected_tls_rule(self):
        def ingress(docs):
            return docs["NetworkPolicy"]["spec"]["ingress"]

        def split_selectors(docs):
            peers = ingress(docs)[-1]["from"]
            peers.append({"podSelector": peers[0].pop("podSelector")})

        cases = [
            ("exactly one ingress rule", lambda docs: ingress(docs).pop()),
            ("exactly one ingress rule", lambda docs: ingress(docs).append({})),
            ("intersect namespace and pod selectors", split_selectors),
            ("intersect namespace and pod selectors", lambda docs: ingress(docs)[-1]["from"][0][
                "podSelector"]["matchLabels"].clear()),
            ("intersect namespace and pod selectors", lambda docs: ingress(docs)[-1]["ports"].append(
                {"protocol": "TCP", "port": 80})),
            ("intersect namespace and pod selectors", lambda docs: ingress(docs)[-1]["ports"][0].update(port=80)),
            ("only add the ingress rule", lambda docs: ingress(docs)[0]["from"].pop()),
            ("only add the ingress rule", lambda docs: docs["Service"]["metadata"].update(name="other")),
        ]
        for error, mutate in cases:
            with self.subTest(check=error):
                self.check_mutation("control", mutate, overrides=verify.SCRAPE_SETS, error=error)

    def test_invalid_metrics_values_must_be_refused_with_expected_error(self):
        cases = [
            *[["--set", f"secrets.metricsName={name}"]
              for name in ("Metrics", "metrics-", "a" * 254, "0", "control-auth")],
            ["--set", "metrics=false"],
            ["--set", "metrics.scrapeFrom=false"],
            *[[flag, f"metrics.scrapeFrom.{selector}{assignment}"]
              for selector in ("namespaceLabels", "podLabels")
              for flag, assignment in (("--set", ".role=collector"), ("--set", "=false"),
                                       ("--set", ".role=true"), ("--set-json", '={"":"collector"}'))],
        ]
        for option in cases:
            for exit_code in (0, 1):
                with self.subTest(option=option, exit_code=exit_code):
                    def run(args, **kwargs):
                        if (args[1] == "template" and Path(args[3]).name == "control"
                                and args[-2:] == option):
                            return subprocess.CompletedProcess(args, exit_code, "", "unrelated error")
                        return copy.deepcopy(helm_result(tuple(args)))

                    with patch.object(verify.subprocess, "run", side_effect=run):
                        with self.assertRaisesRegex(AssertionError, "unrelated error"):
                            verify.main()

    def test_configured_runtime_class_must_only_change_worker_pod_spec(self):
        def pod(docs):
            return docs["StatefulSet"]["spec"]["template"]["spec"]

        cases = {
            "missing name": lambda docs: pod(docs).pop("runtimeClassName"),
            "wrong name": lambda docs: pod(docs).update(runtimeClassName="unexpected"),
            "extra pod field": lambda docs: pod(docs)["containers"][0].update(runtimeClassName="unexpected"),
            "changed service": lambda docs: docs["Service"]["spec"].update(clusterIP="unexpected"),
        }
        for name, edit in cases.items():
            with self.subTest(case=name):
                def mutate(docs):
                    if "runtimeClassName" in pod(docs):
                        edit(docs)

                self.check_mutation("project-worker", mutate)

    def test_control_image_and_security_must_stay_restricted(self):
        cases = [
            (("image",), "hrn:latest"),
            (("securityContext", "readOnlyRootFilesystem"), False),
            (("securityContext", "allowPrivilegeEscalation"), True),
            (("securityContext", "capabilities", "drop"), []),
            (("securityContext", "seccompProfile"), {"type": "Unconfined"}),
        ]
        for path, value in cases:
            with self.subTest(path=path):
                def mutate(docs):
                    target = docs["Deployment"]["spec"]["template"]["spec"]["containers"][0]
                    for key in path[:-1]:
                        target = target[key]
                    target[path[-1]] = value
                self.check_mutation("control", mutate)

        def unconfined_pod(docs):
            docs["Deployment"]["spec"]["template"]["spec"]["securityContext"][
                "seccompProfile"]["type"] = "Unconfined"
        self.check_mutation("control", unconfined_pod)

    def test_egress_selectors_and_ports_must_match(self):
        for chart in ("control", "project-worker"):
            for index in (0, 1):
                for field in ("namespaceSelector", "podSelector", "ports", "protocol"):
                    with self.subTest(chart=chart, rule=index, field=field):
                        def mutate(docs):
                            rule = docs["NetworkPolicy"]["spec"]["egress"][index]
                            if field == "ports":
                                rule["ports"][0]["port"] = 9999
                            elif field == "protocol":
                                rule["ports"][0]["protocol"] = "SCTP"
                            else:
                                rule["to"][0][field] = {}
                        self.check_mutation(chart, mutate)

    def test_containers_must_not_add_capabilities(self):
        def control(docs):
            return docs["Deployment"]["spec"]["template"]["spec"]["containers"][0]

        def worker(index):
            return lambda docs: docs["StatefulSet"]["spec"]["template"]["spec"][
                "containers"][index]

        for chart, container in (("control", control), ("project-worker", worker(0)),
                                 ("project-worker", worker(1))):
            for added in ([], ["NET_ADMIN"]):
                with self.subTest(chart=chart, added=added):
                    def mutate(docs):
                        container(docs)["securityContext"]["capabilities"]["add"] = added
                    self.check_mutation(chart, mutate)

    def test_control_ingress_sources_and_port_must_match(self):
        def peer(index, field, value):
            def mutate(docs):
                docs["NetworkPolicy"]["spec"]["ingress"][0]["from"][index][field] = value
            return mutate

        def rule(edit):
            def mutate(docs):
                edit(docs["NetworkPolicy"]["spec"]["ingress"][0])
            return mutate

        cases = {
            "worker namespace": peer(0, "namespaceSelector", {}),
            "worker pods": peer(0, "podSelector", {}),
            "keda namespace": peer(1, "namespaceSelector", {"matchLabels": {}}),
            "keda pods narrowed": peer(1, "podSelector", {"matchLabels": {"x": "y"}}),
            "admin pods": peer(2, "podSelector", {"matchLabels": {}}),
            "extra source": rule(lambda r: r["from"].append({"ipBlock": {"cidr": "0.0.0.0/0"}})),
            "port": rule(lambda r: r["ports"][0].update(port=9999)),
            "protocol": rule(lambda r: r["ports"][0].update(protocol="UDP")),
            "all ports": rule(lambda r: r.pop("ports")),
        }
        for name, mutate in cases.items():
            with self.subTest(case=name):
                self.check_mutation("control", mutate)

    def test_repeated_kinds_must_not_overwrite_documents(self):
        with patch.object(verify, "helm", return_value="kind: Service\n---\nkind: Service\n"):
            with self.assertRaises(AssertionError):
                verify.render("control", "harness-system", verify.CONTROL_SETS)

    def test_worker_and_fake_model_must_not_have_env_from(self):
        for index in (0, 1):
            with self.subTest(container=index):
                def mutate(docs):
                    docs["StatefulSet"]["spec"]["template"]["spec"]["containers"][index][
                        "envFrom"] = [{"secretRef": {"name": "unexpected"}}]
                self.check_mutation("project-worker", mutate)

    def test_authentication_must_have_exactly_four_secret_references(self):
        for count in (3, 5):
            with self.subTest(count=count):
                def mutate(docs):
                    refs = docs["TriggerAuthentication"]["spec"]["secretTargetRef"]
                    refs[:] = (refs + [refs[0]])[:count]
                self.check_mutation("project-worker", mutate)

    def test_postgres_fixture_must_be_parsed(self):
        read_text = Path.read_text

        def broken_fixture(path, *args, **kwargs):
            if path.name == "postgres.yaml":
                return "kind: Service\n---\nspec: [\n"
            return read_text(path, *args, **kwargs)

        with patch.object(Path, "read_text", new=broken_fixture):
            with self.assertRaises(verify.yaml.YAMLError):
                verify.main()


if __name__ == "__main__":
    unittest.main()
