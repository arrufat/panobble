package notify

import "testing"

func TestEscapeMarkup(t *testing.T) {
	for in, want := range map[string]string{
		"Simon & Garfunkel": "Simon &amp; Garfunkel",
		"<b>bold</b>":       "&lt;b&gt;bold&lt;/b&gt;",
		`"quoted"`:          `"quoted"`,
		"plain":             "plain",
	} {
		if got := EscapeMarkup(in); got != want {
			t.Errorf("EscapeMarkup(%q) = %q, want %q", in, got, want)
		}
	}
}
