"""Report of a run-mint.sh run (docs/06 §6.1, T-9.1).

    python3 mint_report.py REPORT_DIR EXCLUDED MODE COMMIT SUITE...

Reads REPORT_DIR/<suite>/log.json (one JSON entry per test: name,
function, status PASS/FAIL/NA, error...), REPORT_DIR/<suite>/status and
the expected failures of EXCLUDED ("<suite> | <glob> | <category> |
<reason>", the categories of excluded.txt; the glob matches the test name
or "<function> <args>"), and writes report.json and
report.txt: per suite the tests passed, failed (expected or not) and not
applicable (NA: the suite itself skipped them, e.g. a MinIO-only API).
"""

import fnmatch
import json
import os
import re
import sys

CATEGORIES = ("rgw", "deprecated", "p2", "out-of-catalog", "aws", "design", "suite", "minio")

# A field of an entry that is not valid JSON (see salvage).
FIELD = re.compile(r'"(name|function|status)":\s*"((?:[^"\\]|\\.)*)"')


def load_excluded(path):
    rules = []
    if not os.path.exists(path):
        return rules
    with open(path, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            parts = [p.strip() for p in line.split("|", 3)]
            if len(parts) != 4 or not all(parts):
                raise ValueError("%s:%d: expected '<suite> | <function> | <category> | <reason>'" % (path, lineno))
            if parts[2] not in CATEGORIES:
                raise ValueError("%s:%d: unknown category %r" % (path, lineno, parts[2]))
            rules.append({"suite": parts[0], "function": parts[1], "category": parts[2], "reason": parts[3],
                          "line": lineno, "used": 0})
    return rules


def entries(path):
    """The JSON entries of a mint log (other lines are ignored)."""
    out = []
    if not os.path.exists(path):
        return out
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                out.append(json.loads(line))
            except ValueError:
                e = salvage(line)
                if e:
                    out.append(e)
    return out


def salvage(line):
    """The fields of an entry that is not valid JSON: the shell suites
    (awscli) write the output of a failed command into "error" without
    escaping its quotes. None without a status."""
    e = {}
    for m in FIELD.finditer(line):
        k, v = m.group(1), m.group(2)
        if k not in e:
            try:
                e[k] = json.loads('"%s"' % v)
            except ValueError:
                e[k] = v
    if "status" not in e:
        return None
    i = line.find('"error":')
    if i >= 0:
        e["error"] = line[i + len('"error":'):].strip().strip('}').strip().strip('"')
    return e


def test_name(e):
    """The test of an entry: the name after "<suite>:" for the suites that
    name their tests (minio-go, minio-py), else the function without its
    arguments (one entry per API call or command)."""
    name = str(e.get("name", "") or "")
    if ":" in name:
        test = name.split(":", 1)[1].strip()
        if test:
            return test
    fn = str(e.get("function", "") or name)
    return fn.split("(", 1)[0].strip() or "?"


def subjects(e):
    """What a rule's glob is matched against: the test name, and the raw
    function with its arguments (to tell apart the entries of a suite that
    reports only the API call, such as minio-js)."""
    args = e.get("args")
    if isinstance(args, (dict, list)):
        args = json.dumps(args, sort_keys=True)
    return [test_name(e), ("%s %s" % (e.get("function", "") or "", args or "")).strip()]


def main(argv):
    report_dir, excluded_path, mode, commit = argv[1:5]
    suites = argv[5:]
    rules = load_excluded(excluded_path)
    doc = {"suite": "minio/mint", "commit": commit, "mode": mode, "suites": {}, "unexpected_failures": 0, "not_built": 0}
    for s in suites:
        status = ""
        try:
            with open(os.path.join(report_dir, s, "status"), encoding="utf-8") as f:
                status = f.read().strip()
        except OSError:
            pass
        rec = {"status": status, "passed": 0, "failed": 0, "expected": 0, "na": 0, "failures": [], "expected_failures": []}
        if status == "not built":
            doc["not_built"] += 1
        for e in entries(os.path.join(report_dir, s, "log.json")):
            st = str(e.get("status", "")).upper()
            name = test_name(e)
            if st == "PASS":
                rec["passed"] += 1
            elif st == "NA":
                rec["na"] += 1
            elif st == "FAIL":
                subj = subjects(e)
                rule = next((r for r in rules if r["suite"] == s and any(fnmatch.fnmatchcase(x, r["function"]) for x in subj)), None)
                detail = {"function": name, "error": str(e.get("error") or e.get("message") or "")[:300]}
                if rule:
                    rule["used"] += 1
                    rec["expected"] += 1
                    detail.update(category=rule["category"], reason=rule["reason"])
                    rec["expected_failures"].append(detail)
                else:
                    rec["failed"] += 1
                    rec["failures"].append(detail)
        # A suite that exits with an error after its last entry (crash,
        # timeout) has an unaccounted failure.
        if status.startswith("exit ") and not status.startswith("exit 0") and rec["failed"] == 0 and rec["expected"] == 0:
            rec["failed"] += 1
            rec["failures"].append({"function": "(suite)", "error": status + ": see %s/error.log" % s})
        applicable = rec["passed"] + rec["failed"]
        rec["rate"] = round(rec["passed"] / applicable, 4) if applicable else None
        doc["unexpected_failures"] += rec["failed"]
        doc["suites"][s] = rec
    doc["unused_exclusions"] = ["mint-excluded.txt:%d %s %s" % (r["line"], r["suite"], r["function"]) for r in rules if not r["used"] and r["suite"] in suites]
    with open(os.path.join(report_dir, "report.json"), "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
        f.write("\n")
    out = ["Nexo mint report: %s@%s, mode %s" % (doc["suite"], commit[:12], mode), ""]
    out.append("%-16s %7s %7s %9s %5s %7s  %s" % ("suite", "passed", "failed", "expected", "NA", "rate", "status"))
    tot = {"passed": 0, "failed": 0, "expected": 0, "na": 0}
    for s, rec in doc["suites"].items():
        for k in tot:
            tot[k] += rec[k]
        rate = "n/a" if rec["rate"] is None else "%.1f%%" % (rec["rate"] * 100)
        out.append("%-16s %7d %7d %9d %5d %7s  %s" % (s, rec["passed"], rec["failed"], rec["expected"], rec["na"], rate, rec["status"]))
    applicable = tot["passed"] + tot["failed"]
    out.append("%-16s %7d %7d %9d %5d %7s" % ("total", tot["passed"], tot["failed"], tot["expected"], tot["na"],
                                              "%.1f%%" % (100.0 * tot["passed"] / applicable) if applicable else "n/a"))
    out.append("")
    for s, rec in doc["suites"].items():
        for d in rec["failures"]:
            out.append("FAILED   %-16s %s  -- %s" % (s, d["function"], d["error"]))
    for s, rec in doc["suites"].items():
        for d in rec["expected_failures"]:
            out.append("EXPECTED %-16s %s  -- [%s] %s" % (s, d["function"], d["category"], d["reason"]))
    if doc["unused_exclusions"]:
        out.append("")
        out.append("Expected failures that did not occur: " + ", ".join(doc["unused_exclusions"]))
    with open(os.path.join(report_dir, "report.txt"), "w", encoding="utf-8") as f:
        f.write("\n".join(out) + "\n")


if __name__ == "__main__":
    main(sys.argv)
