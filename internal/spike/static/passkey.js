"use strict";

// The only script. It calls navigator.credentials and posts the result.
// It does not keep the PRF output on the page: the hidden field is submitted
// and the result page never receives the output, only its hash.

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

function run() {
  var el = document.getElementById("ceremony");
  var form = document.getElementById("ceremony-form");
  var button = document.getElementById("ceremony-go");
  if (!el || !form || !button) return;
  var opts = JSON.parse(el.textContent);
  document.documentElement.dataset.spikeReady = "1";
  button.addEventListener("click", function () {
    var op = opts.kind === "create" ? create(opts) : get(opts);
    op.then(function (cred) {
      fill(form, opts, cred);
      // fetch, not form.submit(): a scripted submit in headless Chrome sends
      // Origin: null, which must be refused. fetch sends the document origin.
      var params = new URLSearchParams(new FormData(form));
      fetch(form.getAttribute("action"), {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: params.toString(),
        credentials: "same-origin"
      }).then(function (res) {
        return res.text();
      }).then(function (html) {
        document.open();
        document.write(html);
        document.close();
      }).catch(function (err) {
        var out = document.getElementById("ceremony-error");
        if (out) out.textContent = err && err.name ? err.name : "error";
      });
    }).catch(function (err) {
      var out = document.getElementById("ceremony-error");
      if (out) out.textContent = err && err.name ? err.name : "error";
    });
  });
}

function create(opts) {
  var publicKey = {
    rp: { id: opts.rpId, name: "Beacon passkey spike" },
    user: {
      id: b64urlToBytes(opts.userId),
      name: "beacon-spike",
      displayName: "Beacon spike"
    },
    challenge: b64urlToBytes(opts.challenge),
    pubKeyCredParams: [
      { type: "public-key", alg: -7 },
      { type: "public-key", alg: -8 },
      { type: "public-key", alg: -257 }
    ],
    authenticatorSelection: {
      residentKey: "preferred",
      userVerification: "required"
    },
    attestation: "none",
    timeout: 120000
  };
  if (opts.prf) publicKey.extensions = { prf: {} };
  return navigator.credentials.create({ publicKey: publicKey });
}

function get(opts) {
  var evalByCredential = {};
  var first = "";
  (opts.credentials || []).forEach(function (c) {
    evalByCredential[c.id] = { first: b64urlToBytes(c.salt) };
    if (!first) first = c.salt;
  });
  var publicKey = {
    challenge: b64urlToBytes(opts.challenge),
    rpId: opts.rpId,
    allowCredentials: (opts.credentials || []).map(function (c) {
      return { type: "public-key", id: b64urlToBytes(c.id) };
    }),
    userVerification: "required",
    timeout: 120000,
    extensions: {
      prf: {
        eval: { first: b64urlToBytes(first) },
        evalByCredential: evalByCredential
      }
    }
  };
  return navigator.credentials.get({ publicKey: publicKey });
}

function fill(form, opts, cred) {
  var ext = cred.getClientExtensionResults ? cred.getClientExtensionResults() : {};
  setField(form, "credential_id", bytesToB64url(cred.rawId));
  setField(form, "client_data_json", bytesToB64url(cred.response.clientDataJSON));
  if (opts.kind === "create") {
    setField(form, "attestation_object", bytesToB64url(cred.response.attestationObject));
    var enabled = ext && ext.prf && ext.prf.enabled === true;
    setField(form, "prf_enabled", enabled ? "true" : "false");
    return;
  }
  setField(form, "authenticator_data", bytesToB64url(cred.response.authenticatorData));
  setField(form, "signature", bytesToB64url(cred.response.signature));
  var prf = "";
  if (ext && ext.prf && ext.prf.results && ext.prf.results.first) {
    prf = bytesToB64url(ext.prf.results.first);
  }
  setField(form, "prf_output", prf);
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", run);
} else {
  run();
}
