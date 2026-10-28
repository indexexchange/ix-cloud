#!/usr/bin/env python3
"""Validate an agent manifest against the built container image.

Checks that the manifest file (manifest/manifest.json) accurately describes the
image: that the image carries the manifest as the required labels, and that the
endpoints, metrics, and health contracts the manifest DECLARES are actually what
a running container PROVIDES.

Only manifest/manifest.json (the public agent-manifest) is read. manifest/
global-config.json is internal-only (partner, infra, and model-delivery config);
it is never read, validated, or shipped in the image.

Usage:
    python3 scripts/validate-manifest.py [IMAGE] [options]

    IMAGE              image ref to validate (default: rtd-simple:local)
    --manifest PATH    manifest file (default: manifest/manifest.json)
    --models PATH      model dir to mount read-only at /models (default: models)
    --static-only      skip running the container (manifest + image inspect only)
    --keep-container   do not remove the container on exit (for debugging)

Exit code is 0 when every check passes, 1 if any check FAILS. WARN and SKIP do
not fail the run. Needs: docker, python3. Uses grpcurl if present for the gRPC
endpoint checks; without it those checks are skipped (install: brew install
grpcurl).
"""

import argparse
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

# ---- result tracking --------------------------------------------------------

PASS, FAIL, WARN, SKIP = "PASS", "FAIL", "WARN", "SKIP"
_counts = {PASS: 0, FAIL: 0, WARN: 0, SKIP: 0}
_tty = sys.stdout.isatty()
_COLOR = {PASS: "32", FAIL: "31", WARN: "33", SKIP: "90"}


def record(status, msg, detail=""):
    _counts[status] += 1
    tag = f"\033[{_COLOR[status]}m{status:4}\033[0m" if _tty else f"{status:4}"
    print(f"  [{tag}] {msg}")
    if detail:
        for line in str(detail).splitlines():
            print(f"         {line}")


def section(title):
    print(f"\n== {title} ==")


# ---- shell helpers ----------------------------------------------------------

def run(cmd, timeout=30):
    """Run a command, return (returncode, stdout, stderr)."""
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return p.returncode, p.stdout, p.stderr
    except subprocess.TimeoutExpired:
        return 124, "", f"timeout after {timeout}s"
    except FileNotFoundError:
        return 127, "", f"{cmd[0]}: not found"


def http_get(url, timeout=3):
    """GET a URL, return (status, body) or (None, error-string)."""
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return None, str(e)


def tcp_open(host, port, timeout=2):
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


def resolve_grpcurl():
    """Find a way to run grpcurl: the local binary, else a dockerized fallback.

    Returns a dict {argv, host, how} or None. The dockerized fallback reaches the
    container's published ports via host.docker.internal; the local binary uses
    127.0.0.1.
    """
    if shutil.which("grpcurl"):
        return {"argv": ["grpcurl"], "host": "127.0.0.1", "how": "local grpcurl"}
    if not shutil.which("docker"):
        return None
    img = "fullstorydev/grpcurl:latest"
    if run(["docker", "image", "inspect", img])[0] != 0:
        if run(["docker", "pull", img], timeout=180)[0] != 0:
            return None
    return {"argv": ["docker", "run", "--rm", img], "host": "host.docker.internal",
            "how": f"dockerized {img}"}


def grpcurl(gu, grpc_port, *extra, timeout=60):
    """Invoke grpcurl (local or dockerized). Use the literal token ADDR where the
    host:port argument should go."""
    addr = f"{gu['host']}:{grpc_port}"
    args = gu["argv"] + ["-plaintext"] + [addr if a == "ADDR" else a for a in extra]
    return run(args, timeout=timeout)


# ---- manifest structure -----------------------------------------------------

# Manifest schema versions the Index test tool accepts.
SUPPORTED_MANIFEST_VERSIONS = ["1.0.0"]

CALVER_RE = re.compile(r"^\d{4}\.\d{2}\.\d{2}\.\d+(-[A-Za-z0-9.-]+)?(_(AMS|EMEA|APAC))?$")


def _non_camel_keys(o):
    if isinstance(o, dict):
        for k, v in o.items():
            if "_" in k or "-" in k or (k[:1].isupper() and not k.isupper()):
                yield k
            yield from _non_camel_keys(v)
    elif isinstance(o, list):
        for x in o:
            yield from _non_camel_keys(x)


