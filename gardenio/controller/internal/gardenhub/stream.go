package gardenhub

// stream is the versioned patch ring + subscriber set that Hub (HTML fragments)
// open-codes and Farm (structured JSON) reuses. It carries NO lock of its own:
// the owner serialises every call under the same mutex that guards the state
// the patches describe, which is what makes "snapshot at version v" and "patches
// after v" line up without a window in which an update can be lost.
//
// Contract, identical to Hub's (gd-BKx5ddIilZ):
//   - every patch carries (prev, v); a client replays the chain prev->v and any
//     gap (ring overflow, restarted process, v from the future) degrades to a
//     full reset, which is always safe;
//   - a subscriber whose queue fills is collapsed to a data-less reset marker
//     (see send) so a laggard never blocks the broadcaster or its peers;
//   - a catch-up is one delta patch when the owner's log covers the client's
//     version (farm_delta.go) and it is no bigger than the ring's replay, else
//     the replay, else the reset.
type stream struct {
	version uint64
	ring    []ringEntry
	clients map[int]chan Msg
	nextID  int
	resets  uint64
	deltas  uint64
}

func newStream(version uint64) *stream {
	return &stream{version: version, clients: map[int]chan Msg{}}
}

// publish bumps the version, builds the patch body for (prev, v), appends it to
// the ring and fans it out. build is called once, so marshalling happens once
// per broadcast, not once per client.
func (s *stream) publish(build func(prev, v uint64) []byte) Msg {
	return s.publishAt(s.version+1, build)
}

// publishAt is publish to version v, which must be above the current one: a
// per-viewer cut stream (farm_cut_stream.go) takes its versions from a counter
// it shares with every other cut, so a version names one cut's state only.
func (s *stream) publishAt(v uint64, build func(prev, v uint64) []byte) Msg {
	prev := s.version
	s.version = v
	msg := Msg{Event: "patch", V: s.version, Data: build(prev, s.version)}
	s.ring = append(s.ring, ringEntry{prev: prev, v: s.version, msg: msg})
	if len(s.ring) > ringCapacity {
		s.ring = s.ring[len(s.ring)-ringCapacity:]
	}
	for _, ch := range s.clients {
		send(ch, msg)
	}
	return msg
}

// subscribe registers a client that last saw version since (0 = unknown) and
// returns the catch-up messages (catchUp) plus the live channel. cancel, like
// every other call, must be made under the owner's lock.
func (s *stream) subscribe(since uint64, delta func(since uint64) (Msg, bool), reset func() Msg) (catchUp []Msg, live <-chan Msg, cancel func()) {
	catchUp = s.catchUp(since, delta, reset)
	id := s.nextID
	s.nextID++
	ch := make(chan Msg, clientBuffer)
	s.clients[id] = ch
	return catchUp, ch, func() {
		if c, ok := s.clients[id]; ok {
			delete(s.clients, id)
			close(c)
		}
	}
}

// catchUp takes a client from version since to the current one: nothing when it
// is there, else the fewer bytes of one patch built by delta (when delta covers
// since; nil: never) and the ring's patch chain, else one reset built by reset.
func (s *stream) catchUp(since uint64, delta func(since uint64) (Msg, bool), reset func() Msg) []Msg {
	if since == s.version {
		return nil
	}
	replay, ok := s.replay(since)
	if delta != nil {
		if m, dok := delta(since); dok && (!ok || len(m.Data) <= msgBytes(replay)) {
			s.deltas++
			return []Msg{m}
		}
	}
	if ok {
		return replay
	}
	s.resets++
	return []Msg{reset()}
}

func msgBytes(ms []Msg) int {
	n := 0
	for _, m := range ms {
		n += len(m.Data)
	}
	return n
}

// replay returns the patch chain taking a client from since to the current
// version, or false when the ring cannot cover it.
func (s *stream) replay(since uint64) ([]Msg, bool) {
	if since == 0 || since > s.version {
		return nil, false
	}
	start := -1
	for i, e := range s.ring {
		if e.prev == since {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	out := make([]Msg, 0, len(s.ring)-start)
	want := since
	for _, e := range s.ring[start:] {
		if e.prev != want {
			return nil, false
		}
		out = append(out, e.msg)
		want = e.v
	}
	if want != s.version {
		return nil, false
	}
	return out, true
}

func (s *stream) closeAll() {
	for id, ch := range s.clients {
		close(ch)
		delete(s.clients, id)
	}
}
