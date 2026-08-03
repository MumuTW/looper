// Package admission decides whether an Issue may enter the autonomous
// pipeline before any model is called. Its inputs are forge facts plus the
// operator-authored relationship policy; Issue prose is intentionally absent.
package admission

import "strings"

type Preset string

const (
	PresetLegacy        Preset = "legacy"
	PresetPersonal      Preset = "personal"
	PresetMaintainedOSS Preset = "maintained-oss"
	PresetCompany       Preset = "company"
	PresetContributing  Preset = "contributing"
)

type Outcome string

const (
	OutcomeLegacy Outcome = "legacy"
	OutcomeAuto   Outcome = "auto"
	OutcomeAssess Outcome = "assess"
	OutcomeIgnore Outcome = "ignore"
)

type AuthorTier string

const (
	AuthorTierOwner           AuthorTier = "owner"
	AuthorTierMember          AuthorTier = "member"
	AuthorTierPastContributor AuthorTier = "past-contributor"
	AuthorTierUnaffiliated    AuthorTier = "unaffiliated"
	AuthorTierBot             AuthorTier = "bot"
)

type Policy struct {
	Preset    Preset
	Classify  bool
	Overrides map[AuthorTier]Outcome
}

type Input struct {
	AuthorAssociation string
	AuthorLogin       string
	AuthorType        string
	Visibility        string
	Held              bool
}

type Decision struct {
	Outcome    Outcome    `json:"outcome"`
	AuthorTier AuthorTier `json:"authorTier"`
	Preset     Preset     `json:"preset"`
	Rule       string     `json:"rule"`
	Visibility string     `json:"visibility"`
	Classify   bool       `json:"classify"`
}

func Decide(policy Policy, input Input) Decision {
	preset := Preset(strings.ToLower(strings.TrimSpace(string(policy.Preset))))
	visibility := strings.ToLower(strings.TrimSpace(input.Visibility))
	if visibility == "" {
		visibility = "unknown"
	}
	if preset == "" || preset == PresetLegacy {
		return Decision{Outcome: OutcomeLegacy, Preset: PresetLegacy, Rule: "legacy-seven-condition-policy", Visibility: visibility, Classify: true}
	}
	tier := authorTier(input)
	if input.Held {
		return Decision{Outcome: OutcomeIgnore, AuthorTier: tier, Preset: preset, Rule: "global-hold", Visibility: visibility}
	}
	if outcome, ok := policy.Overrides[tier]; ok {
		if outcome == OutcomeAuto && (preset == PresetContributing || tier == AuthorTierBot) {
			return Decision{Outcome: OutcomeAssess, AuthorTier: tier, Preset: preset, Rule: "safety.no-auto." + string(tier), Visibility: visibility, Classify: policy.Classify}
		}
		return finalize(policy, preset, tier, visibility, outcome, "override."+string(tier))
	}
	outcome := presetOutcome(preset, tier)
	return finalize(policy, preset, tier, visibility, outcome, "preset."+string(preset)+"."+string(tier)+"."+visibility)
}

func finalize(policy Policy, preset Preset, tier AuthorTier, visibility string, outcome Outcome, rule string) Decision {
	if outcome == OutcomeAuto && visibility == "unknown" {
		outcome = OutcomeAssess
		rule = "visibility-unknown." + rule
	}
	return Decision{Outcome: outcome, AuthorTier: tier, Preset: preset, Rule: rule, Visibility: visibility, Classify: outcome == OutcomeAssess && policy.Classify}
}

func authorTier(input Input) AuthorTier {
	if strings.EqualFold(strings.TrimSpace(input.AuthorType), "bot") || strings.HasSuffix(strings.ToLower(strings.TrimSpace(input.AuthorLogin)), "[bot]") {
		return AuthorTierBot
	}
	switch strings.ToUpper(strings.TrimSpace(input.AuthorAssociation)) {
	case "OWNER":
		return AuthorTierOwner
	case "MEMBER", "COLLABORATOR":
		return AuthorTierMember
	case "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER":
		return AuthorTierPastContributor
	default:
		return AuthorTierUnaffiliated
	}
}

func presetOutcome(preset Preset, tier AuthorTier) Outcome {
	if tier == AuthorTierBot {
		return OutcomeIgnore
	}
	switch preset {
	case PresetPersonal:
		if tier == AuthorTierOwner {
			return OutcomeAuto
		}
		return OutcomeAssess
	case PresetMaintainedOSS:
		if tier == AuthorTierOwner || tier == AuthorTierMember {
			return OutcomeAuto
		}
		return OutcomeAssess
	case PresetCompany:
		if tier == AuthorTierOwner {
			return OutcomeAuto
		}
		return OutcomeAssess
	case PresetContributing:
		return OutcomeAssess
	default:
		return OutcomeLegacy
	}
}
