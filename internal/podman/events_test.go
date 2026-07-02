package podman

import (
	"strings"
	"testing"
)

func TestScanEventsParsesContainerEvents(t *testing.T) {
	// Real lines captured from `podman events --format json --stream=false`.
	input := `{"ID":"0e379bf2","Image":"ghcr.io/example/app:0.1.0","Name":"sweet_satoshi","Status":"died","time":1778483742,"timeNano":1778483742324880269,"Type":"container","Attributes":{"io.kubernetes.pod.name":"web","io.kubernetes.pod.namespace":"default"}}
{"ID":"91e8bd69","Image":"ghcr.io/example/app:0.1.0","Name":"youthful_dewdney","Status":"create","time":1778680595,"timeNano":1778680595761123306,"Type":"container","Attributes":{"io.kubernetes.pod.name":"db","io.kubernetes.pod.namespace":"prod","io.kubernetes.pod.deployment":"db"}}
`
	var got []Event
	scanEvents(strings.NewReader(input), func(e Event) { got = append(got, e) })

	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Type != "container" || got[0].Status != "died" || got[0].PodName() != "web" || got[0].PodNamespace() != "default" {
		t.Errorf("unexpected first event: %+v", got[0])
	}
	if got[1].Status != "create" || got[1].PodDeployment() != "db" {
		t.Errorf("unexpected second event: %+v", got[1])
	}
}

func TestScanEventsSkipsMalformedLines(t *testing.T) {
	input := `not valid json
{"ID":"abc","Status":"start","Type":"container","Attributes":{"io.kubernetes.pod.name":"ok"}}

`
	var got []Event
	scanEvents(strings.NewReader(input), func(e Event) { got = append(got, e) })

	if len(got) != 1 {
		t.Fatalf("expected 1 event (malformed/blank lines skipped), got %d", len(got))
	}
	if got[0].PodName() != "ok" {
		t.Errorf("unexpected event: %+v", got[0])
	}
}

func TestScanEventsHandlesNilAttributes(t *testing.T) {
	// Non-container event types (e.g. "image") often have null Attributes.
	input := `{"ID":"cc598e3b","Name":"ghcr.io/example/app:0.1.0","Status":"pull","Type":"image","Attributes":null}
`
	var got []Event
	scanEvents(strings.NewReader(input), func(e Event) { got = append(got, e) })

	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].PodName() != "" {
		t.Errorf("expected empty PodName for nil Attributes, got %q", got[0].PodName())
	}
}

func TestScanEventsEmptyInput(t *testing.T) {
	var got []Event
	scanEvents(strings.NewReader(""), func(e Event) { got = append(got, e) })
	if len(got) != 0 {
		t.Fatalf("expected no events, got %d", len(got))
	}
}
