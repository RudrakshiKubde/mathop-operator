#!/usr/bin/env python3
"""
Live terminal watcher for a Workflow's status, using the operator's SSE
stream endpoint — the same one the browser dashboard uses.

Usage:
    python3 watch_workflow.py <transactionID>
"""

import sys
import json
import urllib.request


def render(status):
    phase = status.get("phase", "Unknown")
    print("\n" + "=" * 50)
    if phase == "Succeeded":
        print("WORKFLOW COMPLETE - ALL TASKS SUCCEEDED")
    elif phase == "Failed":
        print("WORKFLOW FAILED")
    else:
        print(f"WORKFLOW STATUS: {phase}")
    print("=" * 50)

    markers = {"Succeeded": "OK  ", "Failed": "FAIL", "Running": "RUN ", "Pending": "WAIT"}
    for t in status.get("tasks", []):
        name = t.get("name", "?")
        tphase = t.get("phase", "?")
        line = f"[{markers.get(tphase, '... ')}] {name}: {tphase}"
        if t.get("error"):
            line += f"  -> {t['error']}"
        print(line)
    print("=" * 50)


def main():
    if len(sys.argv) != 2:
        print("usage: python3 watch_workflow.py <transactionID>")
        sys.exit(1)

    tx_id = sys.argv[1]
    url = f"http://localhost:8090/api/workflows/stream/{tx_id}"
    print(f"Watching {url} ...")

    with urllib.request.urlopen(url) as resp:
        for raw_line in resp:
            line = raw_line.decode("utf-8").strip()
            if not line.startswith("data:"):
                continue
            payload = line[len("data:"):].strip()
            try:
                status = json.loads(payload)
            except json.JSONDecodeError:
                continue
            render(status)

    print("\nStream closed (workflow reached a terminal phase).")


if __name__ == "__main__":
    main()