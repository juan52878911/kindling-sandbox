#!/usr/bin/env bash
# Prueba de extremo a extremo del frontal de sandboxes (kling-sandbox), contra
# un daemon REAL de kindling.
#
# Los tests de Go (internal/frontal, con su daemon falso) cubren la lógica del
# frontal sin KVM, pero no pueden cubrir lo que de verdad importa aquí: que el
# gateway escuche, que un token de verdad autentique, que exec/cp/shell crucen
# la red hasta un sandbox de una microVM real, y que el fondo de precalentadas
# haga que la primera creación de una plantilla popular sea barata. Todo eso
# necesita un daemon de kindling con KVM detrás, y por eso vive aquí y no en el
# CI (mismo motivo que scripts/90-e2e.sh del núcleo).
#
#   ./90-e2e.sh                 compila kling-sandbox y lo prueba
#   KLING_SANDBOX=./bin/x ./90-e2e.sh   usa un binario ya compilado
#   IMAGEN=toolchain ./90-e2e.sh        imagen de la que parte la plantilla
#   KEEP=1 ./90-e2e.sh           no limpia al terminar (para inspeccionar)
#
# Cada comprobación dice qué esperaba y qué obtuvo. Un fallo NO aborta el
# resto: saber que fallan tres cosas relacionadas vale más que enterarse de
# una sola.
set -uo pipefail

IMAGEN="${IMAGEN:-toolchain}"
PUERTO="${PUERTO:-18099}"
KEEP="${KEEP:-0}"
KLING_SANDBOX="${KLING_SANDBOX:-}"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFALLO\033[0m %s\n     esperaba: %s\n     obtuvo:   %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }

# contiene busca una subcadena SIN tuberías.
#
# `algo | grep -q X` es una trampa con `set -o pipefail`: grep sale en cuanto
# encuentra la coincidencia y cierra la tubería, el productor recibe SIGPIPE y
# sale distinto de cero, y la tubería entera se da por fallida AUNQUE el texto
# estuviera. Una prueba que falla sobre algo que funciona es peor que no
# tenerla: manda a corregir lo que no está roto. (La misma trampa hace que un
# `cmd | algo || echo x` imprima DOS veces si cmd también falla por su cuenta:
# por eso aquí cada comprobación guarda la salida en una variable primero y
# la mira después, en vez de encadenar tuberías con `||`.)
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }

need() { command -v "$1" >/dev/null || { echo "falta $1" >&2; exit 1; }; }
need curl

TMP=$(mktemp -d)
TPL="e2e-sbx-$$"
GATEWAY_PID=""
TOKEN_A="tok-e2e-a-$$"
TOKEN_B="tok-e2e-b-$$"
URL="http://127.0.0.1:$PUERTO"
SB=""

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo
    echo "KEEP=1: no limpio. gateway pid=$GATEWAY_PID, plantilla=$TPL, temporal=$TMP"
    return
  fi
  echo
  echo "limpiando..."
  [ -n "$SB" ] && sbxA rm "$SB" >/dev/null 2>&1
  [ -n "$GATEWAY_PID" ] && kill "$GATEWAY_PID" >/dev/null 2>&1
  "$BIN" sbx template rm "$TPL" >/dev/null 2>&1
  rm -rf "$TMP"
}
trap cleanup EXIT

# sbxA/sbxB llaman al binario como cada tenant, sin repetir las variables de
# entorno en cada paso.
sbxA() { KLING_SANDBOX_URL="$URL" KLING_SANDBOX_TOKEN="$TOKEN_A" "$BIN" sbx "$@"; }
sbxB() { KLING_SANDBOX_URL="$URL" KLING_SANDBOX_TOKEN="$TOKEN_B" "$BIN" sbx "$@"; }

# ── 0. el binario ─────────────────────────────────────────────────────────
step "0. kling-sandbox"
if [ -n "$KLING_SANDBOX" ]; then
  BIN="$KLING_SANDBOX"
  ok "uso el binario dado: $BIN"
