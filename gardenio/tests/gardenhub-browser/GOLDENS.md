# Golden screenshots: /garden and the empty composer

A visual-regression gate, not a review aid (gd-D953GccBTY). `TestGardenGoldens`
(`gardenio/controller/internal/gardenhub/golden_browser_test.go`) shoots `/garden` in
Chromium and pixel-diffs every shot against a committed golden. `TestComposerGoldens`
(`gardenio/controller/cmd/beads-viewer/composer_golden_test.go`) does the same for the
beads viewer's empty thread composer (see [The empty composer](#the-empty-composer)). A shot that drifts past
its tolerance fails the `gardenhub browser (chromium)` job in `ci.yml`, which runs on a
release tag, the nightly and dispatch, never on a PR. **A feature PR never commits
`testdata/golden`** (gd-fedEoEGC9i): a change you meant is re-shot on main by the window
bless PR (see [Updating goldens](#updating-goldens)).

| Piece | Where |
|---|---|
| The goldens | `gardenio/controller/internal/gardenhub/testdata/golden/<surface>-<device>-<theme>.png` |
| What is shot, and how each surface is put on screen | `testdata/golden_browser.js` (`SHOTS`) |
| Tolerance per surface | `golden_browser_test.go` (`goldenSurfaces`) |
| The diff, the artifacts, the browser lookup (shared by both gates) | `gardenio/controller/internal/golden` |
| The fixture world | `cardWorld` in `card_browser_test.go` (shared with the card walk) plus `goldenPorch` |
| Pinned fonts | `testdata/golden/fonts/` (Noto subsets, OFL) |
| Make targets | `make test-garden-goldens`, `make update-garden-goldens` (the bless run's command; render-check only in a PR) |
| The bless run and its guard | `.github/workflows/bless-goldens.yml`, `scripts/check-no-pr-goldens.py` |
| Laptop helper | `gardenio/scripts/garden-goldens-from-ci.sh` |

## What is shot

Each surface at a **390 px phone** (390x844, touch) and a **1280 px desktop** (1280x800),
in **light** and **dark** (`colorScheme` emulation, with `garden.sky` pinned to match it so the frozen clock, 20:33 UTC, does not turn every shot into night), at device scale 1.
Every surface shot on the phone is also shot on the phone turned sideways, **`phoneland`** (Playwright's
iPhone 12 landscape, 750x340, touch; M4.1 gd-T3JPWCD9xM), in **light only** (a dark-only row in dark),
which keeps the sweep inside its `-timeout`: `<surface>-phoneland-light.png`. Under 900 px wide it is
still the phone layout, so a row's `show()` is told `phone`. A row with `upright: '<bead>'` is not shot
sideways until that bug is fixed; today that is only the dusk, night and dawn splash (noon stands for the scene's layout). In the Devices column below, `phone` means both phone cells.

| Surface | What is on screen | Devices |
|---|---|---|
| `opening` | `/garden`'s opening cursor (cursor-grid spec 3.5): depth 0 on the first project, the lattice framed, the title bar naming it | phone, desktop |
| `cursorworld`, `cursorproject`, `cursorplan`, `cursorphase` | the cursor's signs (cursor-grid spec 6) at each depth: no plate beside the cursor's Thing, the title bar reading its lineage, whole title and status line (gd-zDdiA3HBh5), child labels where they fit, glyphs elsewhere. On the gardenio project at depth 0 (`?at=gardenio`), on plan A (`?at=gardenio/gd-planA`), then Enter into a phase bed, then Enter onto a step. `cursorproject` also shows the plot rollup stamps (cursor-grid spec 6.6): plan A needing you on its current phase, plan P's nine phases, plan E with every phase done and plan F with no tasks yet | phone, desktop |
| `world` | the World level (the camera cut to z 0.1 from `?at=gardenio`, gd-GgKjBgsc0U): the Project stamps under the boards (gd-H6ZPz5V262), gardenio's plan A carrying its "!1" and heads with no pulsing ring around it or its biome. Its sign carries no wish count, the Project level's word: the zoom across a level says the signs again at the World level (garden.js camChanged, gd-0I89c9ifka), so they say the same whenever the Porch feed lands (gd-owQJjry58h) | phone, desktop |
| `porch` | the Porch open on the farm's gardeners, grouped by the plot they tend with the rest "In the field", each with its state, where it stands ("In the field of gardenio and on <plot>"), its spend chip where a figure is known ("$4.10+", gd-iSYItBAyA5), since and Go to plot, and the header summing the rows' spend (a decision in the Mailbox) | phone, desktop |
| `purseok`, `purselow`, `purseover`, `pursenobudget`, `pursestale`, `pursescoped` | the header purse (gd-H9LerJ0v3g, `web/costs.js` `paintPurse`), from `golden_cost_browser.js`, the header's controls only (`clip`), painted from a fixture cost blob once the farm document has painted it (later documents held off): an admin's Fleet row of $ today and $ remaining with the budget ok, low (the warn level, "low" in the warn ink), over (remaining $0.00 on the alert fill, "over budget"), with no account budget (a dash for $ remaining) and with the rollup down (a dash for today and "as of 20:10 UTC", the budget kept); and a scoped viewer's one row per project, never summed: gardenio low, kbeads with no budget (no $ remaining) and coop stale. The purse in the header moved every golden that shows the header's controls: the card world's fleet reads "$0.00 today · — left" (its spend source has no rows and no account budgets) | phone, desktop |
| `facework`, `faceepic`, `faceplan`, `facedecision`, `facedecisionanswered`, `faceescalation`, `faceescalationdone`, `faceagent`, `facemessage`, `facebundle`, `facereport`, `facegardener`, `facemr`, `facegeneric` | the typed faces (Lands workbench-glass spec WB-1, step 5.3, gd-vFUZG6FnTT): the bead view of one face fixture each (`faceBeads`, `bead_face_test.go`, on no plot), from the panel's top down to the end of its face section (`faceClip`). A decision and an escalation are shot open, with their answer boxes (the escalation as an account that may approve, `body[data-esc]`), and answered; `facegeneric` is a type with no face, its generic fields table opened. Giving plan A a plan face and the `#crew` card's agent a gardener face moved `bead`, `beadplan`, `commonsbead` and `commonsbeadover` | phone, desktop |
| `facerelease` | the release face (gd-j3bQqfZSt7, `web/relface.js`; graded and promoted in step 10, gd-HHMReWCHUi) on `releaseBeads` (`bead_face_release_test.go`): 2026.299.2 live on prod after 30 minutes on staging, its cut, build, commit and previous release, "By plan" with each plan's count inline, and its five PRs newest merge first, 44 px PR links on touch, the `javascript:` row's number plain text | phone, desktop |
| `facedecisiongate`, `facedecisiongateev` | the acceptance decision every answering surface is graded with (`decisiontest` in `internal/decisionview/decisiontest`, epic gd-hmwSI9lwKe step 6, gd-DiU8SB6TAM): the Gate's plot application with five options, a Markdown context past the fold, and `--evidence` naming a task (`gd-GateEvid01`) whose comment carries two `gb image upload` links. `facedecisiongate` is its bead view (`gd-FaceGateDec`, `faceBeads`) at the face's top, the context; `facedecisiongateev` the same scrolled to the screenshot strip, then the five options and Other. The goldens' Mailbox decision (`goldenPorch` d-1) is the same decision, which moved `mailbox`; the Hub reads the evidence through the bead view's reader (`cardWorldRig`) | phone, desktop |
| `queues` | the Queues panel (Lands workbench-glass spec WB-6, step 5.5, gd-HuJdsFabSb) on its Decisions tab (`/garden?queue=decisions`, `fixtureQueues` in `queue_test.go`), the panel only (`clip`): the three tabs with Decisions selected, Mine selected over Everyone's, the longest-waiting row first, each saying why it needs the viewer in gold words and carrying the Mailbox's answer box | phone, desktop |
| `search` | the search panel (Lands workbench-glass spec WB-5, step 5.7, gd-Oqh2suV8ap) on "edge" (`/garden?q=edge`, `fixtureSearch` in `search_test.go`), the panel only (`clip`): its field holding the query, the line saying the hits come closest in meaning first, and three hits (plan A, a task by its short title, a decision), each with its type, state, land, priority and age in words | phone, desktop |
| `bundle` | the bundle panel (Lands workbench-glass spec WB-9, step 5.10, gd-UFvVkR7Mwx) on the face fixture's bundle (`/garden?bundle=gd-FaceBundle1`, `fixtureBundles` in `bundle_test.go`), the panel only (`clip`): the line naming its documents, the bead it was filed for, its pull request and the bundle bead, then its documents each by its format (markdown, a diff, a table, a screenshot, a link, text); on a desktop the contents rail beside them. Registering the bundle kind gave the bundle face its "Read as one document" link, which moved `facebundle` | phone, desktop |
| `picpreview`, `picviewer` | a picture's hover preview and the picture viewer (Lands workbench-glass spec WB-18, step 5.15, gd-uEEeNMlk1a) on the closed step gd-HarvestSt1, whose newest grade has four pictures, `picpreview`, a mouse resting on the first picture raises the preview beside it, the shot clipped to the Harvest section and the preview (clear of the header's Open PR ↗, a glyph the pinned fonts lack, which renders differently by machine); `picviewer`, the bead view's panel only (`clip`), a click on the second opens the viewer (`?pic=`) on it: the picture at the panel's width, Previous, `2 of 4`, Open whole (Open on a phone), Next, and the filmstrip with the second ringed | desktop (`picpreview`: a phone has no hover), phone and desktop (`picviewer`) |
| `cockpit` | the plan cockpit (Lands workbench-glass spec WB-7, step 5.9, gd-nTPjJ2yllf) on plan A (`/garden?plan=gd-planA`, `fixturePlans` in `plan_test.go`), the panel only (`clip`): where the plan stands ("Building.") and its progress, age and land in words, what it waits on (the decision and the escalation in gold words first, then the step that waits on another), and its Foundations phase's steps, each numbered with its state, owner, MR and age. The viewer may not write, so no levers. Registering the plan kind gave every plan's face its Open the cockpit link, which moved `faceplan` and the desktop `beadplan` | phone, desktop |
| `cockpitstale` | the plan cockpit on plan A (`golden_cost_browser.js`, gd-rtE07tg6Nh) scrolled to its crew header, the panel only (`clip`): plan A's spend line stale, `Spend: $12.35, 3 gardeners · stale` in the stale ink, as the farm sends it once a live crew's rollup has not been re-stamped for `planspend.StaleAfter`. The shot hands the farm document on once more with plan A's cost marked stale and holds later documents off; `TestCostPatchUnderChromium` drives the same change through the real stream | phone, desktop |
| `plans`, `plansforest` | the Plans panel (gd-BL2JQbsfyy), opened from the camera's Menu and sorted by Spend, the drawer only (`clip`): the crop tiles, then the Forest at its Grove zoom, each plan with its spend line from the controller's rollup, said as `/plans` says it: plan A's `$12.35, 3 gardeners`, plan C's `$6.80, 2 gardeners · stale` (live crew, stamped two hours ago) and plan B's settled `$3.10, 1 gardener`, then the plans never rolled up, each a dash. The cockpit's crew header says the same figure (`Spend: $12.35, 3 gardeners`), below the `cockpit` clip | phone, desktop |
| `project`, `projectcost` | the project panel (Lands workbench-glass spec WB-10, step 5.11, gd-fT15vJniWQ), the panel only (`clip`): gardenio (`/garden?project=gardenio`, `fixtureProjects` in `project_test.go`) on its Decisions tab, where an ask waits: the line under the title, the eight tabs with their counts, two decisions with the Mailbox's answer box, an escalation and the bead blocked on them; and acme's Cost tab (`/garden?project=acme&tab=cost`), a tenant land whose purse is at the wall, said in words above its credits. Adding the Health tab to the tab row (plan gd-gh5J0vVbJP step 4.1, gd-SKVLOuGToB) moved both, and `projectcostcharts`. acme's owner row, with an admin's Become beside it (plan gd-0SUIdKXWV3 step 3, gd-r5nRAIgagl; the card world's viewer is an admin), moved `projectcost` and `projectpartial` | phone, desktop |
| `projectcostcharts` | the project panel's Cost tab on gardenio (`/garden?project=gardenio&tab=cost`, plan gd-gh5J0vVbJP step 2.3b, gd-mrcuxcJyyj), a project the card world's farm draws (its plans, gd-vViKmjtrTj), so its charts load from the stand-in Grafana (`fixtureGrafanaPanel`): the panel only (`clip`), scrolled to its Charts section: the range chips with 7 days chosen and the cards in view, each titled as a card under the section (not as a section heading of its own), its scope line and Open link, its frame loaded | phone, desktop |
| `gardenercharts`, `agentcharts` | the agent panels (plan gd-gh5J0vVbJP step 4.2, gd-g7Ms8Ddfmj), the panel only (`clip`): guard the plan's Charts tab (`/garden?gardener=gd-GardTrig01&tab=charts`), its runs' five cards (Spend by run first) under the range chips, the frames in view loaded from the stand-in Grafana (`fixtureGrafanaPanel`); and one gardener's charts in the Costs panel (`/garden?costs=agent:gardenio-53-typed-faces`, opened from its face's See its charts), its four cards and no farm figures. The new Charts tab moved `gardener`, and the link moved `faceagent` | phone, desktop |
| `costsland` | a tenant land's credits (plan gd-gh5J0vVbJP step 3.3, gd-kSG5BqJMRo) in the Costs panel (`/garden?costs=land:acme`), the panel only (`clip`), drawn by `credits.js` from `landCredits` in `golden_browser.js`, the answer `GET /garden/charts/costs-land` gives acme's tenant (fulfilled with `page.route`: the card world's farm has no tenant land, and `land_credits_test.go` guards the route): the range chips, the spent tiles, remaining of granted with the wall marked, and burn to the wall with its top value, now and the wall labelled and the run-out dashed; credits per agent is below the fold | phone, desktop |
| `land`, `landwall`, `landcap` | where a tenant lands (Lands Track B step 8.8, gd-vJK94LpTOm): `/garden?land=meadow`, from `golden_land_browser.js`. The card world's farm has no tenant land, so `page.route` rewrites the farm document into a tenant's cut: music's region, plans and steward renamed meadow (the Commons fixture's tenant land) and nothing else, with a purse on its cost row. `land` is the whole page: the camera on the land's region, on a desktop the Commons band open on meadow's `#general` (a phone stays on the Farm), and the plaque as the land header, the land's name and "18.5 of 100 credits left · low". `landwall` is the plaque only, the purse at the wall on the alert fill. `landcap` is the plaque only, the purse low beside the steward resting at its daily run cap (gd-OvKMtyWtrp), "Steward resting: 24 of 24 runs today, back at 00:00 UTC", linking the steward's bead; a phone breaks it before "back". In `land` the steward runs under its cap, so it says nothing there. All three show the header's Steward notes link (music's steward has a notes bead in the card world, gd-q4NgYNGpy8) | phone, desktop |
| `landnotes`, `landnotesguest` | the land header's Steward notes clicked (gd-q4NgYNGpy8), the dialog only (`clip`), drawn by `landnotes.js` from `NOTES` in `golden_land_browser.js`, the answer `GET /garden/notes/meadow` gives (fulfilled with `page.route`; `land_notes_test.go` guards the route): the notes whole by section; `landnotes` as a member, Holds a text box with Cancel and Save Holds; `landnotesguest` as a guest, Holds read like the rest and Close only | phone, desktop |
| `gardener`, `settings` | the gardener and settings panels (Lands workbench-glass spec WB-11, WB-13, step 5.12, gd-au7ywvWAG8), the panel only (`clip`): guard the plan (`/garden?gardener=gd-GardTrig01&tab=runs`, `fixtureGardenerPanels` in `gardener_test.go`) on its Runs tab: the line under the title, its state (a run out), its five levers, the five tabs, the three runs newest first with the failed one marked; and the viewer's settings (`/garden?settings=me`, `fixtureSettings`): Spawning with its prompt template, and You | phone, desktop |
| `projectpartial`, `projectagents`, `projectgardeners`, `projectaccounts`, `projectactivity`, `queueinbox`, `queueescalations`, `gardenerabout`, `searchempty`, `settingsedit`, `costsproject`, `mailbox`, `cmdk` | Workbench panel states the commons-ux-acceptance skill shot as `EXTRA` rows and graded accept, promoted (gd-wSb1jRJnJm; `panelShot` in `golden_browser.js`, which `shoot.js`'s `wbShot` builds on), the panel only (`clip`): acme's Gardeners tab, whose list the daemon did not answer (`/garden?project=acme&tab=gardeners`: "Could not read its gardeners" and Try again, WB-15); guard the plan's About tab (`/garden?gardener=gd-GardTrig01&tab=about`: goal, wakes and stops, counters, owner); a search that finds nothing (`/garden?q=nothing+matches+this`); the settings editor open (`/garden?settings=me`, Edit your settings pressed: the spawn defaults and who the viewer is as one form, its first field focused); the Costs panel on one project (`/garden?costs=gardenio`, its frames loaded from `fixtureGrafanaPanel` as `costs` waits); gardenio's Agents, Gardeners, Accounts and Activity tabs (`&tab=`); the Queues panel's Inbox and Escalations (`?queue=`); the dock's Mailbox tab with its open decision (`clip`: `#dock`); and Cmd-K over the map with "edge" typed (`clip`: `#jump` alone, no farm at its edges). The Costs panel's figures say no spend is recorded because the card world's spend source has no rows (as `costs`). The skill's stress, empty and down rows (`X-UX-Fixture`, `ux_fixture_test.go`) stay out of the goldens. The Project panel's Health tab (gd-SKVLOuGToB) and the Gardener panel's Charts tab (gd-g7Ms8Ddfmj) re-shot the `project*` rows and `gardenerabout`: only their tab strips moved | phone, desktop |
| `glass`, `glassoff` | the Glass, watch (Lands workbench-glass spec GL-1, GL-2, GL-3, step 6.4, gd-9adIfFKLls), the panel only (`clip`): `gd-GlassLive01` (`/garden?glass=gd-GlassLive01`, `fixtureGlass` in `glass_panel_test.go`) live, its stream the recorded session `testdata/glass_session.jsonl` played through the real relay: the state chip, Look only, the header's rows (the stream's cost and context over the bead's) and xterm.js (served from the viewer's `templates/static`) at the PTY's own 100 x 26, scaled down to the panel on a phone; and `gd-GlassOff001`, whose stream ends `unavailable` (no relay key, 6.3b), its cached screen kept and why said in words. Offering the Glass to the fixture's operator gave the agent face its Watch live link, which moved `faceagent`, and a gardener's thread header its Watch live, which moved `commonsgardener` | phone, desktop |
| `glassattach` | the Glass, attach (Lands workbench-glass spec GL-9, GL-10, step 6.6, gd-mWooPMTNwA), the panel only (`clip`): `gd-GlassKey001` (`/garden?glass=gd-GlassKey001`, `fixtureGlass`; the recorded session again, and the one agent the fixture offers attach on, so the watch shots keep no Attach button) after a click on the terminal (it is the input, gd-FpaO6Pb9bR): the banner "Attached as you", the chip saying Attached, the terminal in the accent frame, Fit, Compact and Let go, and Ctrl-C typed into the terminal, so the confirmation asks before it is sent (Cancel first, Send Ctrl-C apart in the danger style, gd-bWtNrk4snt). The fixture agent it adds has no session on the Wall's feed, so the Wall's line says "2 more agents have no live session", which moved `wall` | phone, desktop |
| `creds` | the credential pool (Lands workbench-glass spec GL-15, step 6.9, gd-zM1oXXxlYp), the panel only (`clip`): `/garden?creds=pool`, the card world's pool (`withFixturePool` in `glass_panel_test.go`) of four accounts, one for each health (Healthy, Warming, Throttled until 40 minutes on, No key and not probed), with the line under the title and the strategy, each account's 5 h and 7 d meters and its line, and the read-only footer. Making the fixture's viewer an admin, so the pool is theirs to read, gave gardenio's project header its Credential pool link, which moved `project` | phone, desktop |
| `pad` | the phone's one-row pad under the map (cursor-grid spec 7), Select latched: the pad's box only (`clip`), which runs down behind the tab bar | phone (the desktop has none) |
| `bead` | the in-garden bead view of plan A, the panel only (`clip`), its plan sections filled from the farm document | phone, desktop |
| `beadconv` | the fixture Slack wish's Conversation section (gd-tx2wMH48MV, `commonstest.WishFamily`), the panel only (`clip`) scrolled to it: the person's replies, the crew's card as one state line (its agent linked, its state a chip) and its squawks, and the head's two kd comments in the comment voice, one timeline oldest first; on a phone the list opens on its newest line with the thread's reply composer under it (the card world's Commons lets the viewer reply). A bead bound to a thread now shows this section in place of its Comments section | phone, desktop |
| `beadplan` | plan P's bead view scrolled to its progress and what the retired plot card carried (cursor-grid spec 6.4, build step 6): gardeners here, operator, the nine phases with the current one gold and named so (`clip`) | phone, desktop |
| `plan` | plan P's nine phases, the cursor inside plan P on its first (the title bar names it; the others are stakes) (Enter from `?at=gardenio/gd-planP`), clipped around its plot | phone, desktop |
| `planout` | the same at the bottom of the Plan level (0.47x, by the wheel): the label rides the world, its text fixed | phone, desktop |
| `armsgate` | plan P's arms (heraldry spec 6.3, 6.6, step 4.4) at device scale **2** (the row's `dpr`), where the 48 px bitmap must still land 1:1 on whole device px: the 24 px picture at its plot gate over the first bed, that bed's stake and step bare though both bear arms (Q16, R6), the cursor's label clear of the picture (`gateClip`). Before it shoots, the row checks the words: the plot's link says the arms, the first stake's says its own, the step's none, and no node draws them | phone, desktop |
| `gardeners` | the cursor inside plan A, the wheel out to the Plan level: plot A's gardeners each on an empty tile beside the task it claimed, facing it (gd-HB0xs0CTbk: never capped), the foreman and a phase's gardener beside their beds, one asking and one down on B; plan A's two water channels from each in-field blocker to the step it holds, the sluice at the waiting end (gd-Y4gXDBblIG) (the gardeners, `homestead-agents-as-people.md`; `clip`) | phone, desktop |
| `birds`, `birdsstill` | the `gardeners` framing with tool calls as birds (gd-pLFocqmYyf, `web/birds.js`): each gardener on plan A or B is sent a `farmactivity` call of another class (the first a fold of five, a flock of three; the last a subagent's start, the hawk), then `BIRDS`'s clock is pinned (`BIRDS.pin`) 0.7 s on with motion forced on (`BIRDS.motion`); `birdsstill` is the same calls under reduced motion, one still bird perched over each figure's head (`clip`). Birds are 10 x 6 (sparrow, finch, pigeon) and 14 x 8 (crow, twig bird, gull, swift, hawk) sprites of four colours (gd-cFZPuAjqJu), drawn at a whole number of device px per sprite px, 1.5 CSS px at Plan rounded: 2 at the goldens' DPR 1, so a crow is 28 px wide (21 at DPR 2). By day a dark bird is ringed in a pale outline and a bright one in a dark one; at night a dark bird in a pale one (gd-7dRp5a6r15). Each gardener at work wears the doing pose of its call (4c, gd-kxCviRUVA2; redrawn to tell apart at Bead zoom in 4f, gd-1EMXaKkRPI: knelt with a magnifier for a read, the hoe for an edit, both arms up at the hawk for the subagent, and so on); one asking or down keeps its reason's pose. The still bird stands above every pose's padded map (`birds_test.go`). Each call carries its class's tool word (`Read`, `Edit`, `go test`, `git push`, `kd show`, `WebFetch`, `helm upgrade`, `shell`), which flies with its bird as a small haloed label beside it, one per gardener (gd-VLVayQzQIB, 4k). The words are drawn after the figures and their bubbles and keep off every face, "?" bubble, bird and the cursor's ring, on the next clear side of the bird or not at all (4n, gd-VHkZ3pCEqK) | phone, desktop |
| `birdsbeadstill` | the `birdsstill` calls at the Bead level (`birdsShown(false, 3)`, plot A clipped as `gardenersbead`): each still bird's word beside it, over the figures, clear of the asking gardener's "?" bubble, the faces and the cursor's ring round the phase bed; a still bird hidden under the figure a row up says nothing (4n, gd-VHkZ3pCEqK) | phone, desktop |
| `gardenerarms` | a gardener's task arms (heraldry step 5.9): the `arms` and `armspa` scenes (`transitions_test.go`) put two gardeners on plan P, the cursor inside it at the Bead level; each wears its line's arms as a badge over its head (`HERALDRY.badge`, 8 world px high, day and night pictures), the plan's for g-arms-p on the second phase's step and the first phase's for g-arms-pa, the odd column lifted clear of its neighbour's; the plan's picture stays at the gate | phone, desktop |
| `porcharms` | the Porch after the same scenes: each armed gardener's row carries the run's picture, 16 px, before "Tending:" | phone, desktop |
| `gardenersbead` | the cursor inside plan A at the Bead level: its gardeners on empty tiles beside the plants they tend, facing them, every step visible, and plan A's water channels, thin lines from the blocker's centre along the seams between tiles to the waiting step's centre (gd-osWVpshPrj) (`clip`) | phone, desktop |
| `cellskeep` | beads keep their cell (gd-xhpccozIcC, cursor-grid spec 5.4): the `gardenersbead` framing after a second farm document, sent as the page's next `farmdoc`, files gd-A1new older than every step of plan A's first phase (a cold layout would move each of them on a cell) and no longer lists the last untended step of its second. The row checks the page's memo (`sessionStorage` `garden.cells.v1`) before it shoots: every bead kept its cell, the new step took a free one; the picture shows it in the lowest free cell and the dropped step's cell as stubble (`clip`). Compare it with `gardenersbead`: no other plant moved | phone, desktop |
| `barn` | the cursor on gardenio's Barn zone (`?at=gardenio/barn`, focus moves the cursor onto it): the barn on its keep, the bay under it, and the title bar counting everyone running (28.12: every running gardener stands in the Growing field, the Barn holds no figures) | phone, desktop |
| `barnthread` | thread agents at home (5b, gd-1kFVOmUjiK; H-D10): the cursor on g-thread's stand in the Commons's Growing field (`?at=~/barn`, then `GARDEN.fly`), solid on the east tile of its pair facing the plant for its bead on the west (gd-PeLoWLR7Lq), its pink thread shirt, kneeling with the magnifier for the read it is sent (a `farmactivity` call, `BIRDS`'s clock pinned 0.7 s on with motion forced on) and that bird lifting over the Commons's barn, the title bar saying "In the Commons: a pen for the goats"; g-thread-down, down for want of room in the cluster, a scarecrow in a stand of its own. Adding the two thread agents to the fixture farm re-shot every golden that counts the gardeners or shows the home strips, the Porch or the legend | phone |
| `steward` | music's steward (Lands 9.8) right of its land's sign at the Bead level, its tip open as a hover opens it: led by its memory's In flight line (`clip`) | phone, desktop |
| `graveyard` | the graveyard (3.2, gd-s9KzRk6boh): the cursor on the done closed wish's headstone (`?at=gardenio/wish%3Agd-wishx1`), its tip open as a hover (a desktop) or focus (a phone) opens it: who asked, "Done: the asker closed it", when, the MR its kin merged and its thread; the abandoned and expired wishes' plain stones beside it (`clip`). Adding the three closed wishes to the fixture farm grew gardenio's region and re-shot every golden that shows the world | phone, desktop |
| `goatarms`, `goatgate`, `goatpen` | a goat's tip (heraldry 5.10, gd-WUqr6IkgkM; `goatShot`), hovered on a desktop and tapped on a phone: `goatarms`, gd-wish1 grazing by plan A at the Plan level, its kin's two line runs (`FarmWish.arms`, given in the farm document on its way to the page, since the fixture's plans bear none) before the wish's title; `goatgate`, its open gate in the pen, the same card saying it is out grazing; `goatpen`, gd-wish2 in its pen cell, kinless, a ghost with no arms (`clip`: the tip) | phone, desktop |
| `truckload`, `truckgone`, `truck` | the release truck (gd-yOOVahJMCh, step 7 of plan gd-UZKZ9H25UD; `truckShot`), the cursor on the Commons's home cell (`?at=~/home`) so its label keeps off the truck, the releases blob fed from `testdata/releases_fixture.json` (`TRUCK.feed`) until the farm document carries it: `truckload`, the camera on gardenio's bay under its Barn (28.10) at the Plan level (0.82x), the red truck (72 x 48 world px since gd-Ir8jxl4c57, `S` 3, standing centred on the bay's floor with its wheels on the ruts) loaded with a crate per plan and one for the unplanned, the tail of 2026.299.2 on its gold side plate; `truckgone`, the same release live, still under reduced motion with SHIPPED on a post over its cab; `truck`, at the Bead level, the loading truck's tip open as a hover (desktop) or focus (phone) opens it: the release, loading and when it was cut, its PRs and plans, and that prod runs the previous release. Adding the red truck's legend row moved the phone's `legenddo` | phone, desktop |
| `release`, `shippedplot` | the release truck's other faces, graded and promoted in step 10 (gd-HHMReWCHUi): `release`, Enter on the focused loading truck opens the release panel (`releaseShot`, `web/release.js`; the viewer's `/api/releases/{id}` answered by the shot), the pane only (`clip`): its facts, previous and the release bead, then the changelog, 12 numbered PRs of 14 by plan and step with plan A finished; `shippedplot`, the cursor on plan A at the Plan level, whose newest merged PR shipped in 2026.300.1 (`FarmPlan.shipped`): the shipped stamp on its top-left fence, 2x and below the cursor's ring, and the title bar's status ending "shipped in 2026.300.1 · prod" (`clip`: the plot's corner and the bar; a phone frames the plot wider than the screen, so its stamp is cut at the edge). The stamp moved every golden showing plan A's top-left fence, and the plate's gold margin moved the truck rows | phone, desktop |
| `jumpwish`, `pen`, `graveyardjump` | the pen and the graveyard reached from the camera column on a phone (3.3, gd-NbWjIdLTnd), which has no Down from the Yard to hand: `jumpwish`, Go tapped and "wish" typed, the jump list holding the pen (its two open wishes), gardenio's Graveyard (its three closed wishes), the two goats and the stones (the camera column and the list, `clip`); `pen`, "pen" typed and its row tapped, the cursor on the pen under the Yard, framed whole, with the title bar (`clip`); `graveyardjump`, "graveyard" typed and its row tapped, the cursor on gardenio's Graveyard band, its project framed round it on the whole screen | phone |
| `gohome`, `homein`, `commonsin`, `homeletter`, `homegoat` | a project's home reached from the camera column and the pad on a phone (HW 29.6, gd-cTKXpm8JH7): `gohome`, the cursor on plan A and Go tapped, the empty list led by gardenio's home row (the camera column and the list, `clip`); `homein`, that row tapped and the pad's In, the cursor on a zone and the whole farmstead on the screen; `commonsin`, the same from a bare `/garden` on the Commons's home, which holds only project-less items; `homeletter` and `homegoat`, the pad's In from gardenio's Mailbox and Pen (`?at=`), the cursor on a letter and on a goat with its zone whole round it | phone |
| `legend`, `legenddo` | the legend panel, whose gardener rows draw the real sprites (`clip`); `legenddo` is it scrolled to the doing poses (4c, redrawn in 4f), the magnifier the top row | phone, desktop |
| `ride`, `burst`, `flyback`, `walkhome`, `arrive` | the gardeners' transitions (`homestead-agents-as-people.md` section 7), one keyframe each: a live farm patch pushed to the open page by `cardWorld`'s test-only `POST /test/farm/scene` (undone after the shot), then `ANIM`'s clock pinned (`ANIM.pin`) with motion forced on (`ANIM.motion`). At 0.55x, framed first on gardenio's Barn (`home:gardenio/barn`), then centred on the moment's point along the transition: the truck halfway to plot C; a burst's particles at 15%; the flight back at 60% with the scarecrow up; walking home with the scroll at 50%; the tldr over the barn | phone, desktop |
| `reduced` | the `ride`, `burst` and `walkhome` patches under reduced motion: the end state only, with a probe that no transition runs and no animation frame is asked for | phone, desktop |
| `commons`, `commonsthread`, `commonsplan` | the Commons v2 workbench (plan gd-lRaCxsiLvC step 4) at `/garden/c/n/{id}`, from `golden_workbench_browser.js`: the tree rail, the node's document with its transcript and composer, and the Jobs, Plan and Open rails; `commons` on an epic with a task selected, `commonsthread` on `#general`'s `gd-wishPorch` as a thread root, `commonsplan` on a plan with its building step selected (the crew's jobs, the step marked among its phase's, its ask and PR; step 5 put the holder's state on the composer's reply line, `gardenio-crew-S3 · working`). The phone's push stack is step 7 | desktop |
| `impersonated` | a Become session (plan gd-0SUIdKXWV3 step 4), from `golden_impersonated_browser.js` (the fixture's `X-UX-Impersonator` header stands in for the verified token's claim): the banner "You are phono@example.com, impersonated by matt" with Stop above the opening farm | phone, desktop |
| `splash`, `splashdusk`, `splashnight`, `splashdawn`, `splashflat` | the splash demo (`/garden/splash`, plan gd-EJGA3zDGCk steps 1 and 10) at four hours of the full moon of 26 Oct 2026, each pinned by `?t=`: noon (`12:00:00Z`), dusk (`16:45:00Z`, the sun low in the west), midnight (`00:00:00Z`, the full moon high in the south) and dawn (`06:45:00Z`, the sun low in the east, the moon setting). The WebGL scene is drawn by headless Chromium's SwiftShader at a wider tolerance than the rest (software GL; the reason is on its rows in `goldenSurfaces`), every shot held at the mid quality tier by `?q=mid`, phone and desktop alike, so no runner's cores or frame times change it, in the light scheme only (the scene is lit by its clock, not the scheme). The phone upright frames it at 80 degrees vertical, the desktop at 50 (`SPLASH.fov`), so the crest stays a third of the way up and the stone whole right of centre. Each shot asserts the canvas's name, the almanac's words for its hour (`Dusk over the farm: the sun low in the west; ...`). About a minute of SwiftShader a surface (two shots), four minutes in all. `splashflat` is the same page with WebGL blocked by an `init` script: the still (`web/splash-fallback.png`, the noon frame at 800 x 500 without its controls) covering the viewport with its one-line note | phone, desktop |

Open wishes are goats (gd-ODxztYntyd, `web/goats.js`; a 16 x 12 sprite of five colours in one of three coats, one CSS px per sprite px at Plan, so 16 x 12 px on every screen; at Bead the zoom, never under 2): the fixture's one open wish, gd-wishPorch, whose kin is plan A, grazes in the path under plot A, so every Plan or Bead shot that shows that path carries a white goat. Adding it re-shot `gardeners`, `birds`, `birdsstill`, the phone `cursorplan`, `planout`, and the desktop `beacons`, `ride`, `burst`, `flyback`, `walkhome`, `arrive` and `reduced`. The frozen clock fixes its spot and frame. Since 2.2 (gd-kqTYEsMvhv) it laps the path round plot A (the Yard in the transition scenes) instead of the stretch under it, clear of the gardeners and their birds' ground, which moved the shots that show it. Redrawing the goat at 16 x 12 in five colours, at half the blow-up so its footprint is unchanged (gd-KMCkhEZPfZ), re-shot every golden that shows a goat: `arrive`, `barn` (desktop), `birds`, `birdsstill`, `burst` (desktop), the phone `cursorplan` and `pen`, `flyback`, `gardeners`, `reduced`, `ride` and `walkhome`. Redrawing the birds at 10 x 6 and 14 x 8 in four colours, at half the blow-up (gd-cFZPuAjqJu: the same footprint at DPR 2, one notch bigger at the goldens' DPR 1, where `pxOf` rounds 1.5 CSS px up to 2), re-shot `barnthread`, `birds`, `birdsstill` and `birdsbeadstill`; in `birds` the taller crow's `go test` now sits under it, clear of the face below. Giving the birds a third wing frame, so a flap is up, level, down, level, and the hawk a glide it soars and lands in (gd-BuWCnuLYQv), re-shot `birds` (at 0.7 s a flock's later bird is on its down beat and the hawk glides); the goats' chew and two-step moved no golden (at the frozen clock each goat shot keeps the frame it had).

Every desktop row carries the Commons bar along the bottom (36 px; the world gives it that height), and every phone row the tab bar,
Farm · Commons · Porch · Mailbox. Turning the right-hand rail into that bar (gd-Cyv7OuD66X) re-shot every `*-desktop-*` golden that shows
the whole viewport; the phone goldens did not move. Making the phone's Commons a tab above which its sheet sits (step M1 of
`2026-10-20-lands-commons-phone-design.md`, gd-4hal7KPuxv) re-shot every `*-phone-*` golden; the desktop ones did not move. The cursor pad (cursor-grid spec 7, gd-POdVKGNXfV: a 60 px row on the tab bar, no minimap and no Projects button on a phone) re-shot every `*-phone-*` golden that shows the map, and replaced the `projects` row with `pad`; the desktop ones did not move. Making the phone's camera column a right inset for the cursor's framing (gd-pyrowrsfDF: a channel crossing into the framed region came in under In/Out/Yard) re-shot every `*-phone-*` golden that shows the map; the desktop ones did not move. The pane's fixture land is `commonsWorld` (`commons_world_test.go`), mounted on
`cardWorld` beside the page.

The bar and the phone's Commons tab carry the viewer's unread badge (gd-q20kp0MRzh). The rows share one fixture daemon and run
in `SHOTS` order, so the read positions carry over: the viewer has read nothing until the `commons` row puts `#general` on
screen (36 unread, 3 of them in `#general`), and from there on only `#stress` counts (33). `settle` waits for the counts and
the read mark to be answered before it shoots.

Plan P bears arms (`goldenPlanArms` in `golden_browser_test.go`), and so do its first phase and that phase's first step. From the
Plan depth in, the World hangs the plan's picture at its plot gate (heraldry step 4.3), and no label of the cursor's stands over it
(`CURSOR.keepOff`'s gate, step 4.4). Adding the arms re-shot `plan`, `planout`, `beacons`, the desktop `beadplan` (its Heraldry
section), and the phone `cursorplan`, `gardeners`, `birds` and `birdsstill`, where plot P is the neighbour on the right. `settle`
waits for the picture wherever the camera is at the Plan or Bead level, because it arrives a frame after the page asks for it.

The farm at the crossroads (plan gd-rLs7EoTa7a, HW section 29) moved every golden that shows the world, each once, in
the step that moved its pixels (HW 29.7): the farmstead in slot 0 and the lanes (#2214), the cursor's home cell (#2216),
the courtyard with the region ring and the field's fence gone (#2229), the hedged plots (#2260), the trucks and walkers on
the lanes (#2247) and the camera framing a project on its farmstead (#2271). None of them committed a golden; the window
bless PR re-shoots them on main. The plan's tidy-up step (gd-CBK0GXrMx9) moves no pixel. Drawing each Growing field stand
as a pair, its plant west and its solid figure east facing it (gd-PeLoWLR7Lq, #2251), moves every shot that shows a home
strip's field with a stand in it at the Plan depth or in (`barnthread` among them); it too committed no golden.

A `SHOTS` row with a `clip(page)` shoots only that part of the viewport; `plan` and `planout` clip to the frame
`TestCardUnderChromium`'s `phaseRows` shoots (the plot plus a margin of its neighbours),
and `bead` to the bead view's panel (`paneClip('#bead')`), since the farm behind it is what the other rows guard.

A surface becomes a golden once it has been graded `accept` against the Commons UX rubric
(`gardenio/docs/commons-ux-rubric.md` §5) with the `commons-ux-acceptance` skill
(`.claude/skills/commons-ux-acceptance/SKILL.md`), which shoots these same `SHOTS` rows in the
rubric's full capture matrix. `bead` was the first to go through that loop (gd-vzsNYKoGNX).

## C3 on every phone shot

Each `phone` and `phoneland` shot also runs the Commons UX rubric's DOM probes
(`.claude/skills/commons-ux-acceptance/scripts/probes.js`) under a touch pointer, inside the
surface's probe box (`testdata/golden_roots_browser.js`, shared with the skill's `shoot.js`), and
prints a `golden c3 <shot> {...}` line. `TestGardenGoldens` fails a shot with a **control under
44x44 CSS px** (counting an `::after` hit-slop and a field's own `<label>`; a link in running text
aside), a **box that scrolls sideways** and is not built to (rubric C3, M4.1 gd-T3JPWCD9xM), or a
**field under 16 px**, which iOS zooms into on focus (`zoomField`, M1.3 gd-xWWqgcmzXM). A
known bug that is filed and not fixed yet gets a `goldenC3Known` row in `golden_browser_test.go`
(`<surface>-<device>`, the bead, how many hits of each kind); a shot with more hits than its row
fails, and one with fewer says to lower or delete the row. C3 is judged in update runs too: a
blessed golden does not excuse a small target. The page as a whole scrolling sideways fails the
shot itself, on every device, as before.

A sweep no longer stops at the first shot that fails: each failure prints a `golden failed
<shot>: <why>` line with its stack, the rest are still shot, and the capture fails at the end
naming them all.

## The empty composer

The house voice (gd-5e6xgIOH6B): every empty box where a human types to the fleet shows
`gardenhub.EmptyPrompt` ("if you don't have any ideas, go outside") in the muted
placeholder colour (`.empty-prompt::placeholder` in the viewer's `layout.html`).
`TestComposerGoldens` opens `/threads/{id}` for a Slack-bound wish as an operator and:

- asserts the textarea's placeholder is the constant;
- asserts the placeholder clears **4.5:1** against the textarea's composited background
  in every skin the switcher offers (the log prints each ratio);
- shoots the composer row (textarea and Send) at the phone and desktop sizes, in the
  **light** (`baseline`), **dark** (`dark-terminal`), **homestead** (the Homestead day
  skin, the viewer's default) and **dark-homestead** (its night) skins:
  `cmd/beads-viewer/testdata/golden/composer-<device>-<theme>.png`, 8 PNGs, a few KB each.
  The clock is pinned to noon UTC on 2026-10-20, so the Homestead season is autumn.

The capture script is `cmd/beads-viewer/testdata/composer_golden_browser.js`. It forces
the pinned fonts onto every element (the skins name generic families no `@font-face` can
shadow), and shoots the row rather than the form because the hint line under it carries
a U+2318 the font subsets lack.

## The Lands front of house

`TestLandsGoldens` (`cmd/beads-viewer/lands_golden_test.go`, step 7.5 gd-WFIUrR8dZA) shoots the
viewer's `/lands/` pages: the landing page and the Terms anonymously, and `/lands/accept` as an
applicant (a verified OIDC token, no role) who has not accepted yet, and the GitHub App setup page
(`/lands/github/setup`, step 7.6 gd-WsBvUZTzft) for that applicant signed in with GitHub, once
passed (`landsgithub`) and once refused (`landsgithubrefused`, installed on more than one
repository; it answers 403, which the surface row declares), the plot application form and the
applications page (step 7.7), and the sign-in picker (`/login`, plan gd-JaMxRKsc8z step 4.1). Each
at the phone and desktop sizes, in the system's **light** and **dark** schemes (a stranger has no
stored skin, so the pages follow `prefers-color-scheme` into the Homestead's night, `dark-homestead`,
and the meadow's night scene; the capture asserts the skin):
`cmd/beads-viewer/testdata/golden/lands/<surface>-<device>-<theme>.png`, 60 PNGs, 30-95 KB each, the phone
turned sideways (`phoneland`, 750x340) in both themes since M4.1 (gd-T3JPWCD9xM)
(the Dawn Meadow scene is in every frame). They live in a subdirectory because `golden.Check` treats
every PNG in its directory as one of its own shots.

The capture script is `cmd/beads-viewer/testdata/lands_golden_browser.js`. Before any shot it fails
the run when a page threw or when the document is wider than the viewport; on both phones it runs the
rubric's C3 probes (above) over the whole page and fails the run, once every shot is taken, on any
control under 44x44 px or box scrolling sideways. The Gate has no known-hits list: fix the page. `LANDS_GOLDEN_FULL=<dir>` also writes full-page shots there for grading;
those are never goldens.

The pages render the legal documents, so a change to `gardenio/docs/lands/legal/{terms,privacy,aup}.md`
moves `landsterms` (and the version shown on every page): copy the documents into
`cmd/beads-viewer/lands/legal/` (`TestLandsLegalMirrorsDocs`), pin the new version
(`TestLandsLegalVersionPinsText`); the next window's bless PR re-shoots `landsterms`.

## A bead's arms in the viewer

The viewer shows a bead's arms as their emoji run, text before the title (heraldry step 5.5a,
gd-nLVyw2i9fN), so it has no arms golden: the goldens pin fonts with no emoji face, and
`cmd/beads-viewer/arms_test.go` checks the run's markup on every surface instead. `TestArmsGoldens`
and its 20 PNGs (steps 3.1 and 3.4) went with the picture.

In `/garden` heraldry step 3.4 promoted `faceepic` and `faceplan` (an epic and plan F bearing arms, their
Heraldry section first), `facearms` (Change arms open as a writer, plan F named as a look-alike) and
`commonsfacearms` (plan A's card in its wish thread, the picture at 24 px). `facework` did not move:
the task bears arms and shows none. The card's reply re-shot `commonsthread` and `commonsgardener`.

## When CI goes red on a golden

The job log names each shot and how far it moved, e.g.
`opening-phone-light.png: 120 of 329160 pixels differ, tolerance 16`. The run uploads a
`garden-goldens` artifact with `<gate>/<shot>.actual.png`, `<shot>.expected.png` and
`<shot>.diff.png` (`<gate>` is `garden` or `composer`) (differing pixels red over a faded copy of the golden) for every
failing shot. Look at the diff:

- **Not meant**: fix the code; the golden stays.
- **Meant**: the golden is not yours to update: the next window's bless PR re-shoots it
  on main (below). Say in your PR description which shots you expect to move.
- **Neither: your change cannot have moved it** (the shot is on a surface the PR does not
  touch, or it is red on main or on another PR too): quarantine the shot. Push its row to
  main as its own file, `.github/quarantine.d/golden-<shot>.tsv`. CI reads main's copy, so the shot stops failing every
  PR at once. The `quarantine rows for the golden failures` step prints the row. A
  quarantined shot still renders and diffs, logs `QUARANTINED <shot>: ...`, and keeps its
  PNGs in the artifact. Policy: `.github/known-failures.md`, "Quarantine".

## Updating goldens

**Never commit `testdata/golden` in a feature PR** (owner ruling gd-oxDhsqdFVP, task
gd-fedEoEGC9i). The PNGs were the conflicted files in 56 of 301 main-into-PR merges in one
week, and no PR run verifies them. ci.yml's light tier runs `scripts/check-no-pr-goldens.py`,
which fails a PR that changes any file under
`gardenio/controller/internal/gardenhub/testdata/golden/` or
`gardenio/controller/cmd/beads-viewer/testdata/golden/` (their `fonts/` excepted) unless its
branch starts `bless/goldens-`.

The goldens are re-blessed on main once per release window:
`.github/workflows/bless-goldens.yml` runs at 03:30Z and 15:30Z (and on dispatch), runs
`make update-garden-goldens` on main's head, and when any image moved opens a PR from
`bless/goldens-<UTC yyyymmddHHMM>` listing every changed image with links to main's copy and
the branch's. The merge desk merges it on its light tier like any PR. A newer bless PR closes
an older one still open.

Render-check locally if you like. On Linux (an agent pod, a Linux laptop):

```sh
make update-garden-goldens        # then look at the PNGs that moved
git restore --source=origin/main --staged --worktree -- \
  gardenio/controller/internal/gardenhub/testdata/golden gardenio/controller/cmd/beads-viewer/testdata/golden \
  ':(exclude)gardenio/controller/internal/gardenhub/testdata/golden/fonts'
```

The restore drops every golden change before you commit (the guard prints the same command).
`make update-garden-goldens` rewrites only the goldens that fail their tolerance (an
unchanged shot keeps its bytes), writes goldens for new shots, and deletes goldens no surface
shoots any more.

A whole sweep is heavy (every surface, two devices, two themes, in one Chromium): on a
2 Gi agent pod it can be OOM-killed. It runs in shards (gd-EwwEfGfe6H): up to 4 card worlds,
one per CPU the test may use, each shooting every 4th row of `SHOTS` in its own page at
once, so its wall time falls with the shard count. `GARDENHUB_GOLDEN_SHARDS=N`
sets the count; `GARDENHUB_GOLDEN_SHARDS=1` is the old serial sweep, and the lighter one on
a small pod. A shard sees the farm the serial sweep would, by the same read-mark replay as
`GARDENHUB_GOLDEN_ONLY` below. The capture's `golden time` line gives each shard's time. `GARDENHUB_GOLDEN_ONLY=cursorplan,cursorharvest` (surface names,
comma separated) shoots those surfaces alone, for `/garden` only; the goldens of the other
surfaces are neither compared nor counted stale. The surfaces still see the farm a full
sweep would: the one thing a row leaves for the next is the Commons channels it marked
read (the badges on a channel list), so each skipped row before a chosen one has its
channel opened by its path first (`replayed commonscrew: gardenio/crew` in the capture's
output). Before this, `GARDENHUB_GOLDEN_ONLY=commonsbead` kept the #crew and #decisions
badges and blessed a PNG CI's sweep failed by 612 px (gd-VJWqCUihlQ). A full sweep fails
if a row marks a channel its path does not open (and no row before it did), since the
replay would miss it: give such a row the channel's path. `GARDENHUB_GOLDEN_CELLS=phoneland-light`
(`<device>-<theme>`, comma separated) does the same for cells: only those are shot, and the goldens
of the others are neither compared nor counted stale, so a landscape-only change is blessed in one
cell's time. Run the surfaces your change touches in batches, then let CI's full sweep confirm
nothing else drifted:

```sh
cd gardenio/controller && GARDENHUB_GOLDENS=1 GARDENHUB_GOLDEN_UPDATE=1 GARDENHUB_GOLDEN_ONLY=cursorplan,cursorharvest \
  go test ./internal/gardenhub/ -run '^TestGardenGoldens$' -count=1 -v
```

On macOS the test skips: Chromium there renders text differently, and a Mac-made golden
would fail CI. A PR needs no golden, so there is nothing to do. To look at CI's renders
(a dispatch or nightly run's `garden-goldens` artifact), copy them in, look, and drop them
again before you commit (the restore above):

```sh
gardenio/scripts/garden-goldens-from-ci.sh          # newest ci.yml run on this branch
gardenio/scripts/garden-goldens-from-ci.sh <run-id>
```

## Why a shot repeats exactly

A golden is only a gate if the same code always paints the same pixels. Two runs on one
machine differ by 0 pixels, and so do an agent pod and the CI runner (ci.yml run
39208096382, all 14 shots), so the goldens can be made in a pod and checked in CI. The
exception is a shot that draws a character no pinned font has (*Same fonts* below): the
test names each one, and will not bless it from a pod.

- **Fixture data.** `cardWorld` builds the farm from `fixtureWorld` (fixed ids, created and
  updated times relative to `baseUnix`), so the layout and every count are the same.
- **Frozen clock.** The farm's clock on the server and `Date.now()` in the browser
  (`page.clock.setFixedTime`) are both `goldenNow`, so "2h ago" never ticks. Timers still
  run, so the page settles as usual.
- **Fonts pinned.** `/garden` uses system font stacks, which differ by OS and by package
  set. The capture splices `@font-face` rules for subsets of Noto Sans and Noto Sans Mono
  into the document before `</head>` and points `--font-body` and `--font-display` at them.
  The page draws no text on canvas, so that covers every glyph it shows.
- **No motion.** `reducedMotion: 'reduce'` (flights are jump cuts, nothing pulses), CSS
  animations disabled and the caret hidden at capture.
- **Same browser.** `playwright-core` is pinned in `package-lock.json`, which pins the
  Chromium build. Locale `en-US`, timezone UTC. The capture prints the Chromium and
  playwright-core it ran (`browser: Chromium ..., playwright-core ...` in the test log), and
  a playwright-core other than the pinned one (another `GARDENHUB_PW_MODULES`) is a warning,
  and under `GARDENHUB_GOLDEN_UPDATE=1` a failure: its goldens would not match CI's
  (gd-VJWqCUihlQ).
- **Same fonts, where it can tell.** Text the pinned faces cannot draw falls back to a
  system font, and the pod's fonts are not the runner's: the cost cards' `Open ↗` (U+2197)
  drew in Noto Sans Math on a pod and in another face on CI, 526-550 px apart, while the
  Chromium build was the same on both (now an SVG mask, `.ext` in `garden.css`, gd-zLPoTcMCST). The capture lists, per shot, the characters it draws
  past ASCII and any text whose font stack starts with no pinned face; the test logs each
  shot a system font draws part of (`system font: <shot> draws ↗ (U+2197), in no pinned
  font`), and an update does **not** rewrite such a shot when it differs: take CI's render
  for it with `garden-goldens-from-ci.sh`. The lasting fix is to stop drawing the character
  (see *Glyphs outside the subset* below).

## Tolerance

Per surface, two numbers in `goldenSurfaces`: a pixel *differs* when any RGBA channel
moves by more than `channel` (0-255), and the shot fails when more than `pixels` of them
differ. It is a budget in pixels, not a share of the frame. One letter changed in the
card ("Gardener" to "Gardenet") moves about 120 pixels, which is 0.012% of the desktop
frame and would pass under any ratio loose enough to sound safe. The defaults (40, 16)
catch that one-letter change on every card shot. Measured drift between machines is 0, so
16 is headroom. Loosen a surface only with a measured reason in its comment; the test
logs every shot's drift, which is where to read one.

## Adding a surface (Commons, Workbench, Glass, ...)

A surface inside `/garden`:

1. **Fixture data.** If the surface needs data the world lacks, add it to `cardWorld`'s
   `porch` argument or a sibling of `goldenPorch`. Fix every timestamp against
   `goldenNow`, never `time.Now()`.
2. **A `SHOTS` row** in `testdata/golden_browser.js`: `name` (one word, no `-`), the `path`
   it opens, a `show(page, device)` that puts it on screen and waits until it is there,
   `devices` if it exists at only one width, `themes` if the colour scheme cannot change it
   (`splash`, whose WebGL scene is lit by its pinned clock), `clip` (a selector) to shoot only a pane
   whose backdrop other rows already guard, and `init`, a function run in the page before
   its own scripts (`splashflat` takes WebGL away with one). Wait on the page's own state
   (`waitForSelector`, `waitForFunction`), never on a bare timeout.
3. **A tolerance row** in `goldenSurfaces`. A shot without one fails.
4. Grade it first: `make commons-ux-shoot OUT=<dir> SURFACES=<name>` and the
   `commons-ux-acceptance` skill. Only an `accept` grade is promoted.
5. `make update-garden-goldens` and look at every new PNG, then commit the code without
   them (see [Updating goldens](#updating-goldens)); the next window's bless PR adds them.

A surface outside `/garden` (another route or server, e.g. the Commons if it gets its
own handler): give it its own world (an `httptest` server with fixture beads), its own
capture script modelled on `golden_browser.js` (same font splice, clock and settle
steps), and its own `Test<Name>Goldens` calling `golden.Browser`, `golden.Check` and
`golden.ArtifactsDir("<gate>")`; `TestComposerGoldens` is the worked example. Add its
package and `-run` pattern to `make test-garden-goldens` and `make update-garden-goldens`,
its `PASS` line to the goldens step and its paths to the `gardenhub_browser` filter in
`ci.yml`, and its gate to the `dest` map in `garden-goldens-from-ci.sh`.

A new **axis** (the Garden and Business card themes, say) is a loop in the capture
script and a word in the file name (`<surface>-<device>-<theme>-<skin>.png`).

**Mind the size.** Every golden is in the tip tree every agent pod clones on a cold start
(`scripts/check-large-tracked-files.sh`, ka-0000000431). Keep device scale 1 and shoot
what a regression would actually show up in, not every state. A row sets `dpr` only for what shows nowhere else, as
`armsgate` does for the device-resolution arms picture, and then clips small.

**Glyphs outside the subset** (a new script, emoji, a symbol) fall back to system fonts,
which differ between an agent pod and the CI runner, so the shot drifts by machine.
Noto Sans, the face the page's text renders in, has no dingbats (U+2700-27BF, so no ✓ or
✕), no media symbols (⏸), no arrows (U+2190-21FF, so no ←) and few geometric shapes (no
● ○ ◍; ◌ is there). Noto Sans Mono has the arrows and more shapes, but text in
`--font-body` or `--font-display` never falls back to it. **Draw a status mark as inline
SVG in `currentColor` instead of a text glyph** (the Commons agent card's `stateMarks` in
`internal/commons/cards.go`, gd-icRpoTdPhD), or as a CSS `mask` of an SVG in
`currentColor` when the page's own code draws it (the Commons back arrow, gd-PGuWWmb23a):
no font is involved, so every machine and every browser draws the same mark. A link that
opens a new tab gets the class `ext`, whose `::after` draws the north-east arrow as such a
mask, instead of writing ↗ (gd-zLPoTcMCST).
`TestRenderedTextInGoldenFonts` (`internal/commons/glyphs_test.go`) fails when the Commons
renderers, `commons.js`, the pane's `garden.html` markup or a `cm-` rule's CSS `content`
write a character the pinned Noto Sans lacks, checked with `sfnt` `GlyphIndex`. For text that needs a new
range (a script, punctuation), re-subset with the range added; ranges a face lacks are
silently dropped, so check the result with `GlyphIndex` too:

```sh
cd /usr/share/fonts/truetype/noto   # Debian/Ubuntu fonts-noto-core + fonts-noto-mono
U="U+0020-007E,U+00A0-017F,U+2010-2027,U+2030-205E,U+20AC,U+2190-21FF,U+2212,U+25A0-25FF"
for f in NotoSans-Regular NotoSans-Bold NotoSans-Italic NotoSansMono-Regular NotoSansMono-Bold; do
  pyftsubset $f.ttf --unicodes="$U" --layout-features='kern,liga,calt' --no-hinting \
    --output-file=$OLDPWD/gardenio/controller/internal/gardenhub/testdata/golden/fonts/$f.ttf
done
```

then commit the fonts (they are not goldens to the guard); new font files change every shot,
and the next window's bless PR re-shoots them all.
