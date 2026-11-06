# security-poison fixtures

Planted, KNOWN-BAD inputs the recurring `security-poison` self-test
(`scripts/security/security-poison-selftest.sh`, CI task `test-security-poison`)
feeds to each real scanner to prove the scanner is **actually wired and firing**
— the continuous defense against false-negative complacency (bundle
ka-0000005053 §2.3: "The poison fixture continuously proves the scanners are
wired").

The self-test is the gardener-poison-module pattern applied to the security bar:
plant a known defect, assert the gate goes RED, so a silently-broken scanner
(wrong config, disabled rule, un-invoked binary) fails the build LOUD instead of
green-washing every MR.

| Fixture | Scanner it exercises | Must trigger |
|---|---|---|
| `fake-aws-key.txt` | `gitleaks` (CI / Gitleaks) | an AWS access-key finding |
| `govulncheck-reachable.json` | `govulncheck-diff.py` (CI / Go-Vuln) | a NEW reachable critical vuln vs an empty baseline |
| `govulncheck-imported-only.json` | `govulncheck-diff.py` reachability logic | must NOT trigger — an imported-but-not-called crit is non-gating |
| `osv-report.json` | `osv-diff.py` (CI / OSV-Scan) | a NEW OSV vuln id vs an empty baseline |

These files are **never** scanned by the real pipeline gates — they live under
`scripts/security/poison/` which is allowlisted in `.gitleaks.toml` so the
planted key does not red the repo-wide `gitleaks` job. The self-test copies each
fixture into a throwaway workdir and runs the scanner there with a fixture-free
config, so the planted finding fires exactly where the self-test wants it and
nowhere else.
