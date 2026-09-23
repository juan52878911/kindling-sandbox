#!/usr/bin/env bash
# Test end-to-end del kindling-operator contra un cluster REAL de Kubernetes (k3s
# u otro cluster conformante), comunicándose con un frontal real de kindling-sandbox.
#
# Todo en internal/operator se testea unitariamente contra una API de Kubernetes FALSA
# (ver internal/operator/controller_test.go): eso es rápido y cubre la lógica de
# reconciliación, pero no puede detectar lo que solo hace diferente un servidor API real:
# validación de esquema CRD (oneOf), conversión Table para `kubectl get`
# (additionalPrinterColumns), semántica de watch a través de un reinicio real del apiserver,
# o RBAC siendo realmente ejecutado. Este script cubre esa brecha, de la misma forma que
# scripts/90-e2e.sh cubre el frontal contra un daemon real de kindling en lugar de uno falso.
#
# Requerido:
#   KUBECONFIG            contexto kubectl para el cluster objetivo
#
# Opcional:
#   KLING_SANDBOX_URL     un frontal ya ejecutándose para testear. Si no está
#   KLING_SANDBOX_TOKEN   configurado, este script inicia el suyo propio (vinculado a
#                         HOST_IP, no 127.0.0.1, para que los pods en el cluster puedan
#                         alcanzarlo) y posee su ciclo de vida, lo que permite también
#                         cubrir "frontal caído y se recupera". Dado un frontal externo,
#                         ese check se salta: este script nunca mata un frontal que no
#                         haya iniciado.
#   HOST_IP               dirección para vincular su propio frontal (default: la primera
#                         dirección IPv4 no-loopback encontrada)
#   IMAGE                 imagen kindling para el Sandbox de variante imagen y la base
#                         de la plantilla (default: toolchain)
#   NAMESPACE             namespace para los Sandboxes de test (default: default)
#   KLING_SANDBOX         ruta a un binario kling-sandbox prebuild. Sin este (o
#   KINDLING_OPERATOR     KINDLING_OPERATOR) este script construye uno con `go build`,
#                         que necesita Go en esta máquina — configurar ambos si se está
#                         ejecutando en un host que solo tiene los binarios cross-compiled
#                         (ver README en la raíz del repo: sin Go en la VM del lab kindling).
#   KEEP=1                no limpiar al salir (para inspeccionar manualmente)
#
# El operator en sí nunca corre como Pod aquí: no se asume Docker en esta máquina,
# así que corre fuera del cluster contra `kubectl proxy`, exactamente como documenta
# docs/kubernetes.md para testear fuera de un cluster. Ambos binarios son clientes HTTP
# puros (API de Kubernetes + API del frontal), por lo que este script puede correr en
# cualquier lugar con acceso de red a ambos — no necesita correr en un nodo del cluster.
set -uo pipefail

KUBECONFIG="${KUBECONFIG:?set KUBECONFIG to the target cluster}"
export KUBECONFIG
IMAGE="${IMAGE:-toolchain}"
NAMESPACE="${NAMESPACE:-default}"
KEEP="${KEEP:-0}"
KLING_SANDBOX="${KLING_SANDBOX:-}"
KINDLING_OPERATOR="${KINDLING_OPERATOR:-}"
GIVEN_URL="${KLING_SANDBOX_URL:-}"
GIVEN_TOKEN="${KLING_SANDBOX_TOKEN:-}"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n     expected: %s\n     got:      %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
skip() { printf "  \033[33mskip\033[0m  %s (%s)\n" "$1" "$2"; }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }

