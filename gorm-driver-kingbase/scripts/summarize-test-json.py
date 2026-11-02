#!/usr/bin/env python3
"""Render Go's JSON test stream and count actual top-level test outcomes."""

import json
import os
from pathlib import Path
import sys

outcomes = {}
package_failures = set()
subtest_skips = []
for line in sys.stdin:
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        sys.stderr.write("invalid go test JSON stream: " + line)
        sys.exit(2)
    if "Output" in event:
        sys.stdout.write(event["Output"])
    action, name, package = event.get("Action"), event.get("Test"), event.get("Package")
    if action == "fail" and not name:
        package_failures.add(package)
    if name and action in ("pass", "fail", "skip"):
        if "/" not in name:
            key = package + "/" + name
            if key in outcomes:
                sys.stderr.write("duplicate test result; use -count=1 for an auditable run: " + key + "\n")
                sys.exit(2)
            outcomes[key] = action
        elif action == "skip":
            subtest_skips.append(package + "/" + name)

counts = {action: sum(value == action for value in outcomes.values()) for action in ("pass", "fail", "skip")}
summary = {"top_level": counts, "outcomes": outcomes, "subtest_skips": subtest_skips, "failed_packages": sorted(package_failures)}
if os.environ.get("KINGBASE_TEST_SUMMARY"):
    destination = Path(os.environ["KINGBASE_TEST_SUMMARY"])
    destination.write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
print("Verified test outcomes: pass={pass} fail={fail} skip={skip}; skipped subtests={subtests}".format(**counts, subtests=len(subtest_skips)))
if not outcomes:
    sys.stderr.write("No tests executed; check -run instead of treating this run as PASS.\n")
    sys.exit(2)
sys.exit(1 if package_failures or counts["fail"] else 0)
