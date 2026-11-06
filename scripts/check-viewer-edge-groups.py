#!/usr/bin/env python3
"""check-viewer-edge-groups.py

Guard which Keycloak groups the viewer and mux edges admit.

WHY THIS EXISTS
---------------
Plan project-scoped-humans (gd-oySDauyQcj) lets a member of a `/project-<name>`
group sign in to the viewer at gardenio.clonk.tv, and the viewer then reads as
that person. The coarse gate is the `allowed_groups=` list on each viewer
sign-in check (oauth2-proxy accepts a comma-separated any-of list); kbeads is
the real scope. mux.clonk.tv is the opposite: its nginx block injects the
admin COOPMUX_TOKEN and carries no per-human scope, so a project human who
reached it would hold a shell into every agent. Its check must stay exactly
`/gardenio-admins`.

Both lists are hand-typed in nginx, so each can drift silently:
  - a project added to the realm but not to the viewer list gets a 403 at the
    edge with no error anywhere else;
  - a project group pasted into the mux list is a privilege escalation that
    renders, applies and passes every other check.

WHAT IT CHECKS
--------------
  viewer  every sign-in check in a `server_name gardenio.clonk.tv` block (in
          public-edge.yaml, which must have one, and viewer-authproxy.yaml,
          which has none since its ziti front door went, gd-xNMLrsgajy, but
          is still checked should one return) admits exactly
          /gardenio-admins, /lands-tenants and, for each project group of
          deploy-values/gardenio-k3s.yaml (keycloak.projectGroups[].name and
          any keycloak.groups[] entry named project-*), /project-<name> and
          its land-role subgroups /project-<name>/owners and
          /project-<name>/guests.
  lands   the one exception: /oauth2/auth-lands, the check of
          `location ^~ /lands/` (the Lands front of house), admits that list
          plus /invited. GitHub sign-in is invite-only (owner ruling
          gd-hCMlZ5jnvB = b, gd-ycE484kLyk): a person gets past it once
          their email invite is claimed, and every group the viewer already
          admitted still does, so no member is locked out. public-edge.yaml
          must have it, `^~ /lands/` must use it, no other location may, and
          deploy-values must declare the invited group.
  mux     every sign-in check in a `server_name mux.clonk.tv` block admits
          exactly /gardenio-admins; public-edge.yaml must have one.
  open    a sign-in check with no allowed_groups admits every signed-in realm
          user, a GitHub registrant with no invite included. No location of
          the viewer, the mux or the api host may use one. (`^~ /lands/`
          used one until sign-in became invite-only.)
  demo    the one exception to `open`: /oauth2/auth-lands-demo, an open check
          on gardenio.clonk.tv, lets a stranger reach the demo land's door
          (owner decision gd-T37nCeWMhq = open). Only `location = /lands/demo`
          (asking for a pass) and `location = /lands/accept` (the terms the
          door wants accepted first) may use it, and it must stay open: a
          group list on it would be a second /lands/ list to keep in step.
          The edge need not have it; without it the door is invite-only.
  api     api.clonk.tv (public-edge.yaml) has exactly two sign-in checks:
          /oauth2/auth admits exactly /gardenio-admins (kd and gb on the
          daemon's /v1/*), and /oauth2/auth-land admits exactly
          /project-$land_door_name and its /owners and /guests (the land
          door, Door D.2). Only the land door's proxy location and the land
          feed's (D.9, /land/<name>/v1/lands/v1/lands/<land>/events), each
          inside `location ^~ /land/`, may use the land check, and both
          must. /oauth2/auth-land-token (D.18a, a minted land token asked
          of the daemon's /v1/me) is not a sign-in check and admits no
          group; /oauth2/auth-land reaches it, and no location may
          auth_request it directly.

WHY /lands-tenants
------------------
Every Lands tenant joins the static chart group /lands-tenants (Track B 7.8),
so a land the provisioner creates at runtime needs no edit here: the viewer
admits the tenant through it, and kbeads and the viewer scope them to their
/project-<land> groups. The group carries gardenio-project-member only.

WHY THE SUBGROUPS
-----------------
Land roles are EXCLUSIVE (gd-9aCcG1zCCY, spec section 6.4): an owner or guest
is a direct member of /project-<name>/owners or /guests ONLY, never of the
parent, because kbeads takes the highest role per land and a guest who is also
in the parent reads as a member. Keycloak's full-path groups claim lists only
direct memberships and oauth2-proxy matches exact strings, so the edge must
name both subgroups or it refuses every owner and guest 403 (gd-A9avO8cXBr).

WRITING THE LISTS
-----------------
`viewer_groups()` is the one place the viewer list is derived. Run with
--write to rewrite every viewer sign-in check in both manifests from it
(keeping each check's own %2F-or-/ spelling) instead of hand-typing the list,
then apply the manifests. The mux check is never written.

WHY THE LAND DOOR IS A TEMPLATE
-------------------------------
/land/<name>/v1/* admits the members of the land the path names, so its list
is built per request from $land_door_name (a map on the raw request line in
public-edge.yaml) rather than written from deploy-values: a land created at
runtime has a door with no edit here. /gardenio-admins is left out on purpose;
operators use /v1/* on the same host.

The lists are static: a land added at runtime (Track B 8.6 tenant lands) is
not in deploy-values and so is not listed here; its tenants get in through
/lands-tenants.

Stdlib only (the script-selftests job has no PyYAML). GARDENIO_ROOT overrides
the repo root, for the self-test.
"""
import os
import re
import sys
from urllib.parse import unquote

