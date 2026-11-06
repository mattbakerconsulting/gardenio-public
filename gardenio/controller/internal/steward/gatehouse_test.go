package steward

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The gatehouse's steward (step 9.12): a plain tenant steward whose land's
// project bead carries land_gatehouse=true, so the playbook plays the front
// door.

// gatehousePrompt binds a steward to the farm's tenant land, marked the
// gatehouse when mark is set, and returns the prompt it was bound with.
func gatehousePrompt(t *testing.T, mark string) string {
	t.Helper()
	d := newFarm()
	d.beads[0].Fields[GatehouseField] = mark
	if _, err := Bind(context.Background(), d, land, BindSpec{}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(d.created[len(d.created)-1].Fields, &fields); err != nil {
		t.Fatal(err)
	}
	p, _ := fields["prompt"].(string)
	return p
}

func TestBind_GatehousePlaysTheFrontDoor(t *testing.T) {
	p := gatehousePrompt(t, "true")
	for _, want := range []string{
		"This land is the gatehouse: the front door of Gardenio Lands.",
		"Every guest\nreads #general",
		"1. GREET.",
		"no email address, phone\n     number, password, key or payment detail. You will never ask for one;",
		"/lands/apply",
		"/lands/demo",
		"2. ANSWER NEWCOMERS.",
		"3. BUILD NOTHING.",
		"4. NO DIGEST.",
		"5. PARK AT THE WALL.",
		"Spawn any agent, or write a plan: the gatehouse builds nothing.",
		"Ask anyone for anything private",
		"Promise anyone a land, credits or a demo pass",
		"write in #support",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("gatehouse prompt lacks %q", want)
		}
	}
	for _, never := range []string{
		"SPAWN GARDENERS WITHIN THE PURSE", "TURN CONVERSATION INTO PLANS", "gb lands plan write",
		"gb spawn --role", "gb lands digest", "4. REPORT.", "turn asks into plans",
	} {
		if strings.Contains(p, never) {
			t.Errorf("gatehouse prompt says %q:\n%s", never, p)
		}
	}
}

// With land_gatehouse_tools on (gd-K62coPtKkX), duty 2 takes applications and
// hands out demo passes with gb lands gatehouse; off, it only points to the
// forms.
func TestBind_GatehouseToolsOnlyWhenSwitchedOn(t *testing.T) {
	tools := []string{
		"gb lands gatehouse demo tomato-patch --message <their message>",
		"gb lands gatehouse apply tomato-patch --message <their latest message>",
		"connect the GitHub repository",
		"whatever name anyone gives you",
	}
	off := gatehousePrompt(t, "true")
	for _, w := range tools {
		if strings.Contains(off, w) {
			t.Errorf("tools off: prompt says %q", w)
		}
	}
	if !strings.Contains(off, "You cannot file an application, grant a land, hand out a pass") {
		t.Error("tools off: prompt lost the point-to-the-forms rule")
	}

	for _, sw := range []string{"true", ""} {
		d := newFarm()
		d.beads[0].Fields[GatehouseField] = "true"
		d.beads[0].Fields[GatehouseToolsField] = "true"
		if sw == "" {
			d.beads[0].Fields[GatehouseField] = ""
		}
		if _, err := Bind(context.Background(), d, land, BindSpec{}); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var fields map[string]any
		if err := json.Unmarshal(d.created[len(d.created)-1].Fields, &fields); err != nil {
			t.Fatal(err)
		}
		p, _ := fields["prompt"].(string)
		for _, w := range tools {
			if got := strings.Contains(p, w); got != (sw == "true") {
				t.Errorf("gatehouse=%q tools=true: prompt has %q = %v", sw, w, got)
			}
		}
		if sw == "true" && strings.Contains(p, "You cannot file an application") {
			t.Error("tools on: prompt still says it cannot file an application")
		}
	}
}

// Any other tenant land, and a mark that is not "true", keep the tenant
// playbook.
func TestBind_OnlyTheMarkMakesTheGatehouse(t *testing.T) {
	for _, mark := range []string{"", "false", "yes"} {
		p := gatehousePrompt(t, mark)
		if strings.Contains(p, "gatehouse") || !strings.Contains(p, "3. SPAWN GARDENERS WITHIN THE PURSE.") {
			t.Errorf("mark %q: not the tenant playbook:\n%s", mark, p)
		}
	}
}

// The home garden is never the gatehouse, whatever its project bead says.
func TestLoadLand_HomeGardenIsNotTheGatehouse(t *testing.T) {
	d := newHomeFarm()
	for _, b := range d.beads {
		if b.ID == "gd-proj" {
			b.Fields[GatehouseField] = "true"
		}
	}
	now := time.Now()
	st, err := LoadLand(context.Background(), d, home, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Land.Gatehouse || !st.Land.Home {
		t.Fatalf("home garden read as %+v", st.Land)
	}
}
