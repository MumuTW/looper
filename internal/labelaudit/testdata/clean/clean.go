package clean

import (
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/network/protocol"
)

// Referencing a constant is the intended usage and must not be reported.
var Trigger = labels.DefaultPlanTrigger
var Target = protocol.TargetLabelForNode("red")

// A local variable that merely shares a package's name must not be read as
// that package.
type shadow struct{ Prefix string }

var notAnOwner = shadow{Prefix: "looper:"}
var harmless = notAnOwner.Prefix + "plan"

// An unrelated string is not a label.
const unrelated = "looper-plan"
