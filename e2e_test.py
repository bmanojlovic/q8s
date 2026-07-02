#!/usr/bin/env python3
# /// script
# dependencies = ["pexpect"]
# ///
"""q8s end-to-end test. Run with: uv run e2e_test.py"""

import os
import sys
import time
import subprocess
import signal
import tempfile
import pexpect

# ── config ────────────────────────────────────────────────────────────────────
BINARY   = "/tmp/q8s-e2e"
NS       = "e2e-test"
KUBE     = "/tmp/q8s-e2e.yaml"
XDG_RUN  = os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{os.getuid()}")
XDG_CFG  = os.environ.get("XDG_CONFIG_HOME", os.path.expanduser("~/.config"))
QUADLETS = f"{XDG_CFG}/containers/systemd"
CFGDIR   = f"{XDG_RUN}/q8s/configmaps"
SECDIR   = f"{XDG_RUN}/q8s/secrets"
SOCK     = f"{XDG_RUN}/q8s/api.sock"

env = {**os.environ, "KUBECONFIG": KUBE}

PASS = 0
FAIL = 0
server = None

# ── helpers ───────────────────────────────────────────────────────────────────
def run(cmd, timeout=15):
    """Run a command, return (stdout+stderr, returncode)."""
    r = pexpect.run(cmd, timeout=timeout, withexitstatus=True, env=env)
    return r[0].decode(errors="replace"), r[1]

def ok(desc):
    global PASS
    print(f"\033[32mPASS\033[0m {desc}")
    PASS += 1

def fail(desc, detail=""):
    global FAIL
    msg = f"\033[31mFAIL\033[0m {desc}"
    if detail:
        msg += f"\n     → {detail.strip()}"
    print(msg)
    FAIL += 1

def check(desc, cmd, expect=None, absent=None, rc=0):
    out, code = run(cmd)
    if code != rc:
        fail(desc, f"exit {code}: {out}")
        return out
    if expect and expect not in out:
        fail(desc, f"expected '{expect}' in: {out}")
        return out
    if absent and absent in out:
        fail(desc, f"unexpected '{absent}' in: {out}")
        return out
    ok(desc)
    return out

def file_contains(desc, path, needle):
    try:
        text = open(path).read()
        if needle in text:
            ok(desc)
        else:
            fail(desc, f"'{needle}' not in {path}")
    except FileNotFoundError:
        fail(desc, f"file not found: {path}")

def file_exists(desc, path):
    if os.path.exists(path):
        ok(desc)
    else:
        fail(desc, f"missing: {path}")

def file_absent(desc, path):
    if not os.path.exists(path):
        ok(desc)
    else:
        fail(desc, f"should not exist: {path}")

def wait_container(name, state="running", timeout=30):
    for _ in range(timeout):
        out, _ = run(f"podman inspect --format {{{{.State.Status}}}} {name}")
        if state == "removed":
            if "no such" in out.lower() or out.strip() == "":
                return True
        elif out.strip() == state:
            return True
        time.sleep(1)
    return False

def kube(subcmd, timeout=15):
    return run(f"kubectl -n {NS} {subcmd}", timeout=timeout)

def section(name):
    print(f"\n\033[33m── {name} ──\033[0m")

# ── startup ───────────────────────────────────────────────────────────────────
def start_server():
    global server
    # remove stale socket
    if os.path.exists(SOCK):
        os.remove(SOCK)
    # clear kubectl cache so stale OpenAPI schema isn't reused across runs
    cache_dir = os.path.expanduser("~/.kube/cache")
    if os.path.isdir(cache_dir):
        import shutil
        shutil.rmtree(cache_dir)
    # kill any lingering containers from a previous aborted run
    out, _ = run(f"podman ps -a --format '{{{{.Names}}}}' --filter 'name=^{NS}-'")
    for name in out.splitlines():
        name = name.strip()
        if name:
            run(f"podman rm -f {name}")
    # generate kubeconfig
    subprocess.run([BINARY, "kubeconfig"], stdout=open(KUBE, "w"), check=True)
    server = subprocess.Popen(
        [BINARY, "serve"],
        stdout=open("/tmp/q8s-e2e.log", "w"),
        stderr=subprocess.STDOUT,
    )
    # wait up to 10s
    for _ in range(10):
        time.sleep(1)
        out, rc = run("kubectl get namespaces")
        if rc == 0:
            ok("server started")
            return
    fail("server started", open("/tmp/q8s-e2e.log").read())
    raise SystemExit(1)

