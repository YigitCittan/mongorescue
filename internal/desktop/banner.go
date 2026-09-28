package desktop

import (
	"encoding/json"
	"strings"
)

// bannerID is the element ID of the setup-code banner; the script adds it only once.
const bannerID = "mr-desktop-setup"

// bannerScript draws a dismissible banner at the bottom of the page. The DOM is built
// with textContent and CSSOM styles only, so neither the code nor the page's Content
// Security Policy can turn it into markup.
const bannerScript = `(function (text, code) {
  if (!document.body || document.getElementById("` + bannerID + `")) { return; }
  var b = document.createElement("div");
  b.id = "` + bannerID + `";
  b.setAttribute("role", "status");
  var s = b.style;
  s.position = "fixed"; s.left = "0"; s.right = "0"; s.bottom = "0"; s.zIndex = "2147483647";
  s.display = "flex"; s.gap = "12px"; s.alignItems = "center"; s.justifyContent = "center";
  s.padding = "10px 16px"; s.background = "#1f2937"; s.color = "#fff";
  s.font = "14px system-ui, sans-serif";
  var t = document.createElement("span");
  t.textContent = text;
  var c = document.createElement("code");
  c.textContent = code;
  c.style.userSelect = "all"; c.style.fontWeight = "600"; c.style.fontSize = "15px";
  var x = document.createElement("button");
  x.type = "button";
  x.textContent = "Dismiss";
  x.onclick = function () { b.remove(); };
  b.append(t, c, x);
  document.body.appendChild(b);
})(__TEXT__, __CODE__);`

// SetupBannerScript returns JavaScript that shows the one-time setup code in a
// non-modal banner at the bottom of the window. copied adds a note that the code is
// on the clipboard. The values are embedded as JSON string literals.
func SetupBannerScript(code string, copied bool) string {
	text := "Setup code for the first administrator:"
	if copied {
		text = "Setup code for the first administrator (copied to the clipboard):"
	}
	return strings.NewReplacer("__TEXT__", jsString(text), "__CODE__", jsString(code)).Replace(bannerScript)
}

// jsString returns s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s) // a string always marshals; HTML characters are escaped
	return string(b)
}
