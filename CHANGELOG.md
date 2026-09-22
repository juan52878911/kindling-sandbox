# Changelog

Novedades de kindling-sandbox, el frontal de sandboxes para agentes de código
sobre las microVMs de [kindling](https://github.com/juan52878911/kindling).

| kindling-sandbox | kindling |
|---|---|
| v0.1.x | v0.7.x |

## Sin publicar — v0.1.0

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
- **Passthrough de exec, ficheros y shell** hasta la microVM, en streaming y con
  las tramas validadas por dirección.
- Los sandboxes nacen sin red y durmiéndose al vencer; un segundo plazo
  (24 h por defecto) borra los abandonados.

No es aislamiento entre inquilinos: comparten daemon y host. Es reparto y
contabilidad; quien necesite aislamiento fuerte, hosts separados.
