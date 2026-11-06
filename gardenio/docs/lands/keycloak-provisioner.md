# The Lands Keycloak provisioner client

Step 8.5 (gd-8LjF2lzb4q) of the [Track B spec](../superpowers/specs/2026-10-19-lands-track-b-design.md),
B-D2, B-D6, §3.3 step 3 and §3.7. The Lands provisioner (8.6) gives a land its Keycloak group and
manages who is in it, at runtime, through the Keycloak admin API. It signs in as one confidential
client, `gardenio-lands-provisioner`, which is the only Keycloak credential Lands holds.

Code: `gardenio/controller/internal/keycloakadmin/`. Tests run against an in-memory fake Keycloak
(`keycloakadmin_test.go`); no test talks to a live realm.

## What the client can do

| Call | Admin API | Notes |
|---|---|---|
| `EnsureProjectGroup(land)` | `GET /groups?search=&exact=true`, `POST /groups`, `GET /groups/{id}`, `GET /groups/{id}/role-mappings`, `GET /roles/gardenio-project-member`, `POST /groups/{id}/role-mappings/realm` | Creates top-level `project-<land>` with the attribute `gardenio_land=<land>` and maps `gardenio-project-member` if missing. The create (201 or 409) is read back through the same search, and a group the search cannot see is `ErrGroupHidden` (see [List calls need a user policy](#list-calls-need-a-user-policy)). An existing group without that marker is refused (`ErrNotLandGroup`), and one holding any other realm or client role is refused (`ErrForeignRoles`); both are left alone. |
| `AddProjectMember` / `RemoveProjectMember(land, userID)` | `GET /groups/{id}`, `PUT` / `DELETE /users/{id}/groups/{groupId}` | Never creates the group; the group must carry the land's marker. Removing a user who is not a member, or no longer exists, is a no-op. |
| `AddTenant` / `RemoveTenant(userID)` | same, on `lands-tenants` | The static chart group from step 7.8; looked up, never created. |
| `AddInvited(userID)` | same, on `invited` | The static chart group a claimed sign-in invite joins (gd-ycE484kLyk); no role; looked up, never created. |
| `FindUserByGitHubID(id)` | `GET /users?q=github_id:<id>&exact=true` | The attribute is compared exactly on the client too. No match is `ErrUserNotFound`; two users with one `github_id` is `ErrAmbiguousUser`, and the caller joins neither. |
| `FindUserByEmail(email)` | `GET /users?email=<email>&exact=true`, `GET /users/{id}/federated-identity` | The email is compared exactly on the client too. Returns `emailVerified` and the aliases of the user's identity provider links. No match is `ErrUserNotFound`; two is `ErrAmbiguousUser`. |
| `ProjectMembers(land)` | `GET /groups/{id}`, `GET /groups/{id}/members` (paged) | Direct members of `project-<land>`. The group must carry the land's marker. |
| `EnsureLandRoleGroup(land, role)` | `GET /groups/{id}/children?search=&exact=true`, `POST /groups/{id}/children`, `GET /groups/{id}`, `GET /groups/{id}/role-mappings` | Creates `/project-<land>/owners` or `/guests` (Commons 4.2's land roles) with the same `gardenio_land` marker and no role of its own; they hold `gardenio-project-member` through the parent. An unmarked subgroup is `ErrNotLandGroup`, one holding a role `ErrForeignRoles`. |
| `AddLandMember(land, role, userID)` | as above, `PUT /users/{id}/groups/{groupId}` | Joins the group of the role: the land group for `member`, its subgroup for `owner` and `guest`. Never creates a group. |
| `RemoveFromLand(land, userID)` | as above, `DELETE /users/{id}/groups/{groupId}` | Leaves the land group and each marked role subgroup. |
| `LandMembers(land)` | as above, `GET /groups/{id}/members` (paged) | Owners, members and guests, each once with the first role found. For `gb lands close`. |
| `UserLands(userID)` | `GET /users/{id}/groups` (paged) | The lands whose group or subgroup the user is in, read from group paths; the provisioner takes a user out of `lands-tenants` only when it is empty. |

The boundary is in the code, not only in Keycloak's permissions:

- Callers pass a **land name**, never a group name. It must be a lowercase DNS label of at most 58
  characters (the chart's `keycloak.projectGroups` rule), and the client builds `project-<land>`
  itself. `../gardenio-admins`, `Admins` or `a/b` fail before any request is sent.
- A land whose group the chart renders is **reserved** and refused before any request
  (`ErrReservedLand`): `ReservedGroups` in the code lists every `keycloak.groups` and
  `keycloak.projectGroups` entry in the chart's `values.yaml` and in
  `deploy-values/gardenio-k3s.yaml` (today the five `gardenio-*` groups, `project-claimlayer`,
  `lands-tenants` and `invited`), and `TestReservedGroups_CoverChart` fails when either file grows a group that is
  not listed. 8.6 can add more through `Config.ReservedGroups`. A land named `claimlayer` can never
  reach ClaimLayer's group.
- **Ownership marker.** The client only uses, joins, lists or leaves a `project-` group that carries
  the attribute `gardenio_land` with exactly one value, the land's name. Only the client's own create
  writes it. A group someone else made (a chart group, a hand-made one, a group whose marker names
  another land) is refused with `ErrNotLandGroup`, so closing a land can never remove a member it did
  not add.
- Only a **top-level** group whose name and path match exactly counts. A subgroup called
  `project-x` under another group is ignored.
- **User ids** must be Keycloak UUIDs (lowercase, canonical form); anything else, `..` included,
  is refused (`ErrInvalidUserID`) before any request. Realm users here are local or brokered
  (GitHub IdP), which always have UUID ids; a federated user id (`f:…`) would be refused.
- **Connection.** `New` refuses the `master` realm and any base URL that is not `https`, the
  in-cluster Service included. The in-cluster keycloak-runtime Service serves https on 8443 with
  a cert-manager cert from the chart's own CA (`keycloak.runtime.tls`, gd-XRsosVFAau); the
  controller trusts that CA through `LANDS_KEYCLOAK_CA_FILE` (a caller-supplied `HTTPClient`
  whose RootCAs hold it). The plain-http exception for that Service (decision gd-F060vJkxoM,
  bug gd-4KcSkPwd7K) is gone. The client never follows a redirect (a token POST carries `client_secret`, and a 307/308 would replay
  it to wherever the redirect points). It copies a caller's `HTTPClient` rather than changing it.
- The one role mapping the client writes is `gardenio-project-member`, on a `project-` group. It
  never creates, edits or removes a role, and never removes a role mapping.
- Every membership change logs one line: `keycloakadmin: membership action=add|remove group=…
  group_id=… user_id=…`.

`FindUserByGitHubID` depends on step 7.3's IdP mapper writing the `github_id` user attribute, and on
the attribute being readable by an admin (declared in the realm's user profile, or the realm's
unmanaged-attribute policy set to at least admin-view). If it is not readable, every lookup returns
`ErrUserNotFound`, which fails closed: nobody joins.

## Does the EDP operator leave runtime groups alone?

Yes. Spec §8 asks 8.5 to check it before any land exists, because a chart upgrade that deleted the
groups it did not render would take every tenant's access away. This was checked by reading the
source of the operator the cluster runs (`epam/edp-keycloak-operator` **v1.32.0**,
`deploy-values/gardenio-k3s.yaml`):

1. **Nothing lists the realm's groups and prunes them.** The only group deletion in the operator is
   `internal/controller/keycloakrealmgroup/terminator.go` (`DeleteGroup`), which runs when a
   `KeycloakRealmGroup` CR is deleted, deletes only that CR's own group, and is skipped when the CR
   carries `edp.epam.com/preserve-resources-on-deletion: "true"`. Every group this chart renders
   carries it (`templates/keycloak/groups.yaml`). The `KeycloakRealm` controller does not touch
   groups.
2. **Memberships survive too.** The only membership removal is `SyncUserGroups`
   (`pkg/client/keycloak/adapter/gocloak_adapter_user.go`), for users declared as
   `KeycloakRealmUser` CRs, and it removes unlisted groups only when the CR's
   `reconciliationStrategy` is `full`. The chart renders `addOnly` unless an entry overrides it
   (`templates/keycloak/realm-users.yaml`). **Rule:** never set `reconciliationStrategy: full` on a
   `keycloak.realmUsers` entry for a person who holds a land; the next reconcile would drop them from
   `project-<land>` and `lands-tenants`.
3. **A CR with the same name adopts the runtime group.** `SyncRealmGroup`
   (`gocloak_adapter_groups.go`) finds a group by name and, if it exists, updates it and syncs its
   roles to the CR's list (adding and removing). A chart `projectGroups` entry renders only
   `gardenio-project-member`, so adopting a land's group changes nothing. A chart `groups:` entry
   named `project-<something>` with other roles would rewrite that group's roles; the provisioner
   would then refuse the group (`ErrForeignRoles`) instead of using it. 8.6 should refuse a land
   name already listed in `keycloak.projectGroups` or `keycloak.groups`.
4. **Subgroup sync cannot reach a land group.** `syncSubGroups` detaches unlisted subgroups only
   under a group whose CR declares `subGroups`. Land groups are top-level.

Runtime groups live in Keycloak's database (the in-cluster Postgres, `keycloak.runtime.db.autoWire`),
not in the pod's emptyDir, so a Keycloak restart keeps them. Losing that database loses them, along
with every brokered user; re-running the provisioner for each land restores the groups.

### Manual check after an operator upgrade

Re-run this once the provisioner client exists, and again whenever the keycloak-operator version
changes. It needs a Keycloak admin shell (`kcadm.sh` in the `gardenio-keycloak-runtime-0` pod).

```sh
KC=/opt/keycloak/bin/kcadm.sh
$KC config credentials --server http://localhost:8080/auth --realm master --user "$ADMIN" --password "$PASS"
# 1. A runtime group with a member, created outside any CR.
$KC create groups -r gardenio -s name=project-edp-probe
GID=$($KC get groups -r gardenio -q search=project-edp-probe -q exact=true --fields id --format csv --noquotes)
USER_ID=$($KC get users -r gardenio -q username=<a test user> --fields id --format csv --noquotes)
$KC update users/$USER_ID/groups/$GID -r gardenio -s realm=gardenio -s userId=$USER_ID -s groupId=$GID -n
# 2. Make the operator reconcile every realm CR (or run the release's helm upgrade).
kubectl -n gardenio annotate keycloakrealm,keycloakrealmgroup,keycloakrealmuser --all \
  gardenio.io/reconcile-probe="$(date +%s)" --overwrite
# 3. After a minute: the group and the membership are still there.
$KC get groups/$GID -r gardenio --fields name
$KC get users/$USER_ID/groups -r gardenio --fields name
# 4. Clean up.
$KC delete groups/$GID -r gardenio
kubectl -n gardenio annotate keycloakrealm,keycloakrealmgroup,keycloakrealmuser --all gardenio.io/reconcile-probe-
```

## Operator setup: the `gardenio-lands-provisioner` client

Keycloak 26.7 (chart default) has fine-grained admin permissions v2. The client gets as little as
Keycloak can express; the code above is the rest of the boundary.

1. **Secret.** `kubectl -n gardenio create secret generic gardenio-lands-provisioner
   --from-literal=clientSecret="$(openssl rand -hex 32)"`.
2. **Client, as code.** Add to `keycloak.clients` in `deploy-values/gardenio-k3s.yaml` (do **not** add
   it to `beads.machineClients`: it never calls kbeads):

   ```yaml
   - clientId: gardenio-lands-provisioner
     name: "Gardenio Lands provisioner (Keycloak admin, project-* groups only)"
     public: false
     standardFlowEnabled: false
     directAccess: false
     secret: "$gardenio-lands-provisioner:clientSecret"
     serviceAccount:
       enabled: true
       realmRoles: []
       clientRoles:
         - clientId: realm-management
           roles: [query-users, query-groups]
   ```

3. **Admin permissions (console, realm `gardenio`).** Realm settings → General → turn on **Admin
   Permissions**. Then Permissions → Create, each with one **user** policy *"is
   service-account-gardenio-lands-provisioner"* naming the client's service-account user (Clients →
   `gardenio-lands-provisioner` → Service account roles links to it). Do **not** use a client policy:
   see [List calls need a user policy](#list-calls-need-a-user-policy) below.
   - **Groups**, all groups: `view`, `view-members`, `manage`, `manage-membership` (Keycloak cannot
     scope group creation to a name prefix, and creating a top-level group needs `manage` on all
     groups).
   - **Users**, all users: `view`, `manage-group-membership`.
   - **Roles**, only `gardenio-project-member`: `map-role`.
   - **Deny** on every chart-rendered group except `lands-tenants` and `invited` (whose membership
     the client manages): every `keycloak.groups` and `keycloak.projectGroups` entry as deployed, re-derived
     from `deploy-values/gardenio-k3s.yaml` when you run this. Today that is `gardenio-admins`,
     `gardenio-operators`, `gardenio-viewers`, `gardenio-mentioners`,
     `gardenio-escalation-approvers` and `project-claimlayer`. A Groups permission on those groups
     only, with a negative-logic user policy for `service-account-gardenio-lands-provisioner`, so the
     resource-specific permission overrides the all-groups grant (in list results too: the deny
     filters those groups out of every list the client makes). Add a group here whenever the
     chart gains one (and to `ReservedGroups` in the code; the test above enforces that half).
4. **Acceptance.** With a token for the client
   (`POST /realms/gardenio/protocol/openid-connect/token`, `grant_type=client_credentials`):
   `PUT /admin/realms/gardenio/users/<test user>/groups/<gardenio-admins id>` and the same on
   `<project-claimlayer id>` must answer **403**;
   `POST /admin/realms/gardenio/roles` must answer **403**; `POST /admin/realms/gardenio/groups` with
   `{"name":"project-edp-probe"}` must answer **201** (then delete it as admin). If the first call
   answers 204, the deny in step 3 did not take: remove the user from `gardenio-admins` at once and
   record that the code is the only boundary on group membership.
   Then the list checks, which a client policy fails while every call above passes: with
   `project-edp-probe` still there, `GET /admin/realms/gardenio/groups?search=project-edp-probe&exact=true`
   must return it, and `GET /admin/realms/gardenio/groups?search=gardenio-admins&exact=true` must
   return `[]`. After putting the probe user in it, `GET /groups/<probe id>/members` must list them.
   Finally run the client itself twice against one test land (`EnsureProjectGroup`, then
   `EnsureLandRoleGroup(owner)`): the second run must make no write, and neither may fail with
   `ErrGroupHidden`.
5. **Per-land client scopes (gd-eAfptUBh9Z, owner decision gd-zf6eceweED).** Every land also gets
   an optional client scope `land-<land>` whose one mapper adds aud `land-<land>` to the access
   token, attached as an optional scope of each client in `keycloak.landScopeClients` (default
   `gardenio-cli`). `kd login` / `gb login` against `https://api.clonk.tv/land/<land>` requests
   it, and kbeads, landgate and the land door then hold that token to the one land.

   **This needs no Keycloak grant beyond step 3.** The provisioner's credential stays
   `[query-users, query-groups]`: `manage-clients` would be effectively realm admin here (kbeads
   trusts the `realm_roles` claim, and a `manage-clients` credential can add a hardcoded-claim
   mapper to any client or scope, or regenerate other clients' secrets). Instead the provisioner
   (`internal/lands/landscope`) writes a `KeycloakClientScope` resource
   `gardenio-land-scope-<land>` in the release namespace for the realm resource
   `LANDS_KEYCLOAK_REALM_RESOURCE`, and the EDP keycloak-operator makes the scope. The chart
   (`templates/controller/lands-scope-rbac.yaml`) grants the controller ServiceAccount only
   get/create/update on `keycloakclientscopes` there, and its ValidatingAdmissionPolicy
   `<agents>-land-scopes` admits only the land scope shape: `spec.name` `land-<land>` for the
   land its label names, optional, openid-connect, this chart's realm, the land attributes, and
   exactly one `oidc-audience-mapper` whose only audience is `land-<land>`. The chart test
   `TestLandScopes_AdmissionPolicyCEL` proves the policy offline; its live counterpart is
   `gardenio/scripts/verify-land-scopes-vap.sh`, run as cluster-admin in the helm window
   that turns the provisioner on (gd-EXIoC2EQs3): the control is created in a server dry
   run as the controller SA, and five hostile variants (no mapper, two mappers, a hardcoded
   `realm_roles` claim, another audience, another land's name) are each denied by the
   policy, not by RBAC.

   The controller attaches nothing to a client. `templates/keycloak/clients.yaml` looks the
   resources up (label `gardenio.io/land-scope=true`, made by the operator, i.e. `status.id`
   set) and adds them to `keycloak.landScopeClients`' optional scopes, so **a new land's door
   opens on the next `helm upgrade`** after the provisioner makes its scope. Until then
   `kd login` for that land fails `invalid_scope` and its door refuses every token; the land
   itself is open. The operator only ever adds a client's scopes, so a scope stays attached
   whatever a later render lists.

   A scope that cannot be made never fails a land: the Keycloak step logs it and the
   provisioner retries it on its next pass over provisioned lands (`backfillLandScope`), which
   also gives lands provisioned before this change their scope. The chart's own lands
   (`keycloak.projectGroups`, and `keycloak.landScopes` for a land whose group is declared in
   `keycloak.groups`, such as `claimlayer`) are rendered by `client-scopes.yaml` instead.
   **Acceptance:** after a land is provisioned, `kubectl -n gardenio get keycloakclientscopes
   gardenio-land-scope-<land>` shows status `OK`; after the next `helm upgrade`,
   `gardenio-cli`'s optional scopes list `land-<land>`, and a device-grant sign-in with
   `scope=openid email profile land-<land>` returns an access token whose `aud` contains
   `land-<land>`. As the controller ServiceAccount, a `kubectl create` of a scope with a second
   mapper (`kubectl --as=system:serviceaccount:gardenio:<agents-sa>`) is refused by the policy.

### List calls need a user policy

Every name lookup the client makes is a list call: a group by name (`GET /groups?search=&exact=true`,
`GET /groups/{id}/children?search=`), a group's members, a user by `github_id`. Admin permissions v2
filter list results in the database query, through a partial evaluator
(`org.keycloak.authorization.fgap.evaluation.partial.PartialEvaluator` in Keycloak 26.7). It
evaluates only policy types that implement `PartialEvaluationPolicyProvider`, which are user, group
and role policies; `ClientPolicyProvider` does not, so a permission whose policy is a client policy
grants nothing to a list, and the query is reduced to `id = 'none'`. Calls by id are checked by the
full evaluator, which does honour client policies. So with client policies every list and search
answers 200 `[]` while `GET /groups/{id}`, membership `PUT`/`DELETE` and `POST /groups` work. That
is what the first stage-2 acceptance saw (gd-YjL6XkDlqU, gd-7peNzGjJ3C). `GET /group-by-path/…` does
not help: it resolves names through the same filtered query.

A user policy on the service-account user grants exactly the same scopes to the same principal, and
list filtering honours it. If the client is ever deleted and re-created, its service-account user
gets a new id and the policies must be pointed at the new one; until then everything fails closed.

The client fails loudly when lists are hidden: a create whose read-back cannot see the group returns
`ErrGroupHidden`, never `ErrGroupNotFound`, and `gb lands close` refuses to skip member removal for
a land whose `provision_step` records the Keycloak step but whose group cannot be found.
`UserLands` (`GET /users/{id}/groups`) filters per group with the full evaluator, so it works either
way, but it never shows the denied groups: a tenant who is also in `project-claimlayer` can still be
taken out of `lands-tenants` when they leave their last land.
