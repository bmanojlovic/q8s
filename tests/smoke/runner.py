#!/usr/bin/env python3
"""q8s smoke test runner — reads YAML test definitions and executes them.

Reuses the k6-8s smoke_runner.py format adapted for q8s.
Run: ./tests/smoke/runner.py [file.yaml ...]
"""
import json
import os
import subprocess
import sys
import time

import yaml

CONTEXT = os.environ.get("Q8S_CONTEXT", "q8s")
K = f"kubectl --context={CONTEXT}"
PASS = FAIL = SKIP = 0
FAILURES = []
GREEN = "\033[0;32m"
RED = "\033[0;31m"
YELLOW = "\033[0;33m"
NC = "\033[0m"


def kubectl(args, stdin=None, check=True):
    """Run kubectl and return (returncode, stdout, stderr)."""
    cmd = f"{K} {args}"
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True, input=stdin)
    if check and r.returncode != 0:
        raise RuntimeError(r.stderr.strip() or r.stdout.strip())
    return r.returncode, r.stdout.strip(), r.stderr.strip()


def curl(path, method="GET", data=None):
    """Direct API call via curl with q8s certs."""
    certs_dir = os.path.expanduser("~/.local/share/q8s/certs")
    cmd = [
        "curl", "-sk",
        f"https://localhost:6443{path}",
        "-X", method,
        "--cert", f"{certs_dir}/client.crt",
        "--key", f"{certs_dir}/client.key",
        "--cacert", f"{certs_dir}/ca.crt",
    ]
    if data:
        cmd += ["-H", "Content-Type: application/json", "-d", json.dumps(data)]
    cmd += ["-w", "\n%{http_code}"]
    r = subprocess.run(cmd, capture_output=True, text=True)
    lines = r.stdout.strip().rsplit("\n", 1)
    body = lines[0] if len(lines) > 1 else ""
    code = int(lines[-1]) if lines[-1].isdigit() else 0
    return code, body


def report(name, passed, detail=""):
    global PASS, FAIL
    tag = f"{GREEN}PASS{NC}" if passed else f"{RED}FAIL{NC}"
    suffix = f" ({detail})" if detail else ""
    print(f"  {name:<55} {tag}{suffix}")
    if passed:
        PASS += 1
    else:
        FAIL += 1
        FAILURES.append(f"  {name}: {detail}")


def skip(name, reason):
    global SKIP
    print(f"  {name:<55} {YELLOW}SKIP{NC} ({reason})")
    SKIP += 1


# --- Test executors ---

def run_kubectl(test):
    """Run a kubectl command, check exit code and optional assertions."""
    args = test["kubectl"]
    stdin = None
    if "stdin" in test:
        stdin = test["stdin"]
        if isinstance(stdin, dict):
            stdin = json.dumps(stdin)
    try:
        rc, out, err = kubectl(args, stdin=stdin, check=False)
        ok = True

        if test.get("expect") == "fail":
            ok = rc != 0
        elif test.get("expect") == "success":
            ok = rc == 0
        else:
            ok = rc == 0

        if ok and "contains" in test:
            ok = test["contains"] in out
            if not ok:
                return report(test["name"], False, f"missing '{test['contains']}' in output")

        if ok and "not_contains" in test:
            ok = test["not_contains"] not in out
            if not ok:
                return report(test["name"], False, f"unexpected '{test['not_contains']}' in output")

        report(test["name"], ok, err if not ok else "")
    except Exception as e:
        report(test["name"], False, str(e))


def run_apply(test):
    """Apply a manifest and check result."""
    manifest = test["apply"]
    if isinstance(manifest, dict):
        manifest = yaml.dump(manifest)
    try:
        rc, out, err = kubectl("apply -f -", stdin=manifest, check=False)
        if test.get("expect") == "fail":
            report(test["name"], rc != 0, err if rc == 0 else "")
        else:
            report(test["name"], rc == 0, err if rc != 0 else "")
    except Exception as e:
        report(test["name"], False, str(e))


def run_curl(test):
    """Direct API call."""
    cfg = test["curl"]
    code, body = curl(cfg["path"], cfg.get("method", "GET"), cfg.get("data"))
    expected_code = cfg.get("status")
    ok = True
    detail = f"HTTP {code}"

    if expected_code:
        ok = code == expected_code
    elif test.get("expect") == "fail":
        ok = code >= 400
    else:
        ok = code < 400

    if ok and "contains" in cfg:
        ok = cfg["contains"] in body
        if not ok:
            detail = f"missing '{cfg['contains']}' in response"

    report(test["name"], ok, detail)


