"""pytest plugin of the Nexo s3-tests harness (docs/06 §6.1, T-9.1).

Loaded by run-s3tests.sh with ``-p nexo_s3tests`` (PYTHONPATH=test/compat),
it

* deselects the tests listed in excluded.txt (by name glob or by marker),
  each with a category and the reason it does not apply to Nexo;
* classifies every collected test in a functional group (versioning,
  object lock, multipart, headers, ...), the unit of the compatibility
  target: every group with applicable tests passes at least
  --nexo-threshold of them (95%, T-9.1);
* records the outcome of every test and writes, at the end of the session,
  ``report.json`` (machine readable) and ``report.txt`` (per-group summary,
  exclusions per category, then one line per test) in --nexo-report-dir.

The applicable tests of a group are the ones run and not skipped by the
suite itself (a test that skips, e.g. for a missing configuration, does
not count); the pass rate is passed / applicable.

With --nexo-check-excluded the tests excluded by name are run too (still
counted as excluded): the report lists the ones that pass, whose exclusion
is no longer justified ("names"; "all" also runs the marker exclusions).
"""

import json
import os
import re
import time

# The categories of an exclusion: every line of excluded.txt names one, so
# that the reason can be verified against its source.
CATEGORIES = {
    "rgw": "RGW extension or RGW-only expectation, marked by the suite itself (fails_on_aws, appendobject)",
    "deprecated": "API deprecated by AWS and a non-goal of Nexo (Signature V2, BitTorrent): docs/01 §1.1",
    "p2": "feature P2 of the docs/02 catalog, after v1.0 (the catalog ID is in the reason)",
    "out-of-catalog": "AWS API outside the docs/02 catalog (IAM and STS APIs, SNS, S3 Control, server access logging, website API)",
    "aws": "the test expects RGW's answer where Amazon S3 answers otherwise; Nexo follows S3 (docs/02 §2.4)",
    "design": "documented difference of Nexo (docs/02 §2.4: key-prefix layout, single account, Expect header)",
    "suite": "the test cannot send the request it describes: botocore rewrites the header it sets (fails_on_rgw with a TODO)",
}

# The groups of the files other than test_s3.py (one group per file).
FILE_GROUPS = {
    "test_headers.py": "headers",
    "test_iam.py": "iam",
    "test_sts.py": "sts",
    "test_s3select.py": "s3select",
    "test_sns.py": "sns",
    "test_s3control.py": "s3control",
    "test_utils.py": "suite-utils",
}

# Ordered classification rules of test_s3.py: the first match wins.
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

# The pass rate every group with applicable tests must reach (T-9.1).
DEFAULT_THRESHOLD = 0.95


def group_of(name, path=""):
    """Returns the functional group of a test (its name and file)."""
    group = FILE_GROUPS.get(os.path.basename(path))
    if group:
        return group
    for group, pattern in GROUP_RULES:
        if re.search(pattern, name):
            return group
    return "other"


class Rule:
    """An exclusion: a test-name glob or a marker, its category and reason."""

    def __init__(self, selector, category, reason, lineno):
        self.selector = selector
        self.category = category
        self.reason = reason
        self.lineno = lineno
        self.used = 0
        self.marker = selector[len("marker:"):] if selector.startswith("marker:") else None
        # "*" and "?" are the only wildcards: brackets are literal, as in
        # the names of parametrized tests (test_x[sse-c-...]).
        self.regex = re.compile("^" + re.escape(selector).replace(r"\*", ".*").replace(r"\?", ".") + "$")

    def matches(self, names, markers):
        """names: the test name and, for a parametrized test, its full name
        with the parameters (test_x[a-b])."""
        if self.marker is not None:
            return self.marker in markers
        return any(self.regex.match(n) for n in names)


