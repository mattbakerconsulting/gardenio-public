#!/usr/bin/env python3
"""check-viewer-edge-groups_test.py — unit tests for check-viewer-edge-groups.py

Each case builds a synthetic repo in a tmpdir (deploy-values plus the two edge
manifests) and asserts the guard's verdict. GARDENIO_ROOT points the guard at it.

Usage:  python3 scripts/check-viewer-edge-groups_test.py
"""
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, 'check-viewer-edge-groups.py')

PASS = FAIL = 0

VALUES = """keycloak:
  enabled: true
  groups:
    - name: gardenio-admins
      realmRoles:
        - gardenio-admin
    - name: project-claimlayer
      realmRoles:
        - gardenio-project-member
    - name: invited
  projectGroups:
    - name: scopetest
  realmUsers: []
other:
  groups:
    - name: project-notkeycloak
"""

EDGE = """data:
  nginx.conf: |
    server {
      server_name grafana.clonk.tv;
      location = /oauth2/auth {
        proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%%2Fgardenio-admins;
      }
    }
    # a comment with a { brace
    server {
      listen 8443 ssl;
      server_name gardenio.clonk.tv;
      location = /oauth2/auth {
        proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%(viewer)s;
      }
      location = /oauth2/auth-lands {
        internal;
        proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%(lands)s;
      }
      location ^~ /lands/ {
        auth_request %(lands_check)s;
        proxy_pass http://viewer;
      }%(viewer_extra)s
      location %(viewer_root)s {
        auth_request %(viewer_root_check)s;
        proxy_pass http://viewer;
      }
    }
    server {
      listen 8443 ssl;
      server_name mux.clonk.tv;
      location = /oauth2/auth {
        proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%(mux)s;
      }%(mux_extra)s
    }
    server {
      listen 8443 ssl;
      server_name api.clonk.tv;
      location = /oauth2/auth {
        internal;
        proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%(api)s;
      }
      location = /oauth2/auth-land {
        internal;
        proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%(land)s;
      }
      location ^~ /land/ {
        location ~ "^/land/[a-z][a-z0-9-]{0,62}/v1/(admin|auth)(/|$)" { return 404; }
        location %(feed)s {
          auth_request %(feed_check)s;
          proxy_pass http://$up_land;
        }
        location %(door)s {
          auth_request %(door_check)s;
          proxy_pass http://$up_beads;
        }
        return 404;
      }
      location ^~ /v1/ {
        auth_request %(v1_check)s;
        proxy_pass http://$up_beads;
      }%(api_extra)s
    }
"""

AUTHPROXY = """data:
  nginx.conf: |
    server {
      listen 8443 ssl;
      server_name gardenio.clonk.tv;
      location = /oauth2/auth {
        proxy_pass http://gardenio-forwardauth.gardenio.svc.cluster.local:4180/oauth2/auth?allowed_groups=%(viewer)s;
      }
    }
    server {
      listen 8446 ssl;
      server_name mux.clonk.tv;
      location / { proxy_pass http://coopmux; }
    }
"""

# viewer-authproxy since ziti went (gd-xNMLrsgajy): edge listeners only, no
# sign-in check of its own.
AUTHPROXY_EDGE_ONLY = """data:
  nginx.conf: |
    server {
      listen 8097;
      server_name _;
      location / { proxy_pass http://viewer; }
    }
"""

GOOD = ('%2Fgardenio-admins,%2Flands-tenants,'
        '%2Fproject-claimlayer,%2Fproject-claimlayer%2Fowners,%2Fproject-claimlayer%2Fguests,'
        '%2Fproject-scopetest,%2Fproject-scopetest%2Fowners,%2Fproject-scopetest%2Fguests')
# The list before land-role subgroups (gd-A9avO8cXBr): admins + parents only.
PARENTS_ONLY = '%2Fgardenio-admins,%2Flands-tenants,%2Fproject-claimlayer,%2Fproject-scopetest'
# The list before /lands-tenants (Track B 7.8).
NO_TENANTS = GOOD.replace(',%2Flands-tenants', '')
# The /lands/ check: the viewer list plus /invited (gd-ycE484kLyk).
LANDS = GOOD.replace(',%2Flands-tenants', ',%2Flands-tenants,%2Finvited')
# An any-signed-in check, as ^~ /lands/ had before sign-in was invite-only.
OPEN_CHECK = ('\n      location = /oauth2/auth-signed-in {\n        internal;\n'
              '        proxy_pass http://$up_fa/oauth2/auth;\n      }')
