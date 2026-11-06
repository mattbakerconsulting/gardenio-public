#!/usr/bin/env python3
"""Tests for support_forward.py. No AWS: S3 and SES are injected.

Run: python3 scripts/support-forward/support_forward_test.py
"""

import email
import email.policy
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import support_forward as sf  # noqa: E402

ENV = {"FORWARD_TO": "owner@example.com", "FORWARD_FROM": "support@lands.example.com",
       "MAIL_BUCKET": "gardenio-support-mail-1", "MAIL_PREFIX": "support/"}

RAW = (b"Return-Path: <bounce@tenant.example>\r\n"
       b"DKIM-Signature: v=1; a=rsa-sha256; d=tenant.example; b=abc\r\n"
       b"Message-ID: <m1@tenant.example>\r\n"
       b"From: Ann Tenant <ann@tenant.example>\r\n"
       b"To: support@lands.example.com\r\n"
       b"Subject: I cannot sign in\r\n"
       b"Date: Wed, 21 Oct 2026 22:00:00 +0000\r\n"
       b"Content-Type: text/plain; charset=utf-8\r\n"
       b"\r\n"
       b"My land is tomato-patch and the sign-in page loops.\r\n")


def record(spam="PASS", virus="PASS", message_id="abc123"):
    return {"ses": {
        "mail": {"messageId": message_id, "source": "bounce@tenant.example",
                 "commonHeaders": {"from": ["Ann Tenant <ann@tenant.example>"],
                                   "subject": "I cannot sign in",
                                   "date": "Wed, 21 Oct 2026 22:00:00 +0000"}},
        "receipt": {"spamVerdict": {"status": spam}, "virusVerdict": {"status": virus},
                    "spfVerdict": {"status": "PASS"}, "dkimVerdict": {"status": "PASS"},
                    "dmarcVerdict": {"status": "PASS"}}}}


class Fake:
    def __init__(self, raw=RAW, fail_first_send=False):
        self.raw, self.fail, self.sent, self.got = raw, fail_first_send, [], []

    def get_object(self, bucket, key):
        self.got.append((bucket, key))
        return self.raw

    def send_raw(self, source, dest, data):
        if self.fail:
            self.fail = False
            raise ValueError("Message length is more than 10485760 bytes long")
        self.sent.append((source, dest, email.message_from_bytes(data, policy=email.policy.compat32)))


class ForwardTest(unittest.TestCase):
    def test_forwards_with_reply_to_the_sender(self):
        f = Fake()
        self.assertEqual(sf.forward(record(), f.get_object, f.send_raw, ENV), "forwarded")
        self.assertEqual(f.got, [("gardenio-support-mail-1", "support/abc123")])
        (source, dest, msg), = f.sent
        self.assertEqual((source, dest), (ENV["FORWARD_FROM"], ENV["FORWARD_TO"]))
        self.assertEqual(msg["From"], 'Ann Tenant via support <support@lands.example.com>')
        self.assertEqual(msg["Reply-To"], "Ann Tenant <ann@tenant.example>")
        self.assertEqual(msg["Subject"], "[support] I cannot sign in")
        self.assertIn("tomato-patch", msg.get_payload())
        self.assertEqual(msg[sf.MARKER], "s3://gardenio-support-mail-1/support/abc123")
        self.assertIn("spam=PASS", msg[sf.VERDICTS])
        for h in ("Return-Path", "DKIM-Signature", "Message-ID"):
            self.assertIsNone(msg[h], h)
        self.assertEqual(len(msg.get_all("From")), 1)

    def test_keeps_the_senders_own_reply_to(self):
        f = Fake(raw=RAW.replace(b"Subject:", b"Reply-To: team@tenant.example\r\nSubject:"))
        sf.forward(record(), f.get_object, f.send_raw, ENV)
        self.assertEqual(f.sent[0][2]["Reply-To"], "team@tenant.example")

    def test_spam_is_tagged_not_dropped(self):
        f = Fake()
        sf.forward(record(spam="FAIL"), f.get_object, f.send_raw, ENV)
        self.assertEqual(f.sent[0][2]["Subject"], "[support] [spam?] I cannot sign in")

    def test_virus_sends_a_notice_and_never_reads_the_message(self):
        f = Fake()
        self.assertEqual(sf.forward(record(virus="FAIL"), f.get_object, f.send_raw, ENV), "notice")
        self.assertEqual(f.got, [])
        msg = f.sent[0][2]
        self.assertEqual(msg["Subject"], "[support] not forwarded: it failed the virus scan")
        body = msg.get_payload(decode=True).decode()
        self.assertIn("Ann Tenant <ann@tenant.example>", body)
        self.assertIn("s3://gardenio-support-mail-1/support/abc123", body)
        self.assertNotIn("tomato-patch", body)

    def test_a_refused_send_becomes_a_notice(self):
        f = Fake(fail_first_send=True)
        self.assertEqual(sf.forward(record(), f.get_object, f.send_raw, ENV), "notice")
        self.assertIn("SES would not send it (ValueError)", f.sent[0][2]["Subject"])

    def test_our_own_forward_is_a_loop(self):
        looped = RAW.replace(b"Subject:", sf.MARKER.encode() + b": s3://x/y\r\nSubject:")
        f = Fake(raw=looped)
        self.assertEqual(sf.forward(record(), f.get_object, f.send_raw, ENV), "dropped:loop")
        self.assertEqual(f.sent, [])
        f = Fake(raw=RAW.replace(b"From: Ann Tenant <ann@tenant.example>", b"From: support@lands.example.com"))
        self.assertEqual(sf.forward(record(), f.get_object, f.send_raw, ENV), "dropped:loop")

    def test_sender_name_cannot_inject_a_header(self):
        for hostile in ('"Eve\r\nBcc: x@y" <eve@x>', 'Eve\nBcc: x@y <eve@x>', '"Eve <a@b>" <eve@x>'):
            name = sf.sender_name(hostile)
            self.assertFalse(set(name) & set('\r\n<>"'), name)
        self.assertEqual(sf.sender_name("<>"), "unknown sender")
        f = Fake(raw=RAW.replace(b"Subject: I cannot sign in", b"Subject: a\r\n b"))
        sf.forward(record(), f.get_object, f.send_raw, ENV)
        self.assertEqual(f.sent[0][2]["Subject"], "[support] a b")


if __name__ == "__main__":
    unittest.main()
