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
#   - under SECRET_STORE=k8s (default): the Kubernetes Secret
#     kode-gopher-token (static: a random token, if missing) or
#     $OAUTH_SECRET_NAME (oauth: from the files below; a new keyring
#     only if the Secret doesn't exist yet);
#   - under SECRET_STORE=secret-manager: versioned Google Cloud Secret
#     Manager secrets (seeding from an existing Kubernetes Secret on
#     first migration so active tokens stay valid), per-secret
#     roles/secretmanager.secretAccessor grants to kode-gopher's
#     Workload Identity principal, and pinned sm://.../versions/<N>
#     server startup flags with no Kubernetes Secret volume mount;
# then applies the overlay with the API server's address in the
# network policy, the image, and the per-deployment OAuth flags, and
# replaces the warm pool's sandboxes if they run an older sandbox image
# (scripts/refresh-warm-pool.sh).
#
# Never prints secrets. scripts/smoketest-http.sh reads the static
# token from the Kubernetes Secret or Secret Manager itself.
#
# Flags:
#   --auth static|oauth   (static)
#   --dry-run             print the rendered manifests, apply nothing
#
# Env:
#   KUBECONFIG, CONTEXT   cluster to use; CONTEXT is passed as
#                         --context and the current context is unchanged
#   PROJECT               project for the IP, certificate and Secret Manager
#                         (gcloud config get-value project)
#   IMAGE                 server image (ghcr.io/gke-demos/kode-gopher/server:main)
#   ADDRESS_NAME          global static IP name (kode-gopher-ip)
#   CERT_NAME             Google-managed SSL certificate name (kode-gopher-cert)
#   KG_HOST               public hostname (<ip with dashes>.sslip.io)
#   SECRET_STORE          k8s (Kubernetes Secret, default) or secret-manager
#                         (Google Cloud Secret Manager via Workload Identity)
#   SECRET_PREFIX         Secret Manager secret name (static, default:
#                         kode-gopher-token) or prefix (oauth, default:
#                         $OAUTH_SECRET_NAME -> <prefix>-{google-client,keyring,clients})
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
#   CAPTURE_DIR           directory to write JSON debug records of
#                         execute_go_code calls (e.g. /tmp/kode-gopher-captures)
#   CAPTURE_MODE          failures (default when CAPTURE_DIR is set) or all
#   OTEL_ENDPOINT         OpenTelemetry OTLP gRPC collector endpoint
#                         (e.g. https://telemetry.googleapis.com)
#   static + secret-manager only:
#   TOKEN_SECRET_VERSION  pin an existing Secret Manager version for the
#                         static token (e.g. 1) instead of latest
#   oauth only:
#   OAUTH_SECRET_NAME     Kubernetes Secret name (and default SECRET_PREFIX)
#                         for the OAuth client, keyring and clients
#                         (kode-gopher-oauth)
#   GOOGLE_CLIENT_FILE    the Google OAuth client JSON (required on first deploy)
#   DOMAIN_GOOGLE_CLIENT_FILES  comma-separated <domain>=<path> overrides for
#                         GOOGLE_CLIENT_FILE (stored as google-client-<domain>.json
#                         in k8s or <prefix>-google-client-<domain_with_underscores>
#                         in Secret Manager; kept on redeploy if unset)
#   CLIENTS_FILE          pre-registered clients JSON (kept from the existing
#                         secret if unset, else none); a client with
#                         a service_account uses the client_credentials
#                         grant, and the KSA needs
#                         roles/iam.serviceAccountTokenCreator on that account
#   GOOGLE_CLIENT_SECRET_VERSION, KEYRING_SECRET_VERSION, CLIENTS_SECRET_VERSION
#                         secret-manager only: pin an existing Secret Manager
#                         version (e.g. 1) for atomic rollback
#   DOMAIN_GOOGLE_CLIENT_SECRET_VERSIONS
#                         secret-manager only: comma-separated <domain>=<version>
#                         pins for per-domain Google client secrets
#   ALLOW_DOMAINS         comma-separated hd domains to admit
#   ALLOW_GROUPS          comma-separated Google groups to admit
#   GROUPS_SERVICE_ACCOUNT  service account that checks ALLOW_GROUPS (holds
#                         the Groups Reader admin role; kode-gopher needs
#                         roles/iam.serviceAccountTokenCreator on it)
#   CUSTODY               vault or sealed (vault)
#   VAULT_AUTH_PROVIDER   projects/<p>/locations/<l>/authProviders/<name>
#   DOMAIN_VAULT_AUTH_PROVIDERS  comma-separated <domain>=<authProvider>
#                         overrides for VAULT_AUTH_PROVIDER
#   OPEN_REGISTRATION     true or false (true)

set -euo pipefail

AUTH=static
DRY_RUN=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --auth)    AUTH="$2"; shift 2;;
    --auth=*)  AUTH="${1#--auth=}"; shift;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) sed -n '17,108p' "$0"; exit 0;;
    *)         echo "unknown arg: $1" >&2; exit 2;;
  esac
done

NS=codemode
IMAGE="${IMAGE:-ghcr.io/gke-demos/kode-gopher/server:main}"
ADDRESS_NAME="${ADDRESS_NAME:-kode-gopher-ip}"
CERT_NAME="${CERT_NAME:-kode-gopher-cert}"
OAUTH_SECRET_NAME="${OAUTH_SECRET_NAME:-kode-gopher-oauth}"
SECRET_STORE="${SECRET_STORE:-k8s}"
CUSTODY="${CUSTODY:-vault}"
OPEN_REGISTRATION="${OPEN_REGISTRATION:-true}"

