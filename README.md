# kindling-sandbox

Sandboxes para agentes de código sobre las microVMs Firecracker de
[kindling](https://github.com/juan52878911/kindling), servidos por un frontal con
plantillas, inquilinos y varios hosts detrás.

kindling ya sabe crear un sandbox: `kling sandbox create` levanta una microVM de
usar y tirar, con ejecución dentro, sin red y con un plazo de vida. Lo que añade
esto es lo que el núcleo no debe llevar:

- **Un extremo en la red con autenticación.** El daemon de kindling escucha solo
  en un socket Unix y equivale a root en su host; no se le pone un token, se le
  pone un frontal delante. Es el mismo patrón del gateway de kindling-mcp.
- **Plantillas como receta.** Se declara de qué imagen partir y qué instalar; de
  ahí sale un snapshot dorado del que los sandboxes nacen en milisegundos. Es una
  receta y no un artefacto porque los snapshots están atados a su host: un
  reinicio los invalida y hay que poder rehacerlos sin que nadie recuerde nada.
- **Inquilinos con cuota y propiedad.** Cada token es un inquilino, con un tope de
  sandboxes vivos, y solo ve los suyos.
- **Varios hosts.** kindling es de un host. El reparto entre daemons vive aquí:
  se elige por hueco libre y se reintenta en otro cuando uno dice que no cabe.

No es aislamiento entre inquilinos: comparten daemon y host. Es reparto y
contabilidad. Quien necesite aislamiento fuerte, hosts separados.

## Instalación

Primero kindling (v0.7 o posterior), y esto encima:

```sh
make install                          # kling-sandbox en tu máquina
kling plugins                         # debería salir sandbox
make deploy HOST=ssh://juan@lab       # el frontal, como servicio, en el host del daemon
```

## Uso

```sh
# Una plantilla: de qué imagen parte y qué lleva dentro.
cat > node.json <<'JSON'
{"name":"node","image":"toolchain","build_egress":"internet","mem_mib":1024,
 "steps":[{"cmd":["npm","install","-g","typescript"]}],"pool":1}
JSON
kling sbx template apply -f node.json

# Sandboxes desde esa plantilla: ~300 ms, o inmediato si hay uno precalentado.
kling sbx new -template node -ttl 30m
kling sbx ls
kling sbx exec <id> -- tsc --version
kling sbx shell <id>          # una terminal de verdad dentro
kling sbx rm <id>

kling sbx hosts       # qué daemons hay detrás y cuánto les queda
```

## Kubernetes

`kindling-operator` (`cmd/kindling-operator`) deja pedir sandboxes con
`kubectl apply` en vez de con el CLI: un CRD `Sandbox` declara qué se quiere,
y un operador fino y sin dependencias externas lo crea, lo mantiene y lo
borra hablando con este mismo frontal por HTTP. Las microVMs siguen sin vivir
dentro de ningún clúster; Kubernetes solo hace de plano de control. Ver
[`docs/kubernetes.md`](docs/kubernetes.md).

## Compatibilidad

| kindling-sandbox | kindling |
|---|---|
| v0.1.x | v0.7.x |