# The demo door's any-signed-in check (gd-T37nCeWMhq = open) and the two
# locations that may use it.
DEMO_CHECK = ('\n      location = /oauth2/auth-lands-demo {\n        internal;\n'
              '        proxy_pass http://$up_fa/oauth2/auth;\n      }')


def demo_loc(head, check='/oauth2/auth-lands-demo'):
    return ('\n      location %s {\n        auth_request %s;\n'
            '        proxy_pass http://viewer;\n      }' % (head, check))


DEMO = DEMO_CHECK + demo_loc('= /lands/demo') + demo_loc('= /lands/accept')
ADMINS = '%2Fgardenio-admins'
LAND = ('%2Fproject-$land_door_name,%2Fproject-$land_door_name%2Fowners,'
        '%2Fproject-$land_door_name%2Fguests')
DOOR = '~ "^/land/[a-z][a-z0-9-]{0,62}/v1/"'
FEED = '~ "^/land/[a-z][a-z0-9-]{0,62}/v1/lands/v1/lands/[a-z][a-z0-9-]{0,62}/events$"'


def edge_text(viewer=None, mux=ADMINS, viewer_root='/', viewer_root_check='/oauth2/auth',
              mux_extra='', api=ADMINS, land=LAND, door=DOOR, door_check='/oauth2/auth-land',
              feed=FEED, feed_check='/oauth2/auth-land', v1_check='/oauth2/auth', api_extra='',
              lands=LANDS, lands_check='/oauth2/auth-lands', viewer_extra=''):
    return EDGE % {'viewer': GOOD if viewer is None else viewer, 'mux': mux,
                   'lands': lands, 'lands_check': lands_check, 'viewer_extra': viewer_extra,
                   'viewer_root': viewer_root, 'viewer_root_check': viewer_root_check,
                   'mux_extra': mux_extra, 'api': api, 'land': land, 'door': door,
                   'door_check': door_check, 'feed': feed, 'feed_check': feed_check,
                   'v1_check': v1_check, 'api_extra': api_extra}


def check(name, want_hit, want_substr=None, values=VALUES, viewer=GOOD,
          mux=ADMINS, ap_viewer=None, edge=None, authproxy=None, write=False,
          want_files=None):
    global PASS, FAIL
    root = tempfile.mkdtemp(prefix='cveg-test-')
    try:
        files = {
            'deploy-values/gardenio-k3s.yaml': values,
            'gardenio/helm/gardenio-ziti/public-edge.yaml':
                edge if edge is not None else edge_text(viewer, mux),
            'gardenio/helm/gardenio-ziti/viewer-authproxy.yaml':
                authproxy if authproxy is not None
                else AUTHPROXY % {'viewer': ap_viewer or viewer},
        }
        for rel, body in files.items():
            path = os.path.join(root, rel)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, 'w') as f:
                f.write(body)
        env = dict(os.environ, GARDENIO_ROOT=root)
        p = subprocess.run([sys.executable, GUARD] + (['--write'] if write else []),
                           capture_output=True, text=True, env=env)
        out = p.stdout + p.stderr
        ok = (p.returncode != 0) == want_hit
        if ok and want_substr and want_substr not in out:
            ok = False
            out += '\n(expected message to contain %r)' % want_substr
        for rel, body in (want_files or {}).items():
            with open(os.path.join(root, rel)) as f:
                got = f.read()
            if ok and got != body:
                ok = False
                out += '\n(%s after --write:\n%s)' % (rel, got)
        if ok:
            print('PASS: %s' % name)
            PASS += 1
        else:
            print('FAIL: %s: expected %s, got rc=%d\n%s'
                  % (name, 'a hit' if want_hit else 'no hit', p.returncode, out))
            FAIL += 1
    finally:
        shutil.rmtree(root, ignore_errors=True)


check('good: viewer admits admins + every project, mux admins only', False)
check('good: unencoded slashes and list order do not matter', False,
      viewer='/project-scopetest/guests,/gardenio-admins,/lands-tenants,/project-claimlayer/owners,'
             '/project-scopetest,/project-claimlayer/guests,/project-claimlayer,'
             '/project-scopetest/owners')

# Land roles are exclusive: an owner or guest is in the subgroup only, so a
# list without the subgroups refuses them at the edge.
check('viewer list has parents but no land-role subgroups', True,
      '/project-scopetest/guests', viewer=PARENTS_ONLY)
