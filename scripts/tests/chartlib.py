"""Helpers for the chart and alert rule tests.

The tests need nothing but Python's standard library and helm, like the CI
job that runs them. YAML is read with helm itself: a throwaway chart turns a
YAML file into JSON (fromYaml | toJson), so no YAML package has to be there.
"""

import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "deploy/helm/vbilling"
NAMESPACE = "vbilling-system"

_TO_JSON_TEMPLATE = r"""
{{- $docs := list -}}
{{- range regexSplit "(?m)^---[ \t]*$" .Values.input -1 -}}
{{- if regexMatch "(?m)^[^#\\s]" . -}}
{{- $docs = append $docs (fromYaml .) -}}
{{- end -}}
{{- end -}}
{{- dict "docs" $docs | toJson -}}
"""


class HelmError(AssertionError):
    """helm exited with an error; the message is its output."""


def helm(*args):
    """Runs helm and returns its standard output."""
    result = subprocess.run(["helm", *args], capture_output=True, text=True)
    if result.returncode != 0:
        raise HelmError(f"helm {' '.join(str(a) for a in args)}\n{result.stdout}{result.stderr}")
    return result.stdout


def yaml_documents(text):
    """Parses YAML text (one or several documents, each a mapping) into a list."""
    with tempfile.TemporaryDirectory() as work:
        chart = Path(work) / "tojson"
        (chart / "templates").mkdir(parents=True)
        (chart / "Chart.yaml").write_text("apiVersion: v2\nname: tojson\nversion: 0.0.0\n")
        (chart / "templates/out.yaml").write_text(_TO_JSON_TEMPLATE)
        source = Path(work) / "input.yaml"
        source.write_text(text)
        out = helm("template", "tojson", str(chart), "--set-file", f"input={source}")
    docs = json.loads(out[out.index("{"):])["docs"]
    for doc in docs:
        if set(doc) == {"Error"}:
            raise AssertionError(f"not valid YAML: {doc['Error']}")
    return docs


def yaml_file(path):
    """Parses a YAML file that holds one document."""
    docs = yaml_documents(Path(path).read_text())
    if len(docs) != 1:
        raise AssertionError(f"{path} holds {len(docs)} documents, expected one")
    return docs[0]


def render(*args, chart=CHART, values=None):
    """helm template of the chart, parsed: a list of objects. Hooks are included
    (pass "--no-hooks" to leave them out, as an install's own objects)."""
    cmd = ["template", "vbilling", str(chart), "--namespace", NAMESPACE]
    for f in values or []:
        cmd += ["-f", str(f)]
    return yaml_documents(helm(*cmd, *args))


def notes(*args, values=None):
    """Renders templates/NOTES.txt as an install would print it. helm template
    does not show notes, and a dry-run install needs a cluster with some helm
    versions, so a copy of the chart renders them into a ConfigMap."""
    with tempfile.TemporaryDirectory() as work:
        chart = Path(work) / "vbilling"
        shutil.copytree(CHART, chart)
        shutil.copy(chart / "templates/NOTES.txt", chart / "notes-under-test.txt")
        (chart / "templates/zz-notes-under-test.yaml").write_text(
            'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes-under-test\n'
            'data:\n  notes: {{ tpl (.Files.Get "notes-under-test.txt") . | toJson }}\n')
        docs = render(*args, "--show-only", "templates/zz-notes-under-test.yaml", chart=chart, values=values)
    return docs[0]["data"]["notes"]


def find(docs, kind, name=None):
    """The objects of a kind (and name) in a render."""
    return [d for d in docs if d.get("kind") == kind and (name is None or d["metadata"]["name"] == name)]


def one(docs, kind, name=None):
    found = find(docs, kind, name)
    if len(found) != 1:
        names = sorted(f"{d.get('kind')}/{d['metadata']['name']}" for d in docs)
        raise AssertionError(f"expected one {kind} {name or ''}, found {len(found)} in {names}")
    return found[0]


def is_hook(obj):
    return "helm.sh/hook" in (obj["metadata"].get("annotations") or {})


def pod_spec(obj):
    """The pod spec of a StatefulSet or Job."""
    return obj["spec"]["template"]["spec"]


def container(obj):
    containers = pod_spec(obj)["containers"]
    if len(containers) != 1:
        raise AssertionError(f"{obj['kind']} has {len(containers)} containers")
    return containers[0]


def env(obj):
    """The container's environment as {name: entry}."""
    return {e["name"]: e for e in container(obj).get("env") or []}


# --- the values the templates read ------------------------------------------

