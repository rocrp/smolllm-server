// Package modelspec parses the model strings the server hands to smolllm-go.
//
// A chain is a comma-separated list of `provider/model` or bare `model` entries,
// each optionally suffixed with `!effort`. Everything after the first `/` and
// before the `!` is an opaque wire model name.
//
// The `!effort` suffix is SERVER config syntax, not smolllm-go's: the library
// dropped it in v0.3 so a wire model name could contain any punctuation. The
// server keeps it because a fallback chain wants a different reasoning budget per
// leg, and translates it into one WithLegReasoningEffort option per suffixed leg.
package modelspec

import (
	"fmt"
	"strings"
)

// EffortSeparator introduces a leg's own reasoning effort.
const EffortSeparator = "!"

// Leg is one entry of a fallback chain.
type Leg struct {
	// Spec is the model spec smolllm-go routes on, with no effort suffix.
	Spec string
	// Effort is the leg's own reasoning effort, empty when it named none. The
	// value is lowercased here; which values a provider accepts is smolllm-go's
	// business, and it rejects an unusable one per leg at request time.
	Effort string
}

// Chain is a parsed fallback chain, in the order the legs are tried.
type Chain struct {
	Legs []Leg
}

// Model renders the chain for WithModel: every spec, comma-joined, with the
// effort suffixes taken off.
func (c Chain) Model() string {
	specs := make([]string, 0, len(c.Legs))
	for _, leg := range c.Legs {
		specs = append(specs, leg.Spec)
	}
	return strings.Join(specs, ",")
}

// Parse splits a chain into its legs and their per-leg efforts.
func Parse(chain string) (Chain, error) {
	if strings.TrimSpace(chain) == "" {
		return Chain{}, fmt.Errorf("model must not be empty")
	}

	entries := strings.Split(chain, ",")
	parsed := Chain{Legs: make([]Leg, 0, len(entries))}
	efforts := make(map[string]string, len(entries))

	for _, entry := range entries {
		leg, err := parseLeg(strings.TrimSpace(entry), chain)
		if err != nil {
			return Chain{}, err
		}
		// smolllm-go keys per-leg overrides by spec, so one spec cannot run at two
		// different efforts in the same chain. Rejecting beats keeping the last
		// one and quietly changing what the other leg does.
		if previous, seen := efforts[leg.Spec]; seen && previous != leg.Effort {
			return Chain{}, fmt.Errorf(
				"model %q gives %q two different efforts (%q and %q); "+
					"a spec repeated in one chain must carry the same effort on every leg",
				chain, leg.Spec, previous, leg.Effort)
		}
		efforts[leg.Spec] = leg.Effort
		parsed.Legs = append(parsed.Legs, leg)
	}
	return parsed, nil
}

func parseLeg(entry, chain string) (Leg, error) {
	if entry == "" {
		return Leg{}, fmt.Errorf("model %q has an empty entry in its fallback chain", chain)
	}

	spec, effort, suffixed := strings.Cut(entry, EffortSeparator)
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Leg{}, fmt.Errorf("model entry %q has no model before its %q suffix", entry, EffortSeparator)
	}
	if !suffixed {
		return Leg{Spec: spec, Effort: ""}, nil
	}

	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return Leg{}, fmt.Errorf(
			"model entry %q ends in %q with no effort after it", entry, EffortSeparator)
	}
	if strings.Contains(effort, EffortSeparator) {
		return Leg{}, fmt.Errorf(
			"model entry %q names more than one effort; a leg carries at most one %q suffix",
			entry, EffortSeparator)
	}
	return Leg{Spec: spec, Effort: effort}, nil
}

// Validate reports a malformed chain without keeping the parse.
func Validate(chain string) error {
	_, err := Parse(chain)
	return err
}