ROOT = os.environ.get('GARDENIO_ROOT') or os.path.dirname(
    os.path.dirname(os.path.abspath(__file__)))
VALUES = 'deploy-values/gardenio-k3s.yaml'
EDGE = 'gardenio/helm/gardenio-ziti/public-edge.yaml'
AUTHPROXY = 'gardenio/helm/gardenio-ziti/viewer-authproxy.yaml'
ADMINS = '/gardenio-admins'
TENANTS = '/lands-tenants'
INVITED = '/invited'
LANDS_LOCATION = '^~ /lands/'
LANDS_CHECK = '/oauth2/auth-lands'
DEMO_CHECK = '/oauth2/auth-lands-demo'
DEMO_LOCATIONS = ('= /lands/demo', '= /lands/accept')
LAND_ROLE_SUBGROUPS = ('owners', 'guests')
VIEWER_HOST = 'gardenio.clonk.tv'
API_HOST = 'api.clonk.tv'
ADMIN_CHECK = '/oauth2/auth'
LAND_CHECK = '/oauth2/auth-land'
LAND_TOKEN_CHECK = '/oauth2/auth-land-token'
LAND_VAR = '$land_door_name'
LAND_DOOR_PARENT = '^~ /land/'
LAND_DOOR_LOCATION = '~ "^/land/[a-z][a-z0-9-]{0,62}/v1/"'
LAND_FEED_LOCATION = '~ "^/land/[a-z][a-z0-9-]{0,62}/v1/lands/v1/lands/[a-z][a-z0-9-]{0,62}/events$"'
# The locations that use the land check, by what the messages call them.
LAND_CHECK_USERS = ((LAND_DOOR_LOCATION, 'land door'), (LAND_FEED_LOCATION, 'land feed'))

AUTH_RE = re.compile(r'proxy_pass\s+[^;\n]*?/oauth2/auth\?[^;\n]*?allowed_groups=([^;&\s]*)')
SERVER_NAME_RE = re.compile(r'\bserver_name\s+([^;]+);')
CHECK_RE = re.compile(r'proxy_pass\s+[^;\n]*?/oauth2/auth(\?[^;\n]*)?;')
# A regex location is quoted when it holds a brace ("{0,62}").
LOCATION_RE = re.compile(r'\blocation\s+((?:"[^"]*"|[^{;"])*?)\s*\{')
AUTH_REQUEST_RE = re.compile(r'\bauth_request\s+(\S+);')


def read(rel):
    with open(os.path.join(ROOT, rel)) as f:
        return f.read()


def project_groups(text):
    """Project names from keycloak.projectGroups and keycloak.groups[project-*]."""
    names = set()
    section = None  # the keycloak child list being read
    in_keycloak = False
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        indent = len(line) - len(line.lstrip())
        if indent == 0:
            in_keycloak = line.startswith('keycloak:')
            section = None
            continue
        if not in_keycloak:
            continue
        if indent == 2:
            m = re.match(r'(\w+):\s*(\[\])?\s*(#.*)?$', line.strip())
            section = m.group(1) if m else None
            continue
        m = re.match(r'-\s*name:\s*["\']?([^"\'#\s]+)', line.strip())
        if not m or indent != 4:
            continue
        if section == 'projectGroups':
            names.add(m.group(1))
        elif section == 'groups' and m.group(1).startswith('project-'):
            names.add(m.group(1)[len('project-'):])
    return names