def stop_server():
    if server:
        server.send_signal(signal.SIGTERM)
        server.wait(timeout=5)

# ── tests ─────────────────────────────────────────────────────────────────────
def test_namespace():
    section("Namespace")
    check("create namespace",      f"kubectl create namespace {NS}")
    check("namespace in list",     "kubectl get namespaces", expect=NS)

def test_pod():
    section("Pod")
    check("create pod", f"kubectl -n {NS} run testpod --image=docker.io/library/busybox:latest -- sleep 3600")
    check("pod in API",            f"kubectl -n {NS} get pods", expect="testpod")

    q = f"{QUADLETS}/{NS}-testpod.container"
    file_exists("quadlet created", q)
    file_contains("quadlet has image",   q, "busybox")
    file_contains("quadlet has network", q, f"q8s-{NS}.network")

    if wait_container(f"{NS}-testpod", "running", 30):
        ok("container running in podman")
    else:
        fail("container running in podman", "timed out after 30s")

    check("pod logs reachable", f"kubectl -n {NS} logs testpod")
    check("patch pod annotation", f"kubectl -n {NS} patch pod testpod -p "
          '\'{"metadata":{"annotations":{"test":"value"}}}\'')
    check("annotation in API", f"kubectl -n {NS} get pod testpod -o jsonpath='{{.metadata.annotations.test}}'",
          expect="value")

    check("delete pod", f"kubectl -n {NS} delete pod testpod")
    if wait_container(f"{NS}-testpod", "removed", 20):
        ok("container removed from podman")
    else:
        fail("container removed from podman", "timed out")

def test_configmap():
    section("ConfigMap")
    check("create configmap",
          f"kubectl -n {NS} create configmap myconfig --from-literal=key1=hello --from-literal=key2=world")
    file_exists("configmap file created", f"{CFGDIR}/{NS}/myconfig/key1")
    file_contains("configmap file value", f"{CFGDIR}/{NS}/myconfig/key1", "hello")

    check("patch configmap",
          f"kubectl -n {NS} patch configmap myconfig -p " + '\'{"data":{"key1":"updated"}}\'')
    file_contains("configmap file updated", f"{CFGDIR}/{NS}/myconfig/key1", "updated")

    check("delete configmap", f"kubectl -n {NS} delete configmap myconfig")
    file_absent("configmap dir removed", f"{CFGDIR}/{NS}/myconfig")

def test_secret():
    section("Secret")
    check("create secret",
          f"kubectl -n {NS} create secret generic mysecret --from-literal=password=s3cr3t")
    file_exists("secret file created", f"{SECDIR}/{NS}/mysecret/password")
    file_contains("secret file value",   f"{SECDIR}/{NS}/mysecret/password", "s3cr3t")

    check("patch secret",
          f"kubectl -n {NS} patch secret mysecret -p " + '\'{"stringData":{"password":"newpass"}}\'')
    file_contains("secret file updated", f"{SECDIR}/{NS}/mysecret/password", "newpass")

    check("delete secret", f"kubectl -n {NS} delete secret mysecret")
    file_absent("secret dir removed", f"{SECDIR}/{NS}/mysecret")

