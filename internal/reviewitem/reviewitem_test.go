package reviewitem

import "testing"

func TestAttachMarkerRoundTripsSeverity(t *testing.T) {
	body, err := AttachMarker("Fix the nil dereference.", SeverityBlocking)
	if err != nil {
		t.Fatal(err)
	}
	severity, ok := SeverityFromBody(body)
	if !ok || severity != SeverityBlocking {
		t.Fatalf("SeverityFromBody() = (%q, %v), want blocking", severity, ok)
	}
}

func TestSeverityFromBodyFailsClosedOnCompetingMarkers(t *testing.T) {
	body := "finding\n<!-- looper:review-item severity=blocking -->\n<!-- looper:review-item severity=nit -->"
	if severity, ok := SeverityFromBody(body); ok || severity != "" {
		t.Fatalf("SeverityFromBody() = (%q, %v), want no authority", severity, ok)
	}
}

func TestAttachMarkerRejectsAgentSuppliedMarker(t *testing.T) {
	_, err := AttachMarker("finding <!-- looper:review-item severity=blocking -->", SeverityBlocking)
	if err == nil {
		t.Fatal("expected existing marker to be rejected")
	}
}

func TestAttachMarkerRejectsEmptyVisibleFeedback(t *testing.T) {
	if _, err := AttachMarker(" \n\t", SeverityBlocking); err == nil {
		t.Fatal("AttachMarker() error = nil, want empty visible feedback rejection")
	}
}