def run_wait(test):
    """Wait for a condition."""
    timeout = test.get("timeout", 30)
    interval = test.get("interval", 2)
    elapsed = 0
    while elapsed < timeout:
        rc, out, _ = kubectl(test["wait"], check=False)
        if rc == 0:
            if "contains" in test:
                if test["contains"] in out:
                    return report(test["name"], True, f"{elapsed}s")
            elif "not_contains" in test:
                if test["not_contains"] not in out:
                    return report(test["name"], True, f"{elapsed}s")
            else:
                return report(test["name"], True, f"{elapsed}s")
        elif "not_contains" in test:
            # Command failed (e.g. "No resources found") — counts as not containing
            if test["not_contains"] not in (out + _):
                return report(test["name"], True, f"{elapsed}s")
        time.sleep(interval)
        elapsed += interval
    report(test["name"], False, f"timeout after {timeout}s")


def run_sleep(test):
    """Just sleep."""
    time.sleep(test.get("sleep", 1))


def run_setup(test):
    """Setup step."""
    if "shell" in test:
        r = subprocess.run(test["shell"], shell=True, capture_output=True, text=True)
        if r.returncode != 0 and not test.get("ignore_errors"):
            print(f"  SETUP FAILED: {test.get('name', 'unnamed')}: {r.stderr}")
            sys.exit(1)
    if "kubectl" in test:
        kubectl(test["kubectl"], check=not test.get("ignore_errors", False))


def run_cleanup(test):
    """Cleanup step — ignore errors."""
    if "kubectl" in test:
        kubectl(test["kubectl"], check=False)
    if "shell" in test:
        subprocess.run(test["shell"], shell=True, capture_output=True)


def execute_test(test):
    """Dispatch a single test."""
    if "skip" in test:
        return skip(test["name"], test["skip"])
    if "setup" in test or test.get("type") == "setup":
        return run_setup(test.get("setup", test))
    if "cleanup" in test or test.get("type") == "cleanup":
        return run_cleanup(test.get("cleanup", test))
    if "sleep" in test:
        return run_sleep(test)
    if "apply" in test:
        return run_apply(test)
    if "curl" in test:
        return run_curl(test)
    if "wait" in test:
        return run_wait(test)
    if "kubectl" in test:
        return run_kubectl(test)
    print(f"  Unknown test type: {test}")


def run_suite(suite):
    """Run a test suite."""
    print(f"\n--- {suite['name']} ---")
    for test in suite.get("tests", []):
        execute_test(test)


def main():
    test_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)))

    if len(sys.argv) > 1:
        files = sys.argv[1:]
        test_dir = "."
    else:
        files = sorted(f for f in os.listdir(test_dir) if f.endswith((".yaml", ".yml")))
        if not files:
            print(f"No test files in {test_dir}")
            sys.exit(1)

    print("=" * 50)
    print(" q8s Smoke Tests")
    print("=" * 50)
    print(f" Context: {CONTEXT}")

    for f in files:
        path = os.path.join(test_dir, f) if test_dir != "." else f
        with open(path) as fh:
            doc = yaml.safe_load(fh)
        print(f"\n{'─' * 50}")
        print(f" {doc.get('name', f)}")
        print(f"{'─' * 50}")

        for step in doc.get("setup", []):
            run_setup(step)
        if "suites" in doc:
            for suite in doc["suites"]:
                run_suite(suite)
        elif "tests" in doc:
            run_suite({"name": doc.get("name", f), "tests": doc["tests"]})
        for step in doc.get("cleanup", []):
            run_cleanup(step)

    print(f"\n{'=' * 50}")
    print(f" {GREEN}PASS: {PASS}{NC}  {RED}FAIL: {FAIL}{NC}  {YELLOW}SKIP: {SKIP}{NC}  TOTAL: {PASS+FAIL+SKIP}")
    print("=" * 50)
    if FAILURES:
        print("\nFailures:")
        for f in FAILURES:
            print(f)
    sys.exit(1 if FAIL > 0 else 0)


if __name__ == "__main__":
    main()
