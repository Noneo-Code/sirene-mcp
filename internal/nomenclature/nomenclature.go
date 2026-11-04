// Package nomenclature embeds the Sirene reference tables (NAF rev. 2, NAF 2025,
// legal categories, headcount ranges) as code,libelle CSV files.
//
// NAF 2025 source: INSEE, "Structure NAF 2025 Maj 2026-10.xlsx" (sheet of
// sous-classes), published at
// https://www.insee.fr/fr/information/8617910
// (https://www.insee.fr/fr/statistiques/fichier/8617910/Structure%20NAF%202025%20Maj%202026-10.xlsx).
// Codes use the stock's format NN.NNX (e.g. 01.11Y).
package nomenclature

import (
	"embed"
	"encoding/csv"
	"fmt"
)

//go:embed data/*.csv
var files embed.FS

// Entry is one nomenclature row.
type Entry struct{ Code, Label string }

var tables = mustLoad(map[string]string{
	"naf":                 "data/naf.csv",
	"naf2025":             "data/naf2025.csv",
	"categorie_juridique": "data/categories_juridiques.csv",
	"tranche_effectifs":   "data/tranches_effectifs.csv",
})

// Tables returns the nomenclatures keyed by DB table name.
func Tables() map[string][]Entry { return tables }

func mustLoad(src map[string]string) map[string][]Entry {
	out := make(map[string][]Entry, len(src))
	for name, path := range src {
		f, err := files.Open(path)
		if err != nil {
			panic(fmt.Sprintf("nomenclature: %v", err))
		}
		recs, err := csv.NewReader(f).ReadAll()
		_ = f.Close()
		if err != nil {
			panic(fmt.Sprintf("nomenclature: %s: %v", path, err))
		}
		if len(recs) == 0 || len(recs[0]) != 2 || recs[0][0] != "code" || recs[0][1] != "libelle" {
			panic(fmt.Sprintf("nomenclature: %s: header must be code,libelle", path))
		}
		for _, r := range recs[1:] {
			out[name] = append(out[name], Entry{Code: r[0], Label: r[1]})
		}
	}
	return out
}
