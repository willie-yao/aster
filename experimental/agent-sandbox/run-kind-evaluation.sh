#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
VERSION=v0.5.3
ASSET_SHA256=50f54b0e746376455ae6bb8b90b436bdd8798e1296cff0d72b6267bbeb858e3c
RUN_ID=$(date -u +%Y%m%d%H%M%S)-$$
CLUSTER="pad-agent-sandbox-prod-${RUN_ID}"
DASHBOARD_NAMESPACE=dashboard-test
EXECUTION_NAMESPACE="pad-fix-prod-${RUN_ID}"
EXECUTOR_REPOSITORY=aster/agent-sandbox-fix-executor
EXECUTOR_TAG=production-eval
EXECUTOR_IMAGE="${EXECUTOR_REPOSITORY}:${EXECUTOR_TAG}"
EXECUTOR_BASE_IMAGE="${EXECUTOR_REPOSITORY}:${EXECUTOR_TAG}-base"
GATEWAY_IMAGE=aster/fake-model-gateway:production-eval
RUNTIME_CLASS=runc-test
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/pad-agent-sandbox-prod.XXXXXX")
EVIDENCE_DIR=${EVIDENCE_DIR:-"${TMPDIR:-/tmp}/prow-ai-agent-sandbox-v053-production-evidence-${RUN_ID}"}
ADMIN_KUBECONFIG="$TMP_DIR/admin.kubeconfig"
RUNTIME_KUBECONFIG="$TMP_DIR/runtime.kubeconfig"
ASSET="$TMP_DIR/sandbox.yaml"
TEST_LOG="$EVIDENCE_DIR/primary-test.log"
umask 077
if [[ -e "$EVIDENCE_DIR" ]]; then
  echo "evidence directory already exists and will not be overwritten: $EVIDENCE_DIR" >&2
  exit 1
fi
mkdir "$EVIDENCE_DIR"

