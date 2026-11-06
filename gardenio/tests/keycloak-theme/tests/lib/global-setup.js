// Runs once before every spec: the mail sink Keycloak sends to, and the
// passkey-only test client. A failure here is logged, not thrown, so specs that
// need neither (the S1c screens) still run; the specs that do need them fail
// with their own, clearer error.
const fs = require('fs');
const os = require('os');
const path = require('path');
const { startMailSink } = require('./mail');
const { ensurePasskeyOnlyClient } = require('./admin');

module.exports = async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gd-mail-'));
  process.env.KC_MAIL_DIR = dir; // workers inherit this
  let sink = null;
  try {
    sink = await startMailSink(dir, Number(process.env.KC_SMTP_PORT || 2525));
  } catch (e) {
    console.warn(`global-setup: mail sink not started: ${e.message}`);
  }
  try {
    await ensurePasskeyOnlyClient();
  } catch (e) {
    console.warn(`global-setup: passkey-only client not created: ${e.message}`);
  }
  return async () => {
    if (sink) sink.close();
    fs.rmSync(dir, { recursive: true, force: true });
  };
};
