package frontal

import "testing"

// empezar/terminar es todo lo que hay que probar sin un daemon de por medio:
// que dos peticiones contra la misma (host, plantilla) rota no lancen dos
// reconstrucciones, y que una plantilla distinta (o el mismo host tras
// terminar) sí pueda.
func TestReconstruccionesNoDuplicaElMismoParYDejaPasarOtros(t *testing.T) {
	var r reconstrucciones

	if !r.empezar("a", "node") {
		t.Fatal("la primera reconstrucción de a/node debía poder empezar")
	}
	if r.empezar("a", "node") {
		t.Fatal("una segunda reconstrucción de a/node mientras la primera sigue no debía empezar")
	}
	if !r.enMarcha("a", "node") {
		t.Fatal("a/node debía figurar en marcha")
	}
	if !r.empezar("a", "python") {
		t.Fatal("otra plantilla en el mismo host sí debía poder empezar")
	}
	if !r.empezar("b", "node") {
		t.Fatal("la misma plantilla en otro host sí debía poder empezar")
	}

	r.terminar("a", "node")
	if r.enMarcha("a", "node") {
		t.Fatal("tras terminar, a/node no debía seguir en marcha")
	}
	if !r.empezar("a", "node") {
		t.Fatal("tras terminar, a/node debía poder volver a empezar")
	}
}
