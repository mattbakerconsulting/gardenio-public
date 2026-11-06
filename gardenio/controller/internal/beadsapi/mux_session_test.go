package beadsapi

import "testing"

func TestMuxSessionID(t *testing.T) {
	cases := []struct {
		name string
		b    *BeadDetail
		want string
	}{
		{"nil bead", nil, ""},
		{"nothing", &BeadDetail{}, ""},
		{"pod name wins over last_session_id", &BeadDetail{
			Notes:  "backend: coop\npod_name: crew-gardenio-crew-one-abc\npod_namespace: gardenio\n",
			Fields: map[string]string{"last_session_id": "58f450fc-a2ae-46aa-841b-61ee2238d64c"},
		}, "crew-gardenio-crew-one-abc"},
		{"no pod: last_session_id", &BeadDetail{
			Notes:  "backend: coop\n",
			Fields: map[string]string{"last_session_id": " sess-laptop "},
		}, "sess-laptop"},
		{"blank pod name falls back", &BeadDetail{
			Notes:  "pod_name:   \n",
			Fields: map[string]string{"last_session_id": "sess-2"},
		}, "sess-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.b.MuxSessionID(); got != c.want {
				t.Fatalf("MuxSessionID() = %q, want %q", got, c.want)
			}
		})
	}
}