def test_deployment():
    section("Deployment")
    check("create deployment",
          f"kubectl -n {NS} create deployment myapp --image=docker.io/library/busybox:latest --replicas=2 -- sleep 3600")
    check("deployment in API", f"kubectl -n {NS} get deployments", expect="myapp")

    file_exists("instance-0 quadlet", f"{QUADLETS}/{NS}-myapp-0.container")
    file_exists("instance-1 quadlet", f"{QUADLETS}/{NS}-myapp-1.container")

    started = (wait_container(f"{NS}-myapp-0", "running", 30) and
               wait_container(f"{NS}-myapp-1", "running", 30))
    if started:
        ok("both containers running")
    else:
        fail("both containers running", "timed out")

    check("set env FOO=bar", f"kubectl -n {NS} set env deployment/myapp FOO=bar")
    file_contains("env in quadlet after set env",
                  f"{QUADLETS}/{NS}-myapp-0.container", "FOO=bar")
    check("env in API",
          f"kubectl -n {NS} get deployment myapp -o jsonpath='{{.spec.template.spec.containers[0].env[0].value}}'",
          expect="bar")
    # poll until the restarted container shows the new env var (daemon-reload + restart takes time)
    propagated = False
    for _ in range(30):
        time.sleep(1)
        out, _ = run(f"podman inspect --format '{{{{range .Config.Env}}}}{{{{.}}}} {{{{end}}}}' {NS}-myapp-0")
        if "FOO=bar" in out:
            propagated = True
            break
    if propagated:
        ok("env var propagated to running container in podman")
    else:
        fail("env var propagated to running container in podman", f"podman env: {out.strip()}")

    check("scale up to 3", f"kubectl -n {NS} scale deployment myapp --replicas=3")
    file_exists("instance-2 quadlet created", f"{QUADLETS}/{NS}-myapp-2.container")
    if wait_container(f"{NS}-myapp-2", "running", 20):
        ok("scaled-up container running")
    else:
        fail("scaled-up container running", "timed out")

    check("scale down to 1", f"kubectl -n {NS} scale deployment myapp --replicas=1")
    time.sleep(2)
    file_absent("instance-1 quadlet removed", f"{QUADLETS}/{NS}-myapp-1.container")
    file_absent("instance-2 quadlet removed", f"{QUADLETS}/{NS}-myapp-2.container")

    check("rollout restart", f"kubectl -n {NS} rollout restart deployment myapp")
    time.sleep(3)
    check("restartedAt annotation set",
          f"kubectl -n {NS} get deployment myapp -o jsonpath='{{.spec.template.metadata.annotations}}'",
          expect="restartedAt")

    check("delete deployment", f"kubectl -n {NS} delete deployment myapp")
    time.sleep(2)
    file_absent("all quadlets removed", f"{QUADLETS}/{NS}-myapp-0.container")