check('viewer list misses one land owners subgroup', True,
      '/project-claimlayer/owners',
      viewer=GOOD.replace(',%2Fproject-claimlayer%2Fowners', ''))
check('viewer list names a subgroup of a project not in values', True,
      'allowed_groups', viewer=GOOD + ',%2Fproject-ghost%2Fguests')
check('mux list gained a land-role subgroup', True, 'mux allowed_groups',
      mux='%2Fgardenio-admins,%2Fproject-claimlayer%2Fowners')

# --write regenerates both viewer lists from values, keeps each file's own
# spelling, leaves the grafana and mux checks alone, and then passes.
check('--write fixes a stale list in both files', False, 'wrote', write=True,
      viewer=PARENTS_ONLY, ap_viewer='/gardenio-admins',
      want_files={
          'gardenio/helm/gardenio-ziti/public-edge.yaml':
              edge_text(),
          'gardenio/helm/gardenio-ziti/viewer-authproxy.yaml':
              AUTHPROXY % {'viewer': GOOD.replace('%2F', '/')},
      })
check('--write never widens the mux list and still fails on it', True,
      'mux allowed_groups', write=True,
      mux='%2Fgardenio-admins,%2Fproject-claimlayer')

check('mux list gained a project group', True, 'mux allowed_groups',
      mux='%2Fgardenio-admins,%2Fproject-claimlayer')
check('mux list lost admins', True, 'mux allowed_groups',
      mux='%2Fproject-claimlayer')
check('project in values missing from the viewer list', True, '/project-scopetest',
      viewer='%2Fgardenio-admins,%2Fproject-claimlayer,'
             '%2Fproject-claimlayer%2Fowners,%2Fproject-claimlayer%2Fguests')
check('viewer list is admins only (before this change)', True, 'allowed_groups',
      viewer=ADMINS)
check('viewer list names a project not in values', True, 'allowed_groups',
      viewer=GOOD + ',%2Fproject-ghost')
check('ziti front door (viewer-authproxy) drifts from the edge', True,
      'viewer-authproxy.yaml', ap_viewer=ADMINS)
check('viewer-authproxy with no sign-in check of its own (ziti off)', False,
      authproxy=AUTHPROXY_EDGE_ONLY)
check('ziti off: the edge viewer list still has to match', True, 'public-edge.yaml',
      viewer=PARENTS_ONLY, authproxy=AUTHPROXY_EDGE_ONLY)
check('no viewer check at the edge fails even with none in viewer-authproxy', True,
      'no sign-in check in a gardenio.clonk.tv',
      edge=edge_text().replace('server_name gardenio.clonk.tv', 'server_name other.clonk.tv'),
      authproxy=AUTHPROXY_EDGE_ONLY)
check('a project group dropped from deploy-values narrows the want', True,
      'allowed_groups',
      values=VALUES.replace('    - name: scopetest\n', '    - name: other\n'))
check('no mux check at the edge', True, 'no sign-in check in a mux.clonk.tv',
      edge=edge_text().split('    server {\n      listen 8443 ssl;\n      server_name mux')[0])

# Lands Track B 7.8: every tenant is in /lands-tenants, so the viewer must
# admit it; and a check with no allowed_groups admits any signed-in user, so
# no viewer location may use one (GitHub sign-in is invite-only) and the mux
# may have none.
check('viewer list lacks /lands-tenants', True, '/lands-tenants', viewer=NO_TENANTS)
check('--write adds /lands-tenants', False, 'wrote', write=True, viewer=NO_TENANTS,
      want_files={'gardenio/helm/gardenio-ziti/public-edge.yaml': edge_text()})
check('mux list gained /lands-tenants', True, 'mux allowed_groups',
      mux='%2Fgardenio-admins,%2Flands-tenants')
check('viewer location / uses the any-signed-in check', True,
      '`location /` uses /oauth2/auth-signed-in',
      edge=edge_text(viewer_root_check='/oauth2/auth-signed-in', viewer_extra=OPEN_CHECK))
check('another viewer location uses the any-signed-in check', True,
      '`location ^~ /api/` uses /oauth2/auth-signed-in',
      edge=edge_text(viewer_root='^~ /api/', viewer_root_check='/oauth2/auth-signed-in',
                     viewer_extra=OPEN_CHECK))
check('an any-signed-in check named in a comment is not a use', False,
      edge=edge_text(viewer_extra=OPEN_CHECK).replace(
          '        auth_request /oauth2/auth;\n',
          '        auth_request /oauth2/auth;  # not /oauth2/auth-signed-in;\n'))

