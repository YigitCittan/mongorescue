package desktop

import (
	"encoding/json"
	"strings"
)

// updateScript shows the update status of Updater in the dashboard. It polls
// UpdatePath closely while a check, a download (with its percentage), the install or
// the restart runs (so a failed swap or a declined UAC prompt shows up as an error
// with "Try again") and every 10 minutes otherwise, and shows either a blocking full-screen dialog for a mandatory update
// or a dismissible bar at the bottom of the window for an optional one. A release
// without a verifiable file for this platform only gets the bar, with the release
// page instead of "Update". The mandatory dialog is a
// modal <dialog> (the rest of the page is inert): Escape, the cancel event and a
// close are all undone, and Tab stays inside it; engines without showModal get a
// fixed overlay with the page's other top-level elements made inert. "Later" hides
// the bar for the session (sessionStorage); an "Update" button in the dashboard
// header stays while an optional update is available and shows the bar again
// (or opens the release page without an installable file); while an update runs it
// shows the progress (Downloading x%, Installing…, Restarting…). On Windows, a
// per-machine copy left in Program Files gets a separate bar with "Remove", which
// starts its uninstaller, and "Dismiss", which hides the bar for good
// (localStorage). Release notes are set with textContent
// only, after stripping common markdown marks; nothing is ever parsed as markup.
// Strings are in English, or Turkish when the dashboard's saved language (or,
// without one, navigator.language) starts with "tr".
const updateScript = `(function (paths, header) {
  if (window.__mongorescueUpdate) { return; }
  window.__mongorescueUpdate = true;
  var STRINGS = {
    en: {
      required: "Update required",
      requiredText: "This version can no longer be used. Install MongoRescue {latest} to continue.",
      versions: "Version {current} → {latest}",
      available: "Version {latest} is available",
      yours: "(you have {current})",
      update: "Update",
      headerTitle: "Update to v{latest}",
      updateNow: "Update now",
      notes: "Release notes",
      hideNotes: "Hide release notes",
      later: "Later",
      releasePage: "Open the release page",
      checking: "Checking the release…",
      manual: "This release has no installer for your system yet. Download it from the release page.",
      downloading: "Downloading and verifying the update…",
      downloadingPercent: "Downloading the update… {percent}%",
      installing: "Installing the update…",
      installingSetup: "Installing the update. MongoRescue closes and restarts on the new version.",
      restarting: "Restarting on the new version…",
      headerDownloading: "Downloading {percent}%",
      headerDownloadingUnknown: "Downloading…",
      headerInstalling: "Installing…",
      headerRestarting: "Restarting…",
      legacy: "An older copy of MongoRescue is installed in Program Files.",
      legacyHint: "Your data is shared and stays where it is. Windows asks for administrator rights to remove the copy.",
      legacyRemove: "Remove",
      legacyDismiss: "Dismiss",
      legacyRemoving: "Removing the older copy… Allow the Windows prompt.",
      legacyStarted: "Removing the older copy…",
      legacyFailed: "The older copy was not removed: {error}",
      saved: "Saved to your Downloads folder: {file}. Quit MongoRescue and replace it with the new version.",
      showFile: "Show file",
      failed: "Update failed: {error}",
      retry: "Try again",
      noNotes: "No release notes."
    },
    tr: {
      required: "Güncelleme gerekli",
      requiredText: "Bu sürüm artık kullanılamaz. Devam etmek için MongoRescue {latest} sürümünü kurun.",
      versions: "Sürüm {current} → {latest}",
      available: "{latest} sürümü yayımlandı",
      yours: "(kullanılan: {current})",
      update: "Güncelle",
      headerTitle: "v{latest} sürümüne güncelle",
      updateNow: "Şimdi güncelle",
      notes: "Sürüm notları",
      hideNotes: "Sürüm notlarını gizle",
      later: "Daha sonra",
      releasePage: "Sürüm sayfasını aç",
      checking: "Sürüm denetleniyor…",
      manual: "Bu sürümde sisteminiz için henüz kurulum dosyası yok. Sürüm sayfasından indirin.",
      downloading: "Güncelleme indiriliyor ve doğrulanıyor…",
      downloadingPercent: "Güncelleme indiriliyor… %{percent}",
      installing: "Güncelleme kuruluyor…",
      installingSetup: "Güncelleme kuruluyor. MongoRescue kapanıp yeni sürümle yeniden açılacak.",
      restarting: "Yeni sürümle yeniden başlatılıyor…",
      headerDownloading: "İndiriliyor %{percent}",
      headerDownloadingUnknown: "İndiriliyor…",
      headerInstalling: "Kuruluyor…",
      headerRestarting: "Yeniden başlatılıyor…",
      legacy: "MongoRescue'nun eski bir kopyası Program Files klasöründe kurulu.",
      legacyHint: "Verileriniz ortaktır ve yerinde kalır. Kopyayı kaldırmak için Windows yönetici izni ister.",
      legacyRemove: "Kaldır",
      legacyDismiss: "Kapat",
      legacyRemoving: "Eski kopya kaldırılıyor… Windows onayına izin verin.",
      legacyStarted: "Eski kopya kaldırılıyor…",
      legacyFailed: "Eski kopya kaldırılamadı: {error}",
      saved: "İndirilenler klasörüne kaydedildi: {file}. MongoRescue'dan çıkın ve yeni sürümle değiştirin.",
      showFile: "Dosyayı göster",
      failed: "Güncelleme başarısız: {error}",
      retry: "Tekrar dene",
      noNotes: "Sürüm notu yok."
    }
  };
  var POLL_MS = 1000, SLOW_POLL_MS = 600000, CHECK_LIMIT_MS = 60000, LATER_KEY = "mongorescue_update_later";
  var HEADER_BUTTON_ID = "mr-update-header", HEADER_RETRY_MS = 1000, HEADER_RETRIES = 30;
  var LEGACY_KEY = "mongorescue_legacy_dismissed", LEGACY_BAR_ID = "mr-legacy-bar";
  var started = Date.now(), timer = null, ui = null, status = null, headerTimer = null, headerTries = 0;
  var legacyUI = null, legacySince = 0, legacyHidden = false;

  function language() {
    var saved = "";
    try { saved = window.localStorage.getItem("mongorescue_lang") || ""; } catch (e) { saved = ""; }
    var l = String(saved || navigator.language || "").toLowerCase();
    return l.indexOf("tr") === 0 ? "tr" : "en";
  }
  var S = STRINGS[language()];
  function t(key, vars) {
    return S[key].replace(/\{(\w+)\}/g, function (m, name) {
      return vars && vars[name] != null ? String(vars[name]) : m;
    });
  }
  function plain(md) {
    return String(md || "").replace(/\r\n?/g, "\n")
      .replace(/^#{1,6}[ \t]+/gm, "")
      .replace(/\*\*([^*\n]+)\*\*/g, "$1")
      .replace(/\x60([^\x60\n]+)\x60/g, "$1")
      .replace(/\[([^\]\n]+)\]\([^)\n]*\)/g, "$1")
      .replace(/^([ \t]*)[-*][ \t]+/gm, "$1• ")
      .trim();
  }
  function later(latest) {
    try { return window.sessionStorage.getItem(LATER_KEY) === latest; } catch (e) { return false; }
  }
  function setLater(latest) {
    try { window.sessionStorage.setItem(LATER_KEY, latest); } catch (e) { /* not persisted */ }
  }
  function clearLater() {
    try { window.sessionStorage.removeItem(LATER_KEY); } catch (e) { /* nothing stored */ }
  }
  function legacyDismissed() {
    try { return window.localStorage.getItem(LEGACY_KEY) === "1"; } catch (e) { return false; }
  }
  function dismissLegacy() {
    try { window.localStorage.setItem(LEGACY_KEY, "1"); } catch (e) { /* hidden for this page only */ }
    if (legacyUI) { legacyUI.root.remove(); legacyUI = null; }
    legacyHidden = true;
  }
  function busyState(state) {
    return state === "downloading" || state === "checking" || state === "installing" || state === "restarting";
  }
  // legacyBusy follows the start of the uninstaller, and the uninstall itself for a
  // minute, closely.
  function legacyBusy(s) {
    var l = s && s.legacy;
    if (!l || !l.found) { return false; }
    if (l.state === "removing") { return true; }
    return l.state === "started" && legacySince > 0 && Date.now() - legacySince < CHECK_LIMIT_MS;
  }

  function getStatus() {
    return fetch(paths.status, { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .catch(function () { return null; });
  }
  function post(path) {
    var headers = {};
    headers[header] = "1";
    return fetch(path, { method: "POST", credentials: "same-origin", cache: "no-store", headers: headers });
  }
  function schedule() {
    if (timer) { clearTimeout(timer); }
    timer = setTimeout(poll, POLL_MS);
  }
  // poll follows a check or a download closely, then looks again every
  // SLOW_POLL_MS: the app checks for releases periodically while it runs.
  function poll() {
    timer = null;
    getStatus().then(function (s) {
      if (s) { render(s); }
      var state = s ? s.state : "";
      if (busyState(state) || legacyBusy(s) || (!s && Date.now() - started < CHECK_LIMIT_MS)) {
        schedule();
      } else if (!timer) {
        timer = setTimeout(poll, SLOW_POLL_MS);
      }
    });
  }

  function el(tag, css, text) {
    var e = document.createElement(tag);
    if (css) { e.style.cssText = css; }
    if (text != null) { e.textContent = text; }
    return e;
  }
  function button(cls, text, onClick) {
    var b = el("button", "", text);
    b.type = "button";
    b.className = cls;
    b.addEventListener("click", onClick);
    return b;
  }
  function focusables(root) {
    return Array.prototype.filter.call(root.querySelectorAll("button, a[href], [tabindex]"), function (e) {
      return !e.disabled && e.offsetParent !== null;
    });
  }

  function install() {
    if (ui) { ui.update.disabled = true; }
    post(paths.install).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (body) {
        if (!r.ok) {
          if (ui) { ui.update.disabled = false; ui.state.textContent = t("failed", { error: body.error || r.status }); }
          return;
        }
        render(body);
        schedule();
      });
    }, function (err) {
      if (ui) { ui.update.disabled = false; ui.state.textContent = t("failed", { error: err }); }
    });
  }
  function openReleasePage() {
    post(paths.release).catch(function () { /* nothing else to try */ });
  }
  function legacyError(err) {
    if (legacyUI) {
      legacyUI.remove.disabled = false;
      legacyUI.state.textContent = t("legacyFailed", { error: err });
      legacyUI.state.style.display = "";
    }
  }
  function removeLegacy() {
    if (legacyUI) { legacyUI.remove.disabled = true; }
    legacySince = Date.now();
    post(paths.removeLegacy).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (body) {
        if (!r.ok) { legacyError(body.error || r.status); return; }
        renderLegacy(body);
        schedule();
      });
    }, legacyError);
  }

  // renderLegacy shows the bar about a per-machine copy in Program Files until it
  // is removed or the bar is dismissed. It sits above the optional update bar.
  function renderLegacy(s) {
    var l = s && s.legacy;
    if (!l || !l.found || legacyHidden || legacyDismissed()) {
      if (legacyUI) { legacyUI.root.remove(); legacyUI = null; }
      return;
    }
    if (!legacyUI) {
      var u = {};
      u.root = el("div", "position:fixed;left:0;right:0;bottom:0;z-index:2147482000;box-sizing:border-box;padding:10px 16px;" +
        "display:flex;flex-wrap:wrap;gap:8px 12px;align-items:center;justify-content:space-between;background:var(--surface,#f5f8f6);" +
        "border-top:1px solid var(--border,#d3dcd6);box-shadow:0 -2px 8px rgba(0,0,0,.12);color:var(--text,#1a211d);font:inherit;");
      u.root.id = LEGACY_BAR_ID;
      u.root.setAttribute("role", "region");
      u.root.setAttribute("aria-label", t("legacy"));
      var msg = el("div", "display:flex;flex-direction:column;gap:2px;");
      msg.appendChild(el("strong", "", t("legacy")));
      msg.appendChild(el("span", "color:var(--muted,#56625b);", t("legacyHint")));
      u.state = el("span", "min-height:1.2em;");
      u.state.setAttribute("role", "status");
      u.state.setAttribute("aria-live", "polite");
      msg.appendChild(u.state);
      var actions = el("div", "display:flex;gap:8px;align-items:center;");
      u.remove = button("btn btn-primary btn-sm", t("legacyRemove"), removeLegacy);
      actions.appendChild(u.remove);
      actions.appendChild(button("btn btn-ghost btn-sm", t("legacyDismiss"), dismissLegacy));
      u.root.appendChild(msg);
      u.root.appendChild(actions);
      document.body.appendChild(u.root);
      legacyUI = u;
    }
    var text = "";
    switch (l.state) {
    case "removing":
      text = t("legacyRemoving");
      break;
    case "started":
      text = t("legacyStarted");
      break;
    case "error":
      text = t("legacyFailed", { error: l.error });
      break;
    }
    legacyUI.state.textContent = text;
    legacyUI.state.style.display = text ? "" : "none";
    legacyUI.remove.disabled = l.state === "removing" || l.state === "started";
    legacyUI.root.style.bottom = ui && !ui.mandatory ? ui.root.offsetHeight + "px" : "0";
  }

  // headerButton keeps an "Update" button in the dashboard header while an
  // optional update is available, also after "Later" hid the bar. The header may
  // not exist yet (or at all, on other pages): it is looked for again a few times.
  function headerButton(s) {
    var existing = document.getElementById(HEADER_BUTTON_ID);
    if (!s || !s.available || s.mandatory) {
      if (existing) { existing.remove(); }
      return;
    }
    var b = existing;
    if (!b) {
      var actions = document.querySelector(".topbar-actions");
      if (!actions) {
        if (!headerTimer && headerTries < HEADER_RETRIES) {
          headerTries++;
          headerTimer = setTimeout(function () { headerTimer = null; headerButton(status); }, HEADER_RETRY_MS);
        }
        return;
      }
      b = button("btn btn-primary btn-sm", t("update"), headerClick);
      b.id = HEADER_BUTTON_ID;
      actions.insertBefore(b, actions.firstChild);
    }
    var title = t("headerTitle", { latest: s.latest }), label = t("update");
    switch (s.state) {
    case "downloading":
      label = s.percent >= 0 ? t("headerDownloading", { percent: s.percent }) : t("headerDownloadingUnknown");
      break;
    case "installing":
      label = t("headerInstalling");
      break;
    case "restarting":
      label = t("headerRestarting");
      break;
    }
    b.textContent = label;
    b.title = title;
    b.setAttribute("aria-label", label === t("update") ? title : title + ": " + label);
    b.disabled = busyState(s.state);
  }
  // headerClick runs the bar's action: it shows the bar again (even after
  // "Later") and installs, or opens the release page when there is nothing to
  // install for this platform.
  function headerClick() {
    if (!status || !status.available || status.mandatory) { return; }
    if (!status.installable && status.state !== "error") {
      openReleasePage();
      return;
    }
    clearLater();
    render(status);
    install();
  }

  function build(mandatory) {
    var u = { mandatory: mandatory, blocking: mandatory, released: false };
    var ink = "color:var(--text,#1a211d);font:inherit;";
    u.title = el(mandatory ? "h2" : "strong", mandatory ? "margin:0;font-size:1.5rem;" : "");
    u.title.id = "mr-update-title";
    u.sub = el("p", "margin:0;color:var(--muted,#56625b);");
    u.notes = el("div", "white-space:pre-wrap;overflow:auto;max-height:" + (mandatory ? "40vh" : "30vh") +
      ";padding:12px;border:1px solid var(--border,#d3dcd6);border-radius:6px;background:var(--surface-2,#ebf0ed);" + ink);
    u.notes.tabIndex = 0;
    u.state = el("p", "margin:0;min-height:1.2em;" + ink);
    u.state.setAttribute("role", "status");
    u.state.setAttribute("aria-live", "polite");
    u.update = button("btn btn-primary", t(mandatory ? "updateNow" : "update"), install);
    u.page = button("btn btn-ghost", t("releasePage"), openReleasePage);
    var actions = el("div", "display:flex;flex-wrap:wrap;gap:8px;align-items:center;");
    actions.appendChild(u.update);

    if (mandatory) {
      var useDialog = typeof HTMLDialogElement === "function" && typeof document.createElement("dialog").showModal === "function";
      u.root = el(useDialog ? "dialog" : "div", "position:fixed;inset:0;width:100vw;height:100vh;max-width:none;max-height:none;" +
        "margin:0;padding:0;border:0;box-sizing:border-box;overflow:auto;z-index:2147483647;background:var(--bg,#fff);" + ink);
      u.root.setAttribute("role", "alertdialog");
      u.root.setAttribute("aria-modal", "true");
      u.root.setAttribute("aria-labelledby", u.title.id);
      var card = el("div", "box-sizing:border-box;max-width:680px;min-height:100%;margin:0 auto;padding:40px 24px;" +
        "display:flex;flex-direction:column;justify-content:center;gap:16px;");
      u.intro = el("p", "margin:0;", t("requiredText", {}));
      actions.appendChild(u.page);
      card.appendChild(u.title);
      card.appendChild(u.sub);
      card.appendChild(u.intro);
      card.appendChild(u.notes);
      card.appendChild(u.state);
      card.appendChild(actions);
      u.root.appendChild(card);
      document.body.appendChild(u.root);
      if (useDialog) {
        u.root.addEventListener("cancel", function (e) { e.preventDefault(); });
        var reopen = function () {
          if (!u.released && !u.root.open && u.root.isConnected) { u.root.showModal(); u.update.focus(); }
        };
        u.root.addEventListener("close", reopen);
        if (typeof MutationObserver === "function") {
          new MutationObserver(reopen).observe(u.root, { attributes: true, attributeFilter: ["open"] });
        }
        u.root.showModal();
      } else {
        inertSiblings(u);
      }
      document.addEventListener("keydown", function (e) { trapKeys(u, e); }, true);
      document.addEventListener("focusin", function (e) {
        if (u.blocking && !u.root.contains(e.target)) { setTimeout(function () { u.update.focus(); }, 0); }
      }, true);
      u.update.focus();
    } else {
      u.root = el("div", "position:fixed;left:0;right:0;bottom:0;z-index:2147483000;box-sizing:border-box;padding:12px 16px;" +
        "display:flex;flex-direction:column;gap:8px;background:var(--surface,#f5f8f6);border-top:1px solid var(--border,#d3dcd6);" +
        "box-shadow:0 -2px 8px rgba(0,0,0,.12);" + ink);
      u.root.setAttribute("role", "region");
      u.root.setAttribute("aria-labelledby", u.title.id);
      var row = el("div", "display:flex;flex-wrap:wrap;gap:12px;align-items:center;justify-content:space-between;");
      var msg = el("div", "display:flex;flex-wrap:wrap;gap:8px;align-items:baseline;");
      msg.appendChild(u.title);
      msg.appendChild(u.sub);
      u.notes.hidden = true;
      u.toggle = button("btn btn-secondary", t("notes"), function () {
        u.notes.hidden = !u.notes.hidden;
        u.toggle.textContent = t(u.notes.hidden ? "notes" : "hideNotes");
        u.toggle.setAttribute("aria-expanded", String(!u.notes.hidden));
      });
      u.toggle.setAttribute("aria-expanded", "false");
      u.later = button("btn btn-ghost", t("later"), function () {
        if (status) { setLater(status.latest); }
        remove();
      });
      actions.appendChild(u.toggle);
      actions.appendChild(u.later);
      row.appendChild(msg);
      row.appendChild(actions);
      u.root.appendChild(row);
      u.root.appendChild(u.notes);
      u.root.appendChild(u.state);
      u.pageRow = el("div", "");
      u.pageRow.appendChild(u.page);
      u.pageRow.hidden = true;
      u.root.appendChild(u.pageRow);
      document.body.appendChild(u.root);
    }
    return u;
  }
  function inertSiblings(u) {
    function apply(node) {
      if (node !== u.root && node.nodeType === 1) { node.inert = true; node.setAttribute("inert", ""); node.setAttribute("aria-hidden", "true"); }
    }
    Array.prototype.forEach.call(document.body.children, apply);
    if (typeof MutationObserver === "function") {
      new MutationObserver(function (records) {
        records.forEach(function (r) { Array.prototype.forEach.call(r.addedNodes, apply); });
      }).observe(document.body, { childList: true });
    }
  }
  function trapKeys(u, e) {
    if (!u.blocking) { return; }
    if (e.key === "Escape" || e.key === "Esc") {
      e.preventDefault();
      e.stopImmediatePropagation();
      return;
    }
    if (e.key !== "Tab") { return; }
    var list = focusables(u.root);
    if (!list.length) { e.preventDefault(); return; }
    var first = list[0], last = list[list.length - 1], active = document.activeElement;
    if (!u.root.contains(active)) {
      e.preventDefault();
      first.focus();
    } else if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  }
  function remove() {
    if (!ui || ui.mandatory) { return; }
    ui.root.remove();
    ui = null;
    if (legacyUI) { legacyUI.root.style.bottom = "0"; }
  }

  function render(s) {
    status = s;
    headerButton(s);
    renderUpdate(s);
    renderLegacy(s);
  }
  function renderUpdate(s) {
    if (!s.available) { remove(); return; }
    var active = busyState(s.state) || s.state === "ready" || s.state === "error";
    if (!s.mandatory && !active && later(s.latest)) { remove(); return; }
    if (ui && ui.mandatory !== !!s.mandatory) { remove(); }
    if (!ui) { ui = build(!!s.mandatory); }
    var vars = { current: s.current, latest: s.latest, file: s.file, error: s.error };
    if (ui.mandatory) {
      ui.title.textContent = t("required");
      ui.sub.textContent = t("versions", vars);
      ui.intro.textContent = t("requiredText", vars);
    } else {
      ui.title.textContent = t("available", vars);
      ui.sub.textContent = t("yours", vars);
    }
    ui.notes.textContent = plain(s.notes) || t("noNotes");
    var label = t(ui.mandatory ? "updateNow" : "update"), text = "";
    switch (s.state) {
    case "checking":
      text = t("checking");
      break;
    case "downloading":
      text = s.percent >= 0 ? t("downloadingPercent", { percent: s.percent }) : t("downloading");
      break;
    case "ready":
      text = t("saved", vars);
      label = t("showFile");
      break;
    case "installing":
      text = t(s.action === "launch" ? "installingSetup" : "installing");
      break;
    case "restarting":
      text = t("restarting");
      break;
    case "error":
      text = t("failed", vars);
      label = t("retry");
      break;
    }
    ui.state.textContent = text;
    ui.state.style.display = text || ui.mandatory ? "" : "none";
    ui.update.textContent = label;
    ui.update.disabled = busyState(s.state);
    // Without a verifiable file for this platform (yet), only the release page
    // is offered; "Try again" after an error checks for the files again.
    ui.update.hidden = !s.installable && s.state !== "error";
    if (!s.installable && s.state === "idle") {
      ui.state.textContent = t("manual");
      ui.state.style.display = "";
    }
    if (ui.pageRow) { ui.pageRow.hidden = s.installable && s.state !== "error"; }
  }

  poll();
})(__PATHS__, __HEADER__);`

// UpdateScript returns JavaScript that shows the Updater's status in the dashboard:
// a blocking dialog for a mandatory update, a dismissible bar for an optional one.
// Inject it on every page load (it runs once per page), so a reload cannot get
// around a mandatory update.
func UpdateScript() string {
	return strings.NewReplacer(
		"__PATHS__", updatePathsJSON(),
		"__HEADER__", jsString(UpdateHeader),
	).Replace(updateScript)
}

// updatePathsJSON returns the endpoint paths the update script uses as a JSON
// object literal.
func updatePathsJSON() string {
	b, _ := json.Marshal(struct {
		Status       string `json:"status"`
		Install      string `json:"install"`
		Release      string `json:"release"`
		RemoveLegacy string `json:"removeLegacy"`
	}{UpdatePath, UpdateInstallPath, UpdateReleasePagePath, UpdateRemoveLegacyPath}) // a struct of strings always marshals
	return string(b)
}
