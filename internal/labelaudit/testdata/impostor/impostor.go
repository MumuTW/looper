package impostor

// Declares a value internal/labels already owns, from a different package.
const HoldGlobal = "looper:hold"

// Declared as an expression rather than a literal. An owner that defines a new
// label this way must still have it protected, or the value is free elsewhere.
const Assembled = "looper:" + "assembled"