def test_patch_and_edit():
    section("kubectl patch / apply / edit workflows")

    # --- patch pod env via strategic merge patch (default kubectl patch type) ---
    check("create pod for patch test",
          f"kubectl -n {NS} run patchpod --image=docker.io/library/busybox:latest -- sleep 3600")
    check("patch pod env via strategic merge",
          f"""kubectl -n {NS} patch pod patchpod -p '{{"spec":{{"containers":[{{"name":"patchpod","env":[{{"name":"MY_VAR","value":"hello"}}]}}]}}}}'""")
    check("env visible in API after patch",
          f"kubectl -n {NS} get pod patchpod -o jsonpath='{{.spec.containers[0].env[0].value}}'",
          expect="hello")
    wait_container(f"{NS}-patchpod", "running", 30)
    out, _ = run(f"podman inspect --format '{{{{range .Config.Env}}}}{{{{.}}}} {{{{end}}}}' {NS}-patchpod")
    if "MY_VAR=hello" in out:
        ok("env var propagated to pod container in podman")
    else:
        fail("env var propagated to pod container in podman", f"podman env: {out.strip()}")

    # patch again — add second var, keep first (tests array merge-by-name)
    check("patch second env var",
          f"""kubectl -n {NS} patch pod patchpod -p '{{"spec":{{"containers":[{{"name":"patchpod","env":[{{"name":"SECOND","value":"world"}}]}}]}}}}'""")
    out, _ = run(f"kubectl -n {NS} get pod patchpod -o jsonpath='{{.spec.containers[0].env}}'")
    if "hello" in out and "world" in out:
        ok("both env vars present after second patch (array merge by name)")
    else:
        fail("both env vars present after second patch", f"got: {out}")

    check("delete patch-test pod", f"kubectl -n {NS} delete pod patchpod")

    # --- kubectl edit via non-interactive editor (EDITOR=sed script) ---
    import tempfile, stat
    sed_script = tempfile.NamedTemporaryFile(mode="w", suffix=".sh", delete=False)
    sed_script.write("#!/bin/sh\nsed -i 's/replicas: 1/replicas: 2/' \"$1\"\n")
    sed_script.close()
    os.chmod(sed_script.name, 0o755)

    check("create deployment for edit test",
          f"kubectl -n {NS} create deployment editapp --image=docker.io/library/busybox:latest -- sleep 3600")

    edit_env = {**env, "KUBE_EDITOR": sed_script.name}
    out, rc = pexpect.run(
        f"kubectl -n {NS} edit deployment editapp",
        timeout=15, withexitstatus=True, env=edit_env,
    )
    out = out.decode(errors="replace")
    if rc == 0 or "edited" in out:
        ok("kubectl edit deployment succeeded")
    else:
        fail("kubectl edit deployment", out)

    check("replicas updated via kubectl edit",
          f"kubectl -n {NS} get deployment editapp -o jsonpath='{{.spec.replicas}}'",
          expect="2")
    if wait_container(f"{NS}-editapp-1", "running", 30):
        ok("second replica container running in podman after edit")
    else:
        fail("second replica container running in podman after edit", "timed out")
    os.unlink(sed_script.name)
    check("delete edit-test deployment", f"kubectl -n {NS} delete deployment editapp")

    # --- secret: create, read back data, patch data ---
    check("create secret with multiple keys",
          f"kubectl -n {NS} create secret generic creds --from-literal=user=admin --from-literal=pass=secret")
    file_contains("secret user file",     f"{SECDIR}/{NS}/creds/user", "admin")
    file_contains("secret pass file",     f"{SECDIR}/{NS}/creds/pass", "secret")
    check("secret data in API",
          f"kubectl -n {NS} get secret creds -o jsonpath='{{.data.user}}'",
          expect="YWRtaW4=")   # base64("admin")
    check("patch secret new password",
          f"kubectl -n {NS} patch secret creds -p " + '\'{"stringData":{"pass":"n3wpass"}}\'')
    file_contains("secret file updated after patch", f"{SECDIR}/{NS}/creds/pass", "n3wpass")
    check("delete secret", f"kubectl -n {NS} delete secret creds")
    file_absent("secret dir cleaned up", f"{SECDIR}/{NS}/creds")

    # --- kubectl apply (create then update) ---
    import tempfile as tf
    manifest = tf.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False)
    manifest.write(f"""apiVersion: v1
kind: ConfigMap
metadata:
  name: applied-cm
  namespace: {NS}
data:
  colour: blue
""")
    manifest.close()
    check("kubectl apply create",  f"kubectl apply -f {manifest.name}", expect="created")
    file_contains("applied configmap file", f"{CFGDIR}/{NS}/applied-cm/colour", "blue")

    # update manifest and apply again
    open(manifest.name, "w").write(f"""apiVersion: v1
kind: ConfigMap
metadata:
  name: applied-cm
  namespace: {NS}
data:
  colour: red
""")
    check("kubectl apply update",  f"kubectl apply -f {manifest.name}", expect="configured")
    file_contains("configmap updated via apply", f"{CFGDIR}/{NS}/applied-cm/colour", "red")
    os.unlink(manifest.name)
    check("delete applied configmap", f"kubectl -n {NS} delete configmap applied-cm")


def test_job():
    section("Job")
    check("create job",
          f"kubectl -n {NS} create job testjob --image=docker.io/library/busybox:latest -- echo done")
    file_exists("job quadlet created",  f"{QUADLETS}/{NS}-testjob-job.container")
    file_contains("job has Restart=no", f"{QUADLETS}/{NS}-testjob-job.container", "Restart=no")
    check("delete job", f"kubectl -n {NS} delete job testjob")

def test_cronjob():
    section("CronJob")
    check("create cronjob",
          f"kubectl -n {NS} create cronjob testcron --image=docker.io/library/busybox:latest "
          f"--schedule='*/5 * * * *' -- echo tick")
    TIMER_DIR = f"{XDG_CFG}/systemd/user"
    file_exists("cronjob container quadlet", f"{QUADLETS}/{NS}-testcron-cron.container")
    file_exists("cronjob timer file",        f"{TIMER_DIR}/{NS}-testcron-cron.timer")
    file_contains("timer has OnCalendar",    f"{TIMER_DIR}/{NS}-testcron-cron.timer", "OnCalendar")
    check("delete cronjob", f"kubectl -n {NS} delete cronjob testcron")

