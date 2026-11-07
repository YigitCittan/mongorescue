/**
 * MongoRescue dashboard: the outbound policy.
 *
 * The security settings require_https_endpoints and outbound_deny_private
 * (Settings → Security → API access) and the optional CA certificate of an S3
 * storage target (ca_pem). The strings live in i18n.js (settings.* and storage.*).
 *
 * Loaded after app.js; app.js calls outboundFillSecurity(),
 * outboundCollectSecurity(), outboundFillTargetForm() and outboundTargetPayload().
 * Server values reach the DOM only through form values.
 */

// The largest CA bundle the server accepts (storage.MaxCAPEMBytes).
const OUTBOUND_MAX_PEM = 64 * 1024;

// outboundFillSecurity fills the two switches (app.js fillSecurity).
function outboundFillSecurity(sec) {
  const https = document.getElementById("set-require-https-endpoints");
  const priv = document.getElementById("set-outbound-deny-private");
  if (https) https.checked = !!sec.require_https_endpoints;
  if (priv) priv.checked = !!sec.outbound_deny_private;
}

// outboundCollectSecurity returns the two switches (app.js collectSecurity).
function outboundCollectSecurity() {
  const https = document.getElementById("set-require-https-endpoints");
  const priv = document.getElementById("set-outbound-deny-private");
  const out = {};
  if (https) out.require_https_endpoints = https.checked;
  if (priv) out.outbound_deny_private = priv.checked;
  return out;
}

// outboundFillTargetForm shows the CA certificate of the S3 settings s3 of the
// storage form (app.js openStorageForm); an empty object clears it.
function outboundFillTargetForm(s3) {
  setValue("storage-ca-pem", (s3 && s3.ca_pem) || "");
}

// outboundTargetPayload returns ca_pem for the S3 payload of the storage form
// (app.js collectStorage); a FieldError marks a bundle that is not PEM.
function outboundTargetPayload() {
  const pem = getValue("storage-ca-pem").trim();
  if (pem && (!pem.includes("-----BEGIN CERTIFICATE-----") || pem.length > OUTBOUND_MAX_PEM)) {
    throw new FieldError("storage-ca-pem", t("storage.err_ca_pem"));
  }
  return { ca_pem: pem };
}

// outboundLoadCAFile reads the chosen file into the CA certificate field.
async function outboundLoadCAFile(input) {
  const target = document.getElementById("storage-ca-pem");
  const file = input.files && input.files[0];
  input.value = "";
  if (!target || !file) return;
  if (file.size > OUTBOUND_MAX_PEM) {
    showToast(t("storage.ca_pem_too_large"), "error");
    return;
  }
  target.value = await file.text();
  target.dispatchEvent(new Event("input", { bubbles: true }));
}

function setupOutbound() {
  const input = document.getElementById("storage-ca-pem-file");
  if (input) input.addEventListener("change", () => { outboundLoadCAFile(input); });
}

document.addEventListener("DOMContentLoaded", setupOutbound);
