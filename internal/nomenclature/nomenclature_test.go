package nomenclature

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTablesLoaded(t *testing.T) {
	tb := Tables()
	if n := len(tb["naf"]); n != 732 {
		t.Errorf("naf: got %d, want 732", n)
	}
	if n := len(tb["categorie_juridique"]); n != 260 {
		t.Errorf("categorie_juridique: got %d, want 260", n)
	}
	if n := len(tb["tranche_effectifs"]); n != 16 {
		t.Errorf("tranche_effectifs: got %d, want 16", n)
	}
	if n := len(tb["naf2025"]); n <= 700 {
		t.Errorf("naf2025: got %d, want > 700", n)
	}
}

func TestNafKine(t *testing.T) {
	for _, e := range Tables()["naf"] {
		if e.Code == "86.90E" {
			if !strings.Contains(e.Label, "rééducation") {
				t.Errorf("label %q lacks rééducation", e.Label)
			}
			return
		}
	}
	t.Fatal("86.90E not found")
}

func TestAliasesPointToOfficialCodes(t *testing.T) {
	for table, as := range Aliases() {
		codes := map[string]bool{}
		for _, e := range Tables()[table] {
			codes[e.Code] = true
		}
		if len(as) < 30 {
			t.Errorf("%s: only %d aliases", table, len(as))
		}
		seen := map[string]bool{}
		for _, a := range as {
			if !codes[a.Code] {
				t.Errorf("%s: alias %q points to unknown code %q", table, a.Alias, a.Code)
			}
			if strings.TrimSpace(a.Alias) == "" || seen[a.Alias] {
				t.Errorf("%s: empty or duplicate alias %q", table, a.Alias)
			}
			seen[a.Alias] = true
		}
	}
}

func TestEntryJSON(t *testing.T) {
	b, err := json.Marshal(Entry{Code: "86.90E", Label: "x"})
	if err != nil || string(b) != `{"code":"86.90E","label":"x"}` {
		t.Errorf("json = %s, %v", b, err)
	}
}
