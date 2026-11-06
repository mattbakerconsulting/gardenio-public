package conversation

import (
	"context"
	"errors"
	"testing"
)

func TestDispatchRunsTheHandlerForTheKind(t *testing.T) {
	d := NewDispatcher(nil)
	var got Inbound
	d.Handle(InboundReply, func(_ context.Context, in Inbound) (Answer, error) {
		got = in
		return Answer{Ephemeral: []Message{{Text: "noted"}}}, nil
	})
	in := Inbound{Kind: InboundReply, Surface: SurfaceCommons, Thread: "gd-head", Message: "gd-msg", Text: "hi"}
	a, err := d.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message != "gd-msg" || len(a.Ephemeral) != 1 || a.Ephemeral[0].Text != "noted" {
		t.Fatalf("handler saw %+v, answered %+v", got, a)
	}
	if _, err := d.Dispatch(context.Background(), Inbound{Kind: InboundAction, Surface: SurfaceSlack, ActionID: "kill"}); !errors.Is(err, ErrNoHandler) {
		t.Fatalf("unregistered kind: err = %v, want ErrNoHandler", err)
	}
}

func TestDispatchValidates(t *testing.T) {
	d := NewDispatcher(nil)
	called := false
	d.Handle(InboundReply, func(context.Context, Inbound) (Answer, error) {
		called = true
		return Answer{}, nil
	})
	for name, in := range map[string]Inbound{
		"no kind":     {Surface: SurfaceSlack, Thread: "gd-head"},
		"no surface":  {Kind: InboundReply, Thread: "gd-head"},
		"no thread":   {Kind: InboundReply, Surface: SurfaceCommons},
		"no action":   {Kind: InboundAction, Surface: SurfaceCommons, Message: "gd-card"},
		"no submit":   {Kind: InboundSubmit, Surface: SurfaceSlack},
		"no reaction": {Kind: InboundReact, Surface: SurfaceCommons, Message: "gd-head"},
	} {
		if _, err := d.Dispatch(context.Background(), in); err == nil {
			t.Errorf("%s: dispatched, want an error", name)
		}
	}
	if called {
		t.Fatal("an invalid inbound reached the handler")
	}
	slackRoot := Ref{Surface: SurfaceSlack, Channel: "C0", TS: "1.2"}
	if _, err := d.Dispatch(context.Background(), Inbound{Kind: InboundReply, Surface: SurfaceSlack, Roots: []Ref{slackRoot}}); err != nil {
		t.Fatalf("a reply named by its Slack root: %v", err)
	}
}

func TestDispatchRecoversAPanic(t *testing.T) {
	var seen any
	d := NewDispatcher(func(_ Inbound, v any, stack []byte) {
		if len(stack) == 0 {
			t.Error("no stack")
		}
		seen = v
	})
	d.Handle(InboundReply, func(context.Context, Inbound) (Answer, error) {
		panic("boom")
	})
	_, err := d.Dispatch(context.Background(), Inbound{Kind: InboundReply, Surface: SurfaceSlack, Thread: "gd-head"})
	if !errors.Is(err, ErrHandlerPanic) || seen != "boom" {
		t.Fatalf("err = %v, onPanic saw %v", err, seen)
	}
}

func TestInboundRoot(t *testing.T) {
	in := Inbound{Roots: []Ref{{Surface: SurfaceSlack, Channel: "C0", TS: ""}, {Surface: SurfaceSlack, Channel: "C1", TS: "1.2"}}}
	if r, ok := in.Root(SurfaceSlack); !ok || r.Channel != "C1" {
		t.Fatalf("Root(slack) = %+v, %v; want the first non-zero ref", r, ok)
	}
	if _, ok := in.Root(SurfaceCommons); ok {
		t.Fatal("Root(commons) found a ref that was never supplied")
	}
}
