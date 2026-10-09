"""pytest plugin of the Nexo s3-tests harness (docs/06 §6.1, T-9.1).

Loaded by run-s3tests.sh with ``-p nexo_s3tests`` (PYTHONPATH=test/compat),
it

* deselects the tests listed in excluded.txt (by name glob or by marker),
  each with the reason it does not apply to Nexo;
* classifies every collected test in a functional group (versioning,
  object lock, multipart, ...), the unit of the compatibility targets;
* records the outcome of every test and writes, at the end of the session,
  ``report.json`` (machine readable) and ``report.txt`` (per-group summary,
  then one line per test) in --nexo-report-dir.

The applicable tests of a group are the ones run and not skipped by the
suite itself (a test that skips, e.g. for a missing configuration, does
not count); the pass rate is passed / applicable.
"""

import json
import os
import re
import time

# Ordered classification rules: the first match wins.
GROUP_RULES = [
    ("logging", r"logging"),
    ("tiering", r"restore|read_through|transition|storage_class"),
    ("object-lock", r"object_lock|_retention|legal_hold"),
    ("sse-c", r"sse_c|encrypted_transfer|enc_conflict_c_"),
    ("sse-s3-kms", r"sse_kms|sse_s3|bucket_encryption|enc_conflict|encryption"),
    ("checksum", r"checksum|cksum"),
    ("cors", r"cors"),
    ("lifecycle", r"lifecycle"),
    ("versioning", r"versioning|versioned|version|delete_marker"),
    ("multipart", r"multipart|upload_part|list_parts|abort|_uploads"),
    ("tagging", r"tagging|_tags?(_|$)"),
    ("policy", r"policy|public_block|block_public"),
    ("post-object", r"post_object"),
    ("acl", r"acl|grant|owner|access_bucket|object_writer"),
    ("listing", r"list|key_count"),
    ("bucket", r"^test_bucket|^test_buckets"),
    ("object", r"^test_object|^test_put|^test_get|^test_ranged|^test_atomic|^test_copy|^test_head"
               r"|^test_multi_object|^test_delete_object|^test_100_continue"),
]

# The groups of the M7 compatibility target (DoD T-7.1, T-9.1 anticipated).
TARGET_GROUPS = ["versioning", "object-lock", "multipart", "tagging", "sse-c", "cors", "checksum"]
TARGET_RATE = 0.90


def group_of(name):
    """Returns the functional group of a test name."""
    for group, pattern in GROUP_RULES:
        if re.search(pattern, name):
            return group
    return "other"


class Rule:
    """An exclusion: a test-name glob or a marker, with its reason."""

    def __init__(self, selector, reason, lineno):
        self.selector = selector
        self.reason = reason
        self.lineno = lineno
        self.used = 0
        # "*" and "?" are the only wildcards: brackets are literal, as in
        # the names of parametrized tests (test_x[sse-c-...]).
        self.regex = re.compile("^" + re.escape(selector).replace(r"\*", ".*").replace(r"\?", ".") + "$")

    def matches(self, names, markers):
        """names: the test name and, for a parametrized test, its full name
        with the parameters (test_x[a-b])."""
        if self.selector.startswith("marker:"):
            return self.selector[len("marker:"):] in markers
        return any(self.regex.match(n) for n in names)