else
  BIN="$TMP/kling-sandbox"
  REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  if (cd "$REPO" && GOWORK=off go build -o "$BIN" ./cmd/kling-sandbox) 2>"$TMP/build.log"; then
    ok "compilado en $BIN"
  else
    echo "no se pudo compilar kling-sandbox:" >&2
    cat "$TMP/build.log" >&2
    exit 1
  fi
fi

# ── 1. plantilla mínima ──────────────────────────────────────────────────
# Un paso trivial y pool 1: lo que hace falta para ejercitar Construir y el
# fondo de precalentadas sin que la preparación tarde de verdad.
step "1. Plantilla mínima ($IMAGEN, un paso trivial, pool 1)"
cat > "$TMP/tpl.json" <<JSON
{"name": "$TPL", "image": "$IMAGEN", "pool": 1, "steps": [{"cmd": ["true"]}]}
JSON
out=$("$BIN" sbx template apply -f "$TMP/tpl.json" 2>&1)
if contiene "$out" "built" || contiene "$out" "up to date"; then
  ok "plantilla $TPL construida"
else
  echo "$out"
  bad "template apply" "'built' o 'up to date'" "$out"
  exit 1
fi

# ── 2. el gateway, con dos tenants ──────────────────────────────────────
# Uno con cuota (1 sandbox: lo que hace falta para el 429 del paso 8) y otro
# sin ella, para el aislamiento del paso 7.
step "2. Gateway en $URL con dos tenants"
KLING_SANDBOX_TENANTS="e2e-a:$TOKEN_A:1,e2e-b:$TOKEN_B" \
  "$BIN" sbx gateway -listen "127.0.0.1:$PUERTO" -image "$IMAGEN" >"$TMP/gateway.log" 2>&1 &
GATEWAY_PID=$!

listo=0
for _ in $(seq 1 50); do
  if curl -fsS "$URL/v1/health" >/dev/null 2>&1; then listo=1; break; fi
  sleep 0.1
done
if [ "$listo" = "1" ]; then
  ok "gateway respondiendo (pid $GATEWAY_PID)"
else
  bad "gateway" "escuchando en $PUERTO" "no contestó a tiempo"
  cat "$TMP/gateway.log" >&2
  exit 1
fi

# ── 3. precalentada: la primera creación tiene que ser barata ───────────
# El fondo (pool: 1) tiene que haber dejado una instancia lista para cuando
# llegue la primera petición; la segunda, sin precalentada de sobra, restaura
# desde el snapshot (más cara, pero sigue siendo un thaw, no un arranque frío).
step "3. Precalentada: crear dos veces seguidas"
sleep 2 # tiempo para que la primera vuelta del fondo (pool.Rellenador) actúe

out1=$(sbxA new -template "$TPL" -ttl 10m 2>&1)
sb1=$(printf '%s\n' "$out1" | head -1 | awk '{print $1}')
t1=$(printf '%s\n' "$out1" | sed -n 's/.*ready in \([0-9][0-9.]*[a-zµ]*\) on .*/\1/p')
if [ -n "$sb1" ]; then
  ok "primer sandbox creado: $sb1 (ready in ${t1:-?})"
  sbxA rm "$sb1" >/dev/null 2>&1
else
  bad "primera creación" "un id de sandbox" "$out1"
fi
# "poco" es milisegundos de un solo dígito o dos, nunca segundos: si la cifra
# lleva una "s" suelta (no "ms" ni "µs") es que tardó un segundo entero o más.
if printf '%s' "$t1" | grep -qE '^[0-9]+(\.[0-9]+)?s$'; then
  bad "latencia de la precalentada" "milisegundos (ms o µs)" "$t1"
elif [ -n "$t1" ]; then
  ok "la precalentada tardó $t1, no segundos enteros"
fi

out2=$(sbxA new -template "$TPL" -ttl 10m 2>&1)
SB=$(printf '%s\n' "$out2" | head -1 | awk '{print $1}')
if [ -n "$SB" ]; then
  ok "segundo sandbox creado: $SB"
else
  bad "segunda creación" "un id de sandbox" "$out2"
  exit 1
