"""Tests of the alert rules in deploy/monitoring/vbilling-alerts.yaml."""

import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

import chartlib

RULES = chartlib.ROOT / "deploy/monitoring/vbilling-alerts.yaml"
RULE_TESTS = Path(__file__).with_name("alert_rules_test.yaml")

# Metrics a rule may use before the code that exports them is merged, with
# the labels the rule may show. Empty: every metric a rule uses is exported.
ANNOUNCED = {}
# Also new in v0.3.3, and known here without a rule of their own: what the
# ingest API refuses is the caller's to fix, and excluded clusters are a
# count to look at, not a fault.
KNOWN_WITHOUT_RULE = {
    "vbilling_ingest_requests_refused_total": {"endpoint", "reason"},
    "vbilling_ingest_events_rejected_total": {"client", "reason"},
    "vbilling_excluded_clusters": set(),
}

_METRIC = re.compile(r"\bvbilling_[a-z0-9_]+\b")
_EMIT = re.compile(r'\.(?:Add|Set)\(\s*"(vbilling_[a-z0-9_]+)"')
# A metric whose name is a constant (const xMetric, xHelp = "vbilling_...",
# "...", or xMetric = "vbilling_..." in a const block) is exported by the
# calls that use it.
_EMIT_CONST = re.compile(r'\b\w+Metric\b(?:\s*,\s*\w+)?\s*=\s*"(vbilling_[a-z0-9_]+)"')
_LABEL_KEY = re.compile(r'"([a-z_]+)":')
_DURATION = re.compile(r"^([0-9]+[smhd])+$")


def exported_metrics():
    """{metric: {label keys written as literals in the file that exports it}}
    from the Go sources: the first argument of telemetry Add and Set calls."""
    found = {}
    for top in ("cmd", "internal"):
        for path in (chartlib.ROOT / top).rglob("*.go"):
            if path.name.endswith("_test.go"):
                continue
            text = path.read_text()
            names = set(_EMIT.findall(text)) | set(_EMIT_CONST.findall(text))
            if names:
                labels = set(_LABEL_KEY.findall(text))
                for name in names:
                    found.setdefault(name, set()).update(labels)
    return found


