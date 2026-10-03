#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

#
# scripts/smoketest-http.sh — end to end against in-cluster kode-gopher
# (scripts/deploy-gke-server.sh) over streamable HTTP.
#
# Under --auth=static (read from the deployment's args):
#   - no token and a wrong token get 401 with a Bearer challenge;
#   - initialize, tools/list, gcp_auth_status (mode=access-token, or
#     service when deployed with SERVICE_ACCOUNT);
#   - execute_go_code runs a snippet that gets a token from the
#     sandbox's metadata emulator, with no ADC file in the sandbox (in
#     service mode, as the service account);
#   - sandbox egress: Google APIs and the Go module proxy are reachable,
#     example.com and github.com aren't;
#   - a second session, and DELETE ends both.
# Under --auth=oauth, the parts that need no browser:
#   - protected-resource and authorization-server metadata;
#   - 401 with a resource_metadata pointer;
#   - DCR, then /authorize refusing no PKCE, plain PKCE, a token
#     response and another resource (error redirects), and an unknown
#     redirect_uri (error page, no redirect);
#   - /token refusing a made-up code with invalid_grant;
#   - with --client-credentials: a wrong secret is refused, then the
#     client's token (no refresh token) runs the static-mode session
#     checks as its service account (mode=service).
#
# Flags:
#   --port-forward   reach the Service through kubectl port-forward
#                    instead of the load balancer (before the
#                    certificate is ACTIVE)
#   --url <base>     base URL (https://<kode-gopher-ip>.sslip.io)
#   --file <path>    also run this Go snippet (e.g.
#                    testdata/list_buckets_snippet.go) and print its result
#   --client-credentials <path>
#                    oauth: JSON {"client_id", "client_secret"} of a
#                    pre-registered client-credentials client
#
# Env: KUBECONFIG, CONTEXT (passed as --context), PROJECT.

set -euo pipefail

NS=codemode
PORT_FORWARD=0
BASE=""
FILE=""
CC_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --port-forward) PORT_FORWARD=1; shift;;
    --url)          BASE="$2"; shift 2;;
    --file)         FILE="$2"; shift 2;;
    --client-credentials) CC_FILE="$2"; shift 2;;
    -h|--help)      sed -n '17,53p' "$0"; exit 0;;
    *)              echo "unknown arg: $1" >&2; exit 2;;
  esac
done

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m!!  %s\033[0m\n' "$*" >&2; exit 1; }

ctx=()
[[ -z "${CONTEXT:-}" ]] || ctx=(--context "$CONTEXT")
k() { kubectl "${ctx[@]}" "$@"; }

step "preflight"
for bin in kubectl python3; do
  command -v "$bin" >/dev/null 2>&1 || die "missing prerequisite: $bin"
done
[[ -z "$FILE" || -f "$FILE" ]] || die "no such file: $FILE"
[[ -z "$CC_FILE" || -f "$CC_FILE" ]] || die "no such file: $CC_FILE"
k -n "$NS" rollout status deployment/kode-gopher --timeout=60s

# The deployment's own --auth flag, not which Secrets exist: switching
# a deployment from static to oauth leaves kode-gopher-token behind.
args="$(k -n "$NS" get deployment kode-gopher -o jsonpath='{.spec.template.spec.containers[0].args[*]}')"
if [[ " $args " != *" --auth=oauth "* ]]; then
  AUTH=static
  # Handed to python in the environment; never printed.
  KG_TOKEN="$(k -n "$NS" get secret kode-gopher-token -o jsonpath='{.data.token}' \
    | python3 -c 'import base64, sys; print(base64.b64decode(sys.stdin.read()).decode())')"
  export KG_TOKEN
else
  AUTH=oauth
fi

if [[ $PORT_FORWARD -eq 1 ]]; then
  port=18080
  # kubectl itself, not the k function: $! must be kubectl's PID for
  # the kill below, or it outlives the script.
  kubectl "${ctx[@]}" -n "$NS" port-forward svc/kode-gopher "$port:8080" >/dev/null 2>&1 &
  pf=$!
  trap 'kill $pf 2>/dev/null || true' EXIT
  BASE="http://127.0.0.1:$port"
  for _ in {1..30}; do
    python3 -c "import urllib.request; urllib.request.urlopen('$BASE/healthz', timeout=1)" 2>/dev/null && break
    sleep 1
  done
elif [[ -z "$BASE" ]]; then
  command -v gcloud >/dev/null 2>&1 || die "missing prerequisite: gcloud (or pass --url)"
  PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
  ip="$(gcloud compute addresses describe kode-gopher-ip --global --project "$PROJECT" --format='value(address)')"
  BASE="https://${ip//./-}.sslip.io"
fi
echo "base=$BASE  auth=$AUTH"

step "checks"
python3 - "$BASE" "$AUTH" "$FILE" "$CC_FILE" <<'PY'
import base64, hashlib, json, os, secrets, sys, urllib.error, urllib.parse, urllib.request

base, auth, extra, cc_file = sys.argv[1:5]
failed = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else ": " + str(detail)[:500]))
    if not ok:
        failed.append(name)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **kw):
        return None


