package heraldry

import "strings"

// Tincture is one colour the shield's field could take before arms became a
// run (spec R13, §6.2): a plain colour word and its day and night values.
// Arms no longer have a colour. The palette stays so that Read knows the
// colour word that leads a value stored in the old form ("blue 🔥 . 🧭 .
// 📦"), and because the controller's renderer reads it from this file
// (gardenio/controller/internal/heraldry/kbeadssrc) until it drops it.
type Tincture struct {
	// Name is the lower-case colour word, the first token of the old stored
	// form.
	Name string
	// Day and Night are the field's colour as #rrggbb.
	Day, Night string
}

// palette is the eight tinctures (spec Q13), in their fixed order. Do not
// rename or reorder it or its fields: kbeadssrc parses them.
var palette = []Tincture{
	{Name: "blue", Day: "#3d6cc0", Night: "#2b4e8f"},
	{Name: "red", Day: "#c0443a", Night: "#8e3029"},
	{Name: "green", Day: "#3a8f47", Night: "#2a6834"},
	{Name: "gold", Day: "#d9a933", Night: "#a67e22"},
	{Name: "purple", Day: "#8556b8", Night: "#613f88"},
	{Name: "brown", Day: "#8f5f3a", Night: "#69452a"},
	{Name: "slate", Day: "#627284", Night: "#475363"},
	{Name: "sand", Day: "#d6c29a", Night: "#a69572"},
}

// LookupTincture finds a tincture by its colour word, in any case.
func LookupTincture(name string) (Tincture, bool) {
	name = strings.ToLower(name)
	for _, t := range palette {
		if t.Name == name {
			return t, true
		}
	}
	return Tincture{}, false
}