if [[ "$AUTH" == oauth ]]; then
  SECRET_PREFIX="${SECRET_PREFIX:-$OAUTH_SECRET_NAME}"
else
  SECRET_PREFIX="${SECRET_PREFIX:-kode-gopher-token}"
fi

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
case "$SECRET_STORE" in
  k8s|secret-manager) ;;
  *) die "SECRET_STORE must be k8s or secret-manager, got $SECRET_STORE";;
esac
[[ "$SECRET_PREFIX" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,199}$ ]] \
  || die "SECRET_PREFIX must match ^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,199}$, got $SECRET_PREFIX"
for ver_var in TOKEN_SECRET_VERSION GOOGLE_CLIENT_SECRET_VERSION KEYRING_SECRET_VERSION CLIENTS_SECRET_VERSION; do
  ver_val="${!ver_var:-}"
  if [[ -n "$ver_val" && ! "$ver_val" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$ ]]; then
    die "$ver_var must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$, got $ver_val"
  fi
done
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
echo "project=$PROJECT  auth=$AUTH  secret_store=$SECRET_STORE  image=$IMAGE"

step "static IP $ADDRESS_NAME"
IP="$(gcloud compute addresses describe "$ADDRESS_NAME" --global --project "$PROJECT" --format='value(address)' 2>/dev/null || true)"
if [[ -z "$IP" ]]; then
  [[ $DRY_RUN -eq 0 ]] || die "$ADDRESS_NAME doesn't exist; run without --dry-run to create it"
  gcloud compute addresses create "$ADDRESS_NAME" --global --project "$PROJECT"
  IP="$(gcloud compute addresses describe "$ADDRESS_NAME" --global --project "$PROJECT" --format='value(address)')"
fi
KG_HOST="${KG_HOST:-${IP//./-}.sslip.io}"
ISSUER="https://$KG_HOST"
echo "ip=$IP  host=$KG_HOST"

step "certificate $CERT_NAME"
DOMAINS="$(gcloud compute ssl-certificates describe "$CERT_NAME" --global --project "$PROJECT" --format='value(managed.domains)' 2>/dev/null || true)"
if [[ -z "$DOMAINS" ]]; then
  [[ $DRY_RUN -eq 0 ]] || die "$CERT_NAME doesn't exist; run without --dry-run to create it"
  gcloud compute ssl-certificates create "$CERT_NAME" --global --project "$PROJECT" --domains "$KG_HOST"
elif [[ "$DOMAINS" != "$KG_HOST" ]]; then
  die "$CERT_NAME is for $DOMAINS, not $KG_HOST; delete it or set KG_HOST"
fi
gcloud compute ssl-certificates describe "$CERT_NAME" --global --project "$PROJECT" \
  --format='value(managed.status,managed.domainStatus)'

step "Kubernetes API server address (for the egress policy)"
APISERVER="$(k get endpointslices -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[*].endpoints[*].addresses[*]}')"
[[ -n "$APISERVER" ]] || die "no endpoints for the kubernetes Service"
echo "$APISERVER"

SM_URIS_JSON="{}"
PRESERVED_K8S_DOMAINS=""
tmp="$(mktemp -d)"
chmod 0700 "$tmp"
work="$(mktemp -d)"
trap 'rm -rf "$tmp" "$work"' EXIT

# Return 0 if Kubernetes Secret $1 exists, 1 on NotFound, die on any other kubectl failure.
secret_exists() {
  local err_file="$tmp/k8s_err"
  if k -n "$NS" get secret "$1" >/dev/null 2>"$err_file"; then
    rm -f "$err_file"
    return 0
  fi
  if grep -qiE "NotFound|not found" "$err_file"; then
    rm -f "$err_file"
    return 1
  fi
  local msg
  msg="$(cat "$err_file")"
  rm -f "$err_file"
  die "kubectl get secret $1 failed: $msg"
}

# Extract key $2 from Kubernetes Secret $1 into $3. Returns 1 if the Secret or key is absent; dies on kubectl errors.
k8s_secret_key_to_file() {
  local sname="$1" skey="$2" dst="$3" raw_file="$tmp/k8s_sec.json"
  secret_exists "$sname" || return 1
  k -n "$NS" get secret "$sname" -o json >"$raw_file" || die "failed to read Kubernetes Secret $sname"
  if python3 - "$skey" "$dst" "$raw_file" <<'PY'
import base64, json, sys
key, dst, src = sys.argv[1:4]
with open(src, encoding="utf-8") as f:
    data = json.load(f).get("data") or {}
val = data.get(key)
if not val:
    sys.exit(1)
with open(dst, "wb") as out:
    out.write(base64.b64decode(val))
PY
  then
    rm -f "$raw_file"
    return 0
  fi
  rm -f "$raw_file"
  return 1
}

# Return 0 if Secret Manager secret $1 exists, 1 on NOT_FOUND, die on any other gcloud error.
sm_secret_exists() {
  local err_file="$tmp/sm_err"
  if gcloud secrets describe "$1" --project "$PROJECT" >/dev/null 2>"$err_file"; then
    rm -f "$err_file"
    return 0
  fi
  if grep -qiE "NOT_FOUND|not found" "$err_file"; then
    rm -f "$err_file"
    return 1
  fi
  local msg
  msg="$(cat "$err_file")"
  rm -f "$err_file"
  die "Secret Manager describe $1 failed: $msg"
}

