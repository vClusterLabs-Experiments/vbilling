"""Tests of the Helm chart: the configuration check hook, the values every
template reads, and where the chart places vBilling and its volumes."""

import json
from pathlib import Path
import tempfile
import unittest

import chartlib
from chartlib import HelmError, container, env, find, is_hook, notes, one, pod_spec, render

PRODUCTION_VALUES = chartlib.ROOT / "deploy/helm/production-values.yaml"
TOKENS = ["--set", "api.token=api-token", "--set", "api.ingestToken=ingest-token"]
# What an installation has to name in production-values.yaml: the chart
# refuses the file while a REPLACE_WITH_ placeholder is left in it.
PRODUCTION = ["--set", "region=eu-west-1", "--set", "clusterName=prod-1", "--set", "persistence.storageClass=replicated-ssd"]

# Values that make the chart render every file ConfigMap and every kind of
# Secret reference: files (custom metrics, records formats and pulls, the
# Stripe price card), Prometheus metric definitions, the Lago price card,
# Secrets the operator keeps (existingSecret) and credentials given inline.
FILES_AND_SECRETS = """
adapters: [stripe, lago, metronome, webhook]
tenantSource: platform
platform: {url: "https://platform.example.com", existingSecret: operator-platform, caSecret: platform-ca}
tenantAPI: {externalClustersSecret: external-clusters}
api: {token: api-token, ingestToken: ingest-token, tenantIngestTokens: {acme: token-acme}}
metering:
  customMetrics:
    - {code: inference_output_tokens, unit: tokens, groupKeys: [sku]}
prometheus:
  url: http://prometheus.monitoring.svc:9090
  headers: {X-Scope-OrgID: tenant-1}
  metrics:
    - {code: vllm_generation_tokens, unit: tokens, counter: "vllm:generation_tokens_total{{selector}}"}
records:
  formats:
    kong: {id: request.id, time: started_at, usage: {inference_output_tokens: ai.completion_tokens}}
  pull:
    - {name: kong-logs, format: kong, url: "https://logs.example.com/requests?from={{from}}&to={{to}}"}
  pullEnv:
    - {name: RECORDS_PULL_KONG_TOKEN, secret: kong-logs, key: token}
  pullLiteLLM: {url: "http://litellm.litellm.svc:4000", key: sk-inline}
stripe:
  existingSecret: operator-stripe
  priceCard: {currency: usd, plans: {dedicated: {vcluster_gpu_hours: {amount: "2.20"}}}}
lago:
  apiKey: lago-inline
  priceCard: {vcluster_gpu_hours: {amount: "0"}}
metronome: {apiToken: metronome-inline}
webhook: {url: "https://sink.example.com/usage", secret: webhook-inline}
persistence: {archive: {enabled: true}}
"""


def values_file(test, text):
    work = tempfile.TemporaryDirectory()
    test.addCleanup(work.cleanup)
    path = Path(work.name) / "values.yaml"
    path.write_text(text)
    return path