def load_rules(path):
    """Parses excluded.txt: '<selector> | <reason>' per line, '#' comments."""
    rules = []
    with open(path, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if "|" not in line:
                raise ValueError("%s:%d: expected '<selector> | <reason>'" % (path, lineno))
            selector, reason = (part.strip() for part in line.split("|", 1))
            if not selector or not reason:
                raise ValueError("%s:%d: empty selector or reason" % (path, lineno))
            rules.append(Rule(selector, reason, lineno))
    return rules


class NexoCompat:
    """Session state: exclusions, groups and outcomes."""

    def __init__(self, config):
        self.config = config
        self.report_dir = config.getoption("nexo_report_dir")
        path = config.getoption("nexo_excluded")
        self.rules = load_rules(path) if path else []
        self.tests = {}  # nodeid -> record
        self.started = time.time()

    # Collection: deselect the excluded tests.
    def pytest_collection_modifyitems(self, session, config, items):
        keep, drop = [], []
        for item in items:
            name = getattr(item, "originalname", None) or item.name
            markers = {m.name for m in item.iter_markers()}
            rec = {"name": item.name, "group": group_of(name), "outcome": None, "reason": "", "duration": 0.0}
            # The first matching rule gives the reason; every matching rule
            # counts as used, so that the report lists only stale ones.
            matching = [r for r in self.rules if r.matches((name, item.name), markers)]
            for r in matching:
                r.used += 1
            if matching:
                rec["outcome"], rec["reason"] = "excluded", matching[0].reason
                drop.append(item)
            else:
                keep.append(item)
            self.tests[item.nodeid] = rec
        if drop:
            config.hook.pytest_deselected(items=drop)
        items[:] = keep

    # Execution: one outcome per test (an error in setup or teardown wins).
    def pytest_runtest_logreport(self, report):
        rec = self.tests.get(report.nodeid)
        if rec is None:
            return
        rec["duration"] += report.duration
        if report.when == "call":
            if rec["outcome"] != "error":
                rec["outcome"] = "skipped" if report.skipped else ("passed" if report.passed else "failed")
                if report.failed:
                    rec["reason"] = _short(report)
        elif report.failed:
            rec["outcome"], rec["reason"] = "error", _short(report)
        elif report.skipped and rec["outcome"] is None:
            rec["outcome"] = "skipped"
            rec["reason"] = _skip_reason(report)

    def pytest_sessionfinish(self, session, exitstatus):
        if not self.report_dir:
            return
        os.makedirs(self.report_dir, exist_ok=True)
        groups = {}
        for rec in self.tests.values():
            if rec["outcome"] is None:
                rec["outcome"] = "not-run"
            g = groups.setdefault(rec["group"], {k: 0 for k in
                                                  ("collected", "excluded", "passed", "failed", "error", "skipped", "not-run")})
            g["collected"] += 1
            g[rec["outcome"]] += 1
        for g in groups.values():
            g["applicable"] = g["passed"] + g["failed"] + g["error"]
            g["rate"] = round(g["passed"] / g["applicable"], 4) if g["applicable"] else None
        total = {k: sum(g[k] for g in groups.values()) for k in
                 ("collected", "excluded", "passed", "failed", "error", "skipped", "not-run", "applicable")}
        total["rate"] = round(total["passed"] / total["applicable"], 4) if total["applicable"] else None
        target = {k: sum(groups.get(g, {}).get(k, 0) for g in TARGET_GROUPS) for k in ("passed", "applicable")}
        target["rate"] = round(target["passed"] / target["applicable"], 4) if target["applicable"] else None
        target["groups"] = TARGET_GROUPS
        target["threshold"] = TARGET_RATE
        target["met"] = all(
            groups.get(g, {}).get("rate") is None or groups[g]["rate"] >= TARGET_RATE for g in TARGET_GROUPS)
        unused = ["excluded.txt:%d %s" % (r.lineno, r.selector) for r in self.rules if r.used == 0]
        doc = {
            "suite": os.environ.get("NEXO_S3TESTS_SUITE", "ceph/s3-tests"),
            "commit": os.environ.get("NEXO_S3TESTS_COMMIT", ""),
            "mode": os.environ.get("NEXO_S3TESTS_MODE", ""),
            "duration_seconds": round(time.time() - self.started, 1),
            "total": total,
            "target": target,
            "groups": dict(sorted(groups.items())),
            "unused_exclusions": unused,
            "tests": dict(sorted(self.tests.items())),
        }
        with open(os.path.join(self.report_dir, "report.json"), "w", encoding="utf-8") as f:
            json.dump(doc, f, indent=1, sort_keys=False)
            f.write("\n")
        with open(os.path.join(self.report_dir, "report.txt"), "w", encoding="utf-8") as f:
            f.write(render_text(doc))

    def pytest_terminal_summary(self, terminalreporter):
        if not self.report_dir:
            return
        path = os.path.join(self.report_dir, "report.txt")
        if os.path.exists(path):
            with open(path, encoding="utf-8") as f:
                summary = f.read().split("\n\n", 2)
            terminalreporter.write_sep("=", "Nexo s3-tests compatibility")
            terminalreporter.write("\n\n".join(summary[:2]) + "\n")


def render_text(doc):
    """Renders the report: header, per-group table, per-test lines."""
    out = []
    t = doc["total"]
    out.append("Nexo s3-tests report: %s@%s, mode %s, %.0fs" % (
        doc["suite"], doc["commit"][:12], doc["mode"] or "?", doc["duration_seconds"]))
    out.append("Collected %d, excluded %d, applicable %d: passed %d, failed %d, errors %d (%s); skipped by the suite %d" % (
        t["collected"], t["excluded"], t["applicable"], t["passed"], t["failed"], t["error"], _pct(t["rate"]), t["skipped"]))
    tg = doc["target"]
    out.append("M7 target groups (%s): %d/%d passed (%s), threshold %d%% per group: %s" % (
        ", ".join(tg["groups"]), tg["passed"], tg["applicable"], _pct(tg["rate"]), tg["threshold"] * 100,
        "MET" if tg["met"] else "NOT MET"))
    out.append("")
    out.append("%-12s %9s %8s %10s %7s %7s %6s %8s %7s" % (
        "group", "collected", "excluded", "applicable", "passed", "failed", "error", "skipped", "rate"))
    for name, g in doc["groups"].items():
        out.append("%-12s %9d %8d %10d %7d %7d %6d %8d %7s" % (
            name, g["collected"], g["excluded"], g["applicable"], g["passed"], g["failed"], g["error"], g["skipped"],
            _pct(g["rate"])))
    if doc["unused_exclusions"]:
        out.append("")
        out.append("Exclusions matching no collected test (markers of other suite files, or stale): " + ", ".join(doc["unused_exclusions"]))
    out.append("")
    order = {"failed": 0, "error": 1, "passed": 2, "skipped": 3, "not-run": 4, "excluded": 5}
    for nodeid, rec in sorted(doc["tests"].items(), key=lambda kv: (order.get(kv[1]["outcome"], 9), kv[1]["group"], kv[0])):
        line = "%-8s %-12s %s" % (rec["outcome"].upper(), rec["group"], rec["name"])
        if rec["reason"]:
            line += "  -- " + rec["reason"]
        out.append(line)
    return "\n".join(out) + "\n"


def _pct(rate):
    return "n/a" if rate is None else "%.1f%%" % (rate * 100)


def _short(report):
    """The last line of the failure (the assertion or the exception)."""
    text = getattr(report, "longreprtext", "") or ""
    lines = [l.strip() for l in text.splitlines() if l.strip().startswith("E ")]
    msg = lines[-1][2:].strip() if lines else (text.strip().splitlines() or [""])[-1]
    return msg[:300]


def _skip_reason(report):
    if isinstance(report.longrepr, tuple) and len(report.longrepr) == 3:
        return str(report.longrepr[2])[:300]
    return ""


def pytest_addoption(parser):
    group = parser.getgroup("nexo", "Nexo s3-tests harness")
    group.addoption("--nexo-excluded", dest="nexo_excluded", default=None,
                    help="excluded.txt: tests deselected with their reason")
    group.addoption("--nexo-report-dir", dest="nexo_report_dir", default=None,
                    help="directory of report.json and report.txt")


def pytest_configure(config):
    config.pluginmanager.register(NexoCompat(config), "nexo-compat-session")
