package nomenclature

import (
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
