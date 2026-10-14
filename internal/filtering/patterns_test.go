package filtering

import "testing"

func TestCategoryPatterns_MatchTheirExamples(t *testing.T) {
	tests := []struct {
		name string
		cat  Category
		text string
		want string
	}{
		{"email", CategoryEmail, "contact " + exEmail + " now", exEmail},
		{"ssn us", CategorySSN, "ssn " + exSSNUS + " on file", exSSNUS},
		{"ssn uk", CategorySSN, "nino " + exSSNUK + " here", exSSNUK},
		{"ssn fr", CategorySSN, "nir " + exSSNFR + " here", exSSNFR},
		{"api key openai", CategoryAPIKey, "key " + exAPIKeyOpenAI + " end", exAPIKeyOpenAI},
		{"api key github", CategoryAPIKey, "key " + exAPIKeyGitHub + " end", exAPIKeyGitHub},
		{"api key aws", CategoryAPIKey, "key " + exAPIKeyAWS + " end", exAPIKeyAWS},
		{"api key slack", CategoryAPIKey, "key " + exAPIKeySlack + " end", exAPIKeySlack},
		{"api key google", CategoryAPIKey, "key " + exAPIKeyGoogle + " end", exAPIKeyGoogle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			for _, re := range categoryPatterns[tt.cat] {
				if m := re.FindString(tt.text); m != "" {
					got = m
					break
				}
			}
			if got != tt.want {
				t.Fatalf("category %s matched %q in %q, want %q", tt.cat, got, tt.text, tt.want)
			}
		})
	}
}

func TestCategoryPatterns_EveryEnabledCategoryHasAtLeastOnePattern(t *testing.T) {
	for _, cat := range []Category{CategoryEmail, CategorySSN, CategoryAPIKey} {
		if len(categoryPatterns[cat]) == 0 {
			t.Fatalf("categoryPatterns[%s] is empty", cat)
		}
	}
}

func TestReSSNUS_ExcludesReservedAreaGroupAndSerial(t *testing.T) {
	tests := []string{
		"000-12-3456", // reserved area
		"666-12-3456", // reserved area
		"900-12-3456", // reserved area range
		"999-12-3456", // reserved area range
		"123-00-4567", // reserved group
		"123-45-0000", // reserved serial
	}
	for _, s := range tests {
		if reSSNUS.MatchString(s) {
			t.Fatalf("reSSNUS matched the reserved value %q, want no match", s)
		}
	}
}

func TestReSSNUK_ExcludesDisallowedLetters(t *testing.T) {
	tests := []string{
		"DB123456C", // first letter D excluded
		"AO123456C", // second letter O excluded
	}
	for _, s := range tests {
		if reSSNUK.MatchString(s) {
			t.Fatalf("reSSNUK matched the disallowed value %q, want no match", s)
		}
	}
}

func TestReSSNFR_RejectsAnInvalidMonth(t *testing.T) {
	// Month 13 is not a valid (0[1-9]|1[0-2]) group.
	if reSSNFR.MatchString("185137500312345") {
		t.Fatal("reSSNFR matched a NIR with month 13, want no match")
	}
}

func TestReAPIKey_NoVendorPrefixIsNeverMatched(t *testing.T) {
	for cat, res := range categoryPatterns {
		if cat != CategoryAPIKey {
			continue
		}
		for _, re := range res {
			if re.MatchString(exNoPrefixHex) {
				t.Fatalf("pattern %s matched a non-vendor-prefixed hex string %q, want no match (see Non-goals: no generic entropy heuristic)", re, exNoPrefixHex)
			}
		}
	}
}
