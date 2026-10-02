/**
 * MongoRescue dashboard: forms and accessibility.
 *
 * - The connection string builder of the connection dialog (Paste URI / Build),
 *   with a live preview that never shows the password.
 * - Inline, as-you-type validation of the connection, user, password and API key
 *   forms.
 * - The Sessions settings section (list and revoke) and the account entries of the
 *   user menu.
 * - Screen reader announcements (aria-live) of background changes: backups and
 *   restores that finish while nobody watches the table, and app updates.
 *
 * Loaded after i18n.js and app.js, whose globals it uses (state, auth, apiJSON, t,
 * tf, escapeHtml, setTbody, openModal, ...). Everything here lives in one closure;
 * only the hooks app.js calls (and window.mrAnnounce) are global.
 */
(function () {
  "use strict";

  // -------------------------------------------------------------------------
  // Announcements
  // -------------------------------------------------------------------------

  const announceTimers = {};

  // Speaks msg through a visually hidden live region: polite by default, assertive
  // for failures. The region is cleared first so the same text is read again.
  function announce(msg, assertive) {
    const id = assertive ? "sr-announce-assertive" : "sr-announce-polite";
    const region = document.getElementById(id);
    if (!region || !msg) return;
    region.textContent = "";
    clearTimeout(announceTimers[id]);
    announceTimers[id] = setTimeout(() => {
      region.textContent = String(msg);
    }, 60);
  }

  // Background run announcements: the status of every backup and restore the
  // dashboard has seen (current list pages, the last backup and each job's last
  // backup from /api/v1/stats). A run that leaves pending/in_progress, or a new one
  // that started after the first snapshot and is already done, is announced, unless
  // it was started here (app.js reports those with a toast, see noteAnnounced).
  const bg = { user: "", seen: new Map(), handled: new Set(), primedAt: 0 };

  function bgRecords() {
    const out = [];
    const stats = state.stats || {};
    const add = (kind, r) => {
      if (r && r.id) out.push({ kind, id: r.id, status: r.status, db: kind === "backup" ? r.database : r.target_database, started: r.started_at });
    };
    (state.backups || []).forEach(r => add("backup", r));
    (state.restores || []).forEach(r => add("restore", r));
    add("backup", stats.last_backup);
    Object.values(stats.job_last_backups || {}).forEach(r => add("backup", r));
    return out;
  }

  function bgMessage(r) {
    const db = r.db || "?";
    if (r.status === "failed") return { text: tf(`a11y.${r.kind}_failed`, { db }), failed: true };
    if (r.status === "completed") return { text: tf(`a11y.${r.kind}_done`, { db }), failed: false };
    // Other end states (cancelled, from run control) by their label.
    const status = r.status === "cancelled" ? t("run.status_cancelled", r.status) : r.status;
    return { text: tf(`a11y.${r.kind}_finished`, { db, status }), failed: false };
  }

  function announceBackgroundChanges() {
    const uid = auth.user ? auth.user.id || auth.user.username || "-" : "";
    if (uid !== bg.user) {
      bg.user = uid;
      bg.seen.clear();
      bg.handled.clear();
      bg.primedAt = 0;
    }
    if (!uid || !state.stats) return;
    const primed = bg.primedAt > 0;
    const messages = [];
    const visited = new Set();
    bgRecords().forEach(r => {
      const key = `${r.kind}:${r.id}`;
      if (visited.has(key)) return;
      visited.add(key);
      const prev = bg.seen.get(key);
      bg.seen.set(key, r.status);
      if (!primed || ACTIVE_STATUSES.includes(r.status) || bg.handled.has(key)) return;
      const started = parseDate(r.started);
      const fresh = prev === undefined ? !!started && started.getTime() >= bg.primedAt : ACTIVE_STATUSES.includes(prev);
      if (!fresh) return;
      bg.handled.add(key);
      messages.push(bgMessage(r));
    });
    if (!primed) bg.primedAt = Date.now();
    if (messages.length === 0) return;
    const failed = messages.filter(m => m.failed).length;
    if (messages.length > 3) {
      announce(tf("a11y.runs_summary", { n: messages.length, failed }), failed > 0);
    } else {
      announce(messages.map(m => m.text).join(" "), failed > 0);
    }
  }

  // Runs started in this tab get a toast from app.js; they are not announced twice.
  function noteAnnounced(kind, id) {
    const k = kind === "restores" || kind === "restore" ? "restore" : "backup";
    bg.handled.add(`${k}:${id}`);
  }

  // -------------------------------------------------------------------------
  // Inline validation
  // -------------------------------------------------------------------------

  const FIELD_SELECTOR = "input:not([type=hidden]):not([type=checkbox]):not([type=radio]), select, textarea";

  function validityMessage(el) {
    const v = el.validity;
    if (!v || v.valid) return "";
    if (v.customError) return el.validationMessage;
    if (v.valueMissing) return t("validate.required");
    if (v.tooShort) return tf("validate.too_short", { n: el.minLength });
    if (v.tooLong) return tf("validate.too_long", { n: el.maxLength });
    if (v.rangeUnderflow) return tf("validate.min", { n: el.min });
    if (v.rangeOverflow) return tf("validate.max", { n: el.max });
    return t("validate.invalid");
  }

  function errorElFor(el) {
    const id = `${el.id}-error`;
    let err = document.getElementById(id);
    if (!err) {
      err = document.createElement("span");
      err.id = id;
      err.className = "field-error";
      err.hidden = true;
      const group = el.closest("[data-field-group]") || el.closest(".form-group") || el.parentNode;
      group.appendChild(err);
    }
    return err;
  }

  function setDescribedBy(el, id, on) {
    const ids = (el.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean).filter(x => x !== id);
    if (on) ids.push(id);
    if (ids.length) el.setAttribute("aria-describedby", ids.join(" "));
    else el.removeAttribute("aria-describedby");
  }

  function showFieldError(el, message) {
    if (!el.id) return;
    const err = message ? errorElFor(el) : document.getElementById(`${el.id}-error`);
    if (message) {
      err.textContent = message;
      err.hidden = false;
      el.setAttribute("aria-invalid", "true");
      setDescribedBy(el, err.id, true);
    } else {
      if (err) {
        err.textContent = "";
        err.hidden = true;
        setDescribedBy(el, err.id, false);
      }
      el.removeAttribute("aria-invalid");
    }
  }

  const liveForms = new Map();

  // Validates el: its custom validator (a function returning a message or "") and
  // the native constraints. Returns the message ("" when valid).
  function validateField(el, show) {
    const cfg = liveForms.get(el.form && el.form.id);
    const validator = cfg && cfg.validators[el.id];
    if (validator) el.setCustomValidity(validator(el) || "");
    const message = isShown(el) ? validityMessage(el) : "";
    if (show) showFieldError(el, message);
    return message;
  }

  function isShown(el) {
    return !el.disabled && el.offsetParent !== null;
  }

  function fieldsOf(form) {
    return Array.from(form.querySelectorAll(FIELD_SELECTOR)).filter(el => el.id);
  }

  function clearFormErrors(form) {
    const cfg = liveForms.get(form.id);
    if (cfg) cfg.touched.clear();
    fieldsOf(form).forEach(el => {
      el.setCustomValidity("");
      showFieldError(el, "");
    });
  }

  // validateForm checks every visible field and shows the errors; it returns the
  // first invalid field or null.
  function validateForm(form) {
    const cfg = liveForms.get(form.id);
    let first = null;
    fieldsOf(form).forEach(el => {
      if (cfg) cfg.touched.add(el.id);
      if (validateField(el, true) && !first) first = el;
    });
    return first;
  }

  // Wires as-you-type validation into form: a field shows its error once it was left
  // (or the form submitted) and updates while typing; every touched field is
  // re-checked on each change, so "Confirm password" follows "Password". An invalid
  // form does not submit: the first invalid field gets the focus instead.
  function liveValidate(formId, validators) {
    const form = document.getElementById(formId);
    if (!form) return;
    const cfg = { validators: validators || {}, touched: new Set() };
    liveForms.set(formId, cfg);
    form.noValidate = true;
    const recheck = () => {
      fieldsOf(form).forEach(el => {
        if (cfg.touched.has(el.id)) validateField(el, true);
      });
    };
    form.addEventListener("input", recheck);
    form.addEventListener("change", recheck);
    form.addEventListener("focusout", (e) => {
      const el = e.target;
      if (!(el instanceof Element) || !el.matches(FIELD_SELECTOR) || !el.id) return;
      // Leaving an untouched, empty field (tabbing through) does not flag it yet.
      if (!cfg.touched.has(el.id) && !el.value) return;
      cfg.touched.add(el.id);
      validateField(el, true);
    });
    form.addEventListener("reset", () => setTimeout(() => clearFormErrors(form), 0));
    form.addEventListener("submit", (e) => {
      const first = validateForm(form);
      if (!first) return;
      e.preventDefault();
      e.stopImmediatePropagation();
      first.focus();
    }, true);
  }

  function passwordRule(el) {
    const v = el.value;
    if (!v) return "";
    if (Array.from(v).length < MIN_PASSWORD_LENGTH) return t("auth.err_password_short");
    if (new TextEncoder().encode(v).length > MAX_PASSWORD_BYTES) return t("auth.err_password_long");
    return "";
  }

  function confirmRule(otherId) {
    return (el) => (el.value && el.value !== document.getElementById(otherId).value ? t("auth.err_password_mismatch") : "");
  }

  function usernameRule(el) {
    const v = el.value.trim();
    if (!v) return "";
    return /^[A-Za-z0-9._@-]{1,64}$/.test(v) ? "" : t("validate.username");
  }

  function setupValidation() {
    liveValidate("form-connection", {
      "connection-uri": (el) => uriTextError(el.value),
      "uri-password": builderPasswordError,
      "uri-options": (el) => optionsError(el.value) || (el.value.includes(MASKED_SECRET) ? builderMaskError() : ""),
      "uri-database": (el) => (/[/\\. "$*<>:|?]/.test(el.value) ? t("uri_builder.err_database") : "")
    });
    liveValidate("form-user", {
      "user-username": usernameRule,
      "user-password": passwordRule,
      "user-password-confirm": confirmRule("user-password")
    });
    liveValidate("form-password", {
      "password-current": (el) => (!document.getElementById("password-current-group").hidden && !el.value ? t("auth.err_current_required") : ""),
      "password-new": passwordRule,
      "password-confirm": confirmRule("password-new")
    });
    liveValidate("form-api-key", {
      "api-key-name": (el) => (el.value && !el.value.trim() ? t("validate.required") : "")
    });
  }

  // -------------------------------------------------------------------------
  // Connection strings
  // -------------------------------------------------------------------------

  // Query parameters whose values are secrets, as in internal/redact.
  const SENSITIVE_KEYS = new Set([
    "password", "authmechanismproperties", "tlscertificatekeyfilepassword", "sslpemkeypassword",
    "secretkey", "secret_key", "aws_session_token", "token", "access_token", "api_key", "apikey",
    "sig", "signature"
  ]);
  // Options with their own builder field (lower case).
  const KNOWN_OPTIONS = ["authsource", "replicaset", "tls", "ssl", "tlscafile", "tlsallowinvalidcertificates"];

  function decode(s) {
    try {
      return decodeURIComponent(s);
    } catch (err) {
      return s;
    }
  }

  // Percent-encodes what may not appear raw in the userinfo of a URI.
  function encodeUserinfo(s) {
    return encodeURIComponent(s);
  }

  // Decodes a query key or value as the Go driver's connstring does (url.QueryUnescape):
  // a raw "+" is a space, so "appName=my+app" names "my app".
  function decodeQuery(s) {
    return decode(String(s).replace(/\+/g, " "));
  }

  // Percent-encodes what may not appear raw in a query key or value ("&", "#", "%",
  // "+", whitespace, ...); ":" "," "/" and the like stay readable. Spaces become %20
  // and a literal "+" %2B, so a rebuilt URI keeps its meaning.
  function encodeQuery(s) {
    return String(s).replace(/[^A-Za-z0-9\-._~!$'()*,;:@/=]/g, c => encodeURIComponent(c));
  }

  // Splits a connection string into its parts, or returns null when it is not a
  // mongodb:// or mongodb+srv:// URI. Values are percent-decoded.
  function parseMongoURI(uri) {
    const m = /^(mongodb(?:\+srv)?):\/\/(.*)$/i.exec(String(uri || "").trim());
    if (!m) return null;
    const scheme = m[1].toLowerCase();
    let rest = m[2];
    const authEnd = rest.search(/[/?]/);
    const authority = authEnd === -1 ? rest : rest.slice(0, authEnd);
    rest = authEnd === -1 ? "" : rest.slice(authEnd);
    const at = authority.lastIndexOf("@");
    const userinfo = at === -1 ? null : authority.slice(0, at);
    const hostList = at === -1 ? authority : authority.slice(at + 1);
    let username = "";
    let password = "";
    if (userinfo !== null) {
      const colon = userinfo.indexOf(":");
      username = decode(colon === -1 ? userinfo : userinfo.slice(0, colon));
      password = colon === -1 ? "" : decode(userinfo.slice(colon + 1));
    }
    const hosts = hostList.split(",").map(h => {
      const ipv6 = /^(\[[^\]]*\])(?::(.*))?$/.exec(h);
      if (ipv6) return { host: ipv6[1], port: ipv6[2] || "" };
      const colon = h.lastIndexOf(":");
      return colon === -1 ? { host: h, port: "" } : { host: h.slice(0, colon), port: h.slice(colon + 1) };
    });
    let path = "";
    let query = "";
    if (rest.startsWith("/")) {
      const q = rest.indexOf("?");
      path = decode(q === -1 ? rest.slice(1) : rest.slice(1, q));
      query = q === -1 ? "" : rest.slice(q + 1);
    } else if (rest.startsWith("?")) {
      query = rest.slice(1);
    }
    const options = query ? query.split("&").filter(Boolean).map(pair => {
      const eq = pair.indexOf("=");
      return eq === -1 ? [decodeQuery(pair), ""] : [decodeQuery(pair.slice(0, eq)), decodeQuery(pair.slice(eq + 1))];
    }) : [];
    return { scheme, username, password, hasUserinfo: userinfo !== null, hosts, path, options };
  }

  // Assembles a connection string from parts. With mask, the password and the
  // values of secret options are replaced with the redaction mask (the preview).
  function buildMongoURI(p, mask) {
    let uri = `${p.scheme}://`;
    if (p.username || p.password) {
      uri += encodeUserinfo(p.username);
      if (p.password) uri += `:${mask ? MASKED_SECRET : encodeUserinfo(p.password)}`;
      uri += "@";
    }
    uri += p.hosts.filter(h => h.host).map(h => (h.port ? `${h.host}:${h.port}` : h.host)).join(",");
    const query = p.options.filter(([k]) => k).map(([k, v]) => {
      const value = mask && SENSITIVE_KEYS.has(k.toLowerCase()) ? MASKED_SECRET : encodeQuery(v);
      return `${encodeQuery(k)}=${value}`;
    }).join("&");
    if (p.path || query) uri += `/${p.path ? encodeQuery(p.path) : ""}`;
    if (query) uri += `?${query}`;
    return uri;
  }

  // The redacted display of a connection string, as the server shows it: the
  // userinfo password and secret options masked.
  function redactURI(uri) {
    const p = parseMongoURI(uri);
    if (!p) return String(uri || "");
    const s = String(uri).trim();
    const authStart = s.indexOf("://") + 3;
    const authEnd = s.slice(authStart).search(/[/?]/);
    const authority = authEnd === -1 ? s.slice(authStart) : s.slice(authStart, authStart + authEnd);
    const tail = authEnd === -1 ? "" : s.slice(authStart + authEnd);
    const at = authority.lastIndexOf("@");
    let out = s.slice(0, authStart);
    if (at !== -1) {
      const userinfo = authority.slice(0, at);
      const colon = userinfo.indexOf(":");
      out += colon === -1 ? userinfo : `${userinfo.slice(0, colon)}:${MASKED_SECRET}`;
      out += authority.slice(at);
    } else {
      out += authority;
    }
    return out + tail.replace(/([?&])([^=&#]+)=([^&#]*)/g, (all, sep, key) =>
      (SENSITIVE_KEYS.has(decodeQuery(key).toLowerCase()) ? `${sep}${key}=${MASKED_SECRET}` : all));
  }

  // Structural checks of a pasted connection string, mirroring internal/mongouri
  // (the server checks again): scheme, no whitespace or "#", valid escapes, a host
  // list with numeric ports, the SRV rules and key=value options.
  function uriTextError(value) {
    const uri = String(value || "");
    if (!uri) return "";
    if (!/^mongodb(\+srv)?:\/\//i.test(uri)) return t("uri_builder.err_scheme");
    if (/[\s\u0000-\u001f\u007f]/.test(uri)) return t("uri_builder.err_whitespace");
    if (uri.includes("#")) return t("uri_builder.err_hash");
    if (/%(?![0-9A-Fa-f]{2})/.test(uri)) return t("uri_builder.err_escape");
    const p = parseMongoURI(uri);
    if (!p) return t("uri_builder.err_scheme");
    const hostErr = hostsError(p.scheme, p.hosts);
    if (hostErr) return hostErr;
    if (/[/@]/.test(p.path)) return t("uri_builder.err_database");
    if (uri.includes(MASKED_SECRET) && uri !== builder.opened) return t("uri_builder.err_masked_paste");
    const q = uri.indexOf("?");
    const rawPairs = q === -1 ? [] : uri.slice(q + 1).split("&").filter(Boolean);
    return rawPairs.some(pair => pair.indexOf("=") <= 0) ? t("uri_builder.err_options") : "";
  }

  function hostError(h, srv) {
    if (!h.host) return t("uri_builder.err_host_empty");
    if (!/^(\[[0-9A-Fa-f:.%a-zA-Z]+\]|[A-Za-z0-9\-._~!$&'()*+;=%]+)$/.test(h.host)) return t("uri_builder.err_host");
    if (srv && h.port) return t("uri_builder.err_srv_port");
    if (h.port && (!/^\d{1,5}$/.test(h.port) || Number(h.port) < 1 || Number(h.port) > 65535)) return t("uri_builder.err_port");
    return "";
  }

  function hostsError(scheme, hosts) {
    const srv = scheme === "mongodb+srv";
    if (hosts.length === 0 || hosts.every(h => !h.host)) return t("uri_builder.err_host_empty");
    if (srv && hosts.length > 1) return t("uri_builder.err_srv_hosts");
    for (const h of hosts) {
      const err = hostError(h, srv);
      if (err) return err;
    }
    return "";
  }

  function optionLines(text) {
    return String(text || "").split(/\r?\n|&/).map(s => s.trim()).filter(Boolean);
  }

  function optionsError(text) {
    return optionLines(text).some(line => line.indexOf("=") <= 0) ? t("uri_builder.err_options") : "";
  }

  // -------------------------------------------------------------------------
  // Connection string builder
  // -------------------------------------------------------------------------

  const URI_MODE_KEY = "mongorescue_uri_mode";
  // original is the URI the builder was filled from and baseline what the builder
  // makes of it unchanged: while the fields still produce baseline, the original
  // string is kept byte for byte (a saved connection's redacted URI must come back
  // exactly as served, or the server refuses the masked password).
  // opened is the connection string the dialog opened with.
  const builder = { mode: "paste", original: "", baseline: "", opened: "", hostSeq: 0 };

  function el(id) {
    return document.getElementById(id);
  }

  function hostRows() {
    return Array.from(document.querySelectorAll("#uri-hosts .uri-host"));
  }

  function addHostRow(host, port, focus) {
    const list = el("uri-hosts");
    const n = ++builder.hostSeq;
    const row = document.createElement("div");
    row.className = "uri-host";
    row.setAttribute("data-field-group", "");
    row.innerHTML = `<div class="uri-host-row">
        <input type="text" id="uri-host-${n}" class="form-input mono uri-host-name" autocomplete="off" autocapitalize="none" spellcheck="false" placeholder="db.example.com" required>
        <input type="text" id="uri-port-${n}" class="form-input mono uri-host-port" inputmode="numeric" autocomplete="off" maxlength="5" placeholder="27017">
        <button type="button" class="btn btn-ghost btn-sm uri-host-remove" data-action="uri-remove-host">${icon("x")}</button>
      </div>`;
    row.querySelector(".uri-host-name").value = host || "";
    row.querySelector(".uri-host-port").value = port || "";
    list.appendChild(row);
    labelHostRows();
    if (focus) row.querySelector(".uri-host-name").focus();
    return row;
  }

  function labelHostRows() {
    const rows = hostRows();
    rows.forEach((row, i) => {
      const n = i + 1;
      row.querySelector(".uri-host-name").setAttribute("aria-label", tf("uri_builder.host_n", { n }));
      row.querySelector(".uri-host-port").setAttribute("aria-label", tf("uri_builder.port_n", { n }));
      const remove = row.querySelector(".uri-host-remove");
      const label = tf("uri_builder.remove_host_n", { n });
      remove.setAttribute("aria-label", label);
      remove.title = label;
      remove.hidden = rows.length === 1;
    });
    const srv = el("uri-scheme").value === "mongodb+srv";
    el("uri-add-host").hidden = srv;
    rows.forEach(row => {
      const port = row.querySelector(".uri-host-port");
      port.hidden = srv;
      port.disabled = srv;
    });
  }

  function readBuilder() {
    const options = [];
    const add = (k, v) => {
      if (v) options.push([k, v]);
    };
    add("authSource", el("uri-auth-source").value.trim());
    add("replicaSet", el("uri-replica-set").value.trim());
    const srv = el("uri-scheme").value === "mongodb+srv";
    if (el("uri-tls").checked) {
      if (!srv) add("tls", "true");
      add("tlsCAFile", el("uri-tls-ca").value.trim());
      if (el("uri-tls-insecure").checked) add("tlsAllowInvalidCertificates", "true");
    } else if (srv) {
      add("tls", "false");
    }
    optionLines(el("uri-options").value).forEach(line => {
      const eq = line.indexOf("=");
      options.push(eq === -1 ? [line, ""] : [line.slice(0, eq).trim(), line.slice(eq + 1).trim()]);
    });
    return {
      scheme: el("uri-scheme").value,
      username: el("uri-username").value,
      password: el("uri-password").value,
      hosts: hostRows().map(row => ({
        host: row.querySelector(".uri-host-name").value.trim(),
        port: srv ? "" : row.querySelector(".uri-host-port").value.trim()
      })),
      path: el("uri-database").value.trim(),
      options
    };
  }

  function fillBuilder(p) {
    el("uri-scheme").value = p.scheme;
    el("uri-username").value = p.username;
    el("uri-password").value = p.password;
    el("uri-database").value = p.path;
    el("uri-hosts").textContent = "";
    (p.hosts.length ? p.hosts : [{ host: "", port: "" }]).forEach(h => addHostRow(h.host, h.port, false));
    const opt = (name) => {
      const found = p.options.find(([k]) => k.toLowerCase() === name);
      return found ? found[1] : "";
    };
    el("uri-auth-source").value = opt("authsource");
    el("uri-replica-set").value = opt("replicaset");
    const tlsValue = (opt("tls") || opt("ssl")).toLowerCase();
    el("uri-tls").checked = p.scheme === "mongodb+srv" ? tlsValue !== "false" : tlsValue === "true";
    el("uri-tls-ca").value = opt("tlscafile");
    el("uri-tls-insecure").checked = opt("tlsallowinvalidcertificates").toLowerCase() === "true";
    el("uri-options").value = p.options
      .filter(([k]) => !KNOWN_OPTIONS.includes(k.toLowerCase()))
      .map(([k, v]) => `${k}=${v}`).join("\n");
    updateTlsOptions();
  }

  function updateTlsOptions() {
    el("uri-tls-options").hidden = !el("uri-tls").checked;
  }

  // The URI the builder stands for: the original string while nothing changed.
  function builderURI() {
    const built = buildMongoURI(readBuilder(), false);
    return builder.original && built === builder.baseline ? builder.original : built;
  }

  // A saved connection's secrets come back masked. They may stay masked only while
  // the connection string is unchanged; any other change needs them typed again.
  function builderMaskError() {
    if (builder.mode !== "build") return "";
    return builderURI() === builder.original ? "" : t("uri_builder.err_masked_password");
  }

  function builderPasswordError(input) {
    return input.value === MASKED_SECRET ? builderMaskError() : "";
  }

  // Writes the builder's URI into the connection string field (which the save and
  // test logic of app.js read) and refreshes the preview.
  function syncFromBuilder() {
    const textarea = el("connection-uri");
    const uri = builderURI();
    el("uri-preview").textContent = uri === `${el("uri-scheme").value}://` ? "" : redactURI(uri);
    if (textarea.value !== uri) {
      textarea.value = uri;
      textarea.dispatchEvent(new Event("input", { bubbles: true }));
    }
    // Masked secrets turn invalid as soon as anything else changes.
    ["uri-password", "uri-options"].forEach(id => {
      const input = el(id);
      if (input.value.includes(MASKED_SECRET) || input.getAttribute("aria-invalid") === "true") validateField(input, true);
    });
  }

  function hostRowValidators() {
    const cfg = liveForms.get("form-connection");
    if (!cfg) return;
    const srv = el("uri-scheme").value === "mongodb+srv";
    const rows = hostRows();
    rows.forEach((row, i) => {
      const name = row.querySelector(".uri-host-name");
      const port = row.querySelector(".uri-host-port");
      cfg.validators[name.id] = () => {
        if (srv && rows.length > 1 && i > 0) return t("uri_builder.err_srv_hosts");
        const h = { host: name.value.trim(), port: "" };
        return h.host ? hostError(h, srv) : "";
      };
      cfg.validators[port.id] = () => {
        const v = port.value.trim();
        return v ? hostError({ host: "h", port: v }, false) : "";
      };
    });
  }

  function setUriMode(mode, focus) {
    const textarea = el("connection-uri");
    if (mode === "build") {
      const uri = textarea.value.trim();
      const p = uri ? parseMongoURI(uri) : { scheme: "mongodb", username: "", password: "", hosts: [], path: "", options: [] };
      if (!p) {
        showFieldError(textarea, t("uri_builder.err_unparseable"));
        textarea.focus();
        return;
      }
      fillBuilder(p);
      builder.original = uri;
      builder.baseline = uri ? buildMongoURI(readBuilder(), false) : "";
    }
    builder.mode = mode;
    storageSet(URI_MODE_KEY, mode === "build" ? "build" : "");
    const build = mode === "build";
    el("connection-uri-paste").hidden = build;
    el("connection-uri-builder").hidden = !build;
    textarea.required = !build;
    document.querySelectorAll('[data-action="uri-mode"]').forEach(btn => {
      btn.setAttribute("aria-pressed", String(btn.dataset.mode === mode));
    });
    const form = el("form-connection");
    clearFormErrors(form);
    if (build) {
      hostRowValidators();
      syncFromBuilder();
      if (focus) {
        const first = document.querySelector("#uri-hosts .uri-host-name");
        if (first) first.focus();
      }
    } else if (focus) {
      textarea.focus();
    }
  }

  // Called by openConnectionModal after it filled the form.
  function uriBuilderReset() {
    builder.original = "";
    builder.baseline = "";
    builder.opened = el("connection-uri").value;
    const preferred = storageGet(URI_MODE_KEY) === "build" ? "build" : "paste";
    if (preferred === "build" && !parseMongoURI(el("connection-uri").value || "mongodb://") ) {
      setUriMode("paste", false);
      return;
    }
    setUriMode(preferred, false);
  }

  // The field to focus when the connection string is missing.
  function uriFocusTarget() {
    if (builder.mode === "build") {
      const first = document.querySelector("#uri-hosts .uri-host-name");
      if (first) return first;
    }
    return el("connection-uri");
  }

  function setupBuilder() {
    const box = el("connection-uri-builder");
    if (!box) return;
    box.addEventListener("input", () => {
      syncFromBuilder();
    });
    box.addEventListener("change", (e) => {
      if (e.target.id === "uri-scheme") {
        labelHostRows();
        if (e.target.value === "mongodb+srv") {
          // SRV records name one host and imply TLS.
          hostRows().slice(1).forEach(row => row.remove());
          labelHostRows();
          el("uri-tls").checked = true;
        }
        hostRowValidators();
      }
      if (e.target.id === "uri-tls") updateTlsOptions();
      syncFromBuilder();
    });
    document.addEventListener("click", (e) => {
      const btn = e.target.closest("[data-action]");
      if (!btn || btn.disabled) return;
      switch (btn.dataset.action) {
        case "uri-mode":
          if (btn.dataset.mode !== builder.mode) setUriMode(btn.dataset.mode, true);
          break;
        case "uri-add-host":
          addHostRow("", "", true);
          hostRowValidators();
          syncFromBuilder();
          break;
        case "uri-remove-host": {
          const row = btn.closest(".uri-host");
          const next = row.nextElementSibling || row.previousElementSibling;
          row.remove();
          labelHostRows();
          hostRowValidators();
          syncFromBuilder();
          if (next) next.querySelector(".uri-host-name").focus();
          break;
        }
      }
    });
  }

  // -------------------------------------------------------------------------
  // Sessions
  // -------------------------------------------------------------------------

  const sessions = { items: [], loaded: false, error: "", seq: 0, user: "" };

  async function loadSessions() {
    const all = !!(el("sessions-all") && el("sessions-all").checked);
    const seq = ++sessions.seq;
    const uid = auth.user ? auth.user.id : "";
    if (sessions.user !== uid) {
      sessions.user = uid;
      sessions.loaded = false;
      sessions.items = [];
    }
    try {
      const json = await apiJSON(`/api/v1/auth/sessions${all ? "?all=true" : ""}`);
      if (seq !== sessions.seq) return;
      sessions.items = json.success ? json.data || [] : [];
      sessions.error = json.success ? "" : json.error || t("toasts.request_failed");
    } catch (err) {
      if (seq !== sessions.seq) return;
      sessions.items = [];
      sessions.error = err.message;
    }
    sessions.loaded = true;
    renderSessions();
  }

  function renderSessions() {
    const tbody = el("sessions-tbody");
    if (!tbody || !sessions.loaded) return;
    if (sessions.error) {
      setTbody(tbody, emptyRow(5, sessions.error));
      return;
    }
    if (sessions.items.length === 0) {
      setTbody(tbody, emptyRow(5, t("sessions.empty")));
      return;
    }
    setTbody(tbody, sessions.items.map(s => {
      const name = s.username || s.user_id || "";
      const label = s.current ? t("sessions.revoke_current") : t("settings.revoke");
      return `<tr>
        <td class="cell-primary"><span class="user-cell">${escapeHtml(name)}${s.current ? `<span class="chip chip-accent">${escapeHtml(t("sessions.this_browser"))}</span>` : ""}</span></td>
        <td>${timeCell(s.created_at)}</td>
        <td>${timeCell(s.last_seen_at)}</td>
        <td>${timeCell(s.expires_at)}</td>
        <td class="col-actions"><div class="row-actions">
          <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="revoke-session" data-id="${escapeHtml(s.id)}" aria-label="${escapeHtml(`${label}: ${name}`)}">${escapeHtml(label)}</button>
        </div></td>
      </tr>`;
    }).join(""));
  }

  async function revokeSession(id, btn) {
    const s = sessions.items.find(x => x.id === id);
    if (!s) return;
    const question = s.current ? t("sessions.confirm_revoke_current") : tf("sessions.confirm_revoke", { name: s.username || s.user_id || "" });
    if (!(await confirmDialog({ body: question, danger: true, confirmLabel: t("dialog.confirm") }))) return;
    if (btn) btn.disabled = true;
    try {
      const json = await apiJSON(`/api/v1/auth/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (json.success) {
        if (json.data && json.data.current) {
          // Like logging out: the cookie is gone.
          sessionExpiredShown = true;
          clearAuth();
          showLogin(t("auth.signed_out"));
          return;
        }
        showToast(t("sessions.revoked"), "success");
        loadSessions();
      } else {
        showToast(json.error || t("toasts.request_failed"), "error");
      }
    } catch (err) {
      showToast(err.message, "error");
    } finally {
      if (btn && document.contains(btn)) btn.disabled = false;
    }
  }

  // Opens a settings section from the user menu.
  function openAccountSection(section) {
    toggleUserMenu(false);
    activateTab("tab-settings", false);
    showSettingsSection(section, true);
  }

  function setupSessions() {
    const all = el("sessions-all");
    if (all) all.addEventListener("change", loadSessions);
    document.addEventListener("click", (e) => {
      const btn = e.target.closest("[data-action]");
      if (!btn || btn.disabled) return;
      switch (btn.dataset.action) {
        case "revoke-session":
          revokeSession(btn.dataset.id || "", btn);
          break;
        case "refresh-sessions":
          loadSessions();
          break;
        case "account-section":
          openAccountSection(btn.dataset.section || "");
          break;
      }
    });
    if (typeof onLanguageChange === "function") {
      onLanguageChange(() => {
        renderSessions();
        if (el("uri-hosts") && hostRows().length) labelHostRows();
      });
    }
  }

  // -------------------------------------------------------------------------
  // Boot
  // -------------------------------------------------------------------------

  window.mrAnnounce = announce;
  window.announceBackgroundChanges = announceBackgroundChanges;
  window.noteAnnounced = noteAnnounced;
  window.uriBuilderReset = uriBuilderReset;
  window.uriFocusTarget = uriFocusTarget;
  window.loadSessions = loadSessions;
  // Inline validation of the job, storage and channel forms (tables.js).
  window.mrLiveValidate = liveValidate;
  // For tests in the browser console and the headless check.
  window.mrForms = { parseMongoURI, buildMongoURI, redactURI, uriTextError };

  document.addEventListener("DOMContentLoaded", () => {
    setupValidation();
    setupBuilder();
    setupSessions();
  });
})();