class AlertRuleTests(unittest.TestCase):
    """No shipped rule covered a stale last window, held
    events, failed gateway calls or a source that returns nothing."""

    @classmethod
    def setUpClass(cls):
        cls.doc = chartlib.yaml_file(RULES)
        cls.rules = [r for g in cls.doc["spec"]["groups"] for r in g["rules"]]
        cls.by_name = {r["alert"]: r for r in cls.rules}
        cls.exported = exported_metrics()

    def test_the_file_is_a_prometheus_rule_with_complete_alerts(self):
        self.assertEqual((self.doc["apiVersion"], self.doc["kind"]), ("monitoring.coreos.com/v1", "PrometheusRule"))
        self.assertEqual(len(self.by_name), len(self.rules), "alert names are unique")
        for rule in self.rules:
            name = rule["alert"]
            self.assertRegex(name, r"^VBilling[A-Z][A-Za-z]+$")
            self.assertEqual(set(rule) - {"alert", "expr", "for", "labels", "annotations"}, set(), name)
            self.assertIn(rule["labels"]["severity"], ("warning", "critical"), name)
            # Every rule waits before it fires: none pages on one bad scrape.
            self.assertRegex(rule.get("for", ""), _DURATION, f"{name} needs a for: duration")
            for key in ("summary", "description"):
                self.assertGreater(len(rule["annotations"][key].strip()), 20, f"{name} {key}")
            # What to do, not only what is wrong.
            self.assertRegex(rule["annotations"]["description"], r"Check|Inspect|Grow|Add|Correct|Find|Fix", name)
            expr = rule["expr"]
            for opening, closing in ("()", "{}", "[]"):
                self.assertEqual(expr.count(opening), expr.count(closing), f"{name}: unbalanced {opening}{closing} in {expr!r}")

    def test_promtool_accepts_the_rules(self):
        promtool = shutil.which("promtool")
        if not promtool:
            self.skipTest("promtool is not installed; the structure is checked by the other tests")
        with tempfile.TemporaryDirectory() as work:
            path = Path(work) / "rules.yaml"
            path.write_text(json.dumps(self.doc["spec"]))  # JSON is YAML
            result = subprocess.run([promtool, "check", "rules", str(path)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(f"{len(self.rules)} rules found", result.stdout + result.stderr)

    def test_each_alert_fires_when_it_should(self):
        # alert_rules_test.yaml: promtool rule tests. They compare labels only,
        # so they run against the rules without their annotations.
        promtool = shutil.which("promtool")
        if not promtool:
            self.skipTest("promtool is not installed")
        spec = json.loads(json.dumps(self.doc["spec"]))
        for group in spec["groups"]:
            for rule in group["rules"]:
                del rule["annotations"]
        with tempfile.TemporaryDirectory() as work:
            (Path(work) / "rules.yaml").write_text(json.dumps(spec))
            shutil.copy(RULE_TESTS, Path(work) / RULE_TESTS.name)
            result = subprocess.run([promtool, "test", "rules", RULE_TESTS.name], cwd=work, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_the_rule_tests_cover_every_alert(self):
        tested = set(re.findall(r"alertname: (\w+)", RULE_TESTS.read_text()))
        self.assertEqual(set(self.by_name) - tested, set(), "alerts without a case in alert_rules_test.yaml")
        self.assertEqual(tested - set(self.by_name), set(), "alert_rules_test.yaml names alerts that do not exist")

    def test_every_metric_an_alert_uses_is_exported_or_announced(self):
        self.assertGreater(len(self.exported), 50, "the scan of the Go sources finds the telemetry calls")
        for rule in self.rules:
            used = set(_METRIC.findall(rule["expr"]))
            self.assertTrue(used, f"{rule['alert']} uses no vBilling metric")
            mentioned = used | set(_METRIC.findall(json.dumps(rule["annotations"])))
            for metric in sorted(mentioned):
                self.assertTrue(metric in self.exported or metric in ANNOUNCED or metric in KNOWN_WITHOUT_RULE,
                                f"{rule['alert']} uses {metric}, which no telemetry call in cmd/ or internal/ exports")
        # Every announced metric has its rule, under its exact name.
        used = set(_METRIC.findall(" ".join(r["expr"] for r in self.rules)))
        self.assertEqual(set(ANNOUNCED) - used, set())
        self.assertEqual(set(ANNOUNCED) & set(KNOWN_WITHOUT_RULE), set())

    def test_every_label_an_annotation_shows_is_one_the_metric_has(self):
        for rule in self.rules:
            metrics = set(_METRIC.findall(rule["expr"]))
            known = set()
            for metric in metrics:
                known |= ANNOUNCED.get(metric, set()) | self.exported.get(metric, set())
            shown = set(re.findall(r"\$labels\.([a-z_]+)", json.dumps(rule["annotations"])))
            self.assertEqual(shown - known, set(), f"{rule['alert']} shows labels its metric does not have")
            if rule["expr"].startswith("absent("):
                self.assertEqual(shown, set(), f"{rule['alert']}: absent() has no labels to show")

    def test_the_signals_operators_asked_for_each_have_a_rule(self):
        def rule(metric, *needles, severity=None):
            found = [r for r in self.rules if metric in _METRIC.findall(r["expr"]) and all(n in r["expr"] for n in needles)]
            self.assertEqual(len(found), 1, f"one rule on {metric} {needles}, found {[r['alert'] for r in found]}")
            if severity:
                self.assertEqual(found[0]["labels"]["severity"], severity, found[0]["alert"])
            return found[0]

        # A last committed window older than 5 minutes.
        stale = rule("vbilling_last_window_end_timestamp_seconds", "time() -", "> 300")
        self.assertEqual(stale["expr"], "time() - vbilling_last_window_end_timestamp_seconds > 300")
        # vBilling is one pod: when it is down its series are gone and the
        # rule above cannot fire. This one does.
        rule("vbilling_last_window_end_timestamp_seconds", "absent(", severity="critical")
        # Events held for longer than a threshold.
        held = rule("vbilling_held_events", "> 0")
        self.assertEqual(held["for"], "1h")
        # Failed gateway block and unblock calls.
        rule("vbilling_gateway_block_errors_total", "increase(")
        # A source that returns nothing (a selector that matches no series).
        empty = rule("vbilling_sources", 'status="empty"', severity="warning")
        self.assertEqual(empty["for"], "1h")
        unhealthy = rule("vbilling_sources", "failing|diverged")
        self.assertNotIn("empty", unhealthy["expr"])
        # Delivery lag and dead letters.
        rule("vbilling_destination_lag_seconds", "> 300")
        rule("vbilling_events_dead_lettered_total", "increase(", severity="critical")
        # The metrics v0.3.3 adds.
        rule("vbilling_webhooks_total", 'result=~"refused|unsettled"')
        rule("vbilling_tenants_blocked", "> 0")
        rule("vbilling_enforcement_repairs_total", "increase(")
        mirror = rule("vbilling_ledger_mirror_lag_seconds", "> 300")
        self.assertEqual(mirror["expr"], "vbilling_ledger_mirror_lag_seconds > 300")
        # Usage read from Prometheus: its last window older than 5 minutes
        # (only where the gauge is exported and set), samples that keep
        # arriving late for 15 minutes, and any change of a tenant's billing ID.
        prom = rule("vbilling_prometheus_last_window_end_timestamp_seconds", "time() -", "> 300", severity="warning")
        self.assertIn("vbilling_prometheus_last_window_end_timestamp_seconds > 0", prom["expr"])
        late = rule("vbilling_prometheus_late_samples_total", "increase(", severity="warning")
        self.assertEqual(late["for"], "15m")
        rule("vbilling_tenant_id_changes_total", "increase(", severity="warning")
        # Both volumes: the archive volume has the gauges from its first check now.
        rule("vbilling_volume_free_bytes", "vbilling_volume_size_bytes")

    def test_a_first_event_fires_its_rule(self):
        # increase() cannot see the first sample of a series, and these
        # counters appear with their first event: the rule also fires while
        # the series is new.
        for metric in ("vbilling_events_dead_lettered_total", "vbilling_webhooks_total", "vbilling_enforcement_repairs_total",
                       "vbilling_tenant_id_changes_total"):
            rule = next(r for r in self.rules if metric in r["expr"])
            expr = " ".join(rule["expr"].split())
            window = re.search(r"increase\(" + metric + r"(\{[^}]*\})?\[(\w+)\]\) > 0", expr)
            self.assertTrue(window, f"{rule['alert']}: {expr}")
            selector, duration = window.group(1) or "", window.group(2)
            series = metric + selector
            self.assertIn(f"or ({series} > 0 unless {series} offset {duration})", expr, rule["alert"])

    def test_the_texts_follow_the_writing_rules(self):
        text = RULES.read_text()
        for banned in ("—", "host cluster", "virtual cluster", "neocloud", "Neocloud"):
            self.assertNotIn(banned, text)


if __name__ == "__main__":
    unittest.main()