cleanup() {
  rc=$?
  set +e
  {
    echo "cleanup_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    kind delete cluster --name "$CLUSTER"
    if kind get clusters | grep -Fxq "$CLUSTER"; then
      echo "cluster_deleted=false"
    else
      echo "cluster_deleted=true"
    fi
    python3 - "$ADMIN_KUBECONFIG" "$RUNTIME_KUBECONFIG" <<'PY'
from pathlib import Path
import sys
for value in sys.argv[1:]:
    path=Path(value)
    if path.exists(): path.unlink()
PY
    echo "kubeconfigs_deleted=true"
    echo "cleanup_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >>"$EVIDENCE_DIR/cleanup.txt" 2>&1
  python3 - "$TMP_DIR" <<'PY'
from pathlib import Path
import shutil,sys
path=Path(sys.argv[1])
if path.exists(): shutil.rmtree(path)
PY
  exit "$rc"
}
trap cleanup EXIT INT TERM

for tool in cp curl docker git go helm kind kubectl openssl python3 shasum; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done
docker info >/dev/null
unset AI_TOKEN OPENAI_API_KEY ANTHROPIC_API_KEY CLAUDE_API_KEY KIMI_API_KEY FIX_TOKEN BOT_TOKEN ORKA_API_TOKEN GITHUB_TOKEN GH_TOKEN COPILOT_TOKEN AZURE_OPENAI_API_KEY || true

CA_CERT="$TMP_DIR/gateway-ca.crt"
CA_KEY="$TMP_DIR/gateway-ca.key"
TLS_CERT="$TMP_DIR/gateway-tls.crt"
TLS_KEY="$TMP_DIR/gateway-tls.key"
GATEWAY_DNS="fake-model-gateway.${EXECUTION_NAMESPACE}.svc.cluster.local"
openssl genrsa -out "$CA_KEY" 2048 >/dev/null 2>&1
openssl req -x509 -new -nodes -key "$CA_KEY" -sha256 -days 1 -subj "/CN=Agent Sandbox Fixture CA" -out "$CA_CERT" >/dev/null 2>&1
openssl genrsa -out "$TLS_KEY" 2048 >/dev/null 2>&1
cat >"$TMP_DIR/gateway-cert.cnf" <<CERT
[req]
distinguished_name=req_distinguished_name
req_extensions=v3_req
prompt=no
[req_distinguished_name]
CN=$GATEWAY_DNS
[v3_req]
subjectAltName=@alt_names
[alt_names]
DNS.1=$GATEWAY_DNS
DNS.2=fake-model-gateway.${EXECUTION_NAMESPACE}.svc
CERT
openssl req -new -key "$TLS_KEY" -out "$TMP_DIR/gateway-tls.csr" -config "$TMP_DIR/gateway-cert.cnf" >/dev/null 2>&1
openssl x509 -req -in "$TMP_DIR/gateway-tls.csr" -CA "$CA_CERT" -CAkey "$CA_KEY" -CAcreateserial -out "$TLS_CERT" -days 1 -sha256 -extensions v3_req -extfile "$TMP_DIR/gateway-cert.cnf" >/dev/null 2>&1

cat >"$EVIDENCE_DIR/run-metadata.txt" <<META
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
base_commit=$(git -C "$ROOT" rev-parse HEAD)
agent_sandbox_version=$VERSION
agent_sandbox_asset_sha256=$ASSET_SHA256
cluster=$CLUSTER
execution_namespace=$EXECUTION_NAMESPACE
runtime_class=$RUNTIME_CLASS
kind_version=$(kind version)
kubectl_client=$(kubectl version --client=true -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["clientVersion"]["gitVersion"])')
go_version=$(go version)
META

curl -fsSL "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox.yaml" -o "$ASSET"
echo "$ASSET_SHA256  $ASSET" | shasum -a 256 -c - | tee "$EVIDENCE_DIR/release-asset-check.txt"

cd "$ROOT"
docker build --progress=plain --target agent-sandbox-fix-executor -t "$EXECUTOR_BASE_IMAGE" . >"$EVIDENCE_DIR/executor-image-build.log"
cat >"$TMP_DIR/executor-ca.Dockerfile" <<'DOCKERFILE'
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
USER root
COPY gateway-ca.crt /usr/local/share/ca-certificates/agent-sandbox-fixture.crt
RUN update-ca-certificates
USER 65532:65532
DOCKERFILE
docker build --progress=plain --build-arg BASE_IMAGE="$EXECUTOR_BASE_IMAGE" -t "$EXECUTOR_IMAGE" -f "$TMP_DIR/executor-ca.Dockerfile" "$TMP_DIR" >>"$EVIDENCE_DIR/executor-image-build.log"
docker build --progress=plain -f experimental/agent-sandbox/fake-gateway.Dockerfile -t "$GATEWAY_IMAGE" . >"$EVIDENCE_DIR/gateway-image-build.log"
EXECUTOR_DIGEST=$(docker image inspect "$EXECUTOR_IMAGE" --format '{{.Id}}')
docker image inspect "$EXECUTOR_IMAGE" --format '{{json .RepoTags}} {{.Id}} {{.Architecture}} {{.Os}} {{json .Config.User}}' >"$EVIDENCE_DIR/executor-image.txt"
docker image inspect "$GATEWAY_IMAGE" --format '{{json .RepoTags}} {{.Id}} {{.Architecture}} {{.Os}} {{json .Config.User}}' >"$EVIDENCE_DIR/gateway-image.txt"
echo "executor_digest=$EXECUTOR_DIGEST" >>"$EVIDENCE_DIR/run-metadata.txt"

kind create cluster --name "$CLUSTER" --kubeconfig "$ADMIN_KUBECONFIG" --wait 120s | tee "$EVIDENCE_DIR/kind-create.log"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$ASSET" | tee "$EVIDENCE_DIR/agent-sandbox-install.log"
crd_established=""
for _ in $(seq 1 120); do
  crd_established=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" get crd sandboxes.agents.x-k8s.io -o jsonpath='{range .status.conditions[?(@.type=="Established")]}{.status}{end}' 2>/dev/null || true)
  [[ "$crd_established" == "True" ]] && break
  sleep 1
done
if [[ "$crd_established" != "True" ]]; then
  kubectl --kubeconfig "$ADMIN_KUBECONFIG" get crd sandboxes.agents.x-k8s.io -o yaml >"$EVIDENCE_DIR/crd-establishment-failure.yaml" 2>&1 || true
  echo "Agent Sandbox CRD did not become Established" >&2
  exit 1
fi
echo "crd_established=true" >"$EVIDENCE_DIR/crd-established.txt"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
kind load docker-image --name "$CLUSTER" "$EXECUTOR_IMAGE" "$GATEWAY_IMAGE" | tee "$EVIDENCE_DIR/kind-image-load.log"
# kind registers the loaded tag but not its digest-qualified name. Add a local
# containerd alias so the immutable image reference resolves without a registry.
docker exec "${CLUSTER}-control-plane" ctr -n k8s.io images tag \
  "docker.io/${EXECUTOR_IMAGE}" "docker.io/${EXECUTOR_REPOSITORY}@${EXECUTOR_DIGEST}" \
  >"$EVIDENCE_DIR/kind-image-alias.txt"

kubectl --kubeconfig "$ADMIN_KUBECONFIG" create namespace "$DASHBOARD_NAMESPACE"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" create namespace "$EXECUTION_NAMESPACE"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" label namespace "$EXECUTION_NAMESPACE" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" create secret tls fake-model-gateway-tls \
  --cert="$TLS_CERT" --key="$TLS_KEY" >/dev/null
cat <<YAML | kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: $RUNTIME_CLASS
handler: runc
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: fake-model-gateway
  namespace: $EXECUTION_NAMESPACE
automountServiceAccountToken: false
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fake-model-gateway
  namespace: $EXECUTION_NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels: {app: fake-model-gateway}
  template:
    metadata:
      labels: {app: fake-model-gateway}
    spec:
      runtimeClassName: $RUNTIME_CLASS
      serviceAccountName: fake-model-gateway
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
      - name: gateway
        image: $GATEWAY_IMAGE
        imagePullPolicy: IfNotPresent
        ports: [{name: https, containerPort: 8443}]
        securityContext:
          runAsNonRoot: true
          runAsUser: 65532
          runAsGroup: 65532
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities: {drop: [ALL]}
          seccompProfile: {type: RuntimeDefault}
        env:
        - {name: TLS_CERT_FILE, value: /tls/tls.crt}
        - {name: TLS_KEY_FILE, value: /tls/tls.key}
        resources:
          requests: {cpu: 10m, memory: 32Mi}
          limits: {cpu: 100m, memory: 64Mi}
        readinessProbe: {httpGet: {path: /healthz, port: https, scheme: HTTPS}}
        volumeMounts:
        - {name: tmp, mountPath: /tmp}
        - {name: tls, mountPath: /tls, readOnly: true}
      volumes:
      - name: tmp
        emptyDir: {sizeLimit: 16Mi}
      - name: tls
        secret: {secretName: fake-model-gateway-tls}
---
apiVersion: v1
kind: Service
metadata:
  name: fake-model-gateway
  namespace: $EXECUTION_NAMESPACE
spec:
  type: ClusterIP
  selector: {app: fake-model-gateway}
  ports: [{name: https, port: 8443, targetPort: https}]
YAML
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" rollout status deployment/fake-model-gateway --timeout=180s

cat >"$TMP_DIR/chart-values.yaml" <<VALUES
mode: watch
project:
  config: |
    id: production-eval
    name: Production Evaluation
    testgrid:
      dashboard: production-eval
    storage:
      provider: local
      base: /tmp
    branding:
      title: Production Evaluation
      base_path: /
      site_url: https://example.test
      source_repo:
        owner: octocat
        name: Hello-World
    ai:
      fix_prs:
        enabled: true
        author_name: Fixture
        author_email: fixture@example.test
        max_files: 1
        critique_retries: 0
        agent_runtime:
          type: agent-sandbox
          max_turns: 5
          allow_bash: false
          timeout: 5m
          output_limit_bytes: 262144
          allowed_commands:
            - argv: [git, diff, --cached, --check]
              timeout: 30s
          model_provider:
            credential_mode: gateway
            api: chat_completions
            endpoint: https://fake-model-gateway.${EXECUTION_NAMESPACE}.svc.cluster.local:8443/v1/chat/completions
            model: fixture-model
            auth:
              type: none
  systemPrompt: production evaluation prompt
agentSandbox:
  fixRuntime:
    enabled: true
    namespace: $EXECUTION_NAMESPACE
    runtimeClassName: $RUNTIME_CLASS
    image:
      repository: $EXECUTOR_REPOSITORY
      digest: $EXECUTOR_DIGEST
      pullPolicy: IfNotPresent
    workloadServiceAccount:
      create: true
      name: fix-workload
    modelProvider:
      credentialMode: gateway
      api: chat_completions
      endpoint: https://fake-model-gateway.${EXECUTION_NAMESPACE}.svc.cluster.local:8443/v1/chat/completions
      model: fixture-model
      auth:
        type: none
        existingSecret: ""
        tokenKey: ""
      publicCAPrivateDNS: false
    maxSteps: 5
    maxFiles: 1
    timeout: 5m
    outputLimitBytes: 262144
    allowedCommands:
      - argv: [git, diff, --cached, --check]
        timeout: 30s
    pollInterval: 200ms
    resources:
      requests: {cpu: 100m, memory: 128Mi, ephemeral-storage: 256Mi}
      limits: {cpu: "1", memory: 512Mi, ephemeral-storage: 256Mi}
  rbac:
    create: true
    fixClientServiceAccountName: ""
    scheduledClientServiceAccountName: ""
VALUES
helm template production-eval deploy/helm/aster -n "$DASHBOARD_NAMESPACE" -f "$TMP_DIR/chart-values.yaml" --show-only templates/agent-sandbox-fix-runtime-rbac.yaml >"$TMP_DIR/rbac.yaml"
helm template production-eval deploy/helm/aster -n "$DASHBOARD_NAMESPACE" -f "$TMP_DIR/chart-values.yaml" --show-only templates/agent-sandbox-fix-runtime-admission.yaml >"$TMP_DIR/admission-production.yaml"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$TMP_DIR/rbac.yaml"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$TMP_DIR/admission-production.yaml"

wait_policy_typecheck() {
  local json=$1 yaml=$2
  for _ in $(seq 1 60); do
    kubectl --kubeconfig "$ADMIN_KUBECONFIG" get validatingadmissionpolicy -l app.kubernetes.io/instance=production-eval -o json >"$json"
    if python3 - "$json" <<'PYTYPE'
import json,sys
value=json.load(open(sys.argv[1])); items=value.get('items',[])
if len(items) != 1: raise SystemExit(1)
item=items[0]; status=item.get('status',{})
if status.get('observedGeneration') != item.get('metadata',{}).get('generation'): raise SystemExit(1)
if status.get('typeChecking',{}).get('expressionWarnings',[]): raise SystemExit(1)
PYTYPE
    then
      kubectl --kubeconfig "$ADMIN_KUBECONFIG" get validatingadmissionpolicy -l app.kubernetes.io/instance=production-eval -o yaml >"$yaml"
      return 0
    fi
    sleep 1
  done
  return 1
}
wait_policy_typecheck "$EVIDENCE_DIR/admission-policy-production.json" "$EVIDENCE_DIR/admission-policy-production.yaml"

FIXTURE_DIR="$TMP_DIR/evaluation-fixtures"
mkdir -p "$FIXTURE_DIR"
(
  cd "$ROOT/backend"
  AGENT_SANDBOX_EVALUATION_FIXTURES=1 \
  AGENT_SANDBOX_EVALUATION_FIXTURE_DIR="$FIXTURE_DIR" \
  AGENT_SANDBOX_NAMESPACE="$EXECUTION_NAMESPACE" \
  AGENT_SANDBOX_IMAGE="${EXECUTOR_REPOSITORY}@${EXECUTOR_DIGEST}" \
  AGENT_SANDBOX_RUNTIME_CLASS="$RUNTIME_CLASS" \
  AGENT_SANDBOX_TEST_GATEWAY_ENDPOINT="https://fake-model-gateway.${EXECUTION_NAMESPACE}.svc.cluster.local:8443/v1/chat/completions" \
  go test ./internal/fixruntime -run '^(TestWriteAgentSandboxEvaluationFixtures|TestAgentSandboxPreflightAndSandboxWorkloadParity)$' -count=1 -v
) >"$EVIDENCE_DIR/workload-shape-parity-test.log" 2>&1

CLIENT_SA=production-eval-prow-ai-dashboard-agent-sandbox-fix-client
requester="system:serviceaccount:${DASHBOARD_NAMESPACE}:${CLIENT_SA}"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" create --dry-run=server --as="$requester" -f "$FIXTURE_DIR/production-sandbox.json" -o json >"$EVIDENCE_DIR/production-apparmor-sandbox-dry-run.json"
python3 - "$FIXTURE_DIR/production-sandbox.json" "$FIXTURE_DIR/local-sandbox.json" "$FIXTURE_DIR/local-preflight-pod.json" <<'PYSHAPE'
import copy,json,sys
production,local,preflight=(json.load(open(path)) for path in sys.argv[1:])
prod_pod=production['spec']['podTemplate']['spec']
local_pod=local['spec']['podTemplate']['spec']
preflight_pod=preflight['spec']
assert prod_pod['securityContext']['appArmorProfile']['type']=='RuntimeDefault'
assert prod_pod['containers'][0]['securityContext']['appArmorProfile']['type']=='RuntimeDefault'
assert 'appArmorProfile' not in local_pod['securityContext']
assert 'appArmorProfile' not in local_pod['containers'][0]['securityContext']
assert local_pod == preflight_pod, (local_pod,preflight_pod)
normalized=copy.deepcopy(prod_pod)
normalized['securityContext'].pop('appArmorProfile')
normalized['containers'][0]['securityContext'].pop('appArmorProfile')
assert normalized == local_pod, (normalized,local_pod)
PYSHAPE
cat >"$EVIDENCE_DIR/workload-shape-parity.txt" <<'META'
production_apparmor=RuntimeDefault
local_kind_apparmor=omitted
preflight_sandbox_pod_spec_equal=true
apparmor_enforcement_tested=false
META

python3 - "$TMP_DIR/admission-production.yaml" "$TMP_DIR/admission-local-kind.yaml" <<'PYADMISSION'
from pathlib import Path
import sys
source=Path(sys.argv[1]).read_text()
replacements={
  "variables.pod.securityContext.appArmorProfile.type == 'RuntimeDefault'": "!has(variables.pod.securityContext.appArmorProfile)",
  "variables.container.securityContext.appArmorProfile.type == 'RuntimeDefault'": "!has(variables.container.securityContext.appArmorProfile)",
}
for old,new in replacements.items():
    if source.count(old) != 1: raise SystemExit(f'unexpected production AppArmor predicate count for {old!r}')
    source=source.replace(old,new)
Path(sys.argv[2]).write_text(source)
PYADMISSION
diff -u "$TMP_DIR/admission-production.yaml" "$TMP_DIR/admission-local-kind.yaml" >"$EVIDENCE_DIR/admission-local-kind.diff" || true
kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$TMP_DIR/admission-local-kind.yaml"
wait_policy_typecheck "$EVIDENCE_DIR/admission-policy-local-kind.json" "$EVIDENCE_DIR/admission-policy-local-kind.yaml"

local_admission_ready=false
for _ in $(seq 1 60); do
  if kubectl --kubeconfig "$ADMIN_KUBECONFIG" create --dry-run=server --as="$requester" -f "$FIXTURE_DIR/local-sandbox.json" -o json >"$EVIDENCE_DIR/admission-valid-object.json" 2>"$EVIDENCE_DIR/admission-valid-retry.txt"; then
    local_admission_ready=true
    break
  fi
  sleep 1
done
if [[ "$local_admission_ready" != true ]]; then
  cat "$EVIDENCE_DIR/admission-valid-retry.txt" >&2
  echo "local kind admission policy did not become active" >&2
  exit 1
fi
echo "local_kind_adapter_sandbox_server_dry_run=passed" >"$EVIDENCE_DIR/admission-valid.txt"
python3 - "$FIXTURE_DIR/local-sandbox.json" "$TMP_DIR/unsafe-sandbox.json" "$TMP_DIR/unsafe-secret.json" "$TMP_DIR/unsafe-probe.json" "$TMP_DIR/unsafe-claim.json" "$TMP_DIR/unsafe-apparmor.json" <<'PYMUTATE'
import copy,json,sys
source=json.load(open(sys.argv[1]))
host=copy.deepcopy(source); host['spec']['podTemplate']['spec']['hostNetwork']=True; json.dump(host,open(sys.argv[2],'w'))
secret=copy.deepcopy(source); secret['spec']['podTemplate']['spec']['volumes'][0]['secret']={'secretName':'forbidden'}; json.dump(secret,open(sys.argv[3],'w'))
probe=copy.deepcopy(source); probe['spec']['podTemplate']['spec']['containers'][0]['livenessProbe']={'exec':{'command':['sh','-c','id']}}; json.dump(probe,open(sys.argv[4],'w'))
claim=copy.deepcopy(source); claim['spec']['podTemplate']['spec']['resourceClaims']=[{'name':'accelerator','resourceClaimName':'forbidden'}]; json.dump(claim,open(sys.argv[5],'w'))
apparmor=copy.deepcopy(source); apparmor['spec']['podTemplate']['spec']['containers'][0]['securityContext']['appArmorProfile']={'type':'Unconfined'}; json.dump(apparmor,open(sys.argv[6],'w'))
PYMUTATE
for mutation in sandbox secret probe claim apparmor; do
  if kubectl --kubeconfig "$ADMIN_KUBECONFIG" create --dry-run=server --as="$requester" -f "$TMP_DIR/unsafe-${mutation}.json" >"$EVIDENCE_DIR/admission-unsafe-${mutation}.txt" 2>&1; then
    echo "unsafe ${mutation} Sandbox passed admission" >&2
    exit 1
  fi
done
grep -Fq 'must not join host namespaces' "$EVIDENCE_DIR/admission-unsafe-sandbox.txt"
grep -Fq 'storage must use only bounded emptyDir volumes' "$EVIDENCE_DIR/admission-unsafe-secret.txt"
grep -Fq 'executor process shape is fixed' "$EVIDENCE_DIR/admission-unsafe-probe.txt"
grep -Fq 'must not override placement, scheduling, devices' "$EVIDENCE_DIR/admission-unsafe-claim.txt"
grep -Fq 'container security context is fixed' "$EVIDENCE_DIR/admission-unsafe-apparmor.txt"

# Re-render the same policy in direct bearer mode and exercise every credential
# mutation without creating or reading a provider Secret value.
python3 - "$TMP_DIR/chart-values.yaml" "$TMP_DIR/chart-values-direct-bearer.yaml" <<'PYDIRECTVALUES'
from pathlib import Path
import sys
text=Path(sys.argv[1]).read_text()
text=text.replace('            credential_mode: gateway\n', '            credential_mode: direct\n')
text=text.replace('              type: none\n', '              type: bearer\n')
text=text.replace('      credentialMode: gateway\n', '      credentialMode: direct\n')
text=text.replace('        type: none\n        existingSecret: ""\n        tokenKey: ""\n', '        type: bearer\n        existingSecret: agent-sandbox-model\n        tokenKey: AI_TOKEN\n')
Path(sys.argv[2]).write_text(text)
PYDIRECTVALUES
helm template production-eval deploy/helm/aster -n "$DASHBOARD_NAMESPACE" -f "$TMP_DIR/chart-values-direct-bearer.yaml" --show-only templates/agent-sandbox-fix-runtime-admission.yaml >"$TMP_DIR/admission-direct-production.yaml"
python3 - "$TMP_DIR/admission-direct-production.yaml" "$TMP_DIR/admission-direct-local-kind.yaml" <<'PYDIRECTADMISSION'
from pathlib import Path
import sys
source=Path(sys.argv[1]).read_text()
replacements={
  "variables.pod.securityContext.appArmorProfile.type == 'RuntimeDefault'": "!has(variables.pod.securityContext.appArmorProfile)",
  "variables.container.securityContext.appArmorProfile.type == 'RuntimeDefault'": "!has(variables.container.securityContext.appArmorProfile)",
}
for old,new in replacements.items():
    if source.count(old) != 1: raise SystemExit(f'unexpected direct AppArmor predicate count for {old!r}')
    source=source.replace(old,new)
Path(sys.argv[2]).write_text(source)
PYDIRECTADMISSION
kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$TMP_DIR/admission-direct-local-kind.yaml"
wait_policy_typecheck "$EVIDENCE_DIR/admission-policy-direct.json" "$EVIDENCE_DIR/admission-policy-direct.yaml"
python3 - "$FIXTURE_DIR/local-sandbox.json" "$TMP_DIR/direct-valid.json" "$TMP_DIR/direct-wrong-secret.json" "$TMP_DIR/direct-wrong-key.json" "$TMP_DIR/direct-wrong-env.json" "$TMP_DIR/direct-envfrom.json" "$TMP_DIR/direct-projected.json" "$TMP_DIR/direct-extra-secret.json" <<'PYDIRECTMUTATE'
import copy,json,sys
source=json.load(open(sys.argv[1]))
container=source['spec']['podTemplate']['spec']['containers'][0]
container['env'].append({'name':'PROW_AI_MODEL_PROVIDER_TOKEN','valueFrom':{'secretKeyRef':{'name':'agent-sandbox-model','key':'AI_TOKEN'}}})
json.dump(source,open(sys.argv[2],'w'))
wrong_secret=copy.deepcopy(source); wrong_secret['spec']['podTemplate']['spec']['containers'][0]['env'][1]['valueFrom']['secretKeyRef']['name']='other-secret'; json.dump(wrong_secret,open(sys.argv[3],'w'))
wrong_key=copy.deepcopy(source); wrong_key['spec']['podTemplate']['spec']['containers'][0]['env'][1]['valueFrom']['secretKeyRef']['key']='OTHER_TOKEN'; json.dump(wrong_key,open(sys.argv[4],'w'))
wrong_env=copy.deepcopy(source); wrong_env['spec']['podTemplate']['spec']['containers'][0]['env'][1]['name']='OTHER_TOKEN'; json.dump(wrong_env,open(sys.argv[5],'w'))
envfrom=copy.deepcopy(source); envfrom['spec']['podTemplate']['spec']['containers'][0]['envFrom']=[{'secretRef':{'name':'other-secret'}}]; json.dump(envfrom,open(sys.argv[6],'w'))
projected=copy.deepcopy(source); projected['spec']['podTemplate']['spec']['volumes'].append({'name':'projected-credential','projected':{'sources':[{'secret':{'name':'other-secret'}}]}}); json.dump(projected,open(sys.argv[7],'w'))
extra=copy.deepcopy(source); extra['spec']['podTemplate']['spec']['containers'][0]['env'].append({'name':'EXTRA_TOKEN','valueFrom':{'secretKeyRef':{'name':'other-secret','key':'token'}}}); json.dump(extra,open(sys.argv[8],'w'))
PYDIRECTMUTATE
kubectl --kubeconfig "$ADMIN_KUBECONFIG" create --dry-run=server --as="$requester" -f "$TMP_DIR/direct-valid.json" -o json >"$EVIDENCE_DIR/admission-direct-valid.json"
for mutation in wrong-secret wrong-key wrong-env envfrom extra-secret; do
  if kubectl --kubeconfig "$ADMIN_KUBECONFIG" create --dry-run=server --as="$requester" -f "$TMP_DIR/direct-${mutation}.json" >"$EVIDENCE_DIR/admission-direct-${mutation}.txt" 2>&1; then
    echo "unsafe direct credential mutation passed admission: $mutation" >&2
    exit 1
  fi
  grep -Fq 'environment must match the configured request and provider credential' "$EVIDENCE_DIR/admission-direct-${mutation}.txt"
done
if kubectl --kubeconfig "$ADMIN_KUBECONFIG" create --dry-run=server --as="$requester" -f "$TMP_DIR/direct-projected.json" >"$EVIDENCE_DIR/admission-direct-projected.txt" 2>&1; then
  echo 'projected direct credential passed admission' >&2
  exit 1
fi
grep -Fq 'storage must use only bounded emptyDir volumes' "$EVIDENCE_DIR/admission-direct-projected.txt"

# Restore the gateway policy before executing the tokenless lifecycle fixture.
kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$TMP_DIR/admission-local-kind.yaml"
wait_policy_typecheck "$EVIDENCE_DIR/admission-policy-local-kind-restored.json" "$EVIDENCE_DIR/admission-policy-local-kind-restored.yaml"

kubectl --kubeconfig "$ADMIN_KUBECONFIG" apply -f "$FIXTURE_DIR/local-preflight-pod.json"
preflight_phase=""
for _ in $(seq 1 360); do
  preflight_phase=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" get pod immutable-image-preflight -o jsonpath='{.status.phase}')
  case "$preflight_phase" in Succeeded|Failed) break;; esac
  sleep 1
