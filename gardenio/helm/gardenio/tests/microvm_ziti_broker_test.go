package tests

import (
	"strings"
	"testing"
)

// Per-run Ziti identities for MicroVM agents (ka-0000001924): the
// ziti-access-broker env must render only when zitiBroker.url is set, and the
// whole microvm master switch must keep the render byte-identical when off.

func TestMicrovmZitiBrokerEnvRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		microvmBaseFlags(
			"agents.microvm.zitiBroker.url=http://ziti-access-broker.fabric:8080",
			"agents.microvm.zitiBroker.serverID=broker.fabric",
			"agents.microvm.zitiBroker.enroll=true",
		)...)
	if !strings.Contains(out, "MICROVM_ZITI_BROKER_URL") || !strings.Contains(out, "http://ziti-access-broker.fabric:8080") {
		t.Error("expected MICROVM_ZITI_BROKER_URL to render when zitiBroker.url is set")
	}
	if !strings.Contains(out, "MICROVM_ZITI_BROKER_SERVER_ID") || !strings.Contains(out, `"broker.fabric"`) {
		t.Error("expected MICROVM_ZITI_BROKER_SERVER_ID to render when serverID is set")
	}
	if !strings.Contains(out, "MICROVM_ZITI_BROKER_ENROLL") {
		t.Error("expected MICROVM_ZITI_BROKER_ENROLL to render when enroll=true")
	}
}

func TestMicrovmZitiBrokerEnvAbsentByDefault(t *testing.T) {
	t.Parallel()
	// microvm on, zitiBroker unset → deprecated static-identity path, no
	// broker env (serverID/enroll must not render without a URL either).
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		microvmBaseFlags("agents.microvm.zitiBroker.serverID=orphaned")...)
	if strings.Contains(out, "MICROVM_ZITI_BROKER") {
		t.Error("no MICROVM_ZITI_BROKER_* env may render while zitiBroker.url is unset")
	}
}

func TestMicrovmZitiBrokerEnrollOmittedWhenFalse(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		microvmBaseFlags("agents.microvm.zitiBroker.url=http://broker:8080")...)
	if !strings.Contains(out, "MICROVM_ZITI_BROKER_URL") {
		t.Fatal("expected MICROVM_ZITI_BROKER_URL to render")
	}
	if strings.Contains(out, "MICROVM_ZITI_BROKER_ENROLL") {
		t.Error("MICROVM_ZITI_BROKER_ENROLL must not render when enroll=false (Option A default)")
	}
}

func TestMicrovmZitiBrokerEnvAbsentWhenMicrovmDisabled(t *testing.T) {
	t.Parallel()
	// Whole microvm block off → broker values are ignored even if set (byte-
	// identical render guarantee of the master switch).
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.microvm.zitiBroker.url=http://broker:8080",
	)
	if strings.Contains(out, "MICROVM_ZITI_BROKER") {
		t.Error("no MICROVM_ZITI_BROKER_* env may render while agents.microvm.enabled=false")
	}
}
