# Looper

Looper is a local automation system for running role-based coding-agent workflows across GitHub projects. This context records the product language used when extending Looper's workflow behavior.

## Language

**Workflow Policy Pack**:
A replaceable set of role-bound workflow policies that Looper injects into agent prompts.
_Avoid_: skill pack, prompt bundle, Matt skills

**Role**:
A built-in Looper responsibility lane that decides what kind of work an agent run performs.
_Avoid_: skill, agent type

**Workflow Policy**:
A focused behavior rule that guides one role's agent prompt without changing Looper's lifecycle or output contracts.
_Avoid_: raw prompt, system prompt, skill

**Prompt-Time Policy**:
A workflow policy that can change agent instructions but cannot change queueing, locks, GitHub side effects, lifecycle, recovery, or completion-marker behavior.
_Avoid_: runtime plugin, scheduler extension

**Policy Pack ID**:
A stable machine-readable identifier used to bind a workflow policy pack in configuration.
_Avoid_: display name, title

**Policy Pack Name**:
A human-friendly label for a workflow policy pack shown in CLI output, docs, and diagnostics.
_Avoid_: id, slug

**Role Policy Binding**:
A configuration choice that assigns one workflow policy pack to one Looper role.
_Avoid_: per-policy selection, policy list

**Role-Direct Policy Text**:
Workflow policy text authored directly for a Looper role inside a workflow policy pack.
_Avoid_: abstract policy graph, reusable policy atom

**Policy Precedence**:
The prompt assembly order that places workflow policy pack instructions before user/project custom instructions and before Looper's final lifecycle contracts.
_Avoid_: priority, override order

**Explicit Policy Binding**:
The v1 requirement that a role uses a workflow policy pack only when its role config names that pack.
_Avoid_: implicit global policy, default pack

**Built-In Policy Pack**:
A workflow policy pack shipped with Looper and loaded through the same schema as file-based packs.
_Avoid_: hardcoded prompt constants

**File Policy Pack**:
A workflow policy pack loaded from a user-provided local file.
_Avoid_: Go plugin, remote plugin

**Policy Validation**:
The safety check that ensures workflow policy packs can guide role behavior without overriding Looper's protected lifecycle, safety, disclosure, or completion-marker contracts.
_Avoid_: lint only, schema only

**Policy Visibility**:
The user-facing display of enabled workflow policy packs and role bindings before a loop runs.
_Avoid_: hidden prompt behavior

## Relationships

- A **Workflow Policy Pack** contains one or more **Workflow Policies**.
- A **Workflow Policy Pack** can be bound to one or more **Roles**.
- A **Role** can use a **Workflow Policy Pack** while still preserving Looper's lifecycle, safety, disclosure, and completion-marker contracts.
- A **Prompt-Time Policy** is the v1 boundary for **Workflow Policies**.
- A **Policy Pack ID** identifies a **Workflow Policy Pack** in config.
- A **Policy Pack Name** describes a **Workflow Policy Pack** for humans.
- A **Role Policy Binding** selects a whole **Workflow Policy Pack** for a **Role** in v1.
- **Role-Direct Policy Text** is the v1 content format for a **Workflow Policy Pack**.
- **Policy Precedence** lets user/project custom instructions refine a **Workflow Policy Pack** while Looper's lifecycle, safety, disclosure, and completion-marker contracts remain final.
- **Explicit Policy Binding** prevents **Workflow Policy Packs** from changing a **Role** unless the role names the pack.
- **Built-In Policy Packs** and **File Policy Packs** share the same pack schema and validation path.
- **Policy Validation** applies to both **Built-In Policy Packs** and **File Policy Packs**.
- **Policy Visibility** lets users inspect active **Role Policy Bindings** through config, status, or prompt preview commands.

## Example dialogue

> **Dev:** "Can we make the Matt skills replaceable?"
> **Domain expert:** "Yes, but in Looper they are **Workflow Policy Packs**. Users bind a pack to a **Role**, and Looper injects the matching **Workflow Policies** into that role's prompt."

## Flagged ambiguities

- "Matt skills" was used to mean Codex-local skills and Looper runtime behavior. Resolved: Looper should expose replaceable **Workflow Policy Packs**, not depend on Codex skill installation.
- "Pluggable component" could imply runtime plugins. Resolved: v1 **Workflow Policy Packs** are **Prompt-Time Policies** only.
- "name" could mean either a stable config key or a display label. Resolved: use **Policy Pack ID** for stable references and **Policy Pack Name** for human-friendly display.
- "select policies" could mean choosing individual policies from a pack. Resolved: v1 uses whole-pack **Role Policy Binding** only.
- "policy composition" could mean an abstract graph of reusable policy atoms. Resolved: v1 packs use **Role-Direct Policy Text**.
- "override" could imply policy packs outrank project-specific instructions. Resolved: **Policy Precedence** puts pack text before user/project custom instructions, and Looper contracts last.
- "enabled" could imply all roles automatically use a policy pack. Resolved: v1 requires **Explicit Policy Binding** per role.
- "built-in" could imply hardcoded Go prompt strings. Resolved: built-ins should be bundled files loaded like **File Policy Packs**.
- "validation" could mean only JSON shape validation. Resolved: **Policy Validation** also blocks attempts to alter Looper's protected contracts.
- "active policy" should not be hidden inside agent prompts. Resolved: **Policy Visibility** should show enabled packs and role bindings before runs execute.
