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
# scripts/deploy-gke-server.sh — deploy in-cluster kode-gopher
# (manifests/overlays/gke-server, or gke-server-oauth with
# --auth=oauth) to a GKE cluster with the agent-sandbox addon.
#
# Creates what the overlay can't hold:
#   - the global static IP kode-gopher-ip and the Google-managed
#     certificate kode-gopher-cert for $KG_HOST, if missing;
#   - the Secret kode-gopher-token (static: a random token, if missing)
#     or kode-gopher-oauth (oauth: from the files below; a new keyring
#     only if the Secret doesn't exist yet);
# then applies the overlay with the API server's address in the
# network policy, the image, and the per-deployment OAuth flags, and
# replaces the warm pool's sandboxes if they run an older sandbox image
# (scripts/refresh-warm-pool.sh).
#
# Never prints secrets. scripts/smoketest-http.sh reads the static
# token from the Secret itself.
#
# Flags:
#   --auth static|oauth   (static)
#   --dry-run             print the rendered manifests, apply nothing
#
# Env:
#   KUBECONFIG, CONTEXT   cluster to use; CONTEXT is passed as
#                         --context and the current context is unchanged
#   PROJECT               project for the IP and certificate
#                         (gcloud config get-value project)
#   IMAGE                 server image (ghcr.io/gke-demos/kode-gopher/server:main)
#   KG_HOST               public hostname (<ip with dashes>.sslip.io)
#   EGRESS_ALLOW          extra hosts sandboxes may reach on 443, comma-
#                         separated ("api.example.com,*.example.org"); always
#                         allowed: *.googleapis.com, the Go module proxy, and
#                         the cluster region's *.<region>.gke.goog
#   SERVICE_ACCOUNT       run snippets as this Google service account
#                         (--credentials=service) instead of kode-gopher's
#                         own Workload Identity; the KSA needs
#                         roles/iam.serviceAccountTokenCreator on it.
#                         Static auth only: under oauth every request
#                         carries its user's or client's own token
#   oauth only:
#   GOOGLE_CLIENT_FILE    the Google OAuth client JSON (required on first deploy)
#   CLIENTS_FILE          pre-registered clients JSON (kept from the existing
#                         Secret if unset, else none); a client with
#                         a service_account uses the client_credentials
#                         grant, and the KSA needs
#                         roles/iam.serviceAccountTokenCreator on that account
#   ALLOW_DOMAINS         comma-separated hd domains to admit
#   ALLOW_GROUPS          comma-separated Google groups to admit
#   GROUPS_SERVICE_ACCOUNT  service account that checks ALLOW_GROUPS (holds
#                         the Groups Reader admin role; kode-gopher needs
#                         roles/iam.serviceAccountTokenCreator on it)
#   CUSTODY               vault or sealed (vault)
#   VAULT_AUTH_PROVIDER   projects/<p>/locations/<l>/authProviders/<name>
#   OPEN_REGISTRATION     true or false (true)

set -euo pipefail

AUTH=static
DRY_RUN=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --auth)    AUTH="$2"; shift 2;;
    --auth=*)  AUTH="${1#--auth=}"; shift;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) sed -n '17,71p' "$0"; exit 0;;
    *)         echo "unknown arg: $1" >&2; exit 2;;
  esac
done

NS=codemode
IMAGE="${IMAGE:-ghcr.io/gke-demos/kode-gopher/server:main}"
CUSTODY="${CUSTODY:-vault}"
OPEN_REGISTRATION="${OPEN_REGISTRATION:-true}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m!!  %s\033[0m\n' "$*" >&2; exit 1; }

k() {
  if [[ -n "${CONTEXT:-}" ]]; then
    kubectl --context "$CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

step "preflight"
for bin in kubectl gcloud python3; do
  command -v "$bin" >/dev/null 2>&1 || die "missing prerequisite: $bin"
done
case "$AUTH" in
  static) OVERLAY="$REPO_ROOT/manifests/overlays/gke-server";;
  oauth)  OVERLAY="$REPO_ROOT/manifests/overlays/gke-server-oauth";;
  *)      die "--auth must be static or oauth, got $AUTH";;
esac
[[ -z "${SERVICE_ACCOUNT:-}" || "$AUTH" == static ]] || die "SERVICE_ACCOUNT is for --auth=static (OAuth clients name their own in CLIENTS_FILE)"
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
[[ -n "$PROJECT" ]] || die "PROJECT unset and gcloud has no default project"
k get ns "$NS" >/dev/null || die "namespace $NS not found"
k get crd sandboxtemplates.extensions.agents.x-k8s.io >/dev/null \
  || die "agent-sandbox CRDs not installed"
# Sandbox egress is an allowlist of hostnames (manifests/components/
# egress-allowlist), which needs GKE's FQDN network policies.
k get crd fqdnnetworkpolicies.networking.gke.io >/dev/null 2>&1 \
  || die "FQDN network policy isn't enabled on this cluster; sandbox egress needs it: gcloud container clusters update <cluster> --location <location> --enable-fqdn-network-policy (on Autopilot GKE then restarts the nodes, which can take a few hours)"