done
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" get pod immutable-image-preflight -o wide >"$EVIDENCE_DIR/immutable-image-preflight.txt"
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" logs immutable-image-preflight >"$EVIDENCE_DIR/immutable-image-preflight-result.json" 2>"$EVIDENCE_DIR/immutable-image-preflight-stderr.txt" || true
if [[ "$preflight_phase" != Succeeded ]]; then
  kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" describe pod immutable-image-preflight >>"$EVIDENCE_DIR/immutable-image-preflight.txt" 2>&1 || true
  echo "immutable executor entrypoint preflight failed before Sandbox creation" >&2
  exit 1
fi
python3 - "$EVIDENCE_DIR/immutable-image-preflight-result.json" <<'PYCHECK'
import json,sys
result=json.load(open(sys.argv[1]))
assert result['terminal_state']=='succeeded',result
assert result['base_sha']=='7fd1a60b01f91b314f59955a4e4d4e80d8edf11d',result
assert result['changed_files']==['README'],result
assert result['files']['README']=='Hello Agent Sandbox!\n',result
assert result['command_results'][-1]['argv']==['git','diff','--cached','--check'],result
assert result['command_results'][-1]['exit_code']==0,result
PYCHECK
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" delete pod immutable-image-preflight --wait=true >/dev/null

