"use strict";

function b64urlToBytes(s) {
  var pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  var bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + pad);
  var out = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function bytesToB64url(buf) {
  var bytes = new Uint8Array(buf);
  var bin = "";
  for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function setField(form, name, value) {
  var el = form.querySelector("[name=" + name + "]");
  if (el) el.value = value;
}

function run(diagnostics) {
  var el = document.getElementById("ceremony");
  var form = document.getElementById("ceremony-form");
  var button = document.getElementById("ceremony-go");
  if (!el || !form || !button) return;
  var opts = JSON.parse(el.textContent);
  form.addEventListener("submit", function (event) { event.preventDefault(); });
  button.addEventListener("click", function () {
    if (!form.reportValidity()) return;
    button.disabled = true;
    diagnostics.error = null;
    show(diagnostics);
    var op = opts.kind === "assert" ? approve(opts, diagnostics) : register(opts, diagnostics);
    op.then(function (fields) {
      Object.keys(fields).forEach(function (name) { setField(form, name, fields[name]); });
      var params = new URLSearchParams(new FormData(form));
      form.reset();
      return fetch(form.getAttribute("action"), {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "Origin": location.origin },
        body: params.toString(),
        credentials: "same-origin"
      }).then(async function (res) {
        var body = await res.text();
        if (!res.ok && opts.kind !== "assert") {
          // The server already logged the fixed reason. Keep the browser's PRF and UA visible.
          diagnostics.error = problem("Registration refused", body);
          show(diagnostics);
          button.disabled = false;
        } else {
          replacePage(body);
        }
      });
    }).catch(function (err) { button.disabled = false; failed(diagnostics, err); });
  });
}

function problem(name, message) {
  var err = new Error(message);
  err.name = name;
  return err;
}

function webAuthnAvailable() {
  return window.isSecureContext && !!window.PublicKeyCredential && !!navigator.credentials;
}

function show(diagnostics) {
  if (!diagnostics.out) return;
  var err = diagnostics.error;
  var note = err ? (err.name || "Error") + ": " + (err.message || "No error message.").trim() :
    diagnostics.out.dataset.flow === "sign-in" ? "WebAuthn is not used for this sign-in." : "No passkey error yet.";
  diagnostics.out.textContent = note.slice(0, 512) + " · PRF available: " + diagnostics.available +
    " · PRF returned: " + diagnostics.returned + " · Browser: " + navigator.userAgent.slice(0, 512);
}

function failed(diagnostics, err) {
  diagnostics.error = err || problem("Error", "No error message.");
  show(diagnostics);
  if (!diagnostics.out || diagnostics.out.dataset.flow !== "register") return;
  var names = ["NotAllowedError", "InvalidStateError", "SecurityError", "NotSupportedError", "AbortError",
    "ConstraintError", "UnknownError", "TypeError", "prf_unavailable", "prf_missing", "webauthn_unavailable"];
  // Do not serialize the error or any part of the ceremony. Only send a fixed reason to Beacon.
  var reason = names.includes(diagnostics.error.name) ? diagnostics.error.name : "other";
  fetch("/passkeys/report", {
    method: "POST", headers: { "Content-Type": "text/plain" }, body: reason,
    credentials: "omit", referrerPolicy: "origin", keepalive: true
  }).catch(function () {});
}

function requireWebAuthn() {
  if (!webAuthnAvailable()) {
    throw problem("webauthn_unavailable", "This browser or page does not offer the WebAuthn API.");
  }
}

function prfOutput(cred, diagnostics) {
  var ext = cred && cred.getClientExtensionResults ? cred.getClientExtensionResults() : {};
  var prf = ext && ext.prf && ext.prf.results && ext.prf.results.first;
  if (!prf || prf.byteLength !== 32) {
    diagnostics.returned = prf ? "invalid length" : "no";
    throw problem("prf_missing", "The passkey did not return the required PRF result.");
  }
  diagnostics.available = diagnostics.returned = "yes";
  diagnostics.authenticator = true;
  return bytesToB64url(prf);
}

