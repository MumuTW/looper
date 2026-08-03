package admission

import "testing"

func TestPresetMatrix(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name, association string
		tier              AuthorTier
	}{
		{"owner", "OWNER", AuthorTierOwner},
		{"member", "MEMBER", AuthorTierMember},
		{"collaborator", "COLLABORATOR", AuthorTierMember},
		{"past contributor", "CONTRIBUTOR", AuthorTierPastContributor},
		{"first timer", "FIRST_TIMER", AuthorTierPastContributor},
		{"unaffiliated", "NONE", AuthorTierUnaffiliated},
	}
	want := map[Preset]map[AuthorTier]Outcome{
		PresetPersonal:      {AuthorTierOwner: OutcomeAuto, AuthorTierMember: OutcomeAssess, AuthorTierPastContributor: OutcomeAssess, AuthorTierUnaffiliated: OutcomeAssess},
		PresetMaintainedOSS: {AuthorTierOwner: OutcomeAuto, AuthorTierMember: OutcomeAuto, AuthorTierPastContributor: OutcomeAssess, AuthorTierUnaffiliated: OutcomeAssess},
		PresetCompany:       {AuthorTierOwner: OutcomeAuto, AuthorTierMember: OutcomeAssess, AuthorTierPastContributor: OutcomeAssess, AuthorTierUnaffiliated: OutcomeAssess},
		PresetContributing:  {AuthorTierOwner: OutcomeAssess, AuthorTierMember: OutcomeAssess, AuthorTierPastContributor: OutcomeAssess, AuthorTierUnaffiliated: OutcomeAssess},
	}
	for preset, matrix := range want {
		for _, tier := range tiers {
			t.Run(string(preset)+"/"+tier.name, func(t *testing.T) {
				got := Decide(Policy{Preset: preset, Classify: true}, Input{AuthorAssociation: tier.association, Visibility: "private"})
				if got.Outcome != matrix[tier.tier] || got.AuthorTier != tier.tier || got.Preset != preset {
					t.Fatalf("Decide() = %#v, want %s/%s/%s", got, preset, tier.tier, matrix[tier.tier])
				}
				if got.Classify != (got.Outcome == OutcomeAssess) {
					t.Fatalf("Classify = %v for outcome %q", got.Classify, got.Outcome)
				}
			})
		}
	}
}

func TestOverrideAndSafetyInputs(t *testing.T) {
	t.Parallel()
	policy := Policy{Preset: PresetCompany, Classify: true, Overrides: map[AuthorTier]Outcome{AuthorTierMember: OutcomeAuto, AuthorTierBot: OutcomeAssess}}
	member := Decide(policy, Input{AuthorAssociation: "COLLABORATOR", Visibility: "internal"})
	if member.Outcome != OutcomeAuto || member.Rule != "override.member" || member.Visibility != "internal" {
		t.Fatalf("member override = %#v", member)
	}
	bot := Decide(policy, Input{AuthorAssociation: "OWNER", AuthorType: "Bot", Visibility: "private"})
	if bot.AuthorTier != AuthorTierBot || bot.Outcome != OutcomeAssess || !bot.Classify {
		t.Fatalf("bot override = %#v", bot)
	}
	held := Decide(policy, Input{AuthorAssociation: "OWNER", Visibility: "private", Held: true})
	if held.Outcome != OutcomeIgnore || held.Rule != "global-hold" {
		t.Fatalf("held decision = %#v", held)
	}
	contributing := Decide(Policy{Preset: PresetContributing, Classify: true, Overrides: map[AuthorTier]Outcome{AuthorTierOwner: OutcomeAuto}}, Input{AuthorAssociation: "OWNER", Visibility: "public"})
	if contributing.Outcome != OutcomeAssess || contributing.Rule != "safety.no-auto.owner" {
		t.Fatalf("contributing override = %#v", contributing)
	}
}

func TestLegacyIsExplicitDefault(t *testing.T) {
	t.Parallel()
	for _, preset := range []Preset{"", PresetLegacy} {
		got := Decide(Policy{Preset: preset}, Input{AuthorAssociation: "OWNER", Visibility: "public"})
		if got.Outcome != OutcomeLegacy || got.Preset != PresetLegacy || !got.Classify {
			t.Fatalf("Decide(%q) = %#v", preset, got)
		}
	}
}

func TestUnknownVisibilityCannotAutoAdmit(t *testing.T) {
	t.Parallel()
	got := Decide(Policy{Preset: PresetPersonal, Classify: true}, Input{AuthorAssociation: "OWNER"})
	if got.Outcome != OutcomeAssess || !got.Classify || got.Rule != "visibility-unknown.preset.personal.owner.unknown" {
		t.Fatalf("Decide() = %#v", got)
	}
}