# Print the numeric version ID (e.g. "1") of $1/versions/$2, or empty if the secret or version is NOT_FOUND.
# Dies on any non-404 gcloud failure (PERMISSION_DENIED, SERVICE_DISABLED, network/5xx) so a transient
# error can never trigger accidental keyring or token regeneration.
sm_resolve_version() {
  local sec="$1" ver="${2:-latest}" err_file="$tmp/sm_ver_err" name
  sm_secret_exists "$sec" || return 0
  if name="$(gcloud secrets versions describe "$ver" --secret="$sec" --project="$PROJECT" --format='value(name)' 2>"$err_file")"; then
    rm -f "$err_file"
    [[ -n "$name" ]] && echo "${name##*/}" || true
    return 0
  fi
  if grep -qiE "NOT_FOUND|not found" "$err_file"; then
    rm -f "$err_file"
    return 0
  fi
  local msg
  msg="$(cat "$err_file")"
  rm -f "$err_file"
  die "Secret Manager versions describe $sec/versions/$ver failed: $msg"
}

# Die if Secret Manager secret $1 already has prior versions (e.g. all disabled/destroyed) so we never
# silently mint a conflicting keyring or static token over a disabled secret.
sm_assert_uninitialized() {
  local sec="$1" existing
  sm_secret_exists "$sec" || return 0
  existing="$(gcloud secrets versions list "$sec" --project="$PROJECT" --limit=1 --format='value(name)')" \
    || die "Secret Manager versions list $sec failed"
  if [[ -n "$existing" ]]; then
    die "Secret Manager secret $sec exists with no enabled 'latest' version; re-enable a version or pin *_SECRET_VERSION"
  fi
}

# Ensure $sec exists and add $file_path as a new version only if $sec has no enabled version
# or its latest payload SHA-256 differs from $file_path. Echoes the resolved numeric version ID.
sm_ensure_and_sync_file() {
  local sec="$1" file_path="$2" current_ver="" need_add=1 cur_file="$tmp/sm_cur.bin"
  if ! sm_secret_exists "$sec"; then
    gcloud secrets create "$sec" --project "$PROJECT" --replication-policy=automatic >/dev/null
  else
    current_ver="$(sm_resolve_version "$sec" latest)"
    if [[ -n "$current_ver" ]]; then
      gcloud secrets versions access "$current_ver" --secret="$sec" --project="$PROJECT" --out-file="$cur_file" \
        || die "failed to access Secret Manager $sec/versions/$current_ver"
      if python3 - "$file_path" "$cur_file" <<'PY'
import hashlib, sys
with open(sys.argv[1], "rb") as a, open(sys.argv[2], "rb") as b:
    sys.exit(0 if hashlib.sha256(a.read()).digest() == hashlib.sha256(b.read()).digest() else 1)
PY
      then
        need_add=0
      fi
      rm -f "$cur_file"
    fi
  fi
  if [[ $need_add -eq 1 ]]; then
    local added
    added="$(gcloud secrets versions add "$sec" --project "$PROJECT" --data-file="$file_path" --format='value(name)')"
    echo "${added##*/}"
  else
    echo "$current_ver"
  fi
}

# Discover domains already configured under CUSTODY=sealed (from an existing Deployment or Kubernetes Secret)
# so redeploying or migrating without re-passing DOMAIN_GOOGLE_CLIENT_FILES preserves them.
discover_existing_sealed_domains() {
  python3 - "$tmp/existing_deploy.json" "$tmp/existing_secret.json" <<'PY'
import json, sys
deploy_path, sec_path = sys.argv[1:3]
found = []
def add(d):
    d = d.strip().lower()
    if d and d not in found:
        found.append(d)

try:
    with open(deploy_path, encoding="utf-8") as f:
        args = ((json.load(f).get("spec") or {}).get("template") or {}).get("spec", {}).get("containers", [{}])[0].get("args") or []
    for a in args:
        if a.startswith("--oauth-domain-google-client-files="):
            for item in a.split("=", 1)[1].split(","):
                if "=" in item:
                    add(item.split("=", 1)[0])
except OSError:
    pass

try:
    with open(sec_path, encoding="utf-8") as f:
        data = json.load(f).get("data") or {}
    for k in sorted(data):
        if k.startswith("google-client-") and k.endswith(".json"):
            add(k[len("google-client-") : -len(".json")])
except OSError:
    pass

print(",".join(found))
PY
}

step "Secrets ($SECRET_STORE)"
if [[ "$SECRET_STORE" == k8s ]]; then
  if [[ $DRY_RUN -eq 1 ]]; then
    echo "(dry run: Kubernetes Secrets unchanged)"
    if [[ "$AUTH" == oauth && "$CUSTODY" == sealed ]] && secret_exists "$OAUTH_SECRET_NAME"; then
      k -n "$NS" get secret "$OAUTH_SECRET_NAME" -o json >"$tmp/existing_secret.json"
      PRESERVED_K8S_DOMAINS="$(discover_existing_sealed_domains)"
      rm -f "$tmp/existing_secret.json"
    fi
  elif [[ "$AUTH" == static ]]; then
    if secret_exists kode-gopher-token; then
      echo "kode-gopher-token exists; keeping it"
    else
      python3 -c 'import secrets; print(secrets.token_urlsafe(32), end="")' \
        | k -n "$NS" create secret generic kode-gopher-token --from-file=token=/dev/stdin
    fi
  else
    if secret_exists "$OAUTH_SECRET_NAME"; then
      # Keep the keyring: replacing it would invalidate every token.
      k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "keyring" "$tmp/keyring" \
        || die "Secret $OAUTH_SECRET_NAME has no keyring entry"
      if [[ -z "${GOOGLE_CLIENT_FILE:-}" ]]; then
        k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "google-client.json" "$tmp/google-client.json" || true
      fi
      # Keep the pre-registered clients too, unless CLIENTS_FILE replaces
      # them: a routine redeploy mustn't drop them.
      if [[ -z "${CLIENTS_FILE:-}" ]]; then
        k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "clients.json" "$tmp/clients.json" || true
      fi
      # Keep any domain-specific Google client files already in the Secret.
      k -n "$NS" get secret "$OAUTH_SECRET_NAME" -o json >"$tmp/existing_secret.json"
      python3 - "$tmp" "$tmp/existing_secret.json" <<'PY'