async function approve(opts, diagnostics) {
  requireWebAuthn();
  diagnostics.returned = "no";
  var cred = await navigator.credentials.get({ publicKey: {
    challenge: b64urlToBytes(opts.challenge),
    rpId: opts.rpId,
    allowCredentials: [{ type: "public-key", id: b64urlToBytes(opts.id) }],
    userVerification: "required",
    timeout: 120000,
    extensions: { prf: { eval: { first: b64urlToBytes(opts.salt) } } }
  } });
  var prf = prfOutput(cred, diagnostics);
  return {
    nonce: opts.nonce,
    credential_id: bytesToB64url(cred.rawId),
    client_data_json: bytesToB64url(cred.response.clientDataJSON),
    authenticator_data: bytesToB64url(cred.response.authenticatorData),
    signature: bytesToB64url(cred.response.signature),
    prf_output: prf
  };
}

async function register(opts, diagnostics) {
  requireWebAuthn();
  diagnostics.returned = "not requested";
  var publicKey = {
    rp: { id: opts.rpId, name: "Beacon" },
    user: { id: b64urlToBytes(opts.userId), name: "beacon", displayName: "Beacon" },
    challenge: b64urlToBytes(opts.create),
    pubKeyCredParams: [
      { type: "public-key", alg: -7 },
      { type: "public-key", alg: -8 },
      { type: "public-key", alg: -257 }
    ],
    authenticatorSelection: { residentKey: "preferred", userVerification: "required" },
    attestation: "none",
    timeout: 120000,
    extensions: { prf: {} }
  };
  var created = await navigator.credentials.create({ publicKey: publicKey });
  var ext = created && created.getClientExtensionResults ? created.getClientExtensionResults() : {};
  var enabled = ext && ext.prf && ext.prf.enabled === true;
  diagnostics.authenticator = true;
  diagnostics.available = enabled ? "yes" : "no";
  if (!enabled) throw problem("prf_unavailable", "This passkey did not enable PRF during registration.");
  diagnostics.returned = "no";
  var asserted = await navigator.credentials.get({ publicKey: {
    challenge: b64urlToBytes(opts.assert),
    rpId: opts.rpId,
    allowCredentials: [{ type: "public-key", id: created.rawId }],
    userVerification: "required",
    timeout: 120000,
    extensions: { prf: { eval: { first: b64urlToBytes(opts.salt) } } }
  } });
  var prf = prfOutput(asserted, diagnostics);
  return {
    credential_id: bytesToB64url(created.rawId),
    create_client_data: bytesToB64url(created.response.clientDataJSON),
    assert_client_data: bytesToB64url(asserted.response.clientDataJSON),
    attestation_object: bytesToB64url(created.response.attestationObject),
    prf_enabled: "true",
    authenticator_data: bytesToB64url(asserted.response.authenticatorData),
    signature: bytesToB64url(asserted.response.signature),
    prf_output: prf
  };
}

function replacePage(html) {
  document.open();
  document.write(html);
  document.close();
}

function bindSetup(diagnostics) {
  var form = document.querySelector('form[action="/passkeys/new"]');
  if (!form) return;
  form.dataset.bound = "1";
  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var code = form.querySelector("[name=code]").value;
    fetch("/passkeys/new", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded", "Origin": location.origin },
      body: new URLSearchParams({ code: code }).toString(),
      credentials: "same-origin"
    }).then(function (res) { return res.text(); }).then(replacePage).catch(function (err) { failed(diagnostics, err); });
  });
}

function start() {
  var diagnostics = {
    out: document.getElementById("ceremony-error"),
    available: webAuthnAvailable() ? "unknown" : "no (WebAuthn unavailable)",
    returned: "not requested"
  };
  show(diagnostics);
  if (diagnostics.out && webAuthnAvailable() && typeof PublicKeyCredential.getClientCapabilities === "function") {
    Promise.resolve().then(function () { return PublicKeyCredential.getClientCapabilities(); }).then(function (caps) {
      if (!diagnostics.authenticator && caps && typeof caps["extension:prf"] === "boolean") {
        diagnostics.available = caps["extension:prf"] ? "yes (browser)" : "no (browser)";
        show(diagnostics);
      }
    }).catch(function (err) {
      if (!diagnostics.authenticator && !diagnostics.error) {
        diagnostics.error = err;
        show(diagnostics);
      }
    });
  }
  bindSetup(diagnostics);
  run(diagnostics);
}

if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
else start();