REGION="$(k get nodes -o jsonpath='{.items[0].metadata.labels.topology\.kubernetes\.io/region}')"
[[ -n "$REGION" ]] || die "couldn't read the cluster's region from its nodes"
echo "project=$PROJECT  auth=$AUTH  image=$IMAGE"

step "static IP kode-gopher-ip"
IP="$(gcloud compute addresses describe kode-gopher-ip --global --project "$PROJECT" --format='value(address)' 2>/dev/null || true)"
if [[ -z "$IP" ]]; then
  [[ $DRY_RUN -eq 0 ]] || die "kode-gopher-ip doesn't exist; run without --dry-run to create it"
  gcloud compute addresses create kode-gopher-ip --global --project "$PROJECT"
  IP="$(gcloud compute addresses describe kode-gopher-ip --global --project "$PROJECT" --format='value(address)')"
fi
KG_HOST="${KG_HOST:-${IP//./-}.sslip.io}"
ISSUER="https://$KG_HOST"
echo "ip=$IP  host=$KG_HOST"

step "certificate kode-gopher-cert"
DOMAINS="$(gcloud compute ssl-certificates describe kode-gopher-cert --global --project "$PROJECT" --format='value(managed.domains)' 2>/dev/null || true)"
if [[ -z "$DOMAINS" ]]; then
  [[ $DRY_RUN -eq 0 ]] || die "kode-gopher-cert doesn't exist; run without --dry-run to create it"
  gcloud compute ssl-certificates create kode-gopher-cert --global --project "$PROJECT" --domains "$KG_HOST"
elif [[ "$DOMAINS" != "$KG_HOST" ]]; then
  die "kode-gopher-cert is for $DOMAINS, not $KG_HOST; delete it or set KG_HOST"
fi
gcloud compute ssl-certificates describe kode-gopher-cert --global --project "$PROJECT" \
  --format='value(managed.status,managed.domainStatus)'

step "Kubernetes API server address (for the egress policy)"
APISERVER="$(k get endpointslices -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[*].endpoints[*].addresses[*]}')"
[[ -n "$APISERVER" ]] || die "no endpoints for the kubernetes Service"
echo "$APISERVER"

step "Secrets"
secret_exists() { k -n "$NS" get secret "$1" >/dev/null 2>&1; }
if [[ $DRY_RUN -eq 1 ]]; then
  echo "(dry run: Secrets unchanged)"
elif [[ "$AUTH" == static ]]; then
  if secret_exists kode-gopher-token; then
    echo "kode-gopher-token exists; keeping it"
  else
    python3 -c 'import secrets; print(secrets.token_urlsafe(32), end="")' \
      | k -n "$NS" create secret generic kode-gopher-token --from-file=token=/dev/stdin
  fi
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  if secret_exists kode-gopher-oauth; then
    # Keep the keyring: replacing it would invalidate every token.
    k -n "$NS" get secret kode-gopher-oauth -o jsonpath='{.data.keyring}' \
      | python3 -c 'import base64, sys; sys.stdout.buffer.write(base64.b64decode(sys.stdin.read()))' > "$tmp/keyring"
    if [[ -z "${GOOGLE_CLIENT_FILE:-}" ]]; then
      k -n "$NS" get secret kode-gopher-oauth -o jsonpath='{.data.google-client\.json}' \
        | python3 -c 'import base64, sys; sys.stdout.buffer.write(base64.b64decode(sys.stdin.read()))' > "$tmp/google-client.json"
    fi
    # Keep the pre-registered clients too, unless CLIENTS_FILE replaces
    # them: a routine redeploy mustn't drop them.
    if [[ -z "${CLIENTS_FILE:-}" ]]; then
      k -n "$NS" get secret kode-gopher-oauth -o jsonpath='{.data.clients\.json}' \
        | python3 -c 'import base64, sys; sys.stdout.buffer.write(base64.b64decode(sys.stdin.read()))' > "$tmp/clients.json"
    fi
  else
    python3 -c 'import base64, secrets; print("k1", base64.b64encode(secrets.token_bytes(32)).decode())' > "$tmp/keyring"
  fi
  if [[ -n "${GOOGLE_CLIENT_FILE:-}" ]]; then
    cp "$GOOGLE_CLIENT_FILE" "$tmp/google-client.json"
  fi
  [[ -s "$tmp/google-client.json" ]] || die "GOOGLE_CLIENT_FILE is required on first deploy"
  if [[ -n "${CLIENTS_FILE:-}" ]]; then
    cp "$CLIENTS_FILE" "$tmp/clients.json"
  elif [[ ! -s "$tmp/clients.json" ]]; then
    echo '[]' > "$tmp/clients.json"
  fi
  k -n "$NS" create secret generic kode-gopher-oauth \
    --from-file=google-client.json="$tmp/google-client.json" \
    --from-file=keyring="$tmp/keyring" \
    --from-file=clients.json="$tmp/clients.json" \
    --dry-run=client -o yaml | k apply -f -
  echo "kode-gopher-oauth applied (Google client redirect URIs must include $ISSUER/callback)"
