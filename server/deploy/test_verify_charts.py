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

    def check_mutation(self, chart, mutate):
        def render(name, namespace, values):
            docs = self.render(name, namespace, values)
            if name == chart:
                mutate(docs)
            return docs

        with patch.object(verify, "render", side_effect=render):
            with self.assertRaises(AssertionError):
                verify.main()

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
