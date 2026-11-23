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

function run() {
  var el = document.getElementById("ceremony");
  var form = document.getElementById("ceremony-form");
  var button = document.getElementById("ceremony-go");
  if (!el || !form || !button) return;
  var opts = JSON.parse(el.textContent);
  button.addEventListener("click", function () {
    var op = opts.kind === "approve" ? approve(opts) : register(opts);
    op.then(function (fields) {
      Object.keys(fields).forEach(function (name) { setField(form, name, fields[name]); });
      var params = new URLSearchParams(new FormData(form));
      fetch(form.getAttribute("action"), {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "Origin": location.origin },
        body: params.toString(),
        credentials: "same-origin"
      }).then(function (res) { return res.text(); }).then(function (html) {
        document.open();
        document.write(html);
        document.close();
      }).catch(show);
    }).catch(show);
  });
}

function show(err) {
  var out = document.getElementById("ceremony-error");
  if (out) out.textContent = err && err.name ? err.name : "error";
}

function approve(opts) {
  return navigator.credentials.get({ publicKey: {
    challenge: b64urlToBytes(opts.challenge),
    rpId: opts.rpId,
    allowCredentials: [{ type: "public-key", id: b64urlToBytes(opts.id) }],
    userVerification: "required",
    timeout: 120000,
    extensions: { prf: { eval: { first: b64urlToBytes(opts.salt) } } }
  } }).then(function (cred) {
    var got = cred.getClientExtensionResults ? cred.getClientExtensionResults() : {};
    var prf = got && got.prf && got.prf.results && got.prf.results.first ? bytesToB64url(got.prf.results.first) : "";
    return {
      nonce: opts.nonce,
      credential_id: bytesToB64url(cred.rawId),
      client_data_json: bytesToB64url(cred.response.clientDataJSON),
      authenticator_data: bytesToB64url(cred.response.authenticatorData),
      signature: bytesToB64url(cred.response.signature),
      prf_output: prf
    };
  });
}

function register(opts) {
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
  return navigator.credentials.create({ publicKey: publicKey }).then(function (created) {
    var ext = created.getClientExtensionResults ? created.getClientExtensionResults() : {};
    var enabled = ext && ext.prf && ext.prf.enabled === true;
    if (!enabled) {
      return { credential_id: bytesToB64url(created.rawId), create_client_data: bytesToB64url(created.response.clientDataJSON),
        attestation_object: bytesToB64url(created.response.attestationObject), prf_enabled: "false" };
    }
    return navigator.credentials.get({ publicKey: {
      challenge: b64urlToBytes(opts.assert),
      rpId: opts.rpId,
      allowCredentials: [{ type: "public-key", id: created.rawId }],
      userVerification: "required",
      timeout: 120000,
      extensions: { prf: { eval: { first: b64urlToBytes(opts.salt) } } }
    } }).then(function (asserted) {
      var got = asserted.getClientExtensionResults ? asserted.getClientExtensionResults() : {};
      var prf = got && got.prf && got.prf.results && got.prf.results.first ? bytesToB64url(got.prf.results.first) : "";
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
    });
  });
}

if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", run);
else run();