class ConfigCheckHookTests(unittest.TestCase):
    """A configuration vBilling refuses at start-up stopped
    all metering, because nothing checked it before the single pod was
    replaced. The chart now runs `vbilling check-config` with the new
    configuration in a Job before every install and upgrade."""

    def setUp(self):
        self.values = values_file(self, FILES_AND_SECRETS)
        self.docs = render(values=[self.values])
        self.job = one(self.docs, "Job", "vbilling-config-check")
        self.sts = one(self.docs, "StatefulSet", "vbilling")

    def test_the_hook_renders_by_default_and_runs_before_install_and_upgrade(self):
        job = one(render(*TOKENS), "Job", "vbilling-config-check")
        annotations = job["metadata"]["annotations"]
        self.assertEqual(annotations["helm.sh/hook"], "pre-install,pre-upgrade")
        # Deleted before it is created again and once it has succeeded: both
        # Helm and Argo CD (which runs it as a PreSync hook) honour these. A
        # failed Job is kept, with its log.
        self.assertEqual(annotations["helm.sh/hook-delete-policy"], "before-hook-creation,hook-succeeded")
        self.assertNotIn("hook-failed", annotations["helm.sh/hook-delete-policy"])
        self.assertEqual(annotations["helm.sh/hook-output-log-policy"], "hook-failed")
        self.assertEqual(container(job)["args"], ["check-config", "--allow-missing-secrets"])
        self.assertNotIn("command", container(job))  # the image's entrypoint is vbilling
        self.assertEqual(job["spec"]["backoffLimit"], 0)
        self.assertEqual(job["spec"]["activeDeadlineSeconds"], 180)
        self.assertEqual(pod_spec(job)["restartPolicy"], "Never")

    def test_an_operator_can_switch_the_hook_off(self):
        on = render(*TOKENS, values=[self.values])
        off = render(*TOKENS, "--set", "configCheck.enabled=false", values=[self.values])
        self.assertEqual(find(off, "Job"), [])
        self.assertEqual([d["metadata"]["name"] for d in off if is_hook(d)], [])
        self.assertEqual([d["metadata"]["name"] for d in off if "config-check" in d["metadata"]["name"]], [])
        # Nothing else changes: the release is the same with and without the check.
        self.assertEqual(off, [d for d in on if not is_hook(d)])
        self.assertEqual(off, render(*TOKENS, "--no-hooks", values=[self.values]))

    def test_the_check_reads_the_new_files_from_copies_created_before_the_job(self):
        job_weight = int(self.job["metadata"]["annotations"]["helm.sh/hook-weight"])
        for name in ("vbilling-files", "vbilling-prometheus-metrics", "vbilling-lago-price-card"):
            release, copy = one(self.docs, "ConfigMap", name), one(self.docs, "ConfigMap", name + "-config-check")
            self.assertFalse(is_hook(release), name)
            self.assertEqual(copy["data"], release["data"], name)
            annotations = copy["metadata"]["annotations"]
            self.assertEqual(annotations["helm.sh/hook"], "pre-install,pre-upgrade")
            self.assertEqual(annotations["helm.sh/hook-delete-policy"], "before-hook-creation,hook-succeeded")
            self.assertLess(int(annotations["helm.sh/hook-weight"]), job_weight, name)
        self.assertEqual(sorted(one(self.docs, "ConfigMap", "vbilling-files")["data"]),
                         ["custom-metrics.yaml", "records-formats.yaml", "records-pull.yaml", "stripe-price-card.yaml"])

        # The Job mounts each copy where the pod mounts the original.
        def mounted(obj):  # mount path -> volume (the data and archive volumes come from claim templates)
            volumes = {v["name"]: v for v in pod_spec(obj)["volumes"]}
            return {m["mountPath"]: volumes.get(m["name"], {}) for m in container(obj)["volumeMounts"]}

        pod, check = mounted(self.sts), mounted(self.job)
        for path in ("/etc/vbilling/files", "/etc/vbilling/prometheus", "/etc/vbilling/lago"):
            self.assertEqual(check[path]["configMap"]["name"], pod[path]["configMap"]["name"] + "-config-check", path)
        # Every hook object a mount needs is rendered.
        hooks = {d["metadata"]["name"] for d in self.docs if is_hook(d) and d["kind"] == "ConfigMap"}
        self.assertEqual({v["configMap"]["name"] for v in check.values() if "configMap" in v}, hooks)

    def test_the_check_gets_the_settings_of_the_new_pod(self):
        pod, check = env(self.sts), env(self.job)
        plain = {n: e for n, e in pod.items() if "valueFrom" not in e or "fieldRef" in e["valueFrom"]}
        self.assertGreater(len(plain), 40)
        for name, entry in plain.items():
            self.assertEqual(check.get(name), entry, name)
        self.assertEqual(check["PROMETHEUS_METRICS_FILE"]["value"], "/etc/vbilling/prometheus/metrics.yaml")
        self.assertEqual(check["STRIPE_PRICE_CARD"]["value"], "/etc/vbilling/files/stripe-price-card.yaml")
        self.assertEqual(check["LAGO_PRICE_CARD"]["value"], "/etc/vbilling/lago/price-card.yaml")
        # Nothing the pod is not given.
        self.assertEqual(set(check) - set(pod), set())

    def test_secrets_are_optional_and_the_releases_own_secret_is_not_read(self):
        pod, check = env(self.sts), env(self.job)

        def secret(entry):
            return (entry.get("valueFrom") or {}).get("secretKeyRef")

        # Secrets the operator keeps are read as the pod reads them, if they exist.
        for name in ("STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRET", "PLATFORM_ACCESS_KEY", "RECORDS_PULL_KONG_TOKEN"):
            ref = secret(check[name])
            self.assertEqual({k: v for k, v in ref.items() if k != "optional"},
                             {k: v for k, v in secret(pod[name]).items() if k != "optional"}, name)
        for name, entry in check.items():
            ref = secret(entry)
            if ref:
                self.assertIs(ref.get("optional"), True, f"{name}: the Job runs before a new installation's Secrets exist")
                self.assertNotEqual(ref["name"], "vbilling", f"{name}: the release's own Secret is not there on install "
                                    "and holds the previous release's values on upgrade")
        # What the chart's own Secret will hold is left out, not read stale.
        own = sorted(n for n, e in pod.items() if (secret(e) or {}).get("name") == "vbilling")
        self.assertEqual(own, ["API_TOKEN", "INGEST_TOKEN", "INGEST_TOKENS", "LAGO_API_KEY", "LITELLM_API_KEY",
                               "METRONOME_API_TOKEN", "PROMETHEUS_HEADERS", "WEBHOOK_SECRET"])
        self.assertEqual(sorted(set(pod) - set(check)), own)
        # Files from Secrets: optional too, and never the kubeconfigs.
        volumes = {v["name"]: v for v in pod_spec(self.job)["volumes"]}
        self.assertEqual(volumes["tenant-clusters"]["secret"],
                         {"secretName": "external-clusters", "optional": True, "items": [{"key": "clusters.yaml", "path": "clusters.yaml"}]})
        self.assertEqual(volumes["platform-ca"]["secret"],
                         {"secretName": "platform-ca", "optional": True, "items": [{"key": "ca.crt", "path": "ca.crt"}]})

    def test_the_check_needs_no_rbac_and_no_data(self):
        spec = pod_spec(self.job)
        self.assertIs(spec["automountServiceAccountToken"], False)
        # The release's ServiceAccount does not exist before the first install.
        self.assertNotIn("serviceAccountName", spec)
        self.assertNotIn("serviceAccount", spec)
        names = {v["name"] for v in spec["volumes"]}
        self.assertFalse(names & {"data", "archive"}, names)
        self.assertFalse([v for v in spec["volumes"] if "persistentVolumeClaim" in v])
        paths = {m["mountPath"] for m in container(self.job)["volumeMounts"]}
        self.assertFalse(paths & {"/var/lib/vbilling", "/var/lib/vbilling-archive"}, paths)
        # Its scratch directory, on a read-only root filesystem.
        self.assertIn({"name": "tmp", "emptyDir": {}}, spec["volumes"])
        self.assertIn({"name": "tmp", "mountPath": "/tmp"}, container(self.job)["volumeMounts"])
        # Nothing binds the default ServiceAccount to anything.
        for binding in find(self.docs, "RoleBinding") + find(self.docs, "ClusterRoleBinding"):
            self.assertNotIn("default", [s["name"] for s in binding["subjects"]])

    def test_the_check_runs_as_the_pod_does_where_the_pod_does(self):
        placement = values_file(self, """
imagePullSecrets: [{name: registry-credentials}]
nodeSelector: {node-role.example.com/infra: "true"}
tolerations: [{key: node-role.example.com/infra, operator: Exists, effect: NoSchedule}]
affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: nvidia.com/gpu.present, operator: NotIn, values: ["true"]}]}]}}}
topologySpreadConstraints: [{maxSkew: 1, topologyKey: topology.kubernetes.io/zone, whenUnsatisfiable: ScheduleAnyway}]
priorityClassName: billing-critical
resources: {requests: {cpu: 500m, memory: 512Mi}, limits: {cpu: "2", memory: 2Gi}}
""")
        docs = render(*TOKENS, values=[placement])
        job, sts = one(docs, "Job"), one(docs, "StatefulSet")
        for field in ("imagePullSecrets", "nodeSelector", "tolerations", "affinity", "topologySpreadConstraints",
                      "priorityClassName", "securityContext"):
            self.assertEqual(pod_spec(job)[field], pod_spec(sts)[field], field)
        for field in ("image", "imagePullPolicy", "securityContext", "resources"):
            self.assertEqual(container(job)[field], container(sts)[field], field)
        self.assertEqual(container(job)["securityContext"],
                         {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}})
        self.assertEqual(pod_spec(job)["securityContext"]["runAsNonRoot"], True)

        smaller = render(*TOKENS, "--set", "configCheck.resources.requests.cpu=20m", values=[placement])
        self.assertEqual(container(one(smaller, "Job"))["resources"], {"requests": {"cpu": "20m"}})
        self.assertEqual(container(one(smaller, "StatefulSet"))["resources"], container(sts)["resources"])
        deadline = render(*TOKENS, "--set", "configCheck.activeDeadlineSeconds=600")
        self.assertEqual(one(deadline, "Job")["spec"]["activeDeadlineSeconds"], 600)

    def test_the_check_pod_is_never_taken_for_vbilling(self):
        labels = self.job["spec"]["template"]["metadata"]["labels"]
        for selector in (one(self.docs, "Service")["spec"]["selector"], self.sts["spec"]["selector"]["matchLabels"]):
            self.assertTrue(selector)
            self.assertFalse(all(labels.get(k) == v for k, v in selector.items()),
                             f"the check's pod {labels} matches the selector {selector}")

    def test_the_job_name_fits_a_label_with_a_long_release_name(self):
        docs = render(*TOKENS, "--set", "fullnameOverride=" + "b" * 63)
        name = one(docs, "Job")["metadata"]["name"]
        self.assertLessEqual(len(name), 63)
        self.assertTrue(name.endswith("-config-check"))

    def test_a_price_card_configmap_of_the_operator_is_not_part_of_the_check(self):
        # It is not part of the release and may not exist when the Job runs.
        docs = render(*TOKENS, "--set", "adapters={lago}", "--set", "lago.apiKey=key", "--set", "lago.priceCardConfigMap=finance-prices")
        job, sts = one(docs, "Job"), one(docs, "StatefulSet")
        self.assertEqual(env(sts)["LAGO_PRICE_CARD"]["value"], "/etc/vbilling/lago/price-card.yaml")
        self.assertNotIn("LAGO_PRICE_CARD", env(job))
        self.assertNotIn("lago-price-card", [v["name"] for v in pod_spec(job)["volumes"]])
        self.assertEqual([d["metadata"]["name"] for d in docs if is_hook(d)], ["vbilling-config-check"])