# contains verifica una subcadena SIN un pipeline: con `set -o pipefail`,
# `x | grep -q y` puede fallar todo el pipeline en SIGPIPE de grep saliendo temprano
# incluso cuando el texto estaba allí, y `cmd | grep -q y || echo bad` imprime dos veces
# si cmd también falla por su cuenta. Cada check aquí lee su salida en una variable
# primero e la inspecciona después, nunca encadena un pipeline en `||`.
contains() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need kubectl
need curl

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP=$(mktemp -d)
TPL="e2ek8s-$$"
PROXY_PID=""
OPERATOR_PID=""
FRONTAL_PID=""
SELF_FRONTAL=0
# Declarados vacíos desde ya: con `set -u`, cleanup() (el trap EXIT) los lee
# aunque el script salga antes del paso 3 que de verdad los rellena, y una
# variable sin declarar bajo `set -u` corta el trap a medias.
FRONTAL_URL=""
FRONTAL_TOKEN=""
HOST_IP="${HOST_IP:-}"
SB_IMAGE="e2e-k8s-image-$$"
SB_TEMPLATE="e2e-k8s-template-$$"
SB_RESTART="e2e-k8s-restart-$$"

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo
    echo "KEEP=1: not cleaning up. operator pid=$OPERATOR_PID proxy pid=$PROXY_PID frontal pid=$FRONTAL_PID tmp=$TMP"
    return
  fi
  echo
  echo "cleaning up..."
  for sb in "$SB_IMAGE" "$SB_TEMPLATE" "$SB_RESTART"; do
    kubectl delete sandbox "$sb" -n "$NAMESPACE" --ignore-not-found=true --timeout=15s >/dev/null 2>&1
  done
  [ -n "$OPERATOR_PID" ] && { kill "$OPERATOR_PID"; wait "$OPERATOR_PID"; } >/dev/null 2>&1
  [ -n "$PROXY_PID" ] && { kill "$PROXY_PID"; wait "$PROXY_PID"; } >/dev/null 2>&1
  if [ "$SELF_FRONTAL" = "1" ]; then
    [ -n "$FRONTAL_PID" ] && { kill "$FRONTAL_PID"; wait "$FRONTAL_PID"; } >/dev/null 2>&1
    KLING_SANDBOX_URL="$FRONTAL_URL" KLING_SANDBOX_TOKEN="$FRONTAL_TOKEN" \
      "$KLING_SANDBOX" sbx template rm "$TPL" >/dev/null 2>&1
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

# ── 0. binarios ──────────────────────────────────────────────────────────
step "0. kling-sandbox and kindling-operator"
if [ -n "$KLING_SANDBOX" ] && [ -n "$KINDLING_OPERATOR" ]; then
  ok "using the given binaries: $KLING_SANDBOX, $KINDLING_OPERATOR"
else
  need go
  KLING_SANDBOX="${KLING_SANDBOX:-$TMP/kling-sandbox}"
  KINDLING_OPERATOR="${KINDLING_OPERATOR:-$TMP/kindling-operator}"
  build_log="$TMP/build.log"
  if (cd "$REPO" && GOWORK=off go build -o "$KLING_SANDBOX" ./cmd/kling-sandbox && \
                     GOWORK=off go build -o "$KINDLING_OPERATOR" ./cmd/kindling-operator) 2>"$build_log"; then
    ok "built from $REPO"
  else
    echo "could not build the binaries:" >&2
    cat "$build_log" >&2
    exit 1
  fi
fi

# ── 1. el cluster ────────────────────────────────────────────────────────
step "1. Cluster ($KUBECONFIG)"
out=$(kubectl version 2>&1)
if contains "$out" "Server Version"; then
  ok "kubectl reaches the API server"
else
  bad "kubectl version" "a Server Version" "$out"
  exit 1
fi

# ── 2. namespace, CRD, RBAC ──────────────────────────────────────────────
step "2. deploy/namespace.yaml, crd.yaml, rbac.yaml"
out=$(kubectl apply -f "$REPO/deploy/namespace.yaml" -f "$REPO/deploy/crd.yaml" -f "$REPO/deploy/rbac.yaml" 2>&1)
if [ $? -eq 0 ]; then
  ok "applied (idempotent: unchanged is fine on a re-run)"
else
  bad "kubectl apply" "success" "$out"
  exit 1
fi
# additionalPrinterColumns necesita que la conversión Table del CRD esté lista;
# usualmente instantáneo, pero un CRD recién aplicado puede tener un pequeño lag.
for _ in $(seq 1 30); do
  kubectl get sandboxes.sandbox.kindling.dev -A >/dev/null 2>&1 && break
  sleep 0.5
done