def port_open(host="localhost", port=6443, timeout=2):
    import socket as _sock
    try:
        s = _sock.create_connection((host, port), timeout=timeout)
        s.close()
        return True
    except OSError:
        return False

def test_socket_management():
    section("Socket management (start / stop / enable / disable / status)")

    socket_unit = f"{XDG_CFG}/systemd/user/q8s.socket"
    if not os.path.exists(socket_unit):
        print(f"  \033[33mSKIP\033[0m socket unit not installed (run: {BINARY} install)")
        return

    # ensure clean slate
    run(f"{BINARY} stop")
    time.sleep(1)

    # status must exit non-zero when socket is inactive
    _, rc = run(f"{BINARY} status")
    if rc != 0:
        ok("status exits non-zero when socket inactive")
    else:
        fail("status exits non-zero when socket inactive", "expected non-zero exit")

    # start
    check("q8s start", f"{BINARY} start", expect="started")
    time.sleep(1)

    if port_open():
        ok("port 6443 listening after start")
    else:
        fail("port 6443 listening after start", "connection refused")

    out, rc = run(f"{BINARY} status")
    if rc == 0 and "active" in out and "reachable" in out:
        ok("status shows socket active and port reachable")
    else:
        fail("status shows socket active and port reachable", f"rc={rc}\n{out}")

    # stop
    check("q8s stop", f"{BINARY} stop", expect="stopped")
    time.sleep(1)

    if not port_open():
        ok("port 6443 not listening after stop")
    else:
        fail("port 6443 not listening after stop", "connection still succeeded")

    _, rc = run(f"{BINARY} status")
    if rc != 0:
        ok("status exits non-zero after stop")
    else:
        fail("status exits non-zero after stop", "expected non-zero exit")

    # enable
    check("q8s enable", f"{BINARY} enable", expect="enabled")
    time.sleep(1)

    out, _ = run("systemctl --user is-enabled q8s.socket")
    if "enabled" in out:
        ok("socket is-enabled after q8s enable")
    else:
        fail("socket is-enabled after q8s enable", out)

    if port_open():
        ok("port 6443 listening after enable")
    else:
        fail("port 6443 listening after enable", "connection refused")

    # disable
    check("q8s disable", f"{BINARY} disable", expect="disabled")
    time.sleep(1)

    out, _ = run("systemctl --user is-enabled q8s.socket")
    if "disabled" in out or "static" in out:
        ok("socket is-disabled after q8s disable")
    else:
        fail("socket is-disabled after q8s disable", out)

    if not port_open():
        ok("port 6443 not listening after disable")
    else:
        fail("port 6443 not listening after disable", "connection still succeeded")

def cleanup():
    section("Cleanup")
    for cmd in [
        f"kubectl -n {NS} delete deployment --all --ignore-not-found",
        f"kubectl -n {NS} delete pod --all --ignore-not-found",
        f"kubectl -n {NS} delete job --all --ignore-not-found",
        f"kubectl -n {NS} delete cronjob --all --ignore-not-found",
        f"kubectl -n {NS} delete configmap --all --ignore-not-found",
        f"kubectl -n {NS} delete secret --all --ignore-not-found",
        f"kubectl delete namespace {NS} --ignore-not-found",
    ]:
        run(cmd)
    stop_server()

# ── main ──────────────────────────────────────────────────────────────────────
if __name__ == "__main__":
    print("Building binary...")
    r = subprocess.run(["go", "build", "-o", BINARY, "./cmd/q8s"], capture_output=True, text=True)
    if r.returncode != 0:
        print(f"Build failed:\n{r.stderr}")
        sys.exit(1)

    try:
        start_server()
        test_namespace()
        test_pod()
        test_configmap()
        test_secret()
        test_deployment()
        test_patch_and_edit()
        test_job()
        test_cronjob()
    finally:
        cleanup()
    test_socket_management()
    print(f"\n{'━'*40}")
    print(f"  \033[32m{PASS} passed\033[0m   \033[31m{FAIL} failed\033[0m")
    print(f"{'━'*40}")
    print(f"Server log: /tmp/q8s-e2e.log")
    sys.exit(0 if FAIL == 0 else 1)