_ACTION = re.compile(r"\{\{-?\s*(.*?)\s*-?\}\}", re.DOTALL)
_COMMENT = re.compile(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", re.DOTALL)
_STRING = re.compile(r'"(?:[^"\\]|\\.)*"|`[^`]*`')
_VALUES_PATH = re.compile(r"\.Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)")
# (.Values.a.b).c and ((.Values.a).b).c: optional chaining of a missing parent.
_PARENS = re.compile(r"\(\s*(\.Values(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\)((?:\.[A-Za-z_][A-Za-z0-9_]*)+)")
_BLOCK = re.compile(r"^(if|with|range|define|block)\b")
_SCOPE = re.compile(r"^(with|range)\s+(?:\$\w+\s*(?:,\s*\$\w+\s*)?:=\s*)?\.Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)\s*$")
_RELATIVE = re.compile(r"(?<![\w.$)])\.([A-Za-z_][A-Za-z0-9_]*)")
# $name := .Values.a.b (also .root.Values.a.b): a variable that stands for a values path.
_ALIAS = re.compile(r"^\$(\w+)\s*:=\s*(?:\.root)?\.Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)$")
_ALIAS_USE = re.compile(r"\$(\w+)((?:\.[A-Za-z_][A-Za-z0-9_]*)+)")


def template_value_paths(chart=CHART):
    """Every path under .Values that a template reads, as dotted strings.

    A path through a list's items has "[]" in it ("records.pullEnv[].name").
    Fields read relative to `with .Values.x` and `range .Values.x`, and
    through a variable set to a values path (`$archive := .Values.x`), are
    resolved; a `with` or `range` over anything else hides nothing because
    the chart only reads values from the root inside those.
    """
    paths = set()
    for path in sorted((chart / "templates").iterdir()):
        text = _COMMENT.sub("", path.read_text())
        scopes = []  # one entry per open block: a values prefix, or None
        aliases = {}  # variable -> the values path it stands for
        for match in _ACTION.finditer(text):
            action = _STRING.sub('""', match.group(1))
            previous = None
            while previous != action:  # (.Values.a).b -> .Values.a.b, innermost first
                previous = action
                action = _PARENS.sub(lambda m: m.group(1) + m.group(2), action)
            for found in _VALUES_PATH.findall(action):
                paths.add(found[1:])
            alias = _ALIAS.match(action)
            if alias:
                aliases[alias.group(1)] = alias.group(2)[1:]
            for name, fields in _ALIAS_USE.findall(action):
                if name in aliases:
                    paths.add(aliases[name] + fields)
            if action == "end":
                scopes.pop()
                continue
            if action.startswith("else"):
                continue
            prefix = next((s for s in reversed(scopes) if s), None)
            if prefix:
                for field in _RELATIVE.findall(action):
                    if field not in ("Values", "Release", "Chart", "Files", "Capabilities", "Template"):
                        paths.add(f"{prefix}.{field}")
            if _BLOCK.match(action):
                scope = _SCOPE.match(action)
                if not scope:
                    scopes.append(None)
                elif scope.group(1) == "range":
                    scopes.append(scope.group(2)[1:] + "[]")
                else:
                    scopes.append(scope.group(2)[1:])
        if scopes:
            raise AssertionError(f"{path.name}: {len(scopes)} block(s) left open: the scanner lost track")
    return paths


def values_paths(values):
    """Every key path of a values tree. Lists and empty maps are leaves."""
    out = set()

    def walk(node, prefix):
        for key, value in node.items():
            path = f"{prefix}.{key}" if prefix else key
            out.add(path)
            if isinstance(value, dict) and value:
                walk(value, path)

    walk(values, "")
    return out


def schema_node(schema, path):
    """The schema of a values path, or None when the schema does not describe
    it. A map without "properties" is free-form: it describes all below it."""
    node = schema
    for part in path.replace("[]", ".[]").split("."):
        if part == "[]":
            node = node.get("items")
        elif "properties" in node:
            node = node["properties"].get(part)
        elif "object" in _types(node):
            return node  # free-form map
        else:
            return None
        if node is None:
            return None
    return node


def schema_paths(schema):
    """Every path the schema names, in the form of values_paths."""
    out = set()

    def walk(node, prefix):
        for key, child in (node.get("properties") or {}).items():
            path = f"{prefix}.{key}" if prefix else key
            out.add(path)
            walk(child, path)

    walk(schema, "")
    return out


def _types(node):
    t = node.get("type", [])
    return t if isinstance(t, list) else [t]