import base64, json, os, sys
tmp, src = sys.argv[1:3]
with open(src, encoding="utf-8") as f:
    data = json.load(f).get("data") or {}
for k, v in data.items():
    if k.startswith("google-client-") and k.endswith(".json"):
        with open(os.path.join(tmp, k), "wb") as out:
            out.write(base64.b64decode(v))
PY
      rm -f "$tmp/existing_secret.json"
    elif secret_exists kode-gopher-oauth; then
      # Creating a new Secret while kode-gopher-oauth exists: seed the
      # keyring and pre-registered clients from kode-gopher-oauth so
      # existing tokens and client_credentials clients keep working while
      # leaving kode-gopher-oauth intact for rollback.
      k8s_secret_key_to_file kode-gopher-oauth "keyring" "$tmp/keyring" \
        || die "Secret kode-gopher-oauth has no keyring entry"
      if [[ -z "${CLIENTS_FILE:-}" ]]; then
        k8s_secret_key_to_file kode-gopher-oauth "clients.json" "$tmp/clients.json" || true
      fi
      k -n "$NS" get secret kode-gopher-oauth -o json >"$tmp/existing_secret.json"
      python3 - "$tmp" "$tmp/existing_secret.json" <<'PY'
import base64, json, os, sys
tmp, src = sys.argv[1:3]
with open(src, encoding="utf-8") as f:
    data = json.load(f).get("data") or {}
for k, v in data.items():
    if k.startswith("google-client-") and k.endswith(".json"):
        with open(os.path.join(tmp, k), "wb") as out:
            out.write(base64.b64decode(v))
PY
      rm -f "$tmp/existing_secret.json"
    else
      python3 -c 'import base64, secrets; print("k1", base64.b64encode(secrets.token_bytes(32)).decode())' > "$tmp/keyring"
    fi
    if [[ -n "${GOOGLE_CLIENT_FILE:-}" ]]; then
      cp "$GOOGLE_CLIENT_FILE" "$tmp/google-client.json"
    fi
    [[ -s "$tmp/google-client.json" ]] || die "GOOGLE_CLIENT_FILE is required on first deploy"
    if [[ -n "${DOMAIN_GOOGLE_CLIENT_FILES:-}" || -n "${DOMAIN_VAULT_AUTH_PROVIDERS:-}" ]]; then
      python3 - "$tmp" "${DOMAIN_GOOGLE_CLIENT_FILES:-}" "${DOMAIN_VAULT_AUTH_PROVIDERS:-}" <<'PY'
import os, re, shutil, sys
tmp, raw_files, raw_vaults = sys.argv[1], sys.argv[2], sys.argv[3]
domain_re = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$")
for item in raw_files.split(","):
    item = item.strip()
    if not item:
        continue
    if "=" not in item:
        sys.exit(f"DOMAIN_GOOGLE_CLIENT_FILES entry {item!r} must be <domain>=<path>")
    dom, path = item.split("=", 1)
    dom, path = dom.strip().lower(), path.strip()
    if len(dom) > 253 or not domain_re.match(dom):
        sys.exit(f"DOMAIN_GOOGLE_CLIENT_FILES domain {dom!r} isn't a valid domain")
    if not os.path.isfile(path) or os.path.getsize(path) == 0:
        sys.exit(f"DOMAIN_GOOGLE_CLIENT_FILES file for {dom!r} is missing or empty")
    shutil.copyfile(path, os.path.join(tmp, f"google-client-{dom}.json"))
for item in raw_vaults.split(","):
    item = item.strip()
    if not item:
        continue
    if "=" not in item:
        sys.exit(f"DOMAIN_VAULT_AUTH_PROVIDERS entry {item!r} must be <domain>=<provider>")
    dom, val = item.split("=", 1)
    dom, val = dom.strip().lower(), val.strip()
    if len(dom) > 253 or not domain_re.match(dom) or not val:
        sys.exit(f"DOMAIN_VAULT_AUTH_PROVIDERS entry {item!r} has an invalid domain or empty provider")
    cf = os.path.join(tmp, f"google-client-{dom}.json")
    if not os.path.isfile(cf) or os.path.getsize(cf) == 0:
        sys.exit(f"DOMAIN_VAULT_AUTH_PROVIDERS domain {dom!r} requires DOMAIN_GOOGLE_CLIENT_FILES on first deploy")
