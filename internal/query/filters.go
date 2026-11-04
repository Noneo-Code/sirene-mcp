// Package query implements the read-only queries exposed by the MCP tools.
package query

import (
	"fmt"
	"strings"
	"time"
)

// Filters selects establishments. All values are bound as parameters.
type Filters struct {
	NAF, NAF25, Departements, Communes, CodesPostaux, Tranches, CategoriesJuridiques []string
	CreatedAfter, CreatedBefore                                                      string // YYYY-MM-DD, inclusive
	SiegeOnly                                                                        bool
	Etat                                                                             string // "A" (default when ""), "F", "all"
	Name                                                                             string
}

var accentFold = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ä", "a", "ã", "a", "å", "a",
	"ç", "c", "è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ñ", "n",
	"ò", "o", "ó", "o", "ô", "o", "ö", "o", "õ", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y",
	"œ", "oe", "æ", "ae",
)

func normalizeName(s string) string {
	return accentFold.Replace(strings.ToLower(strings.TrimSpace(s)))
}

func normalizeNAF(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !strings.Contains(s, ".") && len(s) >= 3 {
		s = s[:2] + "." + s[2:]
	}
	return s
}

func normalizeDepartement(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) == 3 && s[0] == '0' && strings.Trim(s, "0123456789") == "" {
		s = s[1:]
	}
	return s
}

func trimAll(vs []string) []string {
	var out []string
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func mapAll(vs []string, f func(string) string) []string {
	var out []string
	for _, v := range trimAll(vs) {
		out = append(out, f(v))
	}
	return out
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Where builds the WHERE clause (without the keyword; "TRUE" when empty) over
// the table aliased `e` (etablissement), plus its bound arguments.
func (f Filters) Where() (string, []any, error) {
	var conds []string
	var args []any

	prefix := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf(`%s LIKE ? ESCAPE '\'`, col)
			args = append(args, escapeLike(v)+"%")
		}
		conds = append(conds, "("+strings.Join(parts, " OR ")+")")
	}
	exact := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		conds = append(conds, col+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}

	prefix("e.activite_principale", mapAll(f.NAF, normalizeNAF))
	prefix("e.activite_principale_naf25", mapAll(f.NAF25, normalizeNAF))
	exact("e.departement", mapAll(f.Departements, normalizeDepartement))
	exact("e.code_commune", trimAll(f.Communes))
	exact("e.code_postal", trimAll(f.CodesPostaux))
	exact("e.tranche_effectifs", trimAll(f.Tranches))
	prefix("e.categorie_juridique", trimAll(f.CategoriesJuridiques))

	for _, d := range []struct{ field, val, op string }{
		{"created_after", f.CreatedAfter, ">="},
		{"created_before", f.CreatedBefore, "<="},
	} {
		if d.val == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", d.val)
		if err != nil {
			return "", nil, fmt.Errorf("%s: invalid date %q, expected YYYY-MM-DD", d.field, d.val)
		}
		conds = append(conds, "e.date_creation "+d.op+" CAST(? AS DATE)")
		args = append(args, t.Format("2006-01-02"))
	}

	if f.SiegeOnly {
		conds = append(conds, "e.est_siege")
	}
	switch f.Etat {
	case "", "A":
		conds = append(conds, "e.etat_administratif = 'A'")
	case "F":
		conds = append(conds, "e.etat_administratif = 'F'")
	case "all":
	default:
		return "", nil, fmt.Errorf("etat: unknown value %q, expected A, F or all", f.Etat)
	}
	if n := normalizeName(f.Name); n != "" {
		conds = append(conds, "e.nom_recherche LIKE '%' || strip_accents(lower(?)) || '%'")
		args = append(args, n)
	}

	if len(conds) == 0 {
		return "TRUE", args, nil
	}
	return strings.Join(conds, " AND "), args, nil
}
