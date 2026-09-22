package plantilla

// LA RECETA COMO IDENTIDAD DEL SNAPSHOT.
//
// Un snapshot dorado no dice de dónde salió: es memoria congelada y un overlay.
// Por eso la receta viaja con él, en una anotación, y con ella su hash: sin el
// hash no hay forma de decidir si el dorado que hay en el host es el que pide
// la plantilla de hoy, y la única opción segura sería reconstruir siempre.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Receta es lo que se guarda en la anotación AnotacionReceta del snapshot.
//
// Lleva la plantilla ENTERA y no solo el hash porque el hash sirve para
// comparar y la plantilla para reconstruir: un host que perdió sus snapshots
// (un reinicio basta) puede rehacerlos sin que nadie recuerde qué se instaló,
// que es justo lo que la plantilla promete.
type Receta struct {
	Plantilla Plantilla `json:"template"`
	Hash      string    `json:"recipe_sha256"`
	Hecho     time.Time `json:"built_at"`
}

// HashReceta es la huella de una receta: mismo contenido, mismo hash, en
// cualquier host y en cualquier orden de escritura.
//
// Se calcula sobre el JSON de la plantilla normalizada. Ese JSON es canónico
// por construcción: Plantilla no tiene mapas —los únicos campos que Go serializa
// en orden indefinido—, así que el orden de los campos lo fija el struct y no
// depende de cómo se escribiera el fichero que la trajo.
func HashReceta(p Plantilla) string {
	b, err := json.Marshal(normalizar(p))
	if err != nil {
		// Inalcanzable: json solo falla con canales, funciones o ciclos, y
		// Plantilla son cadenas, enteros y listas de lo mismo. Se prefiere
		// romper aquí a devolver un hash vacío, que dos plantillas distintas
		// compartirían y haría que Reconciliar las diera por iguales.
		panic(fmt.Sprintf("plantilla: no se puede serializar la receta: %v", err))
	}
	suma := sha256.Sum256(b)
	return hex.EncodeToString(suma[:])
}

// normalizar deja la receta en la forma sobre la que se hashea: dos plantillas
// que producen el MISMO snapshot tienen que dar el mismo hash, o se reconstruye
// por nada (y reconstruir cuesta minutos e internet).
func normalizar(p Plantilla) Plantilla {
	n := p

	// No decir nada y decir "internet" es la misma red para la máquina de
	// preparación. Escribirlo explícitamente no puede obligar a reinstalar.
	n.EgressBuild = egresoDe(p)

	// Los dominios permitidos son un CONJUNTO: el filtro que sale de ellos no
	// depende del orden en que se listen.
	if len(p.AllowBuild) > 0 {
		n.AllowBuild = append([]string(nil), p.AllowBuild...)
		sort.Strings(n.AllowBuild)
	}

	// Pool queda FUERA del hash. Es cuántas instancias se mantienen calientes:
	// una política de uso que no cambia ni un byte del snapshot. Si entrara,
	// subir el pool de 1 a 2 obligaría a reinstalarlo todo.
	n.Pool = 0

	// Pasos y Volumes conservan su orden a propósito: ahí el orden ES la receta
	// (y el de los volúmenes fija además el orden de los discos, que Firecracker
	// congela con la máquina).
	return n
}
