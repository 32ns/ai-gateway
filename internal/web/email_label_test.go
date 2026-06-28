package web

import (
	"strings"
	"testing"
)

func TestDisplayAccountLabelSplitsEmailText(t *testing.T) {
	got := string(displayAccountLabel("OpenAI user@example.com"))
	if !strings.Contains(got, `<span class="email-display">`) {
		t.Fatalf("displayAccountLabel() = %q, want email split markup", got)
	}
	if !strings.Contains(got, "user") || !strings.Contains(got, "@") || !strings.Contains(got, "example.com") {
		t.Fatalf("displayAccountLabel() = %q, want email content preserved", got)
	}
}