def check_manifest_structure(m):
    section("Manifest structure")

    name = m.get("metadata", {}).get("name")
    record(PASS if name else FAIL, f"metadata.name present: {name!r}")

    version = m.get("metadata", {}).get("version", "")
    if version in SUPPORTED_MANIFEST_VERSIONS:
        record(PASS, f"metadata.version is a supported manifest schema version: {version}")
    else:
        record(FAIL, f"metadata.version {version!r} is not a supported manifest schema version (want one of {SUPPORTED_MANIFEST_VERSIONS}; the CalVer is the image tag, not metadata.version)")

    # Manifest keys are camelCase, as in the IAB ARTF spec example.
    bad = sorted(set(_non_camel_keys(m)))
    record(PASS if not bad else FAIL, "all manifest keys are camelCase" if not bad else f"non-camelCase manifest keys: {bad}")

    eps = m.get("endpoints", {})
    # RTB extension point RPCs.
    rpcs = eps.get("rtbExtensionPoint", {}).get("rpcs", [])
    if not rpcs:
        record(FAIL, "endpoints.rtbExtensionPoint.rpcs is empty")
    for rpc in rpcs:
        full = rpc.get("rpc", "")
        if "/" in full and len(full.rsplit("/", 1)) == 2 and all(full.rsplit("/", 1)):
            record(PASS, f"rpc declared: {full}")
        else:
            record(FAIL, f"rpc {full!r} is not in 'package.Service/Method' form")
        # lifecycle weights should sum to ~1.0
        lifecycles = rpc.get("auctionLifecycle", {})
        if lifecycles:
            total = sum(float(v.get("weight", 0)) for v in lifecycles.values())
            if abs(total - 1.0) < 1e-6:
                record(PASS, f"auctionLifecycle weights sum to 1.0 ({', '.join(lifecycles)})")
            else:
                record(WARN, f"auctionLifecycle weights sum to {total} (expected 1.0)")

    # Metrics endpoint.
    met = eps.get("metrics", {})
    if met.get("protocol") == "http" and met.get("path"):
        record(PASS, f"metrics endpoint declared: {met.get('protocol')} {met.get('path')} ({met.get('type')})")
    else:
        record(FAIL, "endpoints.metrics must declare protocol=http and a path")
    defs = met.get("definitions", [])
    record(PASS if defs else FAIL, f"metrics definitions declared: {len(defs)}")

    # Health.
    health = eps.get("health", {})
    for probe in ("liveness", "readiness"):
        h = health.get(probe, {})
        if h.get("protocol") and h.get("rpc" if h.get("protocol") == "grpc" else "path"):
            record(PASS, f"health.{probe} declared: {h.get('protocol')} {h.get('rpc') or h.get('path')}")
        else:
            record(FAIL, f"health.{probe} is not fully declared")

    # Egress.
    egress = m.get("security", {}).get("networkPolicies", {}).get("egress", {}).get("rules")
    if egress == []:
        record(PASS, "security egress rules = [] (declares no egress; runtime egress is not script-verifiable)")
    elif egress is None:
        record(WARN, "security.networkPolicies.egress.rules not declared")
    else:
        record(WARN, f"manifest declares {len(egress)} egress rule(s); confirm the image actually needs them")

    return {
        "name": name,
        "version": version,
        "rpcs": rpcs,
        "metrics_path": met.get("path", "/metrics"),
        "definitions": defs,
    }


# ---- image inspect ----------------------------------------------------------