def load_rules(path):
    """Parses excluded.txt: '<selector> | <category> | <reason>' per line,
    '#' comments."""
    rules = []
    with open(path, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            parts = [part.strip() for part in line.split("|", 2)]
            if len(parts) != 3 or not all(parts):
                raise ValueError("%s:%d: expected '<selector> | <category> | <reason>'" % (path, lineno))
            selector, category, reason = parts
            if category not in CATEGORIES:
                raise ValueError("%s:%d: unknown category %r (one of %s)" % (
                    path, lineno, category, ", ".join(sorted(CATEGORIES))))
            rules.append(Rule(selector, category, reason, lineno))
    return rules


class NexoCompat:
    """Session state: exclusions, groups and outcomes."""

    def __init__(self, config):
        self.config = config
        self.report_dir = config.getoption("nexo_report_dir")
        path = config.getoption("nexo_excluded")
        self.rules = load_rules(path) if path else []
        self.threshold = float(config.getoption("nexo_threshold"))
        self.check = config.getoption("nexo_check_excluded") or ""
        if self.check not in ("", "names", "all"):
            raise ValueError("--nexo-check-excluded: names or all, not %r" % self.check)
        self.tests = {}  # nodeid -> record
        self.started = time.time()

    # Collection: deselect the excluded tests (in check mode, keep the ones
    # to verify).
    def pytest_collection_modifyitems(self, session, config, items):
        keep, drop = [], []
        for item in items:
            name = getattr(item, "originalname", None) or item.name
            markers = {m.name for m in item.iter_markers()}
            rec = {"name": item.name, "file": os.path.basename(str(item.fspath)),
                   "group": group_of(name, str(item.fspath)), "outcome": None, "reason": "", "duration": 0.0}
            # The first matching rule gives the reason; every matching rule
            # counts as used, so that the report lists only stale ones.
            matching = [r for r in self.rules if r.matches((name, item.name), markers)]
            for r in matching:
                r.used += 1
            if matching:
                rule = matching[0]
                rec["outcome"], rec["category"], rec["reason"] = "excluded", rule.category, rule.reason
                rec["rule"] = rule.lineno
                if self.check == "all" or (self.check == "names" and rule.marker is None):
                    rec["check"] = None
                    keep.append(item)
                else:
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
        if rec["outcome"] == "excluded":
            # A test run to verify its exclusion: its outcome goes to
            # "check", the test stays excluded.
            if report.when == "call" and rec.get("check") != "error":
                rec["check"] = "skipped" if report.skipped else ("passed" if report.passed else "failed")
                if report.failed:
                    rec["check_reason"] = _short(report)
            elif report.failed:
                rec["check"], rec["check_reason"] = "error", _short(report)
            elif report.skipped and rec.get("check") is None:
                rec["check"] = "skipped"
            return
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
        doc = build_report(self.tests, self.rules, self.threshold, self.check, time.time() - self.started)
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


def build_report(tests, rules, threshold, check, duration):
    """The report document: totals, groups, target, exclusions, tests."""
    counters = ("collected", "excluded", "passed", "failed", "error", "skipped", "not-run")
    groups = {}
    for rec in tests.values():
        if rec["outcome"] is None:
            rec["outcome"] = "not-run"
        g = groups.setdefault(rec["group"], {k: 0 for k in counters})
        g["collected"] += 1
        g[rec["outcome"]] += 1
    for g in groups.values():
        g["applicable"] = g["passed"] + g["failed"] + g["error"]
        g["rate"] = round(g["passed"] / g["applicable"], 4) if g["applicable"] else None
    total = {k: sum(g[k] for g in groups.values()) for k in counters + ("applicable",)}
    total["rate"] = round(total["passed"] / total["applicable"], 4) if total["applicable"] else None
    below = sorted(name for name, g in groups.items() if g["rate"] is not None and g["rate"] < threshold)
    target = {
        "threshold": threshold,
        "groups": sorted(name for name, g in groups.items() if g["applicable"]),
        "below": below,
        "met": not below,
    }
    categories = {}
    for rec in tests.values():
        if rec["outcome"] == "excluded":
            categories[rec["category"]] = categories.get(rec["category"], 0) + 1
    unused = ["excluded.txt:%d %s" % (r.lineno, r.selector) for r in rules if r.used == 0]
    doc = {
        "suite": os.environ.get("NEXO_S3TESTS_SUITE", "ceph/s3-tests"),
        "commit": os.environ.get("NEXO_S3TESTS_COMMIT", ""),
        "mode": os.environ.get("NEXO_S3TESTS_MODE", ""),
        "duration_seconds": round(duration, 1),
        "total": total,
        "target": target,
        "groups": dict(sorted(groups.items())),
        "exclusions": {
            "categories": {c: {"tests": categories.get(c, 0), "meaning": CATEGORIES[c]} for c in CATEGORIES},
            "unused": unused,
        },
        "tests": dict(sorted(tests.items())),
    }
    if check:
        checked = [rec for rec in tests.values() if rec["outcome"] == "excluded" and "check" in rec]
        doc["check"] = {
            "scope": check,
            "run": len(checked),
            "passing": sorted("excluded.txt:%d %s" % (rec["rule"], rec["name"])
                              for rec in checked if rec["check"] == "passed"),
            "outcomes": {o: sum(1 for rec in checked if rec["check"] == o)
                         for o in ("passed", "failed", "error", "skipped", None)},
        }
        doc["check"]["outcomes"]["not-run"] = doc["check"]["outcomes"].pop(None)
    return doc


def render_text(doc):
    """Renders the report: header, per-group table, exclusions, per-test lines."""
    out = []
    t = doc["total"]
    out.append("Nexo s3-tests report: %s@%s, mode %s, %.0fs" % (
        doc["suite"], doc["commit"][:12], doc["mode"] or "?", doc["duration_seconds"]))
    out.append("Collected %d, excluded %d, applicable %d: passed %d, failed %d, errors %d (%s); skipped by the suite %d" % (
        t["collected"], t["excluded"], t["applicable"], t["passed"], t["failed"], t["error"], _pct(t["rate"]), t["skipped"]))
    tg = doc["target"]
    out.append("Target: every group with applicable tests (%d) at >= %d%%: %s" % (
        len(tg["groups"]), round(tg["threshold"] * 100),
        "MET" if tg["met"] else "NOT MET (" + ", ".join(tg["below"]) + ")"))
    out.append("")
    out.append("%-12s %9s %8s %10s %7s %7s %6s %8s %7s" % (
        "group", "collected", "excluded", "applicable", "passed", "failed", "error", "skipped", "rate"))
    for name, g in doc["groups"].items():
        out.append("%-12s %9d %8d %10d %7d %7d %6d %8d %7s" % (
            name, g["collected"], g["excluded"], g["applicable"], g["passed"], g["failed"], g["error"], g["skipped"],
            _pct(g["rate"])))
    out.append("")
    out.append("Excluded tests per category (test/compat/excluded.txt):")
    for name, c in doc["exclusions"]["categories"].items():
        out.append("  %-15s %4d  %s" % (name, c["tests"], c["meaning"]))
    if doc["exclusions"]["unused"]:
        out.append("Exclusions matching no collected test (markers of files not run, or stale): " +
                   ", ".join(doc["exclusions"]["unused"]))
    if "check" in doc:
        ck = doc["check"]
        out.append("Check of the exclusions (%s): %d excluded tests run: %s" % (
            ck["scope"], ck["run"], ", ".join("%s %d" % kv for kv in ck["outcomes"].items())))
        out.append("Excluded tests that pass (exclusion not justified): " + (", ".join(ck["passing"]) or "none"))
    out.append("")
    order = {"failed": 0, "error": 1, "passed": 2, "skipped": 3, "not-run": 4, "excluded": 5}
    for nodeid, rec in sorted(doc["tests"].items(), key=lambda kv: (order.get(kv[1]["outcome"], 9), kv[1]["group"], kv[0])):
        outcome = rec["outcome"].upper()
        if rec.get("check"):
            outcome += "(" + rec["check"] + ")"
        line = "%-8s %-12s %s" % (outcome, rec["group"], rec["name"])
        if rec["reason"]:
            line += "  -- "
            if rec.get("category"):
                line += "[" + rec["category"] + "] "
            line += rec["reason"]
        if rec.get("check_reason"):
            line += "  ## " + rec["check_reason"]
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
                    help="excluded.txt: tests deselected with their category and reason")
    group.addoption("--nexo-report-dir", dest="nexo_report_dir", default=None,
                    help="directory of report.json and report.txt")
    group.addoption("--nexo-threshold", dest="nexo_threshold", default=str(DEFAULT_THRESHOLD),
                    help="pass rate every group with applicable tests must reach (default 0.95)")
    group.addoption("--nexo-check-excluded", dest="nexo_check_excluded", default="",
                    help="run the excluded tests too and report the passing ones: names (exclusions by "
                         "name) or all (markers too)")


def pytest_configure(config):
    config.pluginmanager.register(NexoCompat(config), "nexo-compat-session")
