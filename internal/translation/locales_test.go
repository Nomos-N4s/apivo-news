package translation

import (
	"slices"
	"testing"
)

func TestTargetLocalesTranslatesIntoEveryReaderLanguageButTheSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		readers []string
		want    []string
	}{
		// The alpha pairs: a Greek item serves the German readers, a
		// German item the Greek ones, and an English item both.
		{"greek item", "el", AlphaReaderLocales, []string{"de"}},
		{"german item", "de", AlphaReaderLocales, []string{"el"}},
		{"english item", "en", AlphaReaderLocales, []string{"el", "de"}},

		// An item already in the one reader language needs nothing.
		{"nothing to translate", "de", []string{"de"}, nil},
		{"no readers at all", "el", nil, nil},

		// Reader order is preserved: it is configuration, and reordering
		// it here would make the cycle's spend order differ from what the
		// configuration says.
		{"reader order preserved", "en", []string{"de", "el"}, []string{"de", "el"}},

		// A duplicated reader locale collapses: two entries could only pay
		// a provider twice for a row the unique index stores once.
		{"duplicate readers collapse", "en", []string{"el", "el", "de"}, []string{"el", "de"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TargetLocales(tt.source, tt.readers)
			if !slices.Equal(got, tt.want) {
				t.Errorf("TargetLocales(%q, %v) = %v, want %v", tt.source, tt.readers, got, tt.want)
			}
		})
	}
}