def viewer_groups(projects):
    """The groups the viewer edge admits, in the order --write spells them."""
    out = [ADMINS, TENANTS]
    for p in sorted(projects):
        out.append('/project-' + p)
        out += ['/project-%s/%s' % (p, r) for r in LAND_ROLE_SUBGROUPS]
    return out


def static_groups(text):
    """Names of the keycloak.groups entries of deploy-values that are not
    project groups."""
    names = set()
    in_keycloak, in_groups = False, False
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        indent = len(line) - len(line.lstrip())
        if indent == 0:
            in_keycloak = line.startswith('keycloak:')
            in_groups = False
            continue
        if not in_keycloak:
            continue
        if indent == 2:
            in_groups = re.match(r'groups:\s*(#.*)?$', line.strip()) is not None
            continue
        m = re.match(r'-\s*name:\s*["\']?([^"\'#\s]+)', line.strip())
        if in_groups and m and indent == 4:
            names.add(m.group(1))
    return names


def lands_groups(projects):
    """The groups the /lands/ check admits: the viewer's, and /invited."""
    out = viewer_groups(projects)
    return out[:2] + [INVITED] + out[2:]


def blank_comments(text):
    """`text` with comments overwritten by spaces, so offsets still index it."""
    return re.sub(r'#[^\n]*', lambda m: ' ' * len(m.group(0)), text)


def block_end(text, i):
    """Offset just past the `}` closing the block whose body starts at `i`."""
    depth = 1
    while i < len(text) and depth:
        depth += {'{': 1, '}': -1}.get(text[i], 0)
        i += 1
    return i


def server_blocks(text):
    """(server_name, start, end) for each nginx `server {` block of `text`,
    which must already have its comments blanked."""
    blocks = []
    for m in re.finditer(r'\bserver\s*\{', text):
        i = block_end(text, m.end())
        n = SERVER_NAME_RE.search(text, m.end(), i)
        blocks.append((n.group(1).split() if n else [], m.end(), i))
    return blocks


def location_spans(text, start, end):
    """(head, body start, body end) of each `location <head> {` directly
    inside text[start:end]."""
    out, i = [], start
    while True:
        m = LOCATION_RE.search(text, i, end)
        if not m:
            return out
        j = block_end(text, m.end())
        out.append((' '.join(m.group(1).split()), m.end(), j - 1))
        i = j


def locations(text, start, end):
    """(head, body) of each `location <head> {` directly inside text[start:end]."""
    return [(h, text[s:e]) for h, s, e in location_spans(text, start, end)]


def all_locations(text, start, end, parent=None):
    """(head, own body, parent head) of every location in text[start:end],
    nested ones included. A location's own body leaves out its children."""
    out = []
    for head, s, e in location_spans(text, start, end):
        own, i = [], s
        for _, cs, ce in location_spans(text, s, e):
            own.append(text[i:cs])
            i = ce + 1
        own.append(text[i:e])
        out.append((head, ''.join(own), parent))
        out += all_locations(text, s, e, head)
    return out


def open_check_uses(text, host):
    """(open check names, [(location head, check name)] of every auth_request
    to one) in blocks serving `host`. An open check is an internal location
    whose sign-in check carries no allowed_groups: it admits every signed-in
    user."""
    text = blank_comments(text)
    names, uses = set(), []
    for server, start, end in server_blocks(text):
        if host not in server:
            continue
        locs = locations(text, start, end)
        for head, body in locs:
            m = CHECK_RE.search(body)
            if m and 'allowed_groups=' not in (m.group(1) or '') and head.startswith('= '):
                names.add(head[2:])
        for head, body in locs:
            for a in AUTH_REQUEST_RE.finditer(body):
                if a.group(1) in names:
                    uses.append((head, a.group(1)))
    return names, uses


def auth_matches(text, host):
    """AUTH_RE matches of every sign-in check in blocks serving `host`.
    Match offsets index `text`."""
    return [m for _, m in named_auth_matches(text, host)]


def named_auth_matches(text, host):
    """(check name, AUTH_RE match) of every sign-in check in blocks serving
    `host`. The name is the `location = <name>` holding the check, or '' for
    one outside such a location. Match offsets index `text`."""
    text = blank_comments(text)
    out = []
    for names, start, end in server_blocks(text):
        if host not in names:
            continue
        named = {}
        for head, s, e in location_spans(text, start, end):
            if head.startswith('= '):
                for m in AUTH_RE.finditer(text, s, e):
                    named[m.start()] = head[2:]
        out += [(named.get(m.start(), ''), m) for m in AUTH_RE.finditer(text, start, end)]
    return out