fi

# kode-gopher reads its Secret only at startup: hash it into the pod
# template, so a changed token, client list or keyring rolls the pod.
secret=kode-gopher-token
[[ "$AUTH" == oauth ]] && secret=kode-gopher-oauth
CONFIG_HASH="$(k -n "$NS" get secret "$secret" -o jsonpath='{.data}' 2>/dev/null | sha256sum | cut -c1-16)"

step "render"
work="$(mktemp -d)"
trap 'rm -rf "$work" "${tmp:-}"' EXIT
python3 - "$work" "$OVERLAY" "$IMAGE" "$AUTH" "$ISSUER" "$PROJECT" "$APISERVER" "$REGION" "$CONFIG_HASH" <<'PY'
import json, os, re, sys

work, overlay, image, auth, issuer, project, apiserver, region, config_hash = sys.argv[1:10]
env = os.environ.get

ops = [{"op": "test", "path": "/spec/egress/2/to/0/ipBlock/cidr", "value": "192.0.2.1/32"},
       {"op": "replace", "path": "/spec/egress/2/to",
        "value": [{"ipBlock": {"cidr": ip + ("/128" if ":" in ip else "/32")}} for ip in apiserver.split()]}]
patches = [{"target": {"kind": "NetworkPolicy", "name": "kode-gopher"}, "patch": json.dumps(ops)}]

# Sandbox egress allowlist: the region's GKE control-plane DNS
# endpoints, so snippets reach clusters as docs/design.md describes,
# plus EGRESS_ALLOW ("host" or "*.domain", comma-separated).
hosts = ["*." + region + ".gke.goog"] + [h.strip() for h in env("EGRESS_ALLOW", "").split(",") if h.strip()]
host_re = re.compile(r"^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$")
for h in hosts:
    if not host_re.match(h):
        sys.exit("EGRESS_ALLOW: %r isn't a hostname or *.domain pattern" % h)
ops = [{"op": "add", "path": "/spec/egress/0/matches/-",
        "value": {"pattern": h} if h.startswith("*.") else {"name": h}} for h in hosts]
patches.append({"target": {"kind": "FQDNNetworkPolicy", "name": "kode-gopher-sandbox-egress"}, "patch": json.dumps(ops)})

args = []
if auth == "oauth":
    args = ["--oauth-issuer=" + issuer, "--oauth-custody=" + env("CUSTODY", "vault"),
            "--oauth-open-registration=" + env("OPEN_REGISTRATION", "true"), "--project=" + project]
    for flag, var in [("--oauth-allow-domains", "ALLOW_DOMAINS"), ("--oauth-allow-groups", "ALLOW_GROUPS"),
                      ("--oauth-groups-service-account", "GROUPS_SERVICE_ACCOUNT"),
                      ("--oauth-vault-auth-provider", "VAULT_AUTH_PROVIDER")]:
        if env(var):
            args.append(flag + "=" + env(var))
# The pod template has no annotations of its own, so this sets the map.
ops = [{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": image},
       {"op": "add", "path": "/spec/template/metadata/annotations", "value": {"kode-gopher/config-hash": config_hash}}]
if env("SERVICE_ACCOUNT"):
    ops += [{"op": "test", "path": "/spec/template/spec/containers/0/args/5", "value": "--credentials=access-token"},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args/5", "value": "--credentials=service"}]
    args.append("--service-account=" + env("SERVICE_ACCOUNT"))
ops += [{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": a} for a in args]
patches.append({"target": {"kind": "Deployment", "name": "kode-gopher"}, "patch": json.dumps(ops)})

# JSON is YAML.
with open(os.path.join(work, "kustomization.yaml"), "w") as f:
    json.dump({"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
               "resources": [os.path.relpath(overlay, work)], "patches": patches}, f, indent=2)
PY

if [[ $DRY_RUN -eq 1 ]]; then
  k kustomize "$work"
  exit 0
fi

step "apply"
k apply -k "$work"
k -n "$NS" rollout status deployment/kode-gopher --timeout=300s

# A new sandbox image only reaches new sessions once the warm pool's
# old-image sandboxes are replaced; agent-sandbox doesn't do it.
step "warm pool"
NS="$NS" CONTEXT="${CONTEXT:-}" "$REPO_ROOT/scripts/refresh-warm-pool.sh"

step "done"
echo "MCP endpoint: $ISSUER/mcp"
echo "certificate:  $(gcloud compute ssl-certificates describe kode-gopher-cert --global --project "$PROJECT" --format='value(managed.status)')"
echo "  (PROVISIONING until $KG_HOST resolves to $IP and the load balancer is up; up to an hour)"
echo "test:         scripts/smoketest-http.sh            (through the load balancer)"
echo "              scripts/smoketest-http.sh --port-forward"