{
  echo "sandbox_create=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" auth can-i create sandboxes.agents.x-k8s.io --as "system:serviceaccount:${DASHBOARD_NAMESPACE}:${CLIENT_SA}" -n "$EXECUTION_NAMESPACE")"
  echo "pod_logs_get=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" auth can-i get pods/log --as "system:serviceaccount:${DASHBOARD_NAMESPACE}:${CLIENT_SA}" -n "$EXECUTION_NAMESPACE")"
  echo "pod_create=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" auth can-i create pods --as "system:serviceaccount:${DASHBOARD_NAMESPACE}:${CLIENT_SA}" -n "$EXECUTION_NAMESPACE")"
  echo "secret_get=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" auth can-i get secrets --as "system:serviceaccount:${DASHBOARD_NAMESPACE}:${CLIENT_SA}" -n "$EXECUTION_NAMESPACE")"
  echo "workload_secret_get=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" auth can-i get secrets --as "system:serviceaccount:${EXECUTION_NAMESPACE}:fix-workload" -n "$EXECUTION_NAMESPACE")"
} >"$EVIDENCE_DIR/rbac-check.txt"
for expected in \
  sandbox_create=yes \
  pod_logs_get=yes \
  pod_create=no \
  secret_get=no \
  workload_secret_get=no; do
  grep -Fxq "$expected" "$EVIDENCE_DIR/rbac-check.txt" || { echo "RBAC preflight failed: expected $expected" >&2; exit 1; }