class ValuesTests(unittest.TestCase):
    """statefulset.yaml read metronome.stripeLink, a value values.yaml
    did not have. Every value a template reads is now in values.yaml and in
    the schema, and this test keeps it so."""

    @classmethod
    def setUpClass(cls):
        cls.values = chartlib.yaml_file(chartlib.CHART / "values.yaml")
        cls.schema = json.loads((chartlib.CHART / "values.schema.json").read_text())
        cls.read = chartlib.template_value_paths()
        cls.documented = chartlib.values_paths(cls.values)

    def test_the_scan_of_the_templates_finds_what_they_read(self):
        # Direct, through optional parents, relative to with and range, and through a variable.
        for path in ("metronome.stripeLink", "billing.priceCheck.mode", "enforcement.gateway.litellm", "records.pullLiteLLM.url",
                     "records.pullEnv[].secret", "crossChecks[].tolerancePercent", "persistence.archive.existingClaim",
                     "lago.priceCardConfigMap", "configCheck.activeDeadlineSeconds", "nameOverride"):
            self.assertIn(path, self.read)
        self.assertGreater(len(self.read), 120)

    def test_every_value_a_template_reads_is_in_values_yaml(self):
        missing = sorted(p for p in self.read if "[]" not in p and p not in self.documented)
        self.assertEqual(missing, [], "templates read values that values.yaml does not have: add each with its default and a comment")

    def test_every_value_a_template_reads_is_in_the_schema(self):
        missing = sorted(p for p in self.read if chartlib.schema_node(self.schema, p) is None)
        self.assertEqual(missing, [], "templates read values that values.schema.json does not describe")

    def test_values_yaml_and_the_schema_describe_the_same_values(self):
        undescribed = sorted(p for p in self.documented if chartlib.schema_node(self.schema, p) is None)
        self.assertEqual(undescribed, [], "values.yaml has values that values.schema.json does not describe")
        unknown = sorted(p for p in chartlib.schema_paths(self.schema) if p not in self.documented)
        self.assertEqual(unknown, [], "values.schema.json describes values that values.yaml does not have")

    def test_every_value_in_values_yaml_is_read_by_a_template(self):
        def read(path):
            return any(r == path or r.startswith((path + ".", path + "[]")) or path.startswith(r + ".") for r in self.read)

        unused = sorted(p for p in self.documented if not read(p))
        self.assertEqual(unused, [], "values.yaml has values no template reads: they would be silently ignored")

    def test_the_schema_accepts_the_shipped_values_and_every_new_one(self):
        render(*TOKENS)
        render(*TOKENS, *PRODUCTION, values=[PRODUCTION_VALUES])
        render(*TOKENS, values=[values_file(self, """
nameOverride: metering
fullnameOverride: billing
imagePullSecrets: [{name: registry-credentials}]
metronome: {stripeLink: true}
configCheck: {enabled: true, activeDeadlineSeconds: 300, resources: {requests: {cpu: 20m, memory: 32Mi}}}
topologySpreadConstraints: [{maxSkew: 1, topologyKey: kubernetes.io/hostname, whenUnsatisfiable: DoNotSchedule}]
priorityClassName: billing-critical
persistence: {storageClass: replicated, archive: {enabled: true, storageClass: other-zone}}
metering: {customMetrics: "inference_input_tokens:tokens:model|region"}
""")])
        # Values saved by an older release, with a key this chart no longer
        # has, must not fail an upgrade.
        render(*TOKENS, "--set", "tenantAPI.enabled=true", "--set", "billing.retention=168h")

    def test_the_schema_refuses_a_value_of_the_wrong_type(self):
        for wrong in (["--set-string", "metronome.stripeLink=yes"],
                      ["--set-string", "configCheck.enabled=false"],  # a string "false" is true to a template
                      ["--set", "configCheck.activeDeadlineSeconds=soon"],
                      ["--set", "priorityClassName.name=critical"],
                      ["--set", "topologySpreadConstraints=zone"],
                      ["--set", "imagePullSecrets={registry-credentials}"],
                      ["--set", "persistence.archive.storageClass.name=slow"]):
            with self.assertRaises(HelmError, msg=wrong) as refused:
                render(*TOKENS, *wrong)
            self.assertIn("schema", str(refused.exception), wrong)

    def test_metronome_stripe_link_from_v0_3_0_still_means_provision(self):
        docs = render(*TOKENS, "--set", "adapters={metronome}", "--set", "metronome.apiToken=token", "--set", "metronome.stripeLink=true")
        self.assertEqual(env(one(docs, "StatefulSet"))["METRONOME_PROVISION"]["value"], "true")
        self.assertIs(self.values["metronome"]["stripeLink"], False)
        self.assertIs(self.values["metronome"]["provision"], False)

    def test_the_service_account_is_only_created_when_asked(self):
        # serviceAccount.create was read for the name only: with create false
        # the chart still rendered a ServiceAccount, named "default" or after
        # the operator's own, and the install failed on the existing object.
        own = render(*TOKENS, "--set", "serviceAccount.create=false", "--set", "serviceAccount.name=billing-reader")
        self.assertEqual(find(own, "ServiceAccount"), [])
        self.assertEqual(pod_spec(one(own, "StatefulSet"))["serviceAccountName"], "billing-reader")
        for binding in find(own, "RoleBinding") + find(own, "ClusterRoleBinding"):
            self.assertEqual([s["name"] for s in binding["subjects"]], ["billing-reader"])
        self.assertEqual(find(render(*TOKENS, "--set", "serviceAccount.create=false"), "ServiceAccount"), [])
        created = one(render(*TOKENS), "ServiceAccount")
        self.assertEqual(created["metadata"]["name"], "vbilling")