PY
    fi
    if [[ -n "${CLIENTS_FILE:-}" ]]; then
      cp "$CLIENTS_FILE" "$tmp/clients.json"
    elif [[ ! -s "$tmp/clients.json" ]]; then
      echo '[]' > "$tmp/clients.json"
    fi
    secret_args=(
      --from-file=google-client.json="$tmp/google-client.json"
      --from-file=keyring="$tmp/keyring"
      --from-file=clients.json="$tmp/clients.json"
    )
    preserved_doms=()
    for df in "$tmp"/google-client-*.json; do
      if [[ -f "$df" ]]; then
        base_df="$(basename "$df")"
        secret_args+=(--from-file="${base_df}=$df")
        dom_part="${base_df#google-client-}"
        preserved_doms+=("${dom_part%.json}")
      fi
    done
    if [[ "$CUSTODY" == sealed && ${#preserved_doms[@]} -gt 0 ]]; then
      PRESERVED_K8S_DOMAINS="$(IFS=,; echo "${preserved_doms[*]}")"
    fi
    k -n "$NS" create secret generic "$OAUTH_SECRET_NAME" "${secret_args[@]}" \
      --dry-run=client -o yaml | k apply -f -
    echo "$OAUTH_SECRET_NAME applied (Google client redirect URIs must include $ISSUER/callback)"
  fi

  # kode-gopher reads its Secret only at startup: hash it into the pod
  # template, so a changed token, client list or keyring rolls the pod.
  secret=kode-gopher-token
  [[ "$AUTH" == oauth ]] && secret="$OAUTH_SECRET_NAME"
  CONFIG_HASH="$(k -n "$NS" get secret "$secret" -o jsonpath='{.data}' 2>/dev/null | sha256sum | cut -c1-16)"
else
  # SECRET_STORE=secret-manager
  sm_secrets_to_bind=()
  if [[ "$AUTH" == static ]]; then
    sec="$SECRET_PREFIX"
    sm_secrets_to_bind+=("$sec")
    ver=""
    if [[ -n "${TOKEN_SECRET_VERSION:-}" ]]; then
      ver="$(sm_resolve_version "$sec" "$TOKEN_SECRET_VERSION")"
      [[ -n "$ver" || $DRY_RUN -eq 1 ]] || die "Secret Manager version $sec/versions/$TOKEN_SECRET_VERSION not found"
      ver="${ver:-$TOKEN_SECRET_VERSION}"
    elif [[ $DRY_RUN -eq 1 ]]; then
      ver="$(sm_resolve_version "$sec" latest)"
      ver="${ver:-latest}"
      echo "(dry run: Secret Manager $sec -> versions/$ver)"
    else
      ver="$(sm_resolve_version "$sec" latest)"
      if [[ -z "$ver" ]]; then
        sm_assert_uninitialized "$sec"
        if ! k8s_secret_key_to_file kode-gopher-token "token" "$tmp/token"; then
          python3 - "$tmp/token" <<'PY'
import secrets, sys
with open(sys.argv[1], "w", encoding="utf-8") as f:
    f.write(secrets.token_urlsafe(32))
PY
        fi
        ver="$(sm_ensure_and_sync_file "$sec" "$tmp/token")"
      fi
    fi
    token_uri="sm://projects/${PROJECT}/secrets/${sec}/versions/${ver}"
    echo "static token -> ${token_uri}"
    SM_URIS_JSON="$(python3 -c 'import json, sys; print(json.dumps({"token": sys.argv[1]}))' "$token_uri")"
  else
    [[ -z "${GOOGLE_CLIENT_FILE:-}" || -z "${GOOGLE_CLIENT_SECRET_VERSION:-}" ]] \
      || die "cannot set both GOOGLE_CLIENT_FILE and GOOGLE_CLIENT_SECRET_VERSION"
    [[ -z "${CLIENTS_FILE:-}" || -z "${CLIENTS_SECRET_VERSION:-}" ]] \
      || die "cannot set both CLIENTS_FILE and CLIENTS_SECRET_VERSION"

    sec_gc="${SECRET_PREFIX}-google-client"
    sec_kr="${SECRET_PREFIX}-keyring"
    sec_cl="${SECRET_PREFIX}-clients"
    sm_secrets_to_bind+=("$sec_gc" "$sec_kr" "$sec_cl")

    # 1. Keyring: keep existing Secret Manager version, or migrate from Kubernetes Secret, or mint k1.
    if [[ -n "${KEYRING_SECRET_VERSION:-}" ]]; then
      ver_kr="$(sm_resolve_version "$sec_kr" "$KEYRING_SECRET_VERSION")"
      [[ -n "$ver_kr" || $DRY_RUN -eq 1 ]] || die "Secret Manager version $sec_kr/versions/$KEYRING_SECRET_VERSION not found"
      ver_kr="${ver_kr:-$KEYRING_SECRET_VERSION}"
    elif [[ $DRY_RUN -eq 1 ]]; then
      ver_kr="$(sm_resolve_version "$sec_kr" latest)"
      ver_kr="${ver_kr:-latest}"
    else
      ver_kr="$(sm_resolve_version "$sec_kr" latest)"
      if [[ -z "$ver_kr" ]]; then
        sm_assert_uninitialized "$sec_kr"
        if ! k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "keyring" "$tmp/keyring" \
          && ! k8s_secret_key_to_file kode-gopher-oauth "keyring" "$tmp/keyring"; then
          python3 -c 'import base64, secrets; print("k1", base64.b64encode(secrets.token_bytes(32)).decode())' > "$tmp/keyring"
        fi
        ver_kr="$(sm_ensure_and_sync_file "$sec_kr" "$tmp/keyring")"
      fi
    fi

    # 2. Google OAuth client JSON.
    if [[ -n "${GOOGLE_CLIENT_SECRET_VERSION:-}" ]]; then
      ver_gc="$(sm_resolve_version "$sec_gc" "$GOOGLE_CLIENT_SECRET_VERSION")"
      [[ -n "$ver_gc" || $DRY_RUN -eq 1 ]] || die "Secret Manager version $sec_gc/versions/$GOOGLE_CLIENT_SECRET_VERSION not found"
      ver_gc="${ver_gc:-$GOOGLE_CLIENT_SECRET_VERSION}"
    elif [[ $DRY_RUN -eq 1 ]]; then
      ver_gc="$(sm_resolve_version "$sec_gc" latest)"
      ver_gc="${ver_gc:-latest}"
    elif [[ -n "${GOOGLE_CLIENT_FILE:-}" ]]; then
      [[ -s "$GOOGLE_CLIENT_FILE" ]] || die "GOOGLE_CLIENT_FILE $GOOGLE_CLIENT_FILE is missing or empty"
      ver_gc="$(sm_ensure_and_sync_file "$sec_gc" "$GOOGLE_CLIENT_FILE")"
    else
      ver_gc="$(sm_resolve_version "$sec_gc" latest)"
      if [[ -z "$ver_gc" ]]; then
        sm_assert_uninitialized "$sec_gc"
        k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "google-client.json" "$tmp/google-client.json" \
          || die "GOOGLE_CLIENT_FILE is required on first deploy"
        ver_gc="$(sm_ensure_and_sync_file "$sec_gc" "$tmp/google-client.json")"
      fi
    fi

    # 3. Pre-registered OAuth clients JSON.
    if [[ -n "${CLIENTS_SECRET_VERSION:-}" ]]; then
      ver_cl="$(sm_resolve_version "$sec_cl" "$CLIENTS_SECRET_VERSION")"
      [[ -n "$ver_cl" || $DRY_RUN -eq 1 ]] || die "Secret Manager version $sec_cl/versions/$CLIENTS_SECRET_VERSION not found"
      ver_cl="${ver_cl:-$CLIENTS_SECRET_VERSION}"
    elif [[ $DRY_RUN -eq 1 ]]; then
      ver_cl="$(sm_resolve_version "$sec_cl" latest)"
      ver_cl="${ver_cl:-latest}"
    elif [[ -n "${CLIENTS_FILE:-}" ]]; then
      [[ -s "$CLIENTS_FILE" ]] || die "CLIENTS_FILE $CLIENTS_FILE is missing or empty"
      ver_cl="$(sm_ensure_and_sync_file "$sec_cl" "$CLIENTS_FILE")"
    else
      ver_cl="$(sm_resolve_version "$sec_cl" latest)"
      if [[ -z "$ver_cl" ]]; then
        sm_assert_uninitialized "$sec_cl"
        if ! k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "clients.json" "$tmp/clients.json" \
          && ! k8s_secret_key_to_file kode-gopher-oauth "clients.json" "$tmp/clients.json"; then
          echo '[]' > "$tmp/clients.json"
        fi
        ver_cl="$(sm_ensure_and_sync_file "$sec_cl" "$tmp/clients.json")"
      fi
    fi

    # 4. Per-domain Google client secrets (if configured, or preserved under sealed custody).
    existing_sealed_domains=""
    if [[ "$CUSTODY" == sealed ]]; then
      k -n "$NS" get deployment kode-gopher -o json >"$tmp/existing_deploy.json" 2>/dev/null || rm -f "$tmp/existing_deploy.json"
      if secret_exists "$OAUTH_SECRET_NAME"; then
        k -n "$NS" get secret "$OAUTH_SECRET_NAME" -o json >"$tmp/existing_secret.json"
      elif secret_exists kode-gopher-oauth; then
        k -n "$NS" get secret kode-gopher-oauth -o json >"$tmp/existing_secret.json"
      fi
      existing_sealed_domains="$(discover_existing_sealed_domains)"
      rm -f "$tmp/existing_deploy.json" "$tmp/existing_secret.json"
    fi

    python3 - "$tmp/domain_plan.json" "${DOMAIN_GOOGLE_CLIENT_FILES:-}" "${DOMAIN_VAULT_AUTH_PROVIDERS:-}" "${DOMAIN_GOOGLE_CLIENT_SECRET_VERSIONS:-}" "$existing_sealed_domains" <<'PY'
import json, os, re, sys
dst, raw_files, raw_vaults, raw_vers, raw_preserved = sys.argv[1:6]
domain_re = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$")
ver_re = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$")

def parse_kv(raw, label):
    out = {}
    for item in raw.split(","):
        item = item.strip()
        if not item:
            continue
        if "=" not in item:
            sys.exit(f"{label} entry {item!r} must be <domain>=<value>")
        dom, val = item.split("=", 1)
        dom, val = dom.strip().lower(), val.strip()
        if len(dom) > 253 or not domain_re.match(dom) or not val:
            sys.exit(f"{label} entry {item!r} has an invalid domain or empty value")
        if dom in out:
            sys.exit(f"{label} has duplicate domain {dom!r}")
        out[dom] = val
    return out

files = parse_kv(raw_files, "DOMAIN_GOOGLE_CLIENT_FILES")
vaults = parse_kv(raw_vaults, "DOMAIN_VAULT_AUTH_PROVIDERS")
vers = parse_kv(raw_vers, "DOMAIN_GOOGLE_CLIENT_SECRET_VERSIONS")
for d, p in files.items():
    if d in vers:
        sys.exit(f"domain {d!r} cannot appear in both DOMAIN_GOOGLE_CLIENT_FILES and DOMAIN_GOOGLE_CLIENT_SECRET_VERSIONS")
    if not os.path.isfile(p) or os.path.getsize(p) == 0:
        sys.exit(f"DOMAIN_GOOGLE_CLIENT_FILES file for {d!r} is missing or empty")
for d, v in vers.items():
    if not ver_re.match(v):
        sys.exit(f"DOMAIN_GOOGLE_CLIENT_SECRET_VERSIONS version {v!r} for {d!r} is invalid")

order = []
for src in (files, vaults, vers):
    for d in src:
        if d not in order:
            order.append(d)
for d in raw_preserved.split(","):
    d = d.strip().lower()
    if d and domain_re.match(d) and d not in order:
        order.append(d)

plan = [{"domain": d, "slug": d.replace(".", "_"), "file": files.get(d, ""), "version": vers.get(d, "")} for d in order]
with open(dst, "w", encoding="utf-8") as f:
    json.dump(plan, f)
PY

    python3 - "$tmp/domain_plan.json" >"$tmp/domain_plan.tsv" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    for item in json.load(f):
        print(f"{item['domain']}\t{item['slug']}\t{item['file']}\t{item['version']}")
PY

    : > "$tmp/domain_uris.tsv"
    while IFS=$'\t' read -r dom slug dfile dver; do
      [[ -n "$dom" ]] || continue
      sec_dom="${SECRET_PREFIX}-google-client-${slug}"
      sm_secrets_to_bind+=("$sec_dom")
      if [[ -n "$dver" ]]; then
        resolved_dom="$(sm_resolve_version "$sec_dom" "$dver")"
        [[ -n "$resolved_dom" || $DRY_RUN -eq 1 ]] || die "Secret Manager version $sec_dom/versions/$dver not found"
        resolved_dom="${resolved_dom:-$dver}"
      elif [[ $DRY_RUN -eq 1 ]]; then
        resolved_dom="$(sm_resolve_version "$sec_dom" latest)"
        resolved_dom="${resolved_dom:-latest}"
      elif [[ -n "$dfile" ]]; then
        resolved_dom="$(sm_ensure_and_sync_file "$sec_dom" "$dfile")"
      else
        resolved_dom="$(sm_resolve_version "$sec_dom" latest)"
        if [[ -z "$resolved_dom" ]]; then
          sm_assert_uninitialized "$sec_dom"
          if ! k8s_secret_key_to_file "$OAUTH_SECRET_NAME" "google-client-${dom}.json" "$tmp/dom-${slug}.json" \
            && ! k8s_secret_key_to_file kode-gopher-oauth "google-client-${dom}.json" "$tmp/dom-${slug}.json"; then
            die "domain $dom requires DOMAIN_GOOGLE_CLIENT_FILES on first deploy"
          fi
          resolved_dom="$(sm_ensure_and_sync_file "$sec_dom" "$tmp/dom-${slug}.json")"
        fi
      fi
      printf '%s\tsm://projects/%s/secrets/%s/versions/%s\n' "$dom" "$PROJECT" "$sec_dom" "$resolved_dom" >> "$tmp/domain_uris.tsv"
    done < "$tmp/domain_plan.tsv"

    uri_gc="sm://projects/${PROJECT}/secrets/${sec_gc}/versions/${ver_gc}"
    uri_kr="sm://projects/${PROJECT}/secrets/${sec_kr}/versions/${ver_kr}"
    uri_cl="sm://projects/${PROJECT}/secrets/${sec_cl}/versions/${ver_cl}"
    echo "google-client -> ${uri_gc}"
    echo "keyring       -> ${uri_kr}"
    echo "clients       -> ${uri_cl}"

    SM_URIS_JSON="$(python3 - "$uri_gc" "$uri_kr" "$uri_cl" "$tmp/domain_uris.tsv" <<'PY'
import json, sys
gc, kr, cl, dom_path = sys.argv[1:5]
domains = []
with open(dom_path, encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if line:
            d, u = line.split("\t", 1)
            domains.append([d, u])
print(json.dumps({"google_client": gc, "keyring": kr, "clients": cl, "domains": domains}))
PY
)"
  fi

  if [[ $DRY_RUN -eq 0 ]]; then
    PROJECT_NUMBER="$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')"
    WIF_PRINCIPAL="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT}.svc.id.goog/subject/ns/${NS}/sa/kode-gopher"
    for sec in "${sm_secrets_to_bind[@]}"; do
      gcloud secrets add-iam-policy-binding "$sec" \
        --project "$PROJECT" \
        --member "$WIF_PRINCIPAL" \
        --role "roles/secretmanager.secretAccessor" \
        --condition=None \
        --quiet >/dev/null
    done
    echo "granted roles/secretmanager.secretAccessor on ${#sm_secrets_to_bind[@]} secret(s) to ${WIF_PRINCIPAL}"
  fi

  CONFIG_HASH="$(printf '%s' "$SM_URIS_JSON" | sha256sum | cut -c1-16)"
fi

step "render"
python3 - "$work" "$OVERLAY" "$IMAGE" "$AUTH" "$ISSUER" "$PROJECT" "$APISERVER" "$REGION" "$CONFIG_HASH" "$ADDRESS_NAME" "$CERT_NAME" "$OAUTH_SECRET_NAME" "$SECRET_STORE" "$SM_URIS_JSON" "$PRESERVED_K8S_DOMAINS" <<'PY'
import json, os, re, sys

(
    work, overlay, image, auth, issuer, project, apiserver, region,
    config_hash, address_name, cert_name, oauth_secret_name, secret_store,
    sm_uris_raw, preserved_k8s_domains,
) = sys.argv[1:16]
sm_uris = json.loads(sm_uris_raw)
env = os.environ.get

ops = [{"op": "test", "path": "/spec/egress/2/to/0/ipBlock/cidr", "value": "192.0.2.1/32"},
       {"op": "replace", "path": "/spec/egress/2/to",
        "value": [{"ipBlock": {"cidr": ip + ("/128" if ":" in ip else "/32")}} for ip in apiserver.split()]}]
patches = [{"target": {"kind": "NetworkPolicy", "name": "kode-gopher"}, "patch": json.dumps(ops)}]

ops = [{"op": "replace", "path": "/spec/addresses/0/value", "value": address_name},
       {"op": "replace", "path": "/spec/listeners/0/tls/options/networking.gke.io~1pre-shared-certs", "value": cert_name}]
patches.append({"target": {"kind": "Gateway", "name": "kode-gopher"}, "patch": json.dumps(ops)})

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
    if secret_store == "secret-manager":
        dom_pairs = sm_uris.get("domains") or []
        if dom_pairs:
            args.append("--oauth-domain-google-client-files=" + ",".join(f"{d}={u}" for d, u in dom_pairs))
    else:
        domain_re = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$")
        domain_order = []
        for raw in [env("DOMAIN_GOOGLE_CLIENT_FILES", ""), env("DOMAIN_VAULT_AUTH_PROVIDERS", "")]:
            for item in raw.split(","):
                item = item.strip()
                if not item:
                    continue
                if "=" not in item:
                    sys.exit("domain override %r must be <domain>=<value>" % item)
                dom, val = item.split("=", 1)
                dom, val = dom.strip().lower(), val.strip()
                if len(dom) > 253 or not domain_re.match(dom) or not val:
                    sys.exit("domain override %r has an invalid domain or empty value" % item)
                if dom not in domain_order:
                    domain_order.append(dom)
        for d in preserved_k8s_domains.split(","):
            d = d.strip().lower()
            if d and domain_re.match(d) and d not in domain_order:
                domain_order.append(d)
        if domain_order:
            args.append("--oauth-domain-google-client-files=" + ",".join(
                f"{d}=/etc/kode-gopher/oauth/google-client-{d}.json" for d in domain_order))
    if env("DOMAIN_VAULT_AUTH_PROVIDERS"):
        args.append("--oauth-domain-vault-auth-providers=" + env("DOMAIN_VAULT_AUTH_PROVIDERS"))
elif env("OTEL_ENDPOINT"):
    args.append("--project=" + project)
for flag, var in [("--capture-dir", "CAPTURE_DIR"), ("--capture-mode", "CAPTURE_MODE"),
                  ("--otel-endpoint", "OTEL_ENDPOINT")]:
    if env(var):
        args.append(flag + "=" + env(var))

# The pod template has no annotations of its own, so this sets the map.
ops = [{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": image},
       {"op": "add", "path": "/spec/template/metadata/annotations", "value": {"kode-gopher/config-hash": config_hash}}]
if secret_store == "secret-manager":
    if auth == "static":
        ops += [
            {"op": "test", "path": "/spec/template/spec/containers/0/args/7", "value": "--auth-token-file=/etc/kode-gopher/token/token"},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args/7", "value": "--auth-token-file=" + sm_uris["token"]},
        ]
    else:
        ops += [
            {"op": "test", "path": "/spec/template/spec/containers/0/args/7", "value": "--oauth-google-client-file=/etc/kode-gopher/oauth/google-client.json"},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args/7", "value": "--oauth-google-client-file=" + sm_uris["google_client"]},
            {"op": "test", "path": "/spec/template/spec/containers/0/args/8", "value": "--oauth-keyring-file=/etc/kode-gopher/oauth/keyring"},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args/8", "value": "--oauth-keyring-file=" + sm_uris["keyring"]},
            {"op": "test", "path": "/spec/template/spec/containers/0/args/9", "value": "--oauth-clients-file=/etc/kode-gopher/oauth/clients.json"},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args/9", "value": "--oauth-clients-file=" + sm_uris["clients"]},
        ]
    vol_name = "token" if auth == "static" else "oauth"
    ops += [
        {"op": "test", "path": "/spec/template/spec/containers/0/volumeMounts/0/name", "value": vol_name},
        {"op": "remove", "path": "/spec/template/spec/containers/0/volumeMounts/0"},
        {"op": "test", "path": "/spec/template/spec/volumes/0/name", "value": vol_name},
        {"op": "remove", "path": "/spec/template/spec/volumes/0"},
    ]
elif auth == "oauth":
    ops.append({"op": "replace", "path": "/spec/template/spec/volumes/0/secret/secretName", "value": oauth_secret_name})

if env("SERVICE_ACCOUNT"):
    ops += [{"op": "test", "path": "/spec/template/spec/containers/0/args/5", "value": "--credentials=access-token"},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args/5", "value": "--credentials=service"}]
    args.append("--service-account=" + env("SERVICE_ACCOUNT"))
ops += [{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": a} for a in args]
patches.append({"target": {"kind": "Deployment", "name": "kode-gopher"}, "patch": json.dumps(ops)})

# JSON is YAML.
with open(os.path.join(work, "kustomization.yaml"), "w", encoding="utf-8") as f:
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
echo "certificate:  $(gcloud compute ssl-certificates describe "$CERT_NAME" --global --project "$PROJECT" --format='value(managed.status)')"
echo "  (PROVISIONING until $KG_HOST resolves to $IP and the load balancer is up; up to an hour)"
echo "test:         scripts/smoketest-http.sh            (through the load balancer)"
echo "              scripts/smoketest-http.sh --port-forward"