# Invite-only GitHub sign-in (gd-ycE484kLyk): ^~ /lands/ admits the viewer
# list plus /invited, through its own check, and no one else uses that check.
check('^~ /lands/ admits any signed-in user again', True,
      '`location ^~ /lands/` uses /oauth2/auth-signed-in',
      edge=edge_text(lands_check='/oauth2/auth-signed-in', viewer_extra=OPEN_CHECK))
check('the /lands/ check lacks /invited', True, '/oauth2/auth-lands allowed_groups',
      edge=edge_text(lands=GOOD))
check('the /lands/ check lacks a project subgroup (locks a member out)', True,
      '/oauth2/auth-lands allowed_groups',
      edge=edge_text(lands=LANDS.replace(',%2Fproject-claimlayer%2Fowners', '')))
check('the viewer root check admits /invited', True, 'viewer allowed_groups',
      edge=edge_text(viewer=LANDS))
check('^~ /lands/ uses the viewer root check', True,
      '`location ^~ /lands/` does not use /oauth2/auth-lands',
      edge=edge_text(lands_check='/oauth2/auth'))
check('another viewer location uses the /lands/ check', True,
      '`location ^~ /api/` uses /oauth2/auth-lands',
      edge=edge_text(viewer_root='^~ /api/', viewer_root_check='/oauth2/auth-lands'))
check('no /lands/ check at all', True, 'no /oauth2/auth-lands check',
      edge=edge_text().replace('location = /oauth2/auth-lands {', 'location = /oauth2/auth-gone {'))
check('deploy-values does not declare invited', True, 'does not declare invited',
      values=VALUES.replace('    - name: invited\n', ''))
check('--write adds /invited to the /lands/ check only', False, 'wrote', write=True,
      edge=edge_text(lands=GOOD),
      want_files={'gardenio/helm/gardenio-ziti/public-edge.yaml': edge_text()})
check('mux grows a check with no allowed_groups', True, 'mux check /oauth2/auth-open',
      edge=edge_text(mux_extra='\n      location = /oauth2/auth-open {\n'
                               '        proxy_pass http://$up_fa/oauth2/auth;\n      }'))

# The demo land's door (gd-T37nCeWMhq = open): any signed-in account reaches
# = /lands/demo and = /lands/accept through /oauth2/auth-lands-demo, and no
# other location may use that check; it never takes a group list.
check('demo door: the open check on = /lands/demo and = /lands/accept', False,
      'any signed-in user: = /lands/demo and = /lands/accept only',
      edge=edge_text(viewer_extra=DEMO))
check('demo door: the open check defined but unused', False, edge=edge_text(viewer_extra=DEMO_CHECK))
check('demo door: another /lands/ location uses the demo check', True,
      '`location = /lands/apply` uses /oauth2/auth-lands-demo',
      edge=edge_text(viewer_extra=DEMO + demo_loc('= /lands/apply')))
check('demo door: ^~ /lands/ uses the demo check', True,
      '`location ^~ /lands/` uses /oauth2/auth-lands-demo',
      edge=edge_text(lands_check='/oauth2/auth-lands-demo', viewer_extra=DEMO))
check('demo door: the viewer root uses the demo check', True,
      '`location /` uses /oauth2/auth-lands-demo',
      edge=edge_text(viewer_root_check='/oauth2/auth-lands-demo', viewer_extra=DEMO))
check('demo door: = /lands/demo uses another open check', True,
      '`location = /lands/demo` uses /oauth2/auth-signed-in',
      edge=edge_text(viewer_extra=OPEN_CHECK + demo_loc('= /lands/demo', '/oauth2/auth-signed-in')))
check('demo door: the demo check gains a group list', True,
      '/oauth2/auth-lands-demo has allowed_groups',
      edge=edge_text(viewer_extra=DEMO.replace('/oauth2/auth;', '/oauth2/auth?allowed_groups=%2Finvited;')))
check('demo door: the mux grows the demo check', True, 'mux check /oauth2/auth-lands-demo',
      edge=edge_text(mux_extra=DEMO_CHECK))
check('demo door: viewer-authproxy may not use the demo check', True,
      'viewer-authproxy.yaml: viewer `location = /lands/demo` uses /oauth2/auth-lands-demo',
      authproxy=AUTHPROXY.replace(
          '      }\n    }\n    server {\n      listen 8446',
          '      }' + DEMO + '\n    }\n    server {\n      listen 8446', 1) % {'viewer': GOOD})

