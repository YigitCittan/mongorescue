package desktop

import (
	"encoding/json"
	"strings"
)

// setupFillTimeoutMS bounds how long the setup script waits for the setup form.
const setupFillTimeoutMS = 60000

// setupFillScript fills the setup code into the first-run setup form, hides the code
// field with its label and hint, and moves the focus to the username. The dashboard
// shows the setup card after its first API calls, so the script watches the DOM with
// a MutationObserver until the card is visible, then disconnects; it gives up after
// setupFillTimeoutMS. It sets properties and CSSOM styles only: neither the code nor
// the page's Content Security Policy can turn it into markup.
const setupFillScript = `(function (code, timeoutMS) {
  var observer = null, timer = null, done = false;
  function el(id) { return document.getElementById(id); }
  function stop() {
    done = true;
    if (observer) { observer.disconnect(); observer = null; }
    if (timer) { clearTimeout(timer); timer = null; }
  }
  function fill() {
    var input = el("setup-code");
    if (!input) { return false; }
    if (input.value !== code) {
      input.value = code;
      input.dispatchEvent(new Event("input", { bubbles: true }));
    }
    input.readOnly = true;
    input.setAttribute("aria-readonly", "true");
    var group = input.closest(".form-group");
    if (group) { group.style.display = "none"; }
    return true;
  }
  function focusUsername() {
    var card = el("setup-card"), user = el("setup-username");
    if (!user || !card || card.hidden) { return false; }
    var active = document.activeElement;
    if (!active || active === document.body || active === el("setup-code")) { user.focus(); }
    return true;
  }
  function run() {
    if (!done && fill() && focusUsername()) { stop(); }
  }
  run();
  if (done || typeof MutationObserver !== "function") { return; }
  observer = new MutationObserver(run);
  observer.observe(document.documentElement, { subtree: true, childList: true, attributes: true, attributeFilter: ["hidden"] });
  timer = setTimeout(stop, timeoutMS);
})(__CODE__, __TIMEOUT__);`

// SetupCodeScript returns JavaScript that fills the one-time setup code into the
// dashboard's first-run setup form, so the desktop user only chooses a username and
// a password. The server still verifies the code. The code is embedded as a JSON
// string literal.
func SetupCodeScript(code string) string {
	return strings.NewReplacer(
		"__CODE__", jsString(code),
		"__TIMEOUT__", jsNumber(setupFillTimeoutMS),
	).Replace(setupFillScript)
}

// jsString returns s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s) // a string always marshals; HTML characters are escaped
	return string(b)
}

// jsNumber returns n as a JavaScript number literal.
func jsNumber(n int) string {
	b, _ := json.Marshal(n) // an int always marshals
	return string(b)
}
