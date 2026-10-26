package geo

// Continent codes as used by GeoIP databases; phase 2 leaderboards filter by them.
const (
	Africa       = "AF"
	Antarctica   = "AN"
	Asia         = "AS"
	Europe       = "EU"
	NorthAmerica = "NA"
	Oceania      = "OC"
	SouthAmerica = "SA"
)

// continents maps every officially assigned ISO 3166-1 alpha-2 code to its continent. Transcontinental countries
// are placed where their players would expect to compete (RU, TR, CY in Europe).
var continents = map[string]string{
	"AD": Europe, "AE": Asia, "AF": Asia, "AG": NorthAmerica, "AI": NorthAmerica, "AL": Europe, "AM": Asia,
	"AO": Africa, "AQ": Antarctica, "AR": SouthAmerica, "AS": Oceania, "AT": Europe, "AU": Oceania,
	"AW": NorthAmerica, "AX": Europe, "AZ": Asia,
	"BA": Europe, "BB": NorthAmerica, "BD": Asia, "BE": Europe, "BF": Africa, "BG": Europe, "BH": Asia,
	"BI": Africa, "BJ": Africa, "BL": NorthAmerica, "BM": NorthAmerica, "BN": Asia, "BO": SouthAmerica,
	"BQ": NorthAmerica, "BR": SouthAmerica, "BS": NorthAmerica, "BT": Asia, "BV": Antarctica, "BW": Africa,
	"BY": Europe, "BZ": NorthAmerica,
	"CA": NorthAmerica, "CC": Asia, "CD": Africa, "CF": Africa, "CG": Africa, "CH": Europe, "CI": Africa,
	"CK": Oceania, "CL": SouthAmerica, "CM": Africa, "CN": Asia, "CO": SouthAmerica, "CR": NorthAmerica,
	"CU": NorthAmerica, "CV": Africa, "CW": NorthAmerica, "CX": Asia, "CY": Europe, "CZ": Europe,
	"DE": Europe, "DJ": Africa, "DK": Europe, "DM": NorthAmerica, "DO": NorthAmerica, "DZ": Africa,
	"EC": SouthAmerica, "EE": Europe, "EG": Africa, "EH": Africa, "ER": Africa, "ES": Europe, "ET": Africa,
	"FI": Europe, "FJ": Oceania, "FK": SouthAmerica, "FM": Oceania, "FO": Europe, "FR": Europe,
	"GA": Africa, "GB": Europe, "GD": NorthAmerica, "GE": Asia, "GF": SouthAmerica, "GG": Europe, "GH": Africa,
	"GI": Europe, "GL": NorthAmerica, "GM": Africa, "GN": Africa, "GP": NorthAmerica, "GQ": Africa, "GR": Europe,
	"GS": Antarctica, "GT": NorthAmerica, "GU": Oceania, "GW": Africa, "GY": SouthAmerica,
	"HK": Asia, "HM": Antarctica, "HN": NorthAmerica, "HR": Europe, "HT": NorthAmerica, "HU": Europe,
	"ID": Asia, "IE": Europe, "IL": Asia, "IM": Europe, "IN": Asia, "IO": Asia, "IQ": Asia, "IR": Asia,
	"IS": Europe, "IT": Europe,
	"JE": Europe, "JM": NorthAmerica, "JO": Asia, "JP": Asia,
	"KE": Africa, "KG": Asia, "KH": Asia, "KI": Oceania, "KM": Africa, "KN": NorthAmerica, "KP": Asia, "KR": Asia,
	"KW": Asia, "KY": NorthAmerica, "KZ": Asia,
	"LA": Asia, "LB": Asia, "LC": NorthAmerica, "LI": Europe, "LK": Asia, "LR": Africa, "LS": Africa, "LT": Europe,
	"LU": Europe, "LV": Europe, "LY": Africa,
	"MA": Africa, "MC": Europe, "MD": Europe, "ME": Europe, "MF": NorthAmerica, "MG": Africa, "MH": Oceania,
	"MK": Europe, "ML": Africa, "MM": Asia, "MN": Asia, "MO": Asia, "MP": Oceania, "MQ": NorthAmerica,
	"MR": Africa, "MS": NorthAmerica, "MT": Europe, "MU": Africa, "MV": Asia, "MW": Africa, "MX": NorthAmerica,
	"MY": Asia, "MZ": Africa,
	"NA": Africa, "NC": Oceania, "NE": Africa, "NF": Oceania, "NG": Africa, "NI": NorthAmerica, "NL": Europe,
	"NO": Europe, "NP": Asia, "NR": Oceania, "NU": Oceania, "NZ": Oceania,
	"OM": Asia,
	"PA": NorthAmerica, "PE": SouthAmerica, "PF": Oceania, "PG": Oceania, "PH": Asia, "PK": Asia, "PL": Europe,
	"PM": NorthAmerica, "PN": Oceania, "PR": NorthAmerica, "PS": Asia, "PT": Europe, "PW": Oceania,
	"PY": SouthAmerica,
	"QA": Asia,
	"RE": Africa, "RO": Europe, "RS": Europe, "RU": Europe, "RW": Africa,
	"SA": Asia, "SB": Oceania, "SC": Africa, "SD": Africa, "SE": Europe, "SG": Asia, "SH": Africa, "SI": Europe,
	"SJ": Europe, "SK": Europe, "SL": Africa, "SM": Europe, "SN": Africa, "SO": Africa, "SR": SouthAmerica,
	"SS": Africa, "ST": Africa, "SV": NorthAmerica, "SX": NorthAmerica, "SY": Asia, "SZ": Africa,
	"TC": NorthAmerica, "TD": Africa, "TF": Antarctica, "TG": Africa, "TH": Asia, "TJ": Asia, "TK": Oceania,
	"TL": Asia, "TM": Asia, "TN": Africa, "TO": Oceania, "TR": Europe, "TT": NorthAmerica, "TV": Oceania,
	"TW": Asia, "TZ": Africa,
	"UA": Europe, "UG": Africa, "UM": Oceania, "US": NorthAmerica, "UY": SouthAmerica, "UZ": Asia,
	"VA": Europe, "VC": NorthAmerica, "VE": SouthAmerica, "VG": NorthAmerica, "VI": NorthAmerica, "VN": Asia,
	"VU": Oceania,
	"WF": Oceania, "WS": Oceania,
	"YE": Asia, "YT": Africa,
	"ZA": Africa, "ZM": Africa, "ZW": Africa,
}

// ValidCountry reports whether code is an officially assigned ISO 3166-1 alpha-2 code in upper case.
func ValidCountry(code string) bool {
	_, ok := continents[code]
	return ok
}

// Continent returns the continent code of a country, "" for unknown codes.
func Continent(code string) string { return continents[code] }
