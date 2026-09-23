# Operador de Kubernetes

`kindling-operator` deja pedir sandboxes con `kubectl apply` en vez de con el
CLI o la API del frontal a mano: un objeto `Sandbox` declara qué se quiere, y
el operador lo crea, lo mantiene y lo borra hablando con el frontal por HTTP.

## Qué ES

- Un **plano de control declarativo**. `kubectl apply -f sandbox.yaml` pide un
  sandbox; borrarlo (`kubectl delete sandbox ...`) lo borra en el frontal.
  Kubernetes guarda la intención (`spec`) y lo último observado (`status`);
  quien de verdad crea y destruye microVMs sigue siendo el frontal.
- Un **operador fino**: un solo binario, sin dependencias externas (nada de
  client-go ni controller-runtime), que habla el API de Kubernetes por HTTP
  igual que hablaría con cualquier otro servicio. Ver
  `internal/operator/kube.go`.
- **Idempotente y resistente a reinicios**: si el operador se cae y vuelve,
  relista todos los `Sandbox`, no crea dos veces uno que ya tiene
  `status.id`, y sigue intentando borrar los que se quedaron a medias (el
  finalizer no se quita hasta que el frontal confirma el borrado, o contesta
  404).

## Qué NO es

- **No es un runtime de pods.** Las microVMs de kindling NUNCA corren dentro
  del clúster: no hay ningún Pod por sandbox, ni CNI, ni CSI, ni scheduler de
  Kubernetes decidiendo dónde vive una máquina. Eso lo decide el frontal (y,
  detrás, `hosts.Intentar`) igual que sin Kubernetes de por medio.
- **No es el plano de datos.** `exec`, `files` y `shell` siguen yendo del
  cliente directo al frontal, con el token del tenant. El operador no los
  intermedia, no los ve pasar, y kubectl no sirve para nada de eso — para
  trabajar dentro de un sandbox se sigue usando `kling sbx exec/shell` o la
  API del frontal, con el `status.id` que el `Sandbox` enseña.
- **No es aislamiento entre namespaces.** Un `Sandbox` en el namespace `equipo-a`
  y otro en `equipo-b` pueden acabar en el mismo host y el mismo daemon si los
  dos usan el mismo tenant del frontal (la cuota y la propiedad las sigue
  poniendo el TOKEN del frontal, no el namespace de Kubernetes). RBAC de
  Kubernetes controla quién puede crear objetos `Sandbox`; no sustituye a los
  tenants del frontal.
- **No tiene alta disponibilidad.** Una réplica, sin lease de liderazgo. Dos
  operadores sobre el mismo clúster no corrompen nada (crear es idempotente,
  borrar tolera el 404) pero sí duplican trabajo; no lo despliegues con
  `replicas` > 1.

## Instalación

Requiere un frontal de kindling-sandbox ya desplegado (ver el README de este
repositorio) y un token de tenant para el operador.

```sh
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/rbac.yaml

# El token de un tenant del frontal, no una contraseña de Kubernetes.
kubectl create secret generic kindling-operator-frontal \
  --namespace kindling-system \
  --from-literal=token='<token del tenant>'

# Edita deploy/deployment.yaml: la imagen y KLING_SANDBOX_URL.
kubectl apply -f deploy/deployment.yaml
```

Fuera de un clúster (para probar contra un `kubectl proxy` local, por
ejemplo) el binario acepta `-kube-url` y `-kube-token` en vez de la
configuración de cuenta de servicio:

```sh
kubectl proxy --port=8001 &
KLING_SANDBOX_URL=https://sandbox.example.internal \
KLING_SANDBOX_TOKEN=... \
  kindling-operator -kube-url http://127.0.0.1:8001
```

## Ejemplo

```yaml
apiVersion: sandbox.kindling.dev/v1alpha1
kind: Sandbox
metadata:
  name: agente-pr-123
  namespace: equipo-a
spec:
  template: node        # o `image: toolchain`, no ambas
  ttlSeconds: 1800
  onTTL: freeze          # por defecto: se congela y reanuda en ms
  egress: none            # por defecto: sin red
```

```sh
kubectl apply -f sandbox.yaml
kubectl get sandbox agente-pr-123 -n equipo-a
# NAME            TEMPLATE   STATE     HOST    EXPIRES                AGE
# agente-pr-123   node       running   host1   2026-09-22T18:30:00Z   4s

kubectl get sandbox agente-pr-123 -n equipo-a -o jsonpath='{.status.id}'
# host1/3f9a2b71...   -> el id que usan `kling sbx exec` y la API del frontal

kubectl delete sandbox agente-pr-123 -n equipo-a
```

Cambiar `spec.ttlSeconds` en un `Sandbox` vivo renueva el TTL en el frontal
(`kubectl edit` o un `apply` con el campo distinto). Cambiar cualquier otro
campo de `spec` no falla, pero tampoco hace nada: el frontal fija red,
memoria y política de TTL al crear, y no los cambia en caliente (ver
`internal/frontal/sandboxes.go`, `puedeReclamar`); para eso hay que borrar el
`Sandbox` y crear uno nuevo.

## `status`

