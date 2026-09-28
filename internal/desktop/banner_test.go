package desktop

import (
	"strings"
	"testing"
)

func TestSetupBannerScript(t *testing.T) {
	js := SetupBannerScript("ABCD-EFGH", true)
	if !strings.Contains(js, `"ABCD-EFGH"`) || !strings.Contains(js, "copied to the clipboard") {
		t.Fatalf("script lacks the code or the clipboard note:\n%s", js)
	}
	if strings.Contains(js, "__CODE__") || strings.Contains(js, "__TEXT__") || strings.Contains(js, "innerHTML") {
		t.Fatal("placeholders left or markup injection used")
	}
	if strings.Contains(SetupBannerScript("X", false), "clipboard") {
		t.Fatal("clipboard note without a copy")
	}
	// A hostile value stays inside its string literal.
	evil := SetupBannerScript(`"); alert(1); ("</script>`, false)
	want := `"\"); alert(1); (\"` + "\\u003c/script\\u003e" + `"`
	if !strings.Contains(evil, want) || strings.Contains(evil, "</script>") {
		t.Fatalf("value escaped its literal:\n%s", evil)
	}
}
