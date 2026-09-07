package modelspec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseChainWithoutEfforts(t *testing.T) {
	t.Parallel()

	chain, err := Parse("cerebras/qwen-3,groq/qwen/qwen3-32b,ollama/frob/hy-mt1.5:latest")
	require.NoError(t, err)
	require.Equal(t, Chain{Legs: []Leg{
		{Spec: "cerebras/qwen-3", Effort: ""},
		{Spec: "groq/qwen/qwen3-32b", Effort: ""},
		{Spec: "ollama/frob/hy-mt1.5:latest", Effort: ""},
	}}, chain)
	require.Equal(t, "cerebras/qwen-3,groq/qwen/qwen3-32b,ollama/frob/hy-mt1.5:latest", chain.Model())
}

// The point of keeping the suffix: one chain, a different reasoning budget per
// leg, which smolllm-go's chain-wide WithReasoningEffort cannot express.
func TestParseMixesEffortsWithinOneChain(t *testing.T) {
	t.Parallel()

	chain, err := Parse("groq/openai/gpt-oss-120b!low,gemini/gemini-3.5-flash-lite,gemini/gemini-flash-latest!none")
	require.NoError(t, err)
	require.Equal(t, Chain{Legs: []Leg{
		{Spec: "groq/openai/gpt-oss-120b", Effort: "low"},
		{Spec: "gemini/gemini-3.5-flash-lite", Effort: ""},
		{Spec: "gemini/gemini-flash-latest", Effort: "none"},
	}}, chain)

	// WithModel never sees a suffix: smolllm-go v0.3 sends the wire model verbatim.
	require.Equal(t,
		"groq/openai/gpt-oss-120b,gemini/gemini-3.5-flash-lite,gemini/gemini-flash-latest",
		chain.Model())
}

func TestParseNormalizesWhitespaceAndCase(t *testing.T) {
	t.Parallel()

	chain, err := Parse(" groq/a ! LOW , gemini/b ")
	require.NoError(t, err)
	require.Equal(t, Chain{Legs: []Leg{
		{Spec: "groq/a", Effort: "low"},
		{Spec: "gemini/b", Effort: ""},
	}}, chain)
}

// A wire model name may contain any punctuation, so only the FIRST "!" separates.
func TestParseKeepsModelNamePunctuation(t *testing.T) {
	t.Parallel()

	chain, err := Parse("openrouter/~deepseek/x,bare-model!high")
	require.NoError(t, err)
	require.Equal(t, "openrouter/~deepseek/x,bare-model", chain.Model())
	require.Equal(t, "high", chain.Legs[1].Effort)
}

// smolllm-go keys per-leg overrides by spec, so the same spec cannot run at two
// efforts in one chain. Silently keeping the last would change what the other
// leg does without saying so.
func TestParseRejectsOneSpecWithTwoEfforts(t *testing.T) {
	t.Parallel()

	_, err := Parse("smolayer/antigravity/pro!high,gemini/flash,smolayer/antigravity/pro!low")
	require.Error(t, err)
	require.Contains(t, err.Error(), "smolayer/antigravity/pro")
	require.Contains(t, err.Error(), "two different efforts")
}

// Repeating a spec at the SAME effort is harmless: the override is identical.
func TestParseAllowsRepeatedSpecAtSameEffort(t *testing.T) {
	t.Parallel()

	chain, err := Parse("gemini/flash!none,groq/x,gemini/flash!none")
	require.NoError(t, err)
	require.Len(t, chain.Legs, 3)
	require.Equal(t, "gemini/flash,groq/x,gemini/flash", chain.Model())
}

func TestParseRejectsMalformedChains(t *testing.T) {
	t.Parallel()

	for name, chain := range map[string]string{
		"empty":          "",
		"blank":          "   ",
		"empty entry":    "gemini/flash,,groq/x",
		"trailing comma": "gemini/flash,",
		"no effort":      "gemini/flash!",
		"blank effort":   "gemini/flash!   ",
		"no model":       "!high",
		"two efforts":    "gemini/flash!low!high",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Error(t, Validate(chain))
		})
	}
}