done
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" get sandboxes -o name >"$EVIDENCE_DIR/pre-primary-sandboxes.txt"
if [[ -s "$EVIDENCE_DIR/pre-primary-sandboxes.txt" ]]; then
  echo "a primary Sandbox already exists in the fresh evaluation namespace" >&2
  exit 1
fi

if [[ "${AGENT_SANDBOX_PREFLIGHT_ONLY:-0}" == "1" ]]; then
  cat >>"$EVIDENCE_DIR/run-metadata.txt" <<META
finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
preflight_only=true
immutable_image_preflight=passed
workload_shape_parity=passed
production_apparmor_dry_run=passed
local_kind_apparmor_requested=false
apparmor_enforcement_tested=false
admission_preflight=passed
rbac_preflight=passed
primary_fixture_runs=0
META
  echo "Agent Sandbox production preflight passed. Evidence: $EVIDENCE_DIR"
  exit 0
fi

PRIMARY_AUTHORIZATION_ID=${AGENT_SANDBOX_PRIMARY_AUTHORIZATION_ID:-}
if [[ ! "$PRIMARY_AUTHORIZATION_ID" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]]; then
  echo "AGENT_SANDBOX_PRIMARY_AUTHORIZATION_ID is required for a primary run and must be a lowercase identifier" >&2
  exit 1