opener = urllib.request.build_opener(NoRedirect)


def http(method, path, body=None, headers=None, form=None):
    data = None
    h = dict(headers or {})
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        h["Content-Type"] = "application/x-www-form-urlencoded"
    elif body is not None:
        data = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    req = urllib.request.Request(base + path, data=data, headers=h, method=method)
    try:
        resp = opener.open(req, timeout=600)
    except urllib.error.HTTPError as e:
        resp = e
    return resp.status, resp.headers, resp.read().decode(errors="replace")


def rpc_result(headers, text):
    """The JSON-RPC response, from a JSON or an SSE body."""
    if headers.get("Content-Type", "").startswith("text/event-stream"):
        for line in text.splitlines():
            if line.startswith("data:"):
                msg = json.loads(line[5:])
                if "id" in msg:
                    return msg
        return None
    return json.loads(text) if text else None


class Session:
    def __init__(self, token):
        self.h = {"Authorization": "Bearer " + token, "Accept": "application/json, text/event-stream"}
        self.n = 0
        status, headers, text = self.post("initialize", {
            "protocolVersion": "2025-06-18", "capabilities": {},
            "clientInfo": {"name": "smoketest-http", "version": "0"}})
        self.init = (status, rpc_result(headers, text))
        self.h["Mcp-Session-Id"] = headers.get("Mcp-Session-Id", "")
        self.h["MCP-Protocol-Version"] = "2025-06-18"
        http("POST", "/mcp", {"jsonrpc": "2.0", "method": "notifications/initialized"}, self.h)

    def post(self, method, params):
        self.n += 1
        return http("POST", "/mcp", {"jsonrpc": "2.0", "id": self.n, "method": method, "params": params}, self.h)

    def call(self, method, params):
        status, headers, text = self.post(method, params)
        msg = rpc_result(headers, text) if status == 200 else None
        return status, (msg or {}).get("result"), text

    def close(self):
        return http("DELETE", "/mcp", headers=self.h)[0]


def tool(s, name, args):
    status, result, text = s.call("tools/call", {"name": name, "arguments": args})
    if not result:
        return None, text
    return result.get("structuredContent"), result


SNIPPET = '''package kode_gopher_snippet

import (
	"context"
	"os"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/google"
)

func run(ctx context.Context) (any, error) {
	c, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, err
	}
	tok, err := c.TokenSource.Token()
	if err != nil {
		return nil, err
	}
	email, _ := metadata.EmailWithContext(ctx, "default")
	_, statErr := os.Stat(os.Getenv("HOME") + "/.config/gcloud/application_default_credentials.json")
	return map[string]any{
		"token_valid": tok.Valid(),
		"email":       email,
		"project":     c.ProjectID,
		"adc_env":     os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		"adc_file":    statErr == nil,
	}, nil
}
'''


