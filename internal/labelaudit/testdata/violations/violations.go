package violations

import (
	l "github.com/MumuTW/looper/internal/labels"
	. "github.com/MumuTW/looper/internal/network/protocol"
)

// 1. bare literal
const bare = "looper:plan"

// 2. case variant of the same label
const cased = "LOOPER:Worker-Ready"

// 3. assembled through an aliased import
var assembledAlias = l.Prefix + "hold"

// 4. assembled through a dot-imported owner constant
var assembledDot = TargetLabelPrefix + "blue"

// 5. a concrete member of the target family, spelled out
const concreteTarget = "looper:target:red"

func scoped() string {
	// 6. assembled through a function-local constant
	const localPrefix = "looper:"
	return localPrefix + "spec-ready"
}
