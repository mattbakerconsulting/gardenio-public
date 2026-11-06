#!/usr/bin/env python3
"""Pins the rule order of the k3s API server audit policy (gd-HXRD9yizAE).

The policy in gardenio/terraform/modules/k3s/templates/node.sh.tftpl is
first-match-wins, so the order is a security property:

  1. the secrets / serviceaccounts/token Metadata rule must come before the
     gardenio-infra-admin RequestResponse rule, or the admin's Secret bodies
     and minted tokens are logged in full;
  2. the infra-admin rule must come before the events/leases drop, or the
     admin's event and lease writes go unaudited.

No PyYAML in the CI job, so the policy is split on its top-level "- level:"
lines and classified by the text of each rule. Run directly; exits non-zero on
any broken invariant. Cases 2-4 mutate the live policy to prove the check can
fail.
"""
import pathlib
import sys

TEMPLATE = (pathlib.Path(__file__).resolve().parent.parent
            / "gardenio/terraform/modules/k3s/templates/node.sh.tftpl")
ADMIN = "system:serviceaccount:gardenio:gardenio-infra-admin"


def policy_rules(text):
    start = text.index("cat > /etc/rancher/k3s/audit-policy.yaml <<'EOF'")
    body = text[start:].split("\n", 1)[1].split("\nEOF\n", 1)[0]
    rules, cur = [], None
    for line in body.splitlines():
        if line.startswith("  - level:"):
            cur = [line]
            rules.append(cur)
        elif cur is not None and line.startswith("    "):
            cur.append(line)
    return ["\n".join(r) for r in rules]


def classify(rule):
    level = rule.splitlines()[0].split(":", 1)[1].strip()
    if level == "Metadata" and '"secrets"' in rule:
        return "secrets"
    if ADMIN in rule:
        return "admin"
    if level == "None" and '"events"' in rule and '"leases"' in rule:
        return "noise"
    return None


def check(rules):
    """Return a list of problems with this rule list ([] when it is sound)."""
    pos = {}
    for i, r in enumerate(rules):
        kind = classify(r)
        if kind is None:
            continue
        if kind in pos:
            return [f"more than one {kind} rule"]
        pos[kind] = i
    missing = {"secrets", "admin", "noise"} - set(pos)
    if missing:
        return [f"missing rule(s): {sorted(missing)}"]
    errs = []
    if not pos["secrets"] < pos["admin"]:
        errs.append("secrets Metadata rule must precede the infra-admin rule")
    if not pos["admin"] < pos["noise"]:
        errs.append("infra-admin rule must precede the events/leases drop")
    if rules[pos["secrets"]].splitlines()[0].split(":", 1)[1].strip() != "Metadata":
        errs.append("secrets rule must be Metadata")
    if not rules[-1].strip().endswith("level: Metadata"):
        errs.append("catch-all must be the last rule, at Metadata")
    return errs


def main():
    rules = policy_rules(TEMPLATE.read_text())
    failures = 0

    def expect(name, got, want_ok):
        nonlocal failures
        ok = (not got) == want_ok
        print(f"{'ok  ' if ok else 'FAIL'} {name}" + ("" if ok else f": {got}"))
        failures += not ok

    expect("live policy is ordered", check(rules), True)

    idx = {classify(r): i for i, r in enumerate(rules) if classify(r)}
    swapped = list(rules)
    swapped[idx["secrets"]], swapped[idx["admin"]] = swapped[idx["admin"]], swapped[idx["secrets"]]
    expect("secrets/admin swap is rejected", check(swapped), False)

    swapped = list(rules)
    swapped[idx["noise"]], swapped[idx["admin"]] = swapped[idx["admin"]], swapped[idx["noise"]]
    expect("noise-above-admin is rejected", check(swapped), False)

    expect("dropped secrets rule is rejected",
           check([r for r in rules if classify(r) != "secrets"]), False)

    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