def viewer_problems(text, rel, want_viewer, want_lands):
    """Problems with the sign-in checks of the gardenio.clonk.tv blocks."""
    problems = []
    found = named_auth_matches(text, VIEWER_HOST)
    if not found and rel == EDGE:
        problems.append('%s: no sign-in check in a %s block' % (rel, VIEWER_HOST))
    for name, m in found:
        got = set(filter(None, unquote(m.group(1)).split(',')))
        if name == DEMO_CHECK:
            problems.append('%s: viewer %s has allowed_groups %s; it is the demo door\'s open check '
                            '(any signed-in account), so it takes none' % (rel, DEMO_CHECK, sorted(got)))
        elif name == LANDS_CHECK:
            if got != want_lands:
                problems.append(
                    '%s: viewer %s allowed_groups %s != %s (the viewer list plus %s: GitHub sign-in is '
                    'invite-only; --write regenerates it)' % (rel, LANDS_CHECK, sorted(got), sorted(want_lands), INVITED))
        elif got != want_viewer:
            problems.append(
                '%s: viewer allowed_groups %s != %s (admins, lands-tenants and the project groups of %s '
                'and their /owners and /guests subgroups; --write regenerates it)'
                % (rel, sorted(got), sorted(want_viewer), VALUES))
    blank = blank_comments(text)
    uses = []
    for server, start, end in server_blocks(blank):
        if VIEWER_HOST in server:
            for head, body, _ in all_locations(blank, start, end):
                uses += [(head, a.group(1)) for a in AUTH_REQUEST_RE.finditer(body)]
    if rel == EDGE:
        if LANDS_CHECK not in {name for name, _ in found}:
            problems.append('%s: %s has no %s check (the viewer list plus %s) for `location %s`'
                            % (rel, VIEWER_HOST, LANDS_CHECK, INVITED, LANDS_LOCATION))
        if (LANDS_LOCATION, LANDS_CHECK) not in uses:
            problems.append('%s: viewer `location %s` does not use %s; only invited people and the '
                            'groups the viewer admits may reach it' % (rel, LANDS_LOCATION, LANDS_CHECK))
    for head, name in uses:
        if name == LANDS_CHECK and head != LANDS_LOCATION:
            problems.append('%s: viewer `location %s` uses %s, which admits %s; only `location %s` may'
                            % (rel, head, LANDS_CHECK, INVITED, LANDS_LOCATION))
    return problems


def checks(text, host):
    """Group sets of every sign-in check in blocks serving `host`."""
    return [set(filter(None, unquote(m.group(1)).split(',')))
            for m in auth_matches(text, host)]


def land_groups():
    """The groups the land door admits, as its check spells them."""
    g = '/project-' + LAND_VAR
    return [g] + ['%s/%s' % (g, r) for r in LAND_ROLE_SUBGROUPS]


def api_problems(text, rel):
    """Problems with the sign-in checks of the api.clonk.tv blocks."""
    text = blank_comments(text)
    want = {ADMIN_CHECK: {ADMINS}, LAND_CHECK: set(land_groups())}
    found, uses = {}, []
    placed = {head: [] for head, _ in LAND_CHECK_USERS}
    for server, start, end in server_blocks(text):
        if API_HOST not in server:
            continue
        for head, body, parent in all_locations(text, start, end):
            m = CHECK_RE.search(body)
            if m and head.startswith('= '):
                a = AUTH_RE.search(body)
                found[head[2:]] = (set(filter(None, unquote(a.group(1)).split(',')))
                                   if a else None)
            for a in AUTH_REQUEST_RE.finditer(body):
                uses.append((head, parent, a.group(1)))
            if head in placed:
                placed[head].append(parent)
    problems = []
    if not found:
        return ['%s: no sign-in check in a %s block' % (rel, API_HOST)]
    for name in sorted(set(found) | set(want)):
        got = found.get(name, 'missing')
        if name not in want:
            problems.append('%s: %s check %s %s; only %s are allowed there'
                            % (rel, API_HOST, name,
                               'admits any signed-in user' if got is None else 'admits %s' % sorted(got),
                               ' and '.join(sorted(want))))
        elif got != want[name]:
            problems.append('%s: %s check %s admits %s, want %s'
                            % (rel, API_HOST, name,
                               got if got in ('missing', None) else sorted(got), sorted(want[name])))
    for head, what in LAND_CHECK_USERS:
        if placed[head] != [LAND_DOOR_PARENT]:
            problems.append('%s: %s needs exactly one %s `location %s` inside `location %s`'
                            % (rel, API_HOST, what, head, LAND_DOOR_PARENT))
        elif (head, LAND_DOOR_PARENT, LAND_CHECK) not in uses:
            problems.append('%s: %s %s `location %s` does not use %s'
                            % (rel, API_HOST, what, head, LAND_CHECK))
    allowed = {(head, LAND_DOOR_PARENT) for head, _ in LAND_CHECK_USERS}
    for head, parent, name in uses:
        if name == LAND_CHECK and (head, parent) not in allowed:
            problems.append('%s: %s `location %s` uses %s; only the land door and land feed may'
                            % (rel, API_HOST, head, LAND_CHECK))
        if name == LAND_TOKEN_CHECK:
            problems.append('%s: %s `location %s` uses %s; only %s may reach it'
                            % (rel, API_HOST, head, LAND_TOKEN_CHECK, LAND_CHECK))
    return problems