def check_image(image, manifest_obj):
    section(f"Image inspect: {image}")

    rc, out, err = run(["docker", "inspect", "--format", "{{json .}}", image])
    if rc != 0:
        record(FAIL, f"image not found / docker inspect failed: {image}", err.strip())
        return None
    info = json.loads(out)

    arch, os_ = info.get("Architecture"), info.get("Os")
    record(PASS if arch == "amd64" else FAIL, f"architecture is {arch} (required: amd64)")
    record(PASS if os_ == "linux" else FAIL, f"os is {os_} (required: linux)")

    cfg = info.get("Config", {}) or {}
    user = (cfg.get("User") or "").strip()
    if user and not user.startswith("0") and user not in ("root", ""):
        record(PASS, f"runs as non-root user: {user}")
    else:
        record(FAIL, f"image User is {user!r}; must be a non-root uid (e.g. 1000:1000)")

    labels = cfg.get("Labels") or {}

    # agent-manifest-version label.
    mver = labels.get("agent-manifest-version")
    record(PASS if mver else FAIL, f"label agent-manifest-version present: {mver!r}")
    if mver:
        record(PASS if mver in SUPPORTED_MANIFEST_VERSIONS else FAIL,
               f"agent-manifest-version {mver!r} is supported (want one of {SUPPORTED_MANIFEST_VERSIONS})")

    # agent-manifest label must be valid JSON and deep-equal the file.
    raw = labels.get("agent-manifest")
    if not raw:
        record(FAIL, "label agent-manifest is missing (built without the manifest label?)")
        return info
    try:
        embedded = json.loads(raw)
    except json.JSONDecodeError as e:
        record(FAIL, "label agent-manifest is not valid JSON", str(e))
        return info
    record(PASS, "label agent-manifest is valid JSON")
    meta_ver = (embedded.get("metadata") or {}).get("version")
    record(PASS if meta_ver in SUPPORTED_MANIFEST_VERSIONS else FAIL,
           f"agent-manifest metadata.version {meta_ver!r} is a supported schema version (want one of {SUPPORTED_MANIFEST_VERSIONS}; the CalVer belongs in the image tag, not here)")
    if mver:
        record(PASS if mver == meta_ver else FAIL,
               f"agent-manifest-version {mver!r} matches agent-manifest metadata.version {meta_ver!r}")
    qps = (embedded.get("resources") or {}).get("targetQps")
    record(PASS if isinstance(qps, (int, float)) and not isinstance(qps, bool) and qps > 0 else FAIL,
           f"resources.targetQps is a positive number: {qps!r}")
    if embedded == manifest_obj:
        record(PASS, "embedded agent-manifest matches manifest file exactly")
    else:
        record(FAIL, "embedded agent-manifest DIFFERS from the manifest file",
               _diff_keys(manifest_obj, embedded))
    return info


def _diff_keys(a, b, path=""):
    """Shallow-ish description of where two JSON objects differ."""
    diffs = []
    if isinstance(a, dict) and isinstance(b, dict):
        for k in sorted(set(a) | set(b)):
            p = f"{path}.{k}" if path else k
            if k not in a:
                diffs.append(f"only in image: {p}")
            elif k not in b:
                diffs.append(f"only in file:  {p}")
            else:
                diffs += _diff_keys(a[k], b[k], p)
    elif a != b:
        diffs.append(f"differs at {path or '(root)'}: file={a!r} image={b!r}")
    return "\n".join(diffs[:20]) if diffs else "(rebuild the image so the label is regenerated from the file)"


# ---- running container: endpoints + metrics ---------------------------------

def start_container(image, models):
    args = ["docker", "run", "-d", "--platform", "linux/amd64", "-p", "50051", "-p", "8080"]
    models_abs = os.path.abspath(models)
    if os.path.isdir(models_abs):
        args += ["-v", f"{models_abs}:/models:ro"]
    args.append(image)
    rc, out, err = run(args)
    if rc != 0:
        return None, None, None, err.strip()
    cid = out.strip()
    # discover published host ports
    def hostport(cport):
        rc2, o2, _ = run(["docker", "port", cid, cport])
        m = re.search(r":(\d+)\s*$", o2.strip().splitlines()[0]) if o2.strip() else None
        return int(m.group(1)) if m else None
    return cid, hostport("50051/tcp"), hostport("8080/tcp"), ""