| Campo                 | Qué significa |
|-----------------------|---------------|
| `id`                  | Id compuesto del frontal (`host/máquina`). Vacío hasta que se crea. |
| `host`                | El host del id, para lectura humana. |
| `state`               | `running`, `warm`, o `gone` — un estado que pone el operador, no el frontal, cuando el sandbox ya no aparece allí (lo borró la limpieza por abandono, o alguien a mano). El operador NO recrea un sandbox `gone`: `status.id` ya no está vacío, y por diseño eso basta para no volver a crear nada. |
| `expiresAt`           | Cuándo actúa `onTTL` si nadie lo usa ni lo renueva antes. |
| `message`             | El último error, si lo hay (cuota agotada, plantilla inexistente...). Vacío cuando todo va bien. |
| `observedGeneration`  | La `generation` de `spec` ya aplicada; sirve para ver si un cambio de `ttlSeconds` todavía está en camino. |

## Limitaciones

- Reconcilia de uno en uno: mientras un `Sandbox` espera al frontal (arrancar
  una microVM en frío tarda segundos), los eventos de los demás esperan. Está
  acotado por los plazos de los clientes HTTP y no se cuelga, pero con cientos de
  `Sandbox` creándose a la vez el último tardará en verse reflejado.
- Sin `allow_domains`: el campo `egress: allowlist` del frontal no está en el
  CRD porque pide una lista de dominios que este `Sandbox` no modela hoy.
  Pedirlo sin dominios lo rechaza el frontal, y el error queda en
  `status.message`.
- Sin lease de liderazgo (ver arriba): una réplica, sin más.
- Un fallo al escribir `status` justo después de crear (la única ventana en
  la que el frontal ya tiene el sandbox pero Kubernetes todavía no lo sabe)
  se cierra solo hasta donde un CRD sin más estado que el propio objeto
  puede: el operador reintenta escribir el status en el siguiente evento,
  pero si el proceso muere exactamente en ese hueco, ese sandbox queda vivo
  en el frontal sin que ningún `Sandbox` lo referencie todavía (no
  duplicado: simplemente huérfano hasta que alguien lo note por `kling sbx
  ls` o lo alcance el abandono del frontal).

## Probado contra un clúster real

Hasta la v0.2.1 esto solo se había probado contra el API de Kubernetes falso
de `internal/operator/controller_test.go`. `scripts/93-e2e-k8s.sh` lo prueba
contra un **k3s real** (`v1.36.4+k3s1`, instalado con `--disable
traefik,servicelb,metrics-server` para no gastar RAM de más en un laboratorio
que también corre kindling) y un frontal real: plantilla e imagen, exec por
`status.id`, renovar `ttlSeconds`, `kubectl delete` con finalizer, borrado a
mano en el frontal (`gone`, sin recrear), reinicio del operador (sin
duplicar), frontal caído y recuperado, watch interrumpido, CRD `oneOf`, y
RBAC. Se ejecuta a mano, no en CI (necesita un daemon de kindling con KVM
detrás, igual que `scripts/90-e2e.sh`).

Dos cosas que el falso API nunca hubiera podido enseñar, porque son
comportamiento del **API server de verdad**, no del operador:

- **La columna `Expires` era `type: date` y siempre salía `<invalid>`.**
  Kubernetes calcula esa columna como "tiempo transcurrido DESDE la marca"
  (`time.Since`), que es exactamente al revés de lo que hace falta para una
  fecha de EXPIRACIÓN: con una marca en el futuro, la resta da negativo y el
  formateador de Kubernetes la imprime como `<invalid>`, siempre, sin
  excepción — con cualquier `expiresAt` válido, no solo con el de este
  operador. `deploy/crd.yaml` usa `type: string` ahora, que muestra la fecha
  tal cual.
- **`status.message` no se limpiaba nunca tras una caída del frontal.**
  `PatchStatus` manda un merge patch a partir de un `SandboxStatus` a medio
  rellenar (solo las claves que trae salen en el JSON); con `Message string
  \`json:"message,omitempty"\``, un mensaje vacío (recuperado, sin error)
  jamás sale en el JSON —omitempty lo quita del todo— así que el merge patch
  nunca tocaba esa clave y el mensaje de la última caída se quedaba para
  siempre. `internal/operator/types.go` usa ahora `*string`: `nil` sigue
  significando "no toques este campo", y un puntero a `""` lo limpia de
  verdad. Cubierto además por
  `TestReconcile_MessageClearsAfterFrontalRecovers`, que sí lo detecta contra
  el API falso (el bug estaba en lo que el cliente manda, no en cómo lo
  entiende Kubernetes; solo hacía falta la prueba).

Apuntar `KLING_SANDBOX_URL` al frontal desde un Pod: el frontal no vive en el
clúster (ver "Qué NO es" arriba), así que un Service normal no sirve. Lo que
hace falta es que el Pod pueda alcanzar por red al host donde corre el
frontal — la IP del nodo (o de la máquina donde esté desplegado, si es otra),
nunca `127.0.0.1` ni `localhost`, y con el frontal escuchando en `0.0.0.0`
(o esa IP en concreto), no solo en loopback. `scripts/93-e2e-k8s.sh` detecta
esa IP con `ip route get 8.8.8.8` (la fuente de la ruta por defecto) en vez
de la primera dirección "global" que encuentre: en un host que también corre
kindling, `ip addr` lista una dirección `172.30.x.x` por cada microVM viva
ANTES que la del NIC real, y esas van y vienen con la microVM que las creó.