fi

# ── 4. exec, con código de salida ────────────────────────────────────────
step "4. Exec"
out=$(sbxA exec "$SB" -- sh -c 'echo out; echo err >&2; exit 3' 2>&1); code=$?
if [ "$code" = "3" ] && contiene "$out" "out"; then
  ok "exec: código 3 y salida por stdout"
else
  bad "exec" "código 3 con 'out' en la salida" "código $code: $out"
fi

# ── 5. cp, ida y vuelta ──────────────────────────────────────────────────
step "5. cp"
printf 'hola e2e\n' > "$TMP/hola.txt"
if sbxA cp "$TMP/hola.txt" "$SB:/tmp/e2e-hola.txt" >/dev/null 2>&1; then
  out=$(sbxA cp "$SB:/tmp/e2e-hola.txt" - 2>&1)
  [ "$out" = "hola e2e" ] && ok "cp fuera trae lo mismo que se subió" \
    || bad "cp fuera" "hola e2e" "$out"
else
  bad "cp dentro" "subida aceptada" "falló"
fi

# ── 6. shell interactiva ─────────────────────────────────────────────────
# Necesita un terminal, así que se le pone uno falso con `script`; sin él,
# `sbx shell` se niega a propósito (mismo motivo que `kling shell`).
step "6. Shell"
if command -v script >/dev/null 2>&1; then
  printf 'exit 7\n' | KLING_SANDBOX_URL="$URL" KLING_SANDBOX_TOKEN="$TOKEN_A" \
    script -qec "$BIN sbx shell $SB" /dev/null >"$TMP/shell.out" 2>&1
  code=$?
  [ "$code" = "7" ] && ok "shell: el código de salida remoto llega al local" \
    || bad "sbx shell" "código 7" "código $code: $(cat "$TMP/shell.out")"
else
  echo "  (sin util-linux script: me salto la shell con terminal)"
fi
out=$(sbxA shell "$SB" </dev/null 2>&1)
contiene "$out" "needs a terminal" && ok "shell: se niega sin terminal, y lo explica" \
  || bad "sbx shell sin tty" "un rechazo mencionando 'needs a terminal'" "$out"

# ── 7. aislamiento entre tenants ─────────────────────────────────────────
step "7. Aislamiento"
codigo=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN_B" "$URL/v1/sandboxes/$SB")
[ "$codigo" = "404" ] && ok "el otro tenant no ve el sandbox ajeno (404)" \
  || bad "aislamiento" "404" "$codigo"
out=$(sbxB ls 2>&1)
contiene "$out" "$SB" && bad "lista de bob" "sin el sandbox de alice" "$out" \
  || ok "la lista del otro tenant no lo incluye"

# ── 8. cuota ──────────────────────────────────────────────────────────────
# e2e-a tiene MaxSandboxes=1 y ya tiene $SB vivo: la siguiente creación tiene
# que rebotar con 429, sin llegar a tocar ningún daemon.
step "8. Cuota (429)"
codigo=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN_A" \
  -X POST -d "{\"template\":\"$TPL\"}" "$URL/v1/sandboxes")
[ "$codigo" = "429" ] && ok "por encima de la cuota, 429" || bad "cuota" "429" "$codigo"

# ── 9. /v1/metrics ────────────────────────────────────────────────────────
step "9. /v1/metrics"
codigo=$(curl -s -o /dev/null -w '%{http_code}' "$URL/v1/metrics")
[ "$codigo" = "401" ] && ok "/v1/metrics exige token" || bad "/v1/metrics sin token" "401" "$codigo"
out=$(curl -s -H "Authorization: Bearer $TOKEN_B" "$URL/v1/metrics")
if contiene "$out" "kling_sandbox_machines" && contiene "$out" "kling_sandbox_creations_total"; then
  ok "/v1/metrics devuelve el formato de Prometheus (un token cualquiera vale)"
else
  bad "/v1/metrics" "líneas kling_sandbox_machines y kling_sandbox_creations_total" "$out"
fi

# ── resumen ──────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fallo(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
