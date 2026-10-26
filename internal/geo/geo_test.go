package geo

import (
	"net/netip"
	"slices"
	"testing"
)

func TestCountries(t *testing.T) {
	// ISO 3166-1 currently assigns 249 alpha-2 codes.
	if len(continents) != 249 {
		t.Fatalf("%d countries, want 249", len(continents))
	}
	valid := map[string]bool{
		Africa: true, Antarctica: true, Asia: true, Europe: true, NorthAmerica: true, Oceania: true, SouthAmerica: true,
	}
	for code, cont := range continents {
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' || !valid[cont] {
			t.Errorf("bad entry %q -> %q", code, cont)
		}
	}
	for _, code := range []string{"DE", "SK", "AQ", "US"} {
		if !ValidCountry(code) {
			t.Errorf("%s invalid", code)
		}
	}
	for _, code := range []string{"", "de", "XK", "EU", "UK", "D"} {
		if ValidCountry(code) {
			t.Errorf("%q valid", code)
		}
	}
	if Continent("SK") != Europe || Continent("BR") != SouthAmerica || Continent("XX") != "" {
		t.Error("continent lookup")
	}
}

func TestNilDB(t *testing.T) {
	var db *DB
	if c := db.Country(netip.MustParseAddr("8.8.8.8")); c != "" {
		t.Fatalf("nil db returned %q", c)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCountriesIn(t *testing.T) {
	total := 0
	for _, continent := range []string{Africa, Antarctica, Asia, Europe, NorthAmerica, Oceania, SouthAmerica} {
		list := CountriesIn(continent)
		if !slices.IsSorted(list) || len(list) == 0 {
			t.Errorf("%s: %v", continent, list)
		}
		for _, c := range list {
			if Continent(c) != continent {
				t.Errorf("%s listed in %s", c, continent)
			}
		}
		total += len(list)
	}
	if total != len(continents) || !slices.Contains(CountriesIn(Europe), "SK") || CountriesIn("XX") != nil {
		t.Error("continent lists incomplete")
	}
}
