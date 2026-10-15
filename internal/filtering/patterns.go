package filtering

import "regexp"

// Category names a built-in PII category. Fixed in this version —
// operators enable or disable one, they cannot define a new one.
type Category string

const (
	CategoryEmail  Category = "email"
	CategorySSN    Category = "ssn"
	CategoryAPIKey Category = "api_key"
)

// categoryPatterns holds every enabled category's compiled patterns,
// tried in the order listed; all are attempted, not just the first.
var categoryPatterns = map[Category][]*regexp.Regexp{
	CategoryEmail:  {reEmail},
	CategorySSN:    {reSSNUS, reSSNUK, reSSNFR},
	CategoryAPIKey: {reAPIKeyOpenAI, reAPIKeyGitHub, reAPIKeyAWS, reAPIKeySlack, reAPIKeyGoogle},
}

var (
	// reEmail: local-part@dotted-host, at least one dot in the host.
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+`)

	// reSSNUS: ###-##-####, excluding the area codes 000, 666 and 900-999
	// and the group 00, which the SSA never issues. Written as explicit
	// alternation, not negative lookahead: Go's regexp package (RE2) does
	// not support (?!...), so the area, group and serial each enumerate
	// their valid ranges directly instead of excluding the invalid ones.
	reSSNUS = regexp.MustCompile(`\b(?:0(?:[1-9]\d|\d[1-9])|[1-5]\d{2}|6(?:[0-5]\d|6[0-57-9]|[7-9]\d)|[7-8]\d{2})-(?:[1-9]\d|0[1-9])-(?:[1-9]\d{3}|0[1-9]\d{2}|0{2}[1-9]\d|0{3}[1-9])\b`)

	// reSSNUK: a National Insurance Number. First letter excludes
	// D,F,I,Q,U,V; second excludes D,F,I,Q,U,V,O; six digits; suffix A-D.
	reSSNUK = regexp.MustCompile(`\b[A-CEGHJ-PRSTW-Z][A-CEGHJ-NPRSTW-Z]\d{6}[A-D]\b`)

	// reSSNFR: a French NIR — sex digit (1/2), YY, a valid MM, a
	// department code (01-95, 2A/2B for Corsica, or 971-976 overseas),
	// commune (3 digits), order (3 digits), 2-digit key. Groups may be
	// space-separated.
	reSSNFR = regexp.MustCompile(`\b[12]\d{2}(?:0[1-9]|1[0-2])\s?(?:0[1-9]|[1-8]\d|9[0-5]|2[AB]|97[1-6])\s?\d{3}\s?\d{3}\s?\d{2}\b`)

	// Vendor-prefixed secrets only — see Non-goals and Alternatives
	// considered for why no generic high-entropy pattern ships.
	reAPIKeyOpenAI = regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)
	reAPIKeyGitHub = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36}\b`)
	reAPIKeyAWS    = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reAPIKeySlack  = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)
	reAPIKeyGoogle = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)
)
