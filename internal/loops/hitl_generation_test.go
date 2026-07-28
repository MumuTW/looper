package loops

import "testing"

func TestGitHubAskDeliveryPending(t *testing.T) {
	t.Parallel()
	if GitHubAskDeliveryPending(HITLAsk{DeliveryPending: true, Transport: "github", Status: "awaiting"}) != true {
		t.Fatal("want true for delivery-pending github park")
	}
	if GitHubAskDeliveryPending(HITLAsk{DeliveryPending: true, Transport: "github", Status: "awaiting", AskCommentID: 1}) {
		t.Fatal("want false once AskCommentID is set")
	}
	if GitHubAskDeliveryPending(HITLAsk{DeliveryPending: true, Transport: "feishu", Status: "awaiting"}) {
		t.Fatal("want false for non-github transport")
	}
	if GitHubAskDeliveryPending(HITLAsk{DeliveryPending: false, Transport: "github", Status: "awaiting"}) {
		t.Fatal("want false without DeliveryPending flag")
	}
	if GitHubAskDeliveryPending(HITLAsk{DeliveryPending: true, Transport: "github", Status: "answered"}) {
		t.Fatal("want false once answered")
	}
}

func TestAskGenerationMatches(t *testing.T) {
	t.Parallel()
	// Legacy park with no generation accepts any card.
	if !AskGenerationMatches(HITLAsk{}, "", "") {
		t.Fatal("empty park generation must accept empty card")
	}
	if !AskGenerationMatches(HITLAsk{}, "x", "y") {
		t.Fatal("empty park generation must accept any card")
	}

	park := HITLAsk{ExecutionID: "exec-2", AskedAt: "2026-07-28T01:00:00Z"}
	// Answer-only /respond omits tokens and authorizes the current park.
	if !AskGenerationMatches(park, "", "") {
		t.Fatal("omitted generation tokens must accept current parked ask")
	}
	if AskGenerationMatches(park, "exec-1", "2026-07-28T01:00:00Z") {
		t.Fatal("wrong executionId must not match")
	}
	if AskGenerationMatches(park, "exec-2", "2026-07-28T00:00:00Z") {
		t.Fatal("wrong askedAt must not match")
	}
	if !AskGenerationMatches(park, "exec-2", "2026-07-28T01:00:00Z") {
		t.Fatal("matching executionId+askedAt must accept")
	}
	// askedAt alone is enough when execution id omitted on card but askedAt matches.
	if !AskGenerationMatches(park, "", "2026-07-28T01:00:00Z") {
		t.Fatal("matching askedAt alone must accept when executionId absent on card")
	}
	// executionId alone is enough when askedAt omitted on card.
	if !AskGenerationMatches(park, "exec-2", "") {
		t.Fatal("matching executionId alone must accept when askedAt absent on card")
	}
}