class EscalationTests(unittest.TestCase):
    """Grace periods are a list in the values, and reach vBilling and its
    configuration check as ENFORCEMENT_ESCALATIONS."""

    def test_the_list_renders_the_setting_for_the_pod_and_the_check(self):
        docs = render(*TOKENS, values=[values_file(self, """
enforcement:
  escalations:
    - {from: delinquent, after: 72h, to: suspended}
    - {from: suspended, after: 7d, to: paused}
""")])
        for kind in ("StatefulSet", "Job"):
            self.assertEqual(env(one(docs, kind))["ENFORCEMENT_ESCALATIONS"]["value"],
                             "delinquent:72h:suspended,suspended:7d:paused", kind)

    def test_without_escalations_the_setting_is_not_there(self):
        self.assertEqual(chartlib.yaml_file(chartlib.CHART / "values.yaml")["enforcement"]["escalations"], [])
        self.assertNotIn("ENFORCEMENT_ESCALATIONS", env(one(render(*TOKENS), "StatefulSet")))

    def test_the_schema_refuses_an_escalation_that_is_not_a_list_of_rules(self):
        for wrong in (["--set", "enforcement.escalations=delinquent:72h:suspended"],
                      ["--set", "enforcement.escalations[0].from=delinquent", "--set", "enforcement.escalations[0].to=suspended"]):
            with self.assertRaises(HelmError, msg=wrong) as refused:
                render(*TOKENS, *wrong)
            self.assertIn("schema", str(refused.exception), wrong)


