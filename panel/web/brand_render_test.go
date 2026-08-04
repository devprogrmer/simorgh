package web

import (
	"html/template"
	"strings"
	"testing"
)

// The brand name must actually REACH the templates, which parsing does not
// prove.
//
// Go templates resolve {{ .brand }} against the data map a handler passes to
// c.HTML, not against gin's c.Set keys, and a missing key renders as the string
// "<no value>" rather than failing. So the first version of this change --
// middleware only -- would have parsed cleanly, built cleanly, passed the
// existing template test, and put "<no value>" in the sidebar, the login page
// and the two-factor issuer.
//
// These render the same constructs the real templates use and assert on the
// output, which is the only thing that catches that.
func TestBrandPlaceholderRendersFromData(t *testing.T) {
	tpl := template.Must(template.New("t").Parse(
		`<a aria-label="{{ .brand }}"><img alt="{{ .brand }}"></a><p>[{{ .brand }}]</p>`))

	var sb strings.Builder
	if err := tpl.Execute(&sb, map[string]any{"brand": "Simorgh"}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, "<no value>") {
		t.Fatalf("brand did not resolve: %s", out)
	}
	if strings.Count(out, "Simorgh") != 3 {
		t.Fatalf("expected the brand three times, got: %s", out)
	}
}

// The failure mode being guarded against, pinned so nobody reintroduces it by
// moving the assignment back into middleware.
//
// It is quieter than expected. A missing key on a MAP renders as the zero value
// -- an empty string -- not as "<no value>", and does not error. So the brand
// would simply have vanished from the sidebar, the login page and the
// two-factor issuer, with no marker in the output and nothing in the logs. That
// is why the positive test above asserts on rendered content rather than
// trusting the template to complain.
func TestMissingBrandKeyRendersEmptyAndSilent(t *testing.T) {
	tpl := template.Must(template.New("t").Parse(`[{{ .brand }}]`))
	var sb strings.Builder
	if err := tpl.Execute(&sb, map[string]any{}); err != nil {
		t.Fatalf("a missing key does not error, which is exactly why this ships unnoticed: %v", err)
	}
	if sb.String() != "[]" {
		t.Fatalf("expected a silently empty render, got %q", sb.String())
	}
}
