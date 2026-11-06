#!/usr/bin/env python3
"""support_forward.py -- forward mail sent to support@ to the operator's mailbox.

Lands Track B §6.1 (gd-UBeJ3kVicQ): people who cannot sign in reach the
operator by mail. SES receives mail for support@<domain>
(module gardenio/terraform/modules/ses-support-inbound), scans it, stores the
raw message in S3 and invokes this Lambda, which re-sends it to the operator:

  From:      "<sender> via support" <FORWARD_FROM>   (SES only sends as our domain)
  Reply-To:  the sender's Reply-To, else their From  (so the operator's reply reaches them)
  Subject:   "[support] <subject>", with "[spam?]" when SES's spam check failed
  X-Gardenio-Support-Verdicts: spam, virus, SPF, DKIM and DMARC as SES saw them

A message that fails the virus scan, or that SES will not send (too big,
malformed), is never forwarded as it is: the operator gets a short plain-text
notice with the sender, the subject and the S3 key instead, so nothing sent to
support@ is lost silently. A message carrying our own forwarded marker, or sent
from FORWARD_FROM, is dropped (a mail loop).

Stdlib only (boto3 is imported lazily, and only in the Lambda handler).

Environment: FORWARD_TO (the operator's mailbox), FORWARD_FROM (an address on
the SES identity, the only one the role may send as), MAIL_BUCKET and
MAIL_PREFIX (where the receipt rule stores the raw message).
"""

import email
import email.message
import email.policy
import email.utils
import os
import sys

MARKER = "X-Gardenio-Support-Forwarded"
VERDICTS = "X-Gardenio-Support-Verdicts"
SUBJECT_TAG = "[support]"
SPAM_TAG = "[spam?]"

# Headers that would make the re-sent message fail SES or DMARC, or route a
# bounce or reply to the wrong place. SES sets its own Message-ID and
# Return-Path, and signs with our DKIM key.
DROP_HEADERS = ("Return-Path", "Sender", "DKIM-Signature", "Message-ID",
                "Reply-To", "From", "Subject", "Errors-To", "Bcc",
                MARKER, VERDICTS)


def verdicts(receipt):
    """receipt: the SES event's receipt object -> {name: status}."""
    out = {}
    for name, key in (("spam", "spamVerdict"), ("virus", "virusVerdict"), ("spf", "spfVerdict"),
                      ("dkim", "dkimVerdict"), ("dmarc", "dmarcVerdict")):
        out[name] = ((receipt or {}).get(key) or {}).get("status", "UNKNOWN")
    return out


def _one_line(s, limit=200):
    s = " ".join(str(s or "").split())
    return s if len(s) <= limit else s[:limit - 1] + "…"


def sender_name(addr_header):
    """'Ann <ann@x>' -> 'Ann', 'ann@x' -> 'ann@x' (one line, no quotes or angle brackets)."""
    name, addr = email.utils.parseaddr(addr_header or "")
    return _one_line((name or addr or "unknown sender").replace('"', "").replace("<", "").replace(">", ""), 80)


def is_loop(msg, forward_from):
    if msg.get(MARKER) is not None:
        return True
    _, addr = email.utils.parseaddr(msg.get("From", ""))
    return addr.lower() == forward_from.lower()


def rewrite(raw, forward_from, v, key):
    """raw bytes of the received message -> raw bytes to send to the operator."""
    msg = email.message_from_bytes(raw, policy=email.policy.compat32)
    orig_from = msg.get("From", "")
    reply_to = msg.get("Reply-To") or orig_from
    subject = _one_line(msg.get("Subject", ""), 900) or "(no subject)"
    for h in DROP_HEADERS:
        del msg[h]
    tags = SUBJECT_TAG + (" " + SPAM_TAG if v.get("spam") == "FAIL" else "")
    msg["From"] = email.utils.formataddr((sender_name(orig_from) + " via support", forward_from))
    if reply_to:
        msg["Reply-To"] = reply_to
    msg["Subject"] = tags + " " + subject
    msg[MARKER] = key
    msg[VERDICTS] = "; ".join(f"{k}={s}" for k, s in v.items())
    return msg.as_bytes()


def notice(raw_headers, forward_from, forward_to, v, location, why):
    """A short plain-text message in place of one that cannot be forwarded as it is."""
    msg = email.message.EmailMessage()
    msg["From"] = email.utils.formataddr(("support@ forwarder", forward_from))
    msg["To"] = forward_to
    msg["Subject"] = f"{SUBJECT_TAG} not forwarded: {why}"
    msg[MARKER] = location
    msg.set_content(
        f"A message to support@ was not forwarded: {why}.\n\n"
        f"From:    {_one_line(raw_headers.get('from'))}\n"
        f"Subject: {_one_line(raw_headers.get('subject'))}\n"
        f"Date:    {_one_line(raw_headers.get('date'))}\n"
        f"Checks:  {'; '.join(f'{k}={s}' for k, s in v.items())}\n\n"
        f"The raw message is kept at {location} until the bucket's expiry.\n"
        "Open it only if you trust it (gardenio/docs/lands/support-runbook.md, 'Mail to support@').\n")
    return msg.as_bytes()


def forward(record, get_object, send_raw, env):
    """One SES receipt record. Returns what was done: forwarded, notice or dropped:<why>."""
    mail, receipt = record["ses"]["mail"], record["ses"]["receipt"]
    v = verdicts(receipt)
    key = env["MAIL_PREFIX"] + mail["messageId"]
    location = f"s3://{env['MAIL_BUCKET']}/{key}"
    headers = mail.get("commonHeaders") or {}
    hdrs = {"from": ", ".join(headers.get("from") or []) or mail.get("source", ""),
            "subject": headers.get("subject", ""), "date": headers.get("date", "")}
    to, frm = env["FORWARD_TO"], env["FORWARD_FROM"]

    if v["virus"] == "FAIL":
        send_raw(frm, to, notice(hdrs, frm, to, v, location, "it failed the virus scan"))
        return "notice"
    raw = get_object(env["MAIL_BUCKET"], key)
    msg = email.message_from_bytes(raw, policy=email.policy.compat32)
    if is_loop(msg, frm):
        return "dropped:loop"
    try:
        send_raw(frm, to, rewrite(raw, frm, v, location))
        return "forwarded"
    except Exception as e:  # SES refused it (size, a malformed header): say so, never lose it
        send_raw(frm, to, notice(hdrs, frm, to, v, location,
                                 f"SES would not send it ({_one_line(type(e).__name__, 60)})"))
        return "notice"


def handler(event, context):
    import boto3  # lazy: stdlib-only everywhere else

    s3, ses = boto3.client("s3"), boto3.client("ses")
    env = {k: os.environ[k] for k in ("FORWARD_TO", "FORWARD_FROM", "MAIL_BUCKET", "MAIL_PREFIX")}

    def get_object(bucket, key):
        return s3.get_object(Bucket=bucket, Key=key)["Body"].read()

    def send_raw(source, dest, data):
        ses.send_raw_email(Source=source, Destinations=[dest], RawMessage={"Data": data})

    done = [forward(r, get_object, send_raw, env) for r in event.get("Records", [])]
    print({"forwarded": done})
    return {"disposition": "CONTINUE"}


if __name__ == "__main__":
    print(__doc__)
    sys.exit(0)