class PricingKeysTests(unittest.TestCase):
    """billing.pricingKeys reaches vBilling as PRICING_KEYS, in the pod and in
    the configuration check that refuses keys a metric's events do not carry."""

    def test_pricing_keys_render_as_the_setting(self):
        values = values_file(self, """
billing:
  pricingKeys:
    vcluster_private_node_hours: [public_ip, zone]
    vcluster_gpu_hours: [gpu_type]
    vcluster_lb_hours: []
""")
        docs = render(*TOKENS, values=[values])
        # Sorted by metric (a template ranges over a map in key order), keys as
        # listed, and a metric without keys left out.
        want = "vcluster_gpu_hours:gpu_type,vcluster_private_node_hours:public_ip|zone"
        self.assertEqual(env(one(docs, "StatefulSet"))["PRICING_KEYS"]["value"], want)
        self.assertEqual(env(one(docs, "Job", "vbilling-config-check"))["PRICING_KEYS"]["value"], want)

    def test_without_pricing_keys_the_setting_is_not_set(self):
        self.assertNotIn("PRICING_KEYS", env(one(render(*TOKENS), "StatefulSet")))
        self.assertEqual(chartlib.yaml_file(chartlib.CHART / "values.yaml")["billing"]["pricingKeys"], {})

    def test_the_schema_wants_a_list_of_keys_per_metric(self):
        with self.assertRaises(HelmError) as refused:
            render(*TOKENS, "--set", "billing.pricingKeys.vcluster_gpu_hours=gpu_type")
        self.assertIn("schema", str(refused.exception))