def wait_ready(http_port, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        status, _ = http_get(f"http://127.0.0.1:{http_port}/healthcheck")
        if status == 200:
            return True
        time.sleep(0.5)
    return False


def drive_grpc_request(gu, grpc_port):
    """Fire one unary RPC (the gRPC health Check) to populate the request-scoped
    metrics. Any unary RPC flows through the same interceptor and records into
    rpc_server_duration, so the health Check (always reflection-resolvable) is a
    reliable driver even when the app proto descriptor is not exposed by reflection."""
    rc, out, err = grpcurl(gu, grpc_port, "ADDR", "grpc.health.v1.Health/Check")
    return rc == 0, (out or "") + (err or "")


def check_grpc_endpoints(gu, grpc_port, manifest):
    section("gRPC endpoints (reflection)")
    if not tcp_open("127.0.0.1", grpc_port):
        record(FAIL, f"gRPC port {grpc_port} is not accepting connections")
        return
    record(PASS, f"gRPC port {grpc_port} is open")

    if not gu:
        record(SKIP, "grpcurl unavailable; skipping service/method/health introspection",
               "install with: brew install grpcurl  (or: docker pull fullstorydev/grpcurl)")
        return

    rc, out, err = grpcurl(gu, grpc_port, "ADDR", "list")
    if rc != 0:
        record(FAIL, "grpcurl list failed (reflection not available?)", err.strip())
        return
    services = set(out.split())
    record(PASS, f"server reflects {len(services)} service(s)")

    # Declared RTB service + method. Presence of the service in ListServices is the
    # authoritative "endpoint is wired up" signal. Enumerating its methods needs the
    # server to expose the proto descriptor via reflection; if the generated code
    # does not register the descriptor globally (common), reflection returns
    # "Symbol not found" even though the method is callable by a compiled client.
    # Treat that as a WARN, not a failure: behavioral GetMutations validation is the
    # job of `make harness` (the Go testrunner, which uses the compiled proto).
    for rpc in manifest["rpcs"]:
        full = rpc["rpc"]
        svc, _, method = full.rpartition("/")
        if svc not in services:
            record(FAIL, f"declared service NOT served: {svc}", f"served: {sorted(services)}")
            continue
        record(PASS, f"declared service is served: {svc}")
        rc2, out2, err2 = grpcurl(gu, grpc_port, "ADDR", "list", svc)
        methods = {line.rpartition(".")[2] for line in out2.split()} if rc2 == 0 else set()
        if method in methods:
            record(PASS, f"declared method is served: {svc}/{method}")
        elif "Symbol not found" in (out2 + err2) or not methods:
            record(WARN, f"method {method} not enumerable via reflection (descriptor not exposed)",
                   "service is served; validate GetMutations behavior with `make harness`")
        else:
            record(FAIL, f"declared method NOT served: {svc}/{method} (found: {sorted(methods)})")

    # Health service (declared under endpoints.health).
    if "grpc.health.v1.Health" in services:
        record(PASS, "gRPC health service is served: grpc.health.v1.Health")
        rc3, out3, err3 = grpcurl(gu, grpc_port, "ADDR", "grpc.health.v1.Health/Check")
        if rc3 == 0 and "SERVING" in out3:
            record(PASS, "health Check returns SERVING")
        else:
            record(FAIL, "health Check did not return SERVING", (out3 or err3).strip())
    else:
        record(FAIL, "gRPC health service NOT served (grpc.health.v1.Health missing)")


def check_metrics(gu, http_port, grpc_port, manifest):
    section("HTTP metrics endpoint")
    path = manifest["metrics_path"]
    status, body = http_get(f"http://127.0.0.1:{http_port}{path}")
    if status != 200:
        record(FAIL, f"GET {path} returned {status}", body if isinstance(body, str) else "")
        return
    record(PASS, f"GET {path} returned 200")
    if "# TYPE" not in body:
        record(FAIL, f"{path} body does not look like Prometheus exposition")
        return

    # Drive one request so request-scoped series (rpc_server_duration, averages)
    # materialize, then re-scrape.
    drove = False
    if gu:
        ok, detail = drive_grpc_request(gu, grpc_port)
        if ok:
            drove = True
            record(PASS, "drove one unary RPC to exercise request-scoped metrics")
            time.sleep(0.3)
            status, body = http_get(f"http://127.0.0.1:{http_port}{path}")
        else:
            record(WARN, "could not drive a gRPC request; request-scoped metric checks limited",
                   detail.strip())
    else:
        record(SKIP, "grpcurl absent: cannot exercise request-scoped metrics (buckets/fields)")

    present_names = set(re.findall(r"^# TYPE (\S+) ", body, re.M))
    for ln in body.splitlines():
        if not ln or ln.startswith("#"):
            continue
        mm = re.match(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)", ln)
        if mm:
            present_names.add(mm.group(1))

    # Metrics the manifest declares vs what the endpoint exposes.
    section("Declared metrics vs exposed")
    request_scoped = {"rpc_server_duration", "rtd_simple_response_seconds_avg"}
    for d in manifest["definitions"]:
        nm = d.get("name")
        base = nm
        if nm in present_names or f"{nm}_bucket" in present_names or f"{nm}_count" in present_names:
            record(PASS, f"metric exposed: {nm}")
        elif nm in request_scoped and not drove:
            record(SKIP, f"metric {nm} not seen, but needs traffic to appear (no gRPC driver)")
        else:
            record(FAIL, f"declared metric NOT exposed: {nm}")

    # Deep-check the required histogram when we have samples.
    section("Required rpc_server_duration contract")
    les = set(re.findall(r'rpc_server_duration_bucket\{[^}]*le="([^"]+)"', body))
    required_les = {"0.001", "0.002", "0.003", "0.004", "0.005", "0.007", "0.010", "+Inf"}
    if not les:
        record(SKIP if not drove else FAIL,
               "no rpc_server_duration_bucket samples to inspect"
               + ("" if drove else " (drive traffic / install grpcurl to verify)"))
        return
    if les == required_les:
        record(PASS, "histogram buckets are the required seconds boundaries (0.001..0.010, +Inf)")
    else:
        record(FAIL, "histogram buckets differ from the required seconds boundaries",
               f"found: {sorted(les)}\nexpected: {sorted(required_les)}")

    # Label (tag) set must match the manifest's declared fields for the metric.
    decl_fields = next((set(d.get("fields", [])) for d in manifest["definitions"]
                        if d.get("name") == "rpc_server_duration"), set())
    sample = re.search(r"rpc_server_duration_bucket\{([^}]*)\}", body)
    if sample and decl_fields:
        keys = set(re.findall(r"([a-zA-Z_][a-zA-Z0-9_]*)=", sample.group(1))) - {"le"}
        if keys == decl_fields:
            record(PASS, f"histogram tags match declared fields: {sorted(keys)}")
        else:
            record(FAIL, "histogram tags do not match declared fields",
                   f"exposed: {sorted(keys)}\ndeclared: {sorted(decl_fields)}")


# ---- main -------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description="Validate a manifest file against a container image.")
    ap.add_argument("image", nargs="?", default="rtd-simple:local")
    ap.add_argument("--manifest", default="manifest/manifest.json")
    ap.add_argument("--models", default="models")
    ap.add_argument("--static-only", action="store_true")
    ap.add_argument("--keep-container", action="store_true")
    args = ap.parse_args()

    print(f"Validating manifest {args.manifest} against image {args.image}")

    if not shutil.which("docker"):
        print("error: docker not found on PATH", file=sys.stderr)
        return 2
    try:
        with open(args.manifest) as f:
            manifest_obj = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        section("Manifest structure")
        record(FAIL, f"cannot read/parse manifest: {args.manifest}", str(e))
        return _summary()

    manifest = check_manifest_structure(manifest_obj)
    check_image(args.image, manifest_obj)

    if args.static_only:
        return _summary()

    section("Starting container")
    cid, grpc_port, http_port, err = start_container(args.image, args.models)
    if not cid:
        record(FAIL, "could not start container", err)
        return _summary()
    record(PASS, f"container {cid[:12]} started (gRPC->{grpc_port}, HTTP->{http_port})")
    gu = resolve_grpcurl()
    if gu:
        print(f"  (gRPC checks use {gu['how']})")
    try:
        if not http_port or not wait_ready(http_port):
            record(FAIL, "container did not become ready on /healthcheck within 30s",
                   _logs(cid))
        else:
            record(PASS, "container is ready (/healthcheck 200)")
            check_grpc_endpoints(gu, grpc_port, manifest)
            check_metrics(gu, http_port, grpc_port, manifest)
    finally:
        if args.keep_container:
            print(f"\n(leaving container {cid[:12]} running; remove with: docker rm -f {cid[:12]})")
        else:
            run(["docker", "rm", "-f", cid])

    return _summary()


def _logs(cid):
    rc, out, err = run(["docker", "logs", "--tail", "20", cid])
    return (out or "") + (err or "")


def _summary():
    print("\n" + "-" * 48)
    print(f"Summary: {_counts[PASS]} pass, {_counts[FAIL]} fail, "
          f"{_counts[WARN]} warn, {_counts[SKIP]} skip")
    return 1 if _counts[FAIL] else 0


if __name__ == "__main__":
    sys.exit(main())
