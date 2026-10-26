// Package geo resolves the country of a client IP and validates ISO 3166-1 country codes.
package geo

import (
	"fmt"
	"net/netip"

	"github.com/oschwald/maxminddb-golang/v2"
)

// DB looks up countries in a MaxMind-format database (GeoLite2-Country, DB-IP Lite Country, ...).
// A nil *DB is valid and knows no countries, so the server runs without a database configured.
type DB struct {
	r *maxminddb.Reader
}

// Open opens the .mmdb file at path.
func Open(path string) (*DB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("geo: open %s: %w", path, err)
	}
	return &DB{r: r}, nil
}

// Close releases the database.
func (db *DB) Close() error {
	if db == nil {
		return nil
	}
	return db.r.Close()
}

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// Country returns the ISO code for ip, or "" when unknown, private, or not an assigned ISO code (e.g. "XK", "EU").
func (db *DB) Country(ip netip.Addr) string {
	if db == nil || !ip.IsValid() {
		return ""
	}
	var rec countryRecord
	if err := db.r.Lookup(ip.Unmap()).Decode(&rec); err != nil {
		return ""
	}
	code := rec.Country.ISOCode
	if code == "" {
		code = rec.RegisteredCountry.ISOCode
	}
	if !ValidCountry(code) {
		return ""
	}
	return code
}
