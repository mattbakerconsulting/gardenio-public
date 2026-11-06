package bridge

// escalationAudienceFields are the fields an escalation's audience is read
// from (beadsapi.EscalationAudience, gd-bmOiteQi36): the freshly read bead's,
// with the event's filling any key it lacks, so a sparse close event or a
// sparse read still sees operation_type, sensitivity and the credentials.
func escalationAudienceFields(fresh, event map[string]string) map[string]string {
	out := make(map[string]string, len(fresh)+len(event))
	for k, v := range event {
		out[k] = v
	}
	for k, v := range fresh {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