# Door D.2: api.clonk.tv has the admins check for /v1/* and the land door's
# per-request /project-<land> check, used by the land door alone.
check('api land door admits the parent, owners and guests of the path land', False,
      edge=edge_text(land=LAND.replace('%2F', '/')))
check('api land door also admits admins', True, '/oauth2/auth-land admits',
      edge=edge_text(land=ADMINS + ',' + LAND))
check('api land door lost the owners subgroup', True, '/oauth2/auth-land admits',
      edge=edge_text(land=LAND.replace(',%2Fproject-$land_door_name%2Fowners', '')))
check('api land door names a fixed land', True, '/oauth2/auth-land admits',
      edge=edge_text(land='%2Fproject-claimlayer'))
check('api land door lets any /lands-tenants member in', True, '/oauth2/auth-land admits',
      edge=edge_text(land=LAND + ',%2Flands-tenants'))
check('api /v1 check gained a project group', True, '/oauth2/auth admits',
      edge=edge_text(api=ADMINS + ',%2Fproject-claimlayer'))
check('api land door uses the admins check', True, 'does not use /oauth2/auth-land',
      edge=edge_text(door_check='/oauth2/auth'))
check('api /v1/ uses the land check', True, '`location ^~ /v1/` uses /oauth2/auth-land',
      edge=edge_text(v1_check='/oauth2/auth-land'))
check('api land door moved out of ^~ /land/', True, 'inside `location ^~ /land/`',
      edge=edge_text(door='~ "^/lond/[a-z][a-z0-9-]{0,62}/v1/"',
                     api_extra='\n      location ~ "^/land/[a-z][a-z0-9-]{0,62}/v1/" {\n'
                               '        auth_request /oauth2/auth-land;\n      }'))
check('api grows a check with no allowed_groups', True, '/oauth2/auth-open admits any signed-in user',
      edge=edge_text(api_extra='\n      location = /oauth2/auth-open {\n'
                               '        proxy_pass http://$up_fa/oauth2/auth;\n      }'))
# Land feed D.9: the land's event stream has its own unbuffered location next
# to the door, behind the same per-land check; no other location gets it.
check('api land feed uses the admins check', True,
      'land feed `location %s` does not use /oauth2/auth-land' % FEED,
      edge=edge_text(feed_check='/oauth2/auth'))
check('api land feed uses the any-signed-in check', True,
      'land feed `location %s` does not use /oauth2/auth-land' % FEED,
      edge=edge_text(feed_check='/oauth2/auth-signed-in'))
check('api land feed location is gone', True, 'needs exactly one land feed',
      edge=edge_text(feed='= /land/feed-gone', feed_check='/oauth2/auth'))
check('api land feed widened to any land path takes the land check', True,
      '`location ~ "^/land/[a-z][a-z0-9-]{0,62}/v1/lands/"` uses /oauth2/auth-land',
      edge=edge_text(feed='~ "^/land/[a-z][a-z0-9-]{0,62}/v1/lands/"'))
check('api land feed moved out of ^~ /land/', True, 'needs exactly one land feed',
      edge=edge_text(feed='= /land/feed-gone', feed_check='/oauth2/auth',
                     api_extra='\n      location %s {\n'
                               '        auth_request /oauth2/auth-land;\n      }' % FEED))
check('another api land location takes the land check', True,
      '`location ~ "^/land/[a-z][a-z0-9-]{0,62}/v2/"` uses /oauth2/auth-land; '
      'only the land door and land feed may',
      edge=edge_text(api_extra='\n      location ^~ /land2/ {\n'
                               '        location ~ "^/land/[a-z][a-z0-9-]{0,62}/v2/" {\n'
                               '          auth_request /oauth2/auth-land;\n        }\n      }'))
# D.18a: the land-token check is reached only from inside /oauth2/auth-land.
check('api land door has a land-token check behind its land check', False,
      edge=edge_text(api_extra='\n      location = /oauth2/auth-land-token {\n'
                               '        internal;\n'
                               '        proxy_pass http://$up_beads_me/v1/me;\n      }'))
check('api location uses the land-token check directly', True,
      '`location ^~ /v2/` uses /oauth2/auth-land-token; only /oauth2/auth-land may reach it',
      edge=edge_text(api_extra='\n      location ^~ /v2/ {\n'
                               '        auth_request /oauth2/auth-land-token;\n      }'))
check('api has no land check', True, '/oauth2/auth-land admits missing',
      edge=edge_text().replace('location = /oauth2/auth-land {', 'location = /oauth2/auth-gone {'))

print('%d passed, %d failed' % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