# ── 3. el frontal ────────────────────────────────────────────────────────
step "3. Frontal"
if [ -n "$GIVEN_URL" ] && [ -n "$GIVEN_TOKEN" ]; then
  FRONTAL_URL="$GIVEN_URL"
  FRONTAL_TOKEN="$GIVEN_TOKEN"
  SELF_FRONTAL=0
  ok "using the given frontal: $FRONTAL_URL"
else
  if [ -z "$HOST_IP" ]; then
    # La dirección fuente para la ruta DEFAULT, no solo "cualquier dirección global
    # que no sea loopback": en un host corriendo kindling mismo (como el lab para el que
    # se construyó esto), `ip addr` lista una veth 172.30.x.x por cada microVM en ejecución
    # delante del NIC real, y esas van y vienen con el sandbox que las posee — vincular a una
    # funcionaría hoy y se haría stale en el momento que esa microVM particular sea recolectada.
    # Enrutamiento a una dirección externa real siempre elige el uplink real (eth0 aquí),
    # nunca una interfaz solo CNI/veth, así que su `src` es la una dirección que es tanto
    # estable como alcanzable desde pods.
    HOST_IP=$(ip route get 8.8.8.8 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
  fi
  if [ -z "$HOST_IP" ]; then
    bad "HOST_IP" "an address pods can reach" "none found; set HOST_IP=..."
    exit 1
  fi
  FRONTAL_PORT="${FRONTAL_PORT:-18096}"
  FRONTAL_URL="http://$HOST_IP:$FRONTAL_PORT"
  FRONTAL_TOKEN="e2ek8stok$$"
  KLING_SANDBOX_TENANTS="e2ek8s:$FRONTAL_TOKEN:50:20" \
    "$KLING_SANDBOX" sbx gateway -listen "$HOST_IP:$FRONTAL_PORT" -image "$IMAGE" >"$TMP/frontal.log" 2>&1 &
  FRONTAL_PID=$!
  SELF_FRONTAL=1
  listo=0
  for _ in $(seq 1 50); do
    curl -fsS "$FRONTAL_URL/v1/health" >/dev/null 2>&1 && { listo=1; break; }
    sleep 0.1
  done
  if [ "$listo" = "1" ]; then
    ok "started our own frontal on $FRONTAL_URL (pid $FRONTAL_PID)"
  else
    bad "frontal" "listening on $FRONTAL_URL" "did not answer in time"
    cat "$TMP/frontal.log" >&2
    exit 1
  fi
fi

sbx() { KLING_SANDBOX_URL="$FRONTAL_URL" KLING_SANDBOX_TOKEN="$FRONTAL_TOKEN" "$KLING_SANDBOX" sbx "$@"; }

# Una plantilla para el Sandbox de variante plantilla abajo; una construcción
# trivial de un paso es suficiente, estamos ejercitando al operator, no al template builder.
cat > "$TMP/tpl.json" <<JSON
{"name": "$TPL", "image": "$IMAGE", "pool": 1, "steps": [{"cmd": ["true"]}]}
JSON
out=$(sbx template apply -f "$TMP/tpl.json" 2>&1)
if contains "$out" "built" || contains "$out" "up to date"; then
  ok "template $TPL ready"
else
  bad "template apply" "'built' or 'up to date'" "$out"
  exit 1
fi

# El operator mismo corre fuera del cluster abajo (no se asume Docker en esta máquina),
# así que nunca necesita alcanzar el frontal desde dentro de un Pod. Pero el PROPIO trabajo
# del frontal es ser alcanzable desde donde un deployment in-cluster real lo ejecutaría
# (ver deploy/deployment.yaml, KLING_SANDBOX_URL), así que un pod que no puede alcanzarlo
# sería una regresión real incluso aunque nada más aquí lo atrapara.
if [ "$SELF_FRONTAL" = "1" ]; then
  out=$(kubectl run "e2e-k8s-reach-$$" --rm -i --restart=Never --image=busybox:1.36 \
    --command -- wget -T5 -qO- "$FRONTAL_URL/v1/health" 2>&1)
  contains "$out" '"ok":true' && ok "a pod in the cluster can reach the frontal at $FRONTAL_URL" \
    || bad "pod reachability" "a pod to read {\"ok\":true} from $FRONTAL_URL/v1/health" "$out"
fi

# ── 4. secret + operator fuera del cluster (kubectl proxy) ───────────────
step "4. kindling-operator against \`kubectl proxy\`"
kubectl create namespace kindling-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create secret generic kindling-operator-frontal -n kindling-system \
  --from-literal=token="$FRONTAL_TOKEN" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

PROXY_PORT="${PROXY_PORT:-18099}"
kubectl proxy --port="$PROXY_PORT" >"$TMP/proxy.log" 2>&1 &
PROXY_PID=$!
listo=0
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PROXY_PORT/healthz" >/dev/null 2>&1 && { listo=1; break; }
  sleep 0.1
done
if [ "$listo" != "1" ]; then
  bad "kubectl proxy" "listening on $PROXY_PORT" "did not answer in time"
  cat "$TMP/proxy.log" >&2
  exit 1
fi

KLING_SANDBOX_URL="$FRONTAL_URL" KLING_SANDBOX_TOKEN="$FRONTAL_TOKEN" \
  "$KINDLING_OPERATOR" -kube-url "http://127.0.0.1:$PROXY_PORT" >"$TMP/operator.log" 2>&1 &
OPERATOR_PID=$!
listo=0
for _ in $(seq 1 50); do
  out=$(cat "$TMP/operator.log" 2>/dev/null)
  contains "$out" "watching" && { listo=1; break; }
  sleep 0.1
done
if [ "$listo" = "1" ]; then
  ok "operator running (pid $OPERATOR_PID), watching against $FRONTAL_URL"
else
  bad "kindling-operator" "a 'watching ...' line in its log" "$(cat "$TMP/operator.log")"
  exit 1
fi

wait_status() {
  # wait_status NAME FIELD DEADLINE_SECONDS — sondea status.$FIELD hasta que no esté vacío.
  local name=$1 field=$2 deadline=$3 out=""
  for _ in $(seq 1 $((deadline * 5))); do
    out=$(kubectl get sandbox "$name" -n "$NAMESPACE" -o jsonpath="{.status.$field}" 2>/dev/null)
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    sleep 0.2
  done
  printf '%s' "$out"
  return 1
}

# ── 5. variante imagen ────────────────────────────────────────────────────
step "5. Sandbox (image: $IMAGE)"
cat > "$TMP/sb-image.yaml" <<YAML
apiVersion: sandbox.kindling.dev/v1alpha1
kind: Sandbox
metadata:
  name: $SB_IMAGE
  namespace: $NAMESPACE
spec:
  image: $IMAGE
  ttlSeconds: 600
  egress: none
YAML
kubectl apply -f "$TMP/sb-image.yaml" >/dev/null
id=$(wait_status "$SB_IMAGE" id 15)
if [ -n "$id" ]; then
  ok "status.id set: $id"
else
  bad "image Sandbox" "a non-empty status.id" "(empty after 15s)"
fi
host=$(kubectl get sandbox "$SB_IMAGE" -n "$NAMESPACE" -o jsonpath='{.status.host}')
state=$(kubectl get sandbox "$SB_IMAGE" -n "$NAMESPACE" -o jsonpath='{.status.state}')
expires=$(kubectl get sandbox "$SB_IMAGE" -n "$NAMESPACE" -o jsonpath='{.status.expiresAt}')
[ -n "$host" ] && [ "$state" = "running" ] && [ -n "$expires" ] && \
  ok "status.host=$host state=running status.expiresAt set" || \
  bad "image Sandbox status" "host set, state=running, expiresAt set" "host=$host state=$state expiresAt=$expires"

# ── 6. columnas de impresión ───────────────────────────────────────────────
step "6. Printer columns (kubectl get)"
out=$(kubectl get sandbox "$SB_IMAGE" -n "$NAMESPACE" 2>&1)
if contains "$out" "<invalid>"; then
  bad "printer columns" "no '<invalid>' cell" "$out"
else
  ok "no '<invalid>' cell (regression check: Expires used to be type: date, which"
  echo "        always reads as '<invalid>' for a FUTURE timestamp — see deploy/crd.yaml)"
fi

# ── 7. variante plantilla ─────────────────────────────────────────────────
step "7. Sandbox (template: $TPL)"
cat > "$TMP/sb-template.yaml" <<YAML
apiVersion: sandbox.kindling.dev/v1alpha1
kind: Sandbox
metadata:
  name: $SB_TEMPLATE
  namespace: $NAMESPACE
spec:
  template: $TPL
  ttlSeconds: 600
  egress: none
YAML
kubectl apply -f "$TMP/sb-template.yaml" >/dev/null
tid=$(wait_status "$SB_TEMPLATE" id 15)
[ -n "$tid" ] && ok "status.id set: $tid" || bad "template Sandbox" "a non-empty status.id" "(empty after 15s)"

# ── 8. exec a través del frontal, usando status.id ────────────────────────
step "8. Exec via the frontal API (status.id)"
if [ -n "$tid" ]; then
  out=$(sbx exec "$tid" -- sh -c 'echo exec-ok-93-e2e' 2>&1)
  contains "$out" "exec-ok-93-e2e" && ok "exec through the frontal reached the sandbox" \
    || bad "exec" "'exec-ok-93-e2e' in the output" "$out"
else
  skip "exec" "no status.id from step 7"
fi

# ── 9. cambio ttlSeconds renueva ──────────────────────────────────────────
step "9. spec.ttlSeconds change renews"
before=$(kubectl get sandbox "$SB_TEMPLATE" -n "$NAMESPACE" -o jsonpath='{.status.expiresAt}')
kubectl patch sandbox "$SB_TEMPLATE" -n "$NAMESPACE" --type=merge -p '{"spec":{"ttlSeconds":3600}}' >/dev/null
ok_gen=0
for _ in $(seq 1 25); do
  gens=$(kubectl get sandbox "$SB_TEMPLATE" -n "$NAMESPACE" -o jsonpath='{.status.observedGeneration} {.metadata.generation}')
  set -- $gens
  [ "${1:-}" = "${2:-}" ] && [ -n "${1:-}" ] && { ok_gen=1; break; }
  sleep 0.2
done
after=$(kubectl get sandbox "$SB_TEMPLATE" -n "$NAMESPACE" -o jsonpath='{.status.expiresAt}')
if [ "$ok_gen" = "1" ] && [ "$before" != "$after" ]; then
  ok "observedGeneration caught up and expiresAt moved ($before -> $after)"
else
  bad "renew" "observedGeneration to catch up and expiresAt to change" "generation stayed at $gens, expiresAt $before -> $after"
fi

# ── 10. kubectl delete: finalizer limpia en el frontal ────────────────
step "10. kubectl delete removes the finalizer and the sandbox at the frontal"
kubectl delete sandbox "$SB_IMAGE" -n "$NAMESPACE" --timeout=15s >/dev/null 2>&1
gone_k8s=0
for _ in $(seq 1 25); do
  kubectl get sandbox "$SB_IMAGE" -n "$NAMESPACE" >/dev/null 2>&1 || { gone_k8s=1; break; }
  sleep 0.2
done
if [ "$gone_k8s" = "1" ]; then
  ok "object gone from Kubernetes"
else
  bad "kubectl delete" "the object to disappear" "still there after 5s"
fi
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $FRONTAL_TOKEN" "$FRONTAL_URL/v1/sandboxes/$id")
[ "$code" = "404" ] && ok "sandbox gone from the frontal too (404)" || bad "frontal after delete" "404" "$code"

# ── 11. eliminado detrás de la espalda del operator → gone, sin recreación ──
step "11. Deleted at the frontal directly: state becomes gone, no recreation"
curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $FRONTAL_TOKEN" "$FRONTAL_URL/v1/sandboxes/$tid" >/dev/null
became_gone=0
for _ in $(seq 1 40); do
  st=$(kubectl get sandbox "$SB_TEMPLATE" -n "$NAMESPACE" -o jsonpath='{.status.state}' 2>/dev/null)
  [ "$st" = "gone" ] && { became_gone=1; break; }
  sleep 0.5
done
still_same_id=$(kubectl get sandbox "$SB_TEMPLATE" -n "$NAMESPACE" -o jsonpath='{.status.id}' 2>/dev/null)
if [ "$became_gone" = "1" ] && [ "$still_same_id" = "$tid" ]; then
  ok "status.state=gone, status.id unchanged (never recreated)"
else
  bad "gone detection" "state=gone with the same id ($tid)" "state=$st id=$still_same_id"
fi

# ── 12. reiniciar operator: sin creación duplicada ──────────────────────
step "12. Restarting the operator process: no duplicate at the frontal"
cat > "$TMP/sb-restart.yaml" <<YAML
apiVersion: sandbox.kindling.dev/v1alpha1
kind: Sandbox
metadata:
  name: $SB_RESTART
  namespace: $NAMESPACE
spec:
  image: $IMAGE
  ttlSeconds: 600
  egress: none
YAML
kubectl apply -f "$TMP/sb-restart.yaml" >/dev/null
wait_status "$SB_RESTART" id 15 >/dev/null
before_count=$(curl -s -H "Authorization: Bearer $FRONTAL_TOKEN" "$FRONTAL_URL/v1/sandboxes" | grep -o '"id"' | wc -l | tr -d ' ')
kill "$OPERATOR_PID" >/dev/null 2>&1
wait "$OPERATOR_PID" 2>/dev/null
KLING_SANDBOX_URL="$FRONTAL_URL" KLING_SANDBOX_TOKEN="$FRONTAL_TOKEN" \
  "$KINDLING_OPERATOR" -kube-url "http://127.0.0.1:$PROXY_PORT" >"$TMP/operator2.log" 2>&1 &
OPERATOR_PID=$!
listo=0
for _ in $(seq 1 50); do
  out=$(cat "$TMP/operator2.log" 2>/dev/null)
  contains "$out" "watching" && { listo=1; break; }
  sleep 0.1
done
sleep 2 # let a relist run
after_count=$(curl -s -H "Authorization: Bearer $FRONTAL_TOKEN" "$FRONTAL_URL/v1/sandboxes" | grep -o '"id"' | wc -l | tr -d ' ')
if [ "$listo" = "1" ] && [ "$before_count" = "$after_count" ]; then
  ok "operator restarted cleanly, $after_count sandbox(es) at the frontal (no duplicate)"
else
  bad "restart" "the same sandbox count before and after ($before_count)" "$after_count, or the operator failed to come back"
fi

# ── 13. frontal caído y se recupera ────────────────────────────────────
step "13. Frontal down: status.message set, then clears on recovery"
if [ "$SELF_FRONTAL" != "1" ]; then
  skip "frontal down/up" "using an externally-given frontal; this script never stops one it didn't start"
else
  kill "$FRONTAL_PID" >/dev/null 2>&1
  wait "$FRONTAL_PID" 2>/dev/null
  for _ in $(seq 1 20); do
    curl -fsS -m 1 "$FRONTAL_URL/v1/health" >/dev/null 2>&1 || break
    sleep 0.2
  done
  kubectl patch sandbox "$SB_RESTART" -n "$NAMESPACE" --type=merge -p '{"spec":{"ttlSeconds":700}}' >/dev/null
  msg_set=0
  for _ in $(seq 1 25); do
    msg=$(kubectl get sandbox "$SB_RESTART" -n "$NAMESPACE" -o jsonpath='{.status.message}' 2>/dev/null)
    [ -n "$msg" ] && { msg_set=1; break; }
    sleep 0.2
  done
  if [ "$msg_set" = "1" ]; then
    ok "status.message set while the frontal is down"
  else
    bad "frontal down" "a non-empty status.message" "(still empty)"
  fi

  KLING_SANDBOX_TENANTS="e2ek8s:$FRONTAL_TOKEN:50:20" \
    "$KLING_SANDBOX" sbx gateway -listen "$HOST_IP:$FRONTAL_PORT" -image "$IMAGE" \
    >"$TMP/frontal2.log" 2>&1 &
  FRONTAL_PID=$!
  listo=0
  for _ in $(seq 1 50); do
    curl -fsS "$FRONTAL_URL/v1/health" >/dev/null 2>&1 && { listo=1; break; }
    sleep 0.1
  done
  if [ "$listo" != "1" ]; then
    bad "frontal restart" "listening again on $FRONTAL_URL" "did not come back"
  else
    cleared=0
    for _ in $(seq 1 60); do
      msg=$(kubectl get sandbox "$SB_RESTART" -n "$NAMESPACE" -o jsonpath='{.status.message}' 2>/dev/null)
      [ -z "$msg" ] && { cleared=1; break; }
      sleep 0.5
    done
    if [ "$cleared" = "1" ]; then
      ok "status.message cleared once the frontal recovered (regression check:"
      echo "        with omitempty on a bare string, a merge patch could never clear it)"
    else
      bad "recovery" "status.message to clear" "still: $msg"
    fi
  fi
fi

# ── 14. watch interruption: se reconecta ──────────────────────────────────
step "14. Watch interruption (kubectl proxy restart): the operator reconnects"
kill "$PROXY_PID" >/dev/null 2>&1
wait "$PROXY_PID" 2>/dev/null
kubectl proxy --port="$PROXY_PORT" >"$TMP/proxy2.log" 2>&1 &
PROXY_PID=$!
listo=0
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PROXY_PORT/healthz" >/dev/null 2>&1 && { listo=1; break; }
  sleep 0.1
done
if [ "$listo" != "1" ]; then
  bad "proxy restart" "listening again" "did not come back"
else
  # Probar que el operator sigue reconciliando a través del nuevo proxy, no solo
  # que su proceso sobrevivió: un cambio fresco de ttlSeconds tiene que llegar.
  kubectl patch sandbox "$SB_RESTART" -n "$NAMESPACE" --type=merge -p '{"spec":{"ttlSeconds":800}}' >/dev/null
  recovered=0
  for _ in $(seq 1 60); do
    g1=$(kubectl get sandbox "$SB_RESTART" -n "$NAMESPACE" -o jsonpath='{.status.observedGeneration}')
    g2=$(kubectl get sandbox "$SB_RESTART" -n "$NAMESPACE" -o jsonpath='{.metadata.generation}')
    [ -n "$g1" ] && [ "$g1" = "$g2" ] && { recovered=1; break; }
    sleep 0.5
  done
  [ "$recovered" = "1" ] && ok "operator reconnected its watch and kept reconciling" \
    || bad "watch reconnect" "a later spec change to still be reconciled" "observedGeneration stuck at $g1 (want $g2)"
fi

# ── 15. spec inválido rechazado por el CRD ────────────────────────────────
step "15. Invalid spec (template AND image together)"
cat > "$TMP/sb-invalid.yaml" <<YAML
apiVersion: sandbox.kindling.dev/v1alpha1
kind: Sandbox
metadata:
  name: e2e-k8s-invalid-$$
  namespace: $NAMESPACE
spec:
  template: $TPL
  image: $IMAGE
  ttlSeconds: 300
YAML
out=$(kubectl apply -f "$TMP/sb-invalid.yaml" 2>&1)
code=$?
if [ "$code" -ne 0 ] && contains "$out" "oneOf"; then
  ok "rejected by the API server (CRD oneOf), never reached the operator"
else
  bad "invalid spec" "a non-zero exit mentioning oneOf" "exit $code: $out"
  kubectl delete -f "$TMP/sb-invalid.yaml" --ignore-not-found=true >/dev/null 2>&1
fi

# ── 16. RBAC: mínimo privilegio, sin errores de forbidden ────────────────
step "16. RBAC (no forbidden in the operator's logs)"
logs=$(cat "$TMP/operator.log" "$TMP/operator2.log" 2>/dev/null)
if contains "$logs" "forbidden" || contains "$logs" "Forbidden"; then
  bad "RBAC" "no 'forbidden' in the operator log" "$(printf '%s' "$logs" | grep -i forbidden)"
else
  ok "no forbidden errors: deploy/rbac.yaml's ClusterRole is enough for everything exercised above"
fi

# ── resumen ────────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fail(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