EGRESS_SNIPPET = '''package kode_gopher_snippet

import (
	"context"
	"net"
	"time"
)

// Which hosts the sandbox can open a connection to on 443.
func run(ctx context.Context) (any, error) {
	out := map[string]string{}
	for _, h := range []string{"storage.googleapis.com", "proxy.golang.org", "example.com", "github.com"} {
		c, err := net.DialTimeout("tcp", h+":443", 5*time.Second)
		if err != nil {
			out[h] = "blocked"
			continue
		}
		c.Close()
		out[h] = "open"
	}
	return out, nil
}
'''


def session_checks(token, modes):
    """initialize, tools, gcp_auth_status in modes, the metadata-emulator
    snippet, the --file snippet, a second session and DELETE."""
    s = Session(token)
    check("initialize", s.init[0] == 200 and s.h["Mcp-Session-Id"] != "", s.init)
    status, result, text = s.call("tools/list", {})
    names = sorted(t["name"] for t in (result or {}).get("tools", []))
    check("tools/list", {"execute_go_code", "gcp_auth_status"} <= set(names), names or text)

    st, raw = tool(s, "gcp_auth_status", {})
    who = st or {}
    check("gcp_auth_status: " + " or ".join(modes), who.get("mode") in modes, raw)
    print("     ", json.dumps(st))

    st, raw = tool(s, "execute_go_code", {"code": SNIPPET})
    v = (((st or {}).get("result") or {}).get("value")) or {}
    check("execute_go_code: token from the metadata emulator, no ADC file",
          v.get("token_valid") is True and v.get("adc_env") == "" and v.get("adc_file") is False, raw)
    if who.get("mode") == "service":
        check("execute_go_code: runs as " + str(who.get("email")), v.get("email") == who.get("email"), v)
    print("      build_ms=%s duration_ms=%s value=%s" % ((st or {}).get("build_ms"), (st or {}).get("duration_ms"), json.dumps(v)))

    # The deployment's sandboxes may reach only the egress allowlist
    # (manifests/components/egress-allowlist).
    st, raw = tool(s, "execute_go_code", {"code": EGRESS_SNIPPET})
    v = (((st or {}).get("result") or {}).get("value")) or {}
    check("egress: Google APIs and the Go proxy open",
          v.get("storage.googleapis.com") == "open" and v.get("proxy.golang.org") == "open", v or raw)
    check("egress: other hosts blocked",
          v.get("example.com") == "blocked" and v.get("github.com") == "blocked", v or raw)

    if extra:
        st, raw = tool(s, "execute_go_code", {"code": open(extra).read()})
        check("execute_go_code: " + extra, (st or {}).get("exit_code") == 0, raw)
        print(json.dumps(st, indent=2)[:4000])

    s2 = Session(token)
    check("second session", s2.init[0] == 200 and s2.h["Mcp-Session-Id"] not in ("", s.h["Mcp-Session-Id"]), s2.init)
    for sess in (s, s2):
        code = sess.close()
        check("DELETE session", code in (200, 202, 204), code)
    status, result, text = s.call("tools/list", {})
    check("deleted session refused", status == 404, (status, text))


init_body = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "smoketest-http", "version": "0"}}}
accept = {"Accept": "application/json, text/event-stream"}

status, headers, _ = http("POST", "/mcp", init_body, accept)
challenge = headers.get("WWW-Authenticate", "")
check("no token: 401 Bearer", status == 401 and challenge.startswith("Bearer"), (status, challenge))

if auth == "static":
    status, headers, _ = http("POST", "/mcp", init_body, {**accept, "Authorization": "Bearer " + "x" * 43})
    check("wrong token: 401", status == 401, status)

    session_checks(os.environ["KG_TOKEN"], ("access-token", "service"))

