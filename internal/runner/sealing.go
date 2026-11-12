package runner

import (
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/seal"
	"github.com/CrossTerra/beacon/internal/source"
)

// sealing is the level a send or hold item leaves at, and why.
type sealing struct {
	// Level is the level after the raise and the floor; None is plain.
	Level seal.Level
	// Rule is the rule's own level.
	Rule seal.Level
	// Raised says what raised it above the rule's level. It is for explain only: it can name a matched word.
	Raised []string
	// Held is why it is held instead: body_needs_seal or sensitive_shared.
	Held logs.Reason
}

// sealing decides an item's level after a rule decided send or hold. It starts at the rule's seal and can only go up:
// to owner when seal.owner matches what the source read on the machine (DEC-33), when its meeting was sent at owner
// (DEC-35), and to the subject's floor (DEC-34). It never unseals, lowers, adds a field or revives never. Signal seals
// only a person's own subjects, so an item on a shared subject that would be sealed is held as sensitive_shared; a
// personal body that would leave plain is held as body_needs_seal (DEC-32).
func (r *Runner) sealing(it source.Item, rule *rules.Rule) sealing {
	s := sealing{Rule: seal.Level(rule.Seal)}
	s.Level = s.Rule
	personal := source.PersonalStem(it.Subject) != ""
	if why := r.SealOwner.Match(it.Local); why != "" && s.Level != seal.Owner {
		s.Raised = append(s.Raised, "seal.owner: "+why)
		if !personal {
			s.Level, s.Held = seal.None, logs.ReasonSensitiveShared
			return s
		}
		s.Level = seal.Owner
	}
	if it.Local != nil && it.Local.Meeting != "" && s.Level != seal.Owner && r.Floor.Get(it.Local.Meeting) == seal.Owner {
		s.Level = seal.Owner
		s.Raised = append(s.Raised, "its meeting was sent at owner (DEC-35)")
	}
	if f := r.Floor.Get(it.Subject); f.Rank() > s.Level.Rank() {
		s.Level = f
		s.Raised = append(s.Raised, "the subject was sent at "+string(f)+" before (DEC-34)")
	}
	if s.Level != seal.None && !personal {
		s.Level, s.Held = seal.None, logs.ReasonSensitiveShared
		return s
	}
	if s.Level == seal.None && project.NeedsSeal(it, rule) {
		s.Held = logs.ReasonBodyNeedsSeal
	}
	return s
}

// logSeal logs what this pass sealed, raised and held for sealing: counts and levels only, never why (AGENTS.md).
func (r *Runner) logSeal() {
	c := r.sealCounts
	if c.owner+c.agents > 0 {
		r.Log.Log(logs.Sealed, logs.Count(seal.Owner.LogName(), c.owner), logs.Count(seal.Agents.LogName(), c.agents))
	}
	if c.raised > 0 {
		r.Log.Log(logs.Raised, logs.Count("items", c.raised))
	}
	for _, reason := range []logs.Reason{logs.ReasonBodyNeedsSeal, logs.ReasonSensitiveShared} {
		if n := c.held[reason]; n > 0 {
			r.Log.Log(logs.SealHeld, logs.Why(reason), logs.Count("items", n))
		}
	}
}