def write_viewer(text, groups, lands):
    """`text` with every viewer sign-in check's list set to `groups`, and the
    /lands/ check's to `lands`."""
    for name, m in reversed(named_auth_matches(text, VIEWER_HOST)):
        old = m.group(1)
        value = ','.join(lands if name == LANDS_CHECK else groups)
        if '%2f' in old.lower():
            value = value.replace('/', '%2F')
        text = text[:m.start(1)] + value + text[m.end(1):]
    return text


def main():
    problems = []
    values = read(VALUES)
    projects = project_groups(values)
    want_viewer = set(viewer_groups(projects))
    want_lands = set(lands_groups(projects))
    if INVITED[1:] not in static_groups(values):
        problems.append('%s: keycloak.groups does not declare %s, which the /lands/ check admits'
                        % (VALUES, INVITED[1:]))

    if '--write' in sys.argv[1:]:
        for rel in (EDGE, AUTHPROXY):
            text = read(rel)
            new = write_viewer(text, viewer_groups(projects), lands_groups(projects))
            if new != text:
                with open(os.path.join(ROOT, rel), 'w') as f:
                    f.write(new)
                print('check-viewer-edge-groups: wrote %s' % rel)

    for rel in (EDGE, AUTHPROXY):
        problems += viewer_problems(read(rel), rel, want_viewer, want_lands)

    for rel in (EDGE, AUTHPROXY):
        _, uses = open_check_uses(read(rel), VIEWER_HOST)
        for head, name in uses:
            if rel == EDGE and name == DEMO_CHECK and head in DEMO_LOCATIONS:
                continue
            problems.append(
                '%s: viewer `location %s` uses %s, a sign-in check with no '
                'allowed_groups (any signed-in user); GitHub sign-in is invite-only, so no '
                'viewer location may' % (rel, head, name))
        names, _ = open_check_uses(read(rel), 'mux.clonk.tv')
        for name in sorted(names):
            problems.append(
                '%s: mux check %s has no allowed_groups, so it admits any signed-in '
                'user to the admin coopmux token' % (rel, name))

    problems += api_problems(read(EDGE), EDGE)

    mux = checks(read(EDGE), 'mux.clonk.tv') + checks(read(AUTHPROXY), 'mux.clonk.tv')
    if not checks(read(EDGE), 'mux.clonk.tv'):
        problems.append('%s: no sign-in check in a mux.clonk.tv block' % EDGE)
    for got in mux:
        if got != {ADMINS}:
            problems.append(
                'mux allowed_groups %s != [%s]: the mux injects the admin coopmux '
                'token, so a project human must never be admitted' % (sorted(got), ADMINS))

    if problems:
        print('check-viewer-edge-groups: FAIL')
        for p in problems:
            print('  - ' + p)
        return 1
    print('check-viewer-edge-groups: OK (viewer: %s; location %s: those and %s; any signed-in user: '
          '%s only; mux: %s; api: %s, land door and feed: %s)'
          % (', '.join(sorted(want_viewer)), LANDS_LOCATION, INVITED, ' and '.join(DEMO_LOCATIONS),
             ADMINS, ADMINS, ', '.join(land_groups())))
    return 0


if __name__ == '__main__':
    sys.exit(main())
