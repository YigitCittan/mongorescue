package desktop

import (
	"strings"
	"testing"
)

func TestSetupCodeScript(t *testing.T) {
	js := SetupCodeScript("ABCD-EFGH")
	for _, want := range []string{
		`})("ABCD-EFGH", 60000);`,      // the values are passed as literals
		`el("setup-code")`,             // fills the code field
		`new Event("input"`,            // and tells the page about it
		`input.readOnly = true`,        // the code cannot be edited
		`group.style.display = "none"`, // label, field and hint are hidden
		`el("setup-username")`,         // the focus moves to the username
		"observer.disconnect()",        // the observer does not outlive the fill
		"setTimeout(stop,",             // nor the timeout
	} {
		if !strings.Contains(js, want) {
			t.Errorf("script lacks %q:\n%s", want, js)
		}
	}
	for _, bad := range []string{"__CODE__", "__TIMEOUT__", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "clipboard"} {
		if strings.Contains(js, bad) {
			t.Errorf("script contains %q", bad)
		}
	}

	// A hostile value stays inside its string literal.
	evil := SetupCodeScript(`"); alert(1); ("</script>`)
	want := `"\"); alert(1); (\"` + "\\u003c/script\\u003e" + `"`
	if !strings.Contains(evil, want) || strings.Contains(evil, "</script>") {
		t.Fatalf("value escaped its literal:\n%s", evil)
	}
}