// The live config at ~/.config/smolllm-server/config.yaml, copied verbatim. It
// must keep loading unchanged: efforts mixed within a chain, legs with and
// without a suffix, and one alias whose every leg carries one.
func TestParseLiveConfigAliases(t *testing.T) {
	t.Parallel()

	live := map[string]string{
		"fast":      "groq/openai/gpt-oss-120b!low,smolayer/codex/gpt-5.6-luna!low,gemini/gemini-3.5-flash-lite,omlx/Qwen3.8-27B-4bit,gemini/gemini-flash-latest!none,smolayer/antigravity/gemini-3-flash,deepseek/deepseek-v4-flash",
		"balance":   "omlx/Qwen3.8-27B-8bit,smolayer/codex/gpt-5.6-luna!low,deepseek/deepseek-v4-flash,gemini/gemini-3.5-flash-lite",
		"explain":   "groq/groq/compound,groq/groq/compound-mini,gemini/gemini-3.5-flash-lite,gemini/gemini-flash-latest",
		"translate": "omlx/Hy-MT2-1.8B-oQ6,ollama/frob/hy-mt1.5:latest,omlx/Qwen3.8-27B-8bit,gemini/gemini-flash-lite-latest",
		"complex":   "smolayer/codex/gpt-5.6-sol!high,jake/kimi,smolayer/antigravity/gemini-3.1-pro!high,smolayer/antigravity/gemini-3-flash,gemini/gemini-flash-latest,deepseek/deepseek-v4-pro!none",
		"correct":   "smolayer/codex/gpt-5.6-sol!high,jake/kimi,smolayer/antigravity/gemini-3.1-pro!high,smolayer/antigravity/gemini-3-flash,gemini/gemini-flash-latest,deepseek/deepseek-v4-pro!none",
		"summary":   "smolayer/codex/gpt-5.6-sol!high,jake/kimi,smolayer/antigravity/gemini-3.1-pro!high,smolayer/antigravity/gemini-3-flash,gemini/gemini-flash-latest,deepseek/deepseek-v4-pro!none",
		"vision":    "omlx/Qwen3.8-27B-8bit,smolayer/codex/gpt-5.6-sol!low,smolayer/antigravity/gemini-3-flash,gemini/gemini-flash-latest",
		"genz":      "omlx/genz-writer-v13-clean-8bit,ollama/genz-writer:v13-clean",
		"jake":      "jake/kimi!none,jake/glm!none,jake/minimax",
		"agent":     "smolayer/codex/gpt-5.6-sol!low,deepseek/deepseek-v4-flash!none,groq/openai/gpt-oss-120b!low,gemini/gemini-3.5-flash-lite,omlx/Qwen3.8-27B-8bit",
	}

	for alias, chain := range live {
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			parsed, err := Parse(chain)
			require.NoError(t, err)
			require.NotContains(t, parsed.Model(), EffortSeparator,
				"no effort suffix may survive into the string smolllm-go routes on")
			require.Len(t, parsed.Legs, len(splitCount(chain)))
		})
	}

	// The alias that mixes three states in one chain is the reason the suffix
	// survives at all: two legs at "low", one at "none", four with no override.
	fast, err := Parse(live["fast"])
	require.NoError(t, err)
	require.Equal(t, []Leg{
		{Spec: "groq/openai/gpt-oss-120b", Effort: "low"},
		{Spec: "smolayer/codex/gpt-5.6-luna", Effort: "low"},
		{Spec: "gemini/gemini-3.5-flash-lite", Effort: ""},
		{Spec: "omlx/Qwen3.8-27B-4bit", Effort: ""},
		{Spec: "gemini/gemini-flash-latest", Effort: "none"},
		{Spec: "smolayer/antigravity/gemini-3-flash", Effort: ""},
		{Spec: "deepseek/deepseek-v4-flash", Effort: ""},
	}, fast.Legs)
}

// splitCount counts the comma-separated entries of a chain, so the leg count is
// checked against the raw string rather than against the parser's own output.
func splitCount(chain string) []string {
	count := []string{""}
	for _, char := range chain {
		if char == ',' {
			count = append(count, "")
		}
	}
	return count
}