class PlacementTests(unittest.TestCase):
    """In the validation lab vBilling ran on the control plane
    cluster's only node, a GPU node, with its ledger and its archive on
    node-local volumes of that node. The chart makes the right placement the
    easy one and says so when the values show a fragile one."""

    def test_placement_and_storage_classes_reach_the_statefulset(self):
        docs = render(*TOKENS, values=[values_file(self, """
nodeSelector: {node-role.example.com/infra: "true"}
tolerations: [{key: node-role.example.com/infra, operator: Exists}]
affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: nvidia.com/gpu.present, operator: NotIn, values: ["true"]}]}]}}}
topologySpreadConstraints: [{maxSkew: 1, topologyKey: topology.kubernetes.io/zone, whenUnsatisfiable: ScheduleAnyway}]
priorityClassName: billing-critical
persistence: {storageClass: replicated-ssd, archive: {enabled: true, storageClass: other-zone-hdd}}
""")])
        sts = one(docs, "StatefulSet")
        spec = pod_spec(sts)
        self.assertEqual(spec["nodeSelector"], {"node-role.example.com/infra": "true"})
        self.assertEqual(spec["tolerations"], [{"key": "node-role.example.com/infra", "operator": "Exists"}])
        self.assertEqual(spec["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"][0]
                         ["matchExpressions"][0]["key"], "nvidia.com/gpu.present")
        self.assertEqual(spec["topologySpreadConstraints"][0]["topologyKey"], "topology.kubernetes.io/zone")
        self.assertEqual(spec["priorityClassName"], "billing-critical")
        classes = {t["metadata"]["name"]: t["spec"].get("storageClassName") for t in sts["spec"]["volumeClaimTemplates"]}
        self.assertEqual(classes, {"data": "replicated-ssd", "archive": "other-zone-hdd"})

    def test_without_them_the_pod_spec_is_as_before(self):
        spec = pod_spec(one(render(*TOKENS), "StatefulSet"))
        for field in ("nodeSelector", "tolerations", "affinity", "topologySpreadConstraints", "priorityClassName", "imagePullSecrets"):
            self.assertNotIn(field, spec)

    def test_the_notes_warn_about_a_fragile_install(self):
        no_archive = "there is no archive volume"
        same_class = "use the same storage class"
        no_placement = "nothing says where vBilling runs"

        default = notes(*TOKENS)
        self.assertIn(no_archive, default)
        self.assertIn("only copy of the usage ledger", default)
        self.assertIn(no_placement, default)
        self.assertNotIn(same_class, default)

        both_default = notes(*TOKENS, "--set", "persistence.archive.enabled=true")
        self.assertIn(same_class + " (the cluster's default)", both_default)
        self.assertNotIn(no_archive, both_default)
        both_named = notes(*TOKENS, "--set", "persistence.archive.enabled=true", "--set", "persistence.storageClass=local-path",
                           "--set", "persistence.archive.storageClass=local-path")
        self.assertIn(same_class + " (local-path)", both_named)

        for sound in (["--set", "persistence.archive.enabled=true", "--set", "persistence.archive.storageClass=other-zone"],
                      ["--set", "persistence.archive.enabled=true", "--set", "persistence.archive.existingClaim=vbilling-archive"]):
            text = notes(*TOKENS, *sound, "--set", "nodeSelector.pool=infra")
            for warning in (no_archive, same_class, no_placement, "WARNING"):
                self.assertNotIn(warning, text, sound)

        self.assertNotIn(no_placement, notes(*TOKENS, "--set", "affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=k"))
        off = notes(*TOKENS, "--set", "persistence.enabled=false")
        self.assertIn("persistence is disabled", off)
        self.assertNotIn(no_archive, off)

        # An archive without the mirror keeps no copy of the newest seven days.
        no_mirror = "the ledger mirror is off"
        sound = ["--set", "persistence.archive.enabled=true", "--set", "persistence.archive.existingClaim=vbilling-archive", "--set", "nodeSelector.pool=infra"]
        self.assertIn(no_mirror, notes(*TOKENS, *sound, "--set", "persistence.archive.mirror=off"))
        self.assertIn(no_mirror, notes(*TOKENS, *sound, "--set", "persistence.archive.mirror=false"))
        for kept in ("auto", "on"):
            self.assertNotIn(no_mirror, notes(*TOKENS, *sound, "--set", "persistence.archive.mirror=" + kept))
        self.assertNotIn(no_mirror, notes(*TOKENS, "--set", "persistence.archive.mirror=off"))  # no archive: said already

    def test_the_notes_say_how_to_read_a_failed_check(self):
        self.assertIn("kubectl -n vbilling-system logs job/vbilling-config-check", notes(*TOKENS))
        off = notes(*TOKENS, "--set", "configCheck.enabled=false")
        self.assertIn("WARNING: the configuration check is off", off)
        self.assertNotIn("logs job/", off)

    def test_the_ledger_mirror_follows_the_archive(self):
        # No archive volume: nowhere to keep a mirror, and no setting for one.
        self.assertNotIn("LEDGER_MIRROR", env(one(render(*TOKENS), "StatefulSet")))
        archive = ["--set", "persistence.archive.enabled=true"]
        docs = render(*TOKENS, *archive)
        self.assertEqual(env(one(docs, "StatefulSet"))["LEDGER_MIRROR"]["value"], "auto")
        # The configuration check sees the setting the new pod gets.
        self.assertEqual(env(one(docs, "Job"))["LEDGER_MIRROR"]["value"], "auto")
        # A bare on or off in a values file is a YAML boolean: both spellings work.
        for given, want in (("mirror: off", "off"), ("mirror: on", "on"), ('mirror: "off"', "off"), ('mirror: "on"', "on"),
                            ("mirror: false", "off"), ("mirror: true", "on"), ("mirror: auto", "auto")):
            values = values_file(self, "persistence:\n  archive:\n    enabled: true\n    %s\n" % given)
            self.assertEqual(env(one(render(*TOKENS, values=[values]), "StatefulSet"))["LEDGER_MIRROR"]["value"], want, given)
        with self.assertRaises(HelmError) as refused:
            render(*TOKENS, *archive, "--set", "persistence.archive.mirror=sometimes")
        self.assertIn("mirror", str(refused.exception))

    def test_values_with_a_placeholder_left_in_are_refused(self):
        # A region or cluster name left as REPLACE_WITH_... would be written
        # on every usage event, and a placeholder as storage class leaves the
        # pod Pending: the chart refuses to render, and names each one.
        with self.assertRaises(HelmError) as refused:
            render(*TOKENS, values=[PRODUCTION_VALUES])
        message = str(refused.exception)
        for placeholder in ("REPLACE_WITH_REGION", "REPLACE_WITH_CONTROL_PLANE_CLUSTER", "REPLACE_WITH_REPLICATED_STORAGE_CLASS"):
            self.assertIn(placeholder, message)
        # Replacing some is not enough, wherever the placeholder sits.
        with self.assertRaises(HelmError) as refused:
            render(*TOKENS, *PRODUCTION[:4], values=[PRODUCTION_VALUES])
        self.assertIn("REPLACE_WITH_REPLICATED_STORAGE_CLASS", str(refused.exception))
        self.assertNotIn("REPLACE_WITH_REGION", str(refused.exception))
        with self.assertRaises(HelmError) as refused:
            render(*TOKENS, "--set", "nodeSelector.pool=REPLACE_WITH_INFRA_NODE_LABEL_VALUE")
        self.assertIn("REPLACE_WITH_INFRA_NODE_LABEL_VALUE", str(refused.exception))
        # The shipped defaults hold none.
        render(*TOKENS)

    def test_production_values_keep_vbilling_off_gpu_nodes_and_its_volumes_apart(self):
        docs = render(*TOKENS, *PRODUCTION, values=[PRODUCTION_VALUES])
        sts = one(docs, "StatefulSet")
        terms = pod_spec(sts)["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        self.assertEqual(terms, [{"matchExpressions": [{"key": "nvidia.com/gpu.present", "operator": "NotIn", "values": ["true"]}]}])
        self.assertEqual(pod_spec(one(docs, "Job"))["affinity"], pod_spec(sts)["affinity"])
        # The data volume: a class the operator has to name, never the default.
        templates = sts["spec"]["volumeClaimTemplates"]
        self.assertEqual([t["metadata"]["name"] for t in templates], ["data"])
        self.assertEqual(templates[0]["spec"]["storageClassName"], "replicated-ssd", templates[0]["spec"])
        self.assertTrue(chartlib.yaml_file(PRODUCTION_VALUES)["persistence"]["storageClass"].startswith("REPLACE_WITH_"))
        # The archive: on, and a volume provisioned apart from the data volume.
        claims = [v["persistentVolumeClaim"]["claimName"] for v in pod_spec(sts)["volumes"] if "persistentVolumeClaim" in v]
        self.assertEqual(claims, ["vbilling-archive"])
        self.assertIn("ARCHIVE_DIR", env(sts))
        text = notes(*TOKENS, *PRODUCTION, values=[PRODUCTION_VALUES])
        self.assertNotIn("WARNING", text)
        self.assertNotIn("nothing says where vBilling runs", text)
        # Placeholders are marked as such, and none names a product's term we do not use.
        source = PRODUCTION_VALUES.read_text()
        self.assertIn("REPLACE_WITH_REPLICATED_STORAGE_CLASS", source)
        self.assertIn("REPLACE_WITH_ARCHIVE_STORAGE_CLASS", source)
        self.assertNotIn("HOST_CLUSTER", source)


class AllowancesTests(unittest.TestCase):
    """billing.allowances becomes ALLOWANCES_FILE as metering.customMetrics
    becomes CUSTOM_METRICS_FILE: a file in the files ConfigMap that the pod
    and the configuration check both read."""

    ALLOWANCES = [
        {"metric": "vcluster_storage_gb_hours", "included": 32, "per": "node"},
        {"metric": "vcluster_storage_gb_hours", "included": 10.5, "per": "volume", "storageClasses": ["hdd", "archive"]},
    ]

    def test_the_pod_and_the_check_read_the_same_file(self):
        values = values_file(self, json.dumps({"billing": {"allowances": self.ALLOWANCES}}))
        docs = render(*TOKENS, values=[values])
        sts, job = one(docs, "StatefulSet"), one(docs, "Job")
        for obj in (sts, job):
            self.assertEqual(env(obj)["ALLOWANCES_FILE"], {"name": "ALLOWANCES_FILE", "value": "/etc/vbilling/files/allowances.yaml"})
            self.assertIn("/etc/vbilling/files", [m["mountPath"] for m in container(obj)["volumeMounts"]])
        files, copy = one(docs, "ConfigMap", "vbilling-files"), one(docs, "ConfigMap", "vbilling-files-config-check")
        self.assertEqual(sorted(files["data"]), ["allowances.yaml"])
        self.assertEqual(copy["data"], files["data"])
        # A YAML list: parsed under a key, since the helper reads mappings.
        text = files["data"]["allowances.yaml"]
        parsed = chartlib.yaml_documents("allowances:\n" + "".join("  " + line + "\n" for line in text.splitlines()))[0]
        self.assertEqual(parsed["allowances"], self.ALLOWANCES)

    def test_without_allowances_nothing_is_set(self):
        docs = render(*TOKENS)
        self.assertNotIn("ALLOWANCES_FILE", env(one(docs, "StatefulSet")))
        self.assertEqual(find(docs, "ConfigMap", "vbilling-files"), [])
        self.assertEqual(chartlib.yaml_file(chartlib.CHART / "values.yaml")["billing"]["allowances"], [])

    def test_the_schema_refuses_allowances_of_the_wrong_type(self):
        entry = ["--set", "billing.allowances[0].metric=vcluster_storage_gb_hours", "--set", "billing.allowances[0].per=node"]
        render(*TOKENS, *entry, "--set", "billing.allowances[0].included=32")
        for wrong in (["--set", "billing.allowances=32"],
                      [*entry, "--set-string", "billing.allowances[0].included=32"],
                      [*entry],  # nothing included
                      [*entry, "--set", "billing.allowances[0].included=32", "--set", "billing.allowances[0].storageClasses=fast"]):
            with self.assertRaises(HelmError, msg=wrong) as refused:
                render(*TOKENS, *wrong)
            self.assertIn("schema", str(refused.exception), wrong)


if __name__ == "__main__":
    unittest.main()
