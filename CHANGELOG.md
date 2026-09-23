# Changelog

Novedades de kindling-sandbox, el frontal de sandboxes para agentes de código
sobre las microVMs de [kindling](https://github.com/juan52878911/kindling).

| kindling-sandbox | kindling |
|---|---|
| v0.2.x | v0.8.x |
| v0.1.x | v0.7.x |

## v0.2.2 — 2026-09-23

- **`kindling-operator` probado contra un k3s real** (`v1.36.4+k3s1`), no solo
  contra el API falso de sus tests. Dos fallos que solo un clúster de verdad
  podía enseñar, los dos arreglados:
  - `deploy/crd.yaml`: la columna `Expires` usaba `type: date`, que Kubernetes
    calcula como tiempo transcurrido DESDE la marca — con una fecha de
    expiración (en el futuro) eso siempre sale `<invalid>`. Ahora es
    `type: string`.
  - `status.message` no se limpiaba nunca tras una caída del frontal: un
    `SandboxStatus{Message: ""}` con `omitempty` no manda la clave en el
    merge patch, así que el mensaje de la última caída se quedaba para
    siempre. `Message` es ahora `*string` (`internal/operator/types.go`):
    `nil` no toca el campo, un puntero a `""` lo limpia de verdad.
- `deploy/deployment.yaml` apunta a la imagen `v0.2.2`, que trae el arreglo de
  `status.message`.
- **`scripts/93-e2e-k8s.sh`**: prueba de extremo a extremo del operador contra
  un clúster de Kubernetes real y un frontal real (plantilla e imagen, exec,
  renovar TTL, `kubectl delete`, borrado detrás de las espaldas del operador,
  reinicio del operador, frontal caído y recuperado, watch interrumpido, CRD
  `oneOf`, RBAC). Ver `docs/kubernetes.md`.

## v0.2.1 — 2026-09-23

- **La imagen del operador se publica** en `ghcr.io/juan52878911/kindling-operator`,
  para amd64 y arm64, y `deploy/deployment.yaml` la fija por versión. En v0.2.0 el
  manifiesto apuntaba a una imagen que no existía.
- La release incluye los binarios del operador para linux y un tar con los
  manifiestos de `deploy/`.

## v0.2.0 — 2026-09-23

- Depende de kindling v0.8, y un host con el disco casi lleno (el 503 nuevo del
  núcleo) se reintenta en otro igual que uno sin memoria.
- **`GET /v1/metrics`**, en texto de Prometheus y detrás del mismo token que el
  resto de la API (uno de cualquier inquilino vale): sandboxes vivos por
  inquilino y estado, creaciones totales por resultado, latencia de creación,
  precalentadas disponibles por plantilla, hosts vivos y memoria disponible por
  host, y sesiones de shell abiertas por inquilino.
- **Cuota de shells**: `Tenant.MaxShells` (0 = sin tope) rechaza con 429 la
  sesión que se pasa. `KLING_SANDBOX_TENANTS` admite ahora un cuarto campo,
  `nombre:token[:máximo de sandboxes[:máximo de shells]]`, sin romper el
  formato de antes.
- **Autocuración de plantillas tras un reinicio del host**: cuando restaurar un
  sandbox desde un snapshot falla por el fallo de TSC que invalida los
  dorados, el frontal reconstruye esa plantilla en ese host en segundo plano
  (con la receta que colgaba del propio snapshot) y contesta 503 con
  `Retry-After` mientras dura. Otros hosts sanos con la misma plantilla siguen
  sirviendo mientras tanto: el fallo de TSC se reintenta en otro host antes de
  rendirse, igual que "no cabe".
- **`GET /v1/templates`**: qué plantillas hay listas para pedir y en qué hosts,
  de solo lectura.
- **`kindling-operator`**: operador fino de Kubernetes, sin dependencias
  externas (habla el API de Kubernetes por HTTP a mano). Un CRD `Sandbox`
  declara qué se quiere; el operador lo crea, renueva y borra contra este
  frontal. Las microVMs siguen sin vivir en el clúster: Kubernetes es solo el
  plano de control. Ver `docs/kubernetes.md`.
- **`scripts/90-e2e.sh`**: prueba de extremo a extremo contra un daemon y un
  gateway reales (plantilla, cuota, aislamiento, exec, cp, shell, métricas y
  precalentadas).

## v0.1.0 — 2026-09-23

Primera versión. Lo que kindling no debe llevar dentro: un extremo en la red con
autenticación, plantillas, inquilinos y varios hosts.

- **`kling sbx gateway`**: API HTTP v1 de sandboxes con token por inquilino,
  cuotas y propiedad. El daemon de kindling sigue sin escuchar en ningún puerto.
- **Plantillas como receta** (`kling sbx template apply|ls|rebuild|rm`): de qué
  imagen partir y qué instalar; de ahí sale un snapshot dorado, y la receta queda
  anotada en él para poder reconstruirlo en cualquier host.
- **Fondo de precalentadas** por plantilla: reclamar una cuesta 16 ms medidos,
  frente a 683 ms creándola desde el snapshot.
- **Varios hosts**: se elige por hueco libre y se reintenta en otro cuando uno
  dice que no cabe o que llegó a su tope de máquinas.
- **`kling sbx shell`**: terminal interactiva dentro del sandbox a través del
  gateway, con el mismo protocolo de tramas que `kling shell`.
- **Passthrough de exec, ficheros y shell** hasta la microVM, en streaming y con
  las tramas validadas por dirección.
- Los sandboxes nacen sin red y durmiéndose al vencer; un segundo plazo
  (24 h por defecto) borra los abandonados.

No es aislamiento entre inquilinos: comparten daemon y host. Es reparto y
contabilidad; quien necesite aislamiento fuerte, hosts separados.