else:
    check("401 points at resource metadata", "resource_metadata=" in challenge, challenge)
    status, _, text = http("GET", "/.well-known/oauth-protected-resource/mcp")
    prm = json.loads(text) if status == 200 else {}
    check("protected-resource metadata", prm.get("resource") == base + "/mcp", (status, text))
    status, _, text = http("GET", "/.well-known/oauth-authorization-server")
    asm = json.loads(text) if status == 200 else {}
    check("authorization-server metadata: S256 only",
          asm.get("issuer") == base and asm.get("code_challenge_methods_supported") == ["S256"], (status, text))

    redirect = "http://127.0.0.1:1/callback"
    status, _, text = http("POST", "/register", {"redirect_uris": [redirect], "client_name": "smoketest-http",
                                                 "token_endpoint_auth_method": "none"})
    if status == 404:
        print("SKIP /authorize checks: registration is closed")
    else:
        client = json.loads(text).get("client_id", "") if status == 201 else ""
        check("DCR", client != "", (status, text))
        verifier = secrets.token_urlsafe(48)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()

        def authorize(**over):
            q = {"response_type": "code", "client_id": client, "redirect_uri": redirect, "state": "st",
                 "code_challenge": challenge, "code_challenge_method": "S256", "resource": base + "/mcp",
                 "scope": "mcp"}
            q.update(over)
            q = {k: v for k, v in q.items() if v is not None}
            return http("GET", "/authorize?" + urllib.parse.urlencode(q))

        for name, over, want in [
            ("no PKCE", {"code_challenge": None, "code_challenge_method": None}, "invalid_request"),
            ("plain PKCE", {"code_challenge": verifier, "code_challenge_method": "plain"}, "invalid_request"),
            ("token response", {"response_type": "token"}, "unsupported_response_type"),
            ("other resource", {"resource": "https://evil.example/mcp"}, "invalid_target"),
        ]:
            status, headers, _ = authorize(**over)
            q = urllib.parse.parse_qs(urllib.parse.urlparse(headers.get("Location", "")).query)
            check("/authorize " + name + ": " + want, status in (302, 303) and q.get("error") == [want], (status, headers.get("Location")))
        status, headers, _ = authorize(redirect_uri="https://evil.example/cb")
        check("/authorize unknown redirect_uri: error page", status == 400 and not headers.get("Location"), status)
        status, headers, _ = authorize()
        check("/authorize valid request: consent page", status == 200, status)

    status, _, text = http("POST", "/token", form={"grant_type": "authorization_code", "code": "made-up",
                                                   "redirect_uri": redirect, "client_id": "x", "code_verifier": "v" * 43})
    err = json.loads(text).get("error") if text.startswith("{") else text
    check("/token made-up code: 400", status in (400, 401) and err in ("invalid_grant", "invalid_client"), (status, text))

    if cc_file:
        cc = json.load(open(cc_file))
        cid, csecret = cc["client_id"], cc["client_secret"]
        status, _, text = http("POST", "/token", form={"grant_type": "client_credentials", "client_id": cid,
                                                       "client_secret": csecret + "x"})
        check("client_credentials wrong secret: 401 invalid_client",
              status == 401 and json.loads(text).get("error") == "invalid_client", (status, text))
        status, _, text = http("POST", "/token", form={"grant_type": "client_credentials", "client_id": cid,
                                                       "client_secret": csecret, "resource": base + "/mcp"})
        tok = json.loads(text) if status == 200 else {}
        check("client_credentials: access token, no refresh token",
              tok.get("access_token") and "refresh_token" not in tok, (status, text[:200]))
        if tok.get("access_token"):
            session_checks(tok["access_token"], ("service",))

print()
if failed:
    print("FAILED: " + ", ".join(failed))
    sys.exit(1)
print("all checks passed")
PY