fi
PRIMARY_ATTEMPT_GUARD="${TMPDIR:-/tmp}/prow-ai-agent-sandbox-v053-primary-${PRIMARY_AUTHORIZATION_ID}.attempted"
if ! (set -o noclobber; printf 'authorization_id=%s\nbase_commit=%s\nevidence_dir=%s\nstarted=%s\n' \
  "$PRIMARY_AUTHORIZATION_ID" "$(git -C "$ROOT" rev-parse HEAD)" "$EVIDENCE_DIR" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$PRIMARY_ATTEMPT_GUARD") 2>/dev/null; then
  echo "primary authorization ID was already consumed: $PRIMARY_AUTHORIZATION_ID" >&2
  exit 1
fi
cp "$PRIMARY_ATTEMPT_GUARD" "$EVIDENCE_DIR/primary-attempt.txt"
echo "primary_authorization_id=$PRIMARY_AUTHORIZATION_ID" >>"$EVIDENCE_DIR/run-metadata.txt"
echo "primary_attempts=1" >>"$EVIDENCE_DIR/run-metadata.txt"

server=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')
ca_data=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
token=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$DASHBOARD_NAMESPACE" create token "$CLIENT_SA" --duration=15m)
cat >"$RUNTIME_KUBECONFIG" <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
- name: disposable
  cluster:
    server: $server
    certificate-authority-data: $ca_data
users:
- name: agent-sandbox-fix-client
  user:
    token: $token
contexts:
- name: disposable
  context:
    cluster: disposable
    namespace: $EXECUTION_NAMESPACE
    user: agent-sandbox-fix-client
current-context: disposable
KUBECONFIG
unset token ca_data server
chmod 600 "$RUNTIME_KUBECONFIG"

# Fake-client timeout and cancellation mapping, without creating a second Sandbox.
(
  cd "$ROOT/backend"
  go test ./internal/fixruntime -run '^TestAgentSandboxRuntimeTimeoutAndCancellation$' -count=1 -v
) >"$EVIDENCE_DIR/timeout-cancellation-test.log" 2>&1

unset AI_TOKEN OPENAI_API_KEY ANTHROPIC_API_KEY CLAUDE_API_KEY KIMI_API_KEY FIX_TOKEN BOT_TOKEN ORKA_API_TOKEN GITHUB_TOKEN GH_TOKEN COPILOT_TOKEN AZURE_OPENAI_API_KEY || true
set +e
(
  cd "$ROOT/backend"
  KUBECONFIG="$RUNTIME_KUBECONFIG" \
  AGENT_SANDBOX_LIVE=1 \
  AGENT_SANDBOX_NAMESPACE="$EXECUTION_NAMESPACE" \
  AGENT_SANDBOX_IMAGE="${EXECUTOR_REPOSITORY}@${EXECUTOR_DIGEST}" \
  AGENT_SANDBOX_SERVICE_ACCOUNT=fix-workload \
  AGENT_SANDBOX_RUNTIME_CLASS="$RUNTIME_CLASS" \
  AGENT_SANDBOX_POLL_INTERVAL=200ms \
  AGENT_SANDBOX_TEST_GATEWAY_ENDPOINT="https://fake-model-gateway.${EXECUTION_NAMESPACE}.svc.cluster.local:8443/v1/chat/completions" \
  AGENT_SANDBOX_EVIDENCE_DIR="$EVIDENCE_DIR" \
  go test ./internal/fixruntime -run '^TestAgentSandboxProductionKindFixture$' -count=1 -v
) 2>&1 | tee "$TEST_LOG"
test_status=${PIPESTATUS[0]}
set -e

kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" get sandboxes -o name >"$EVIDENCE_DIR/remaining-sandboxes.txt" 2>&1 || true
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" get pods -l prow-ai-dashboard/execution -o name >"$EVIDENCE_DIR/remaining-executor-pods.txt" 2>&1 || true
kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" logs deployment/fake-model-gateway >"$EVIDENCE_DIR/gateway.log" 2>&1 || true
if [[ $test_status -ne 0 ]]; then
  kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n "$EXECUTION_NAMESPACE" get events --sort-by=.lastTimestamp >"$EVIDENCE_DIR/failure-events.txt" 2>&1 || true
  echo "Primary productionization fixture failed. Evidence is preserved at $EVIDENCE_DIR. Do not rerun before diagnosis." >&2
  exit "$test_status"
fi
if [[ -s "$EVIDENCE_DIR/remaining-sandboxes.txt" || -s "$EVIDENCE_DIR/remaining-executor-pods.txt" ]]; then
  echo "Sandbox resources remain after the primary fixture" >&2
  exit 1
fi

python3 - "$EVIDENCE_DIR" <<'PY'
import json,pathlib,subprocess,sys,tempfile
root=pathlib.Path(sys.argv[1]); result=json.loads((root/'primary-result.json').read_text()); patch=result['diff']; (root/'primary.patch').write_text(patch)
with tempfile.TemporaryDirectory(prefix='pad-agent-sandbox-prod-verify-') as work:
    subprocess.run(['git','clone','--no-checkout','https://github.com/octocat/Hello-World.git',work],check=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
    subprocess.run(['git','checkout','--detach',result['base_sha']],cwd=work,check=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
    subprocess.run(['git','apply','--check',str(root/'primary.patch')],cwd=work,check=True)
    subprocess.run(['git','apply','--index',str(root/'primary.patch')],cwd=work,check=True)
    subprocess.run(['git','diff','--cached','--check'],cwd=work,check=True)
    changed=subprocess.check_output(['git','diff','--cached','--name-only'],cwd=work,text=True).splitlines()
    content=(pathlib.Path(work)/'README').read_text()
    if changed != ['README'] or content != 'Hello Agent Sandbox!\n': raise SystemExit(f'unexpected reconstruction: {changed} {content!r}')
(root/'independent-verification.txt').write_text('git_apply_check=passed\ngit_diff_cached_check=passed\nchanged_files=README\ncontent=Hello Agent Sandbox!\\n\n')
PY

if grep -ERin '(github_pat_|ghp_|bearer[[:space:]]+[A-Za-z0-9._-]+|authorization|api[_-]?key|x-api-key|access[_-]?token|provider[_-]?token|oauth[_-]?token|github[_-]?token|client[_-]?secret|kubeconfig|kubernetes[[:space:]_-]*credential)' \
  "$EVIDENCE_DIR/primary-result.json" "$EVIDENCE_DIR/primary-test.log" "$EVIDENCE_DIR/primary.patch" "$EVIDENCE_DIR/gateway.log" >"$EVIDENCE_DIR/credential-scan.txt"; then
  echo "credential-like text found in evaluation evidence" >&2
  exit 1
else
  echo "credential_like_text=none" >"$EVIDENCE_DIR/credential-scan.txt"
fi
cat >>"$EVIDENCE_DIR/run-metadata.txt" <<META
finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
primary_fixture=passed
primary_fixture_runs=1
production_executor=opencode-1.18.2
model_provider=gateway-tokenless-chat-completions
independent_patch_verification=passed
remaining_sandboxes=0
remaining_executor_pods=0
kind_runc_isolation_proven=false
META
echo "Agent Sandbox v0.5.3 productionization fixture passed. Evidence: $EVIDENCE_DIR"
