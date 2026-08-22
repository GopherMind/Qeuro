// Package catalog is the curated list of models exposed through the backend
// proxy and direct provider integrations.
// It is intentionally hand-picked: only strong, current models are listed,
// grouped by brand so the UI can offer a brand → model drill-down.
package catalog

// Effort is a reasoning-effort level. Not every model supports every level;
// each Model declares the subset it accepts.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
)

// EffortNote is a short human description per level (for the effort selector).
var EffortNote = map[Effort]string{
	EffortLow:    "fast and cheap, simple edits",
	EffortMedium: "balanced, everyday tasks",
	EffortHigh:   "deeper reasoning, complex tasks",
	EffortXHigh:  "maximum reasoning, architecture",
}

// Model is one selectable model.
type Model struct {
	ID      string   // provider/catalog model id, e.g. "anthropic/claude-opus-4.8"
	Label   string   // short display name, e.g. "Opus 4.8"
	Note    string   // one-line description
	Efforts []Effort // supported effort levels (first = default)
}

// DefaultEffort returns the model's recommended effort (first in the list).
func (m Model) DefaultEffort() Effort {
	if len(m.Efforts) == 0 {
		return EffortMedium
	}
	return m.Efforts[0]
}

// Supports reports whether the model accepts the given effort.
func (m Model) Supports(e Effort) bool {
	for _, x := range m.Efforts {
		if x == e {
			return true
		}
	}
	return false
}

// Brand groups models from one provider.
type Brand struct {
	Key    string // stable key, e.g. "anthropic"
	Name   string // display name, e.g. "Anthropic"
	Models []Model
}

// reasoning is the common effort set for reasoning-capable models.
var reasoning = []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}

// brands is the curated catalogue. Weak/legacy models are deliberately omitted.
var brands = []Brand{
	{
		Key:  "anthropic",
		Name: "Anthropic",
		Models: []Model{
			{"anthropic/claude-opus-4.8", "Opus 4.8", "best for architecture and complex code", reasoning},
			{"anthropic/claude-sonnet-4.6", "Sonnet 4.6", "best price/quality balance", reasoning},
			{"anthropic/claude-fable-5", "Claude Fable 5", "fast recent Anthropic model", reasoning},
			{"anthropic/claude-haiku-4.5", "Haiku 4.5", "fast and cheap", []Effort{EffortLow, EffortMedium}},
		},
	},
	{
		Key:  "openai",
		Name: "OpenAI",
		Models: []Model{
			{"openai/gpt-5.5", "GPT-5.5", "top-tier all-rounder", reasoning},
			{"openai/gpt-5.4", "GPT-5.4", "strong, slightly cheaper than 5.5", reasoning},
			{"openai/gpt-5.4-mini", "GPT-5.4 Mini", "fast and cheap", []Effort{EffortLow, EffortMedium, EffortHigh}},
		},
	},
	{
		Key:  "qwen",
		Name: "Qwen",
		Models: []Model{
			{"qwen/qwen3.7-max", "Qwen3.7 Max", "Qwen flagship ($1.25/$3.75)", reasoning},
			{"qwen/qwen3.7-plus", "Qwen3.7 Plus", "workhorse ($0.28/$1.20)", []Effort{EffortLow, EffortMedium, EffortHigh}},
			{"qwen/qwen3-235b-a22b-2507", "Qwen3 235B", "235B brains for pennies", []Effort{EffortLow, EffortMedium}},
		},
	},
	{
		Key:  "zai",
		Name: "Z.AI (GLM)",
		Models: []Model{
			{"z-ai/glm-5.2", "GLM 5.2", "latest GLM flagship", reasoning},
			{"z-ai/glm-5.1", "GLM 5.1", "strong and affordable", []Effort{EffortLow, EffortMedium, EffortHigh}},
			{"z-ai/glm-4.7-flash", "GLM 4.7 Flash", "very cheap", []Effort{EffortLow, EffortMedium}},
		},
	},
	{
		Key:  "deepseek",
		Name: "DeepSeek",
		Models: []Model{
			{"deepseek/deepseek-v4-pro", "DeepSeek V4 Pro", "strong at code", reasoning},
			{"deepseek/deepseek-v4-flash", "DeepSeek V4 Flash", "fresh and fast, 1M context", []Effort{EffortLow, EffortMedium}},
			{"deepseek/deepseek-r1", "DeepSeek R1", "low-cost reasoning", []Effort{EffortMedium, EffortHigh}},
		},
	},
	{
		Key:  "moonshot",
		Name: "MoonshotAI",
		Models: []Model{
			{"moonshotai/kimi-k2.7-code", "Kimi K2.7 Code", "big context, code specialist", reasoning},
		},
	},
	{
		Key:  "minimax",
		Name: "MiniMax",
		Models: []Model{
			{"minimax/minimax-m3", "MiniMax M3", "recent multimodal", []Effort{EffortLow, EffortMedium, EffortHigh}},
		},
	},
	{
		Key:  "mimo",
		Name: "Xiaomi MiMo",
		Models: []Model{
			{"xiaomi/mimo-v2.5-pro", "MiMo v2.5 Pro", "improved MiMo", reasoning},
			{"xiaomi/mimo-v2.5", "MiMo v2.5", "compact and quick", []Effort{EffortLow, EffortMedium, EffortHigh}},
		},
	},
	{Key: "free", Name: "NVIDIA Free"},
}

var nvidiaFreeModels = []Model{
	{"deepseek-ai/deepseek-v4-flash", "DeepSeek V4 Flash", "NVIDIA free endpoint, 1M context, coding and agents", []Effort{EffortLow, EffortMedium, EffortHigh}},
	{"deepseek-ai/deepseek-v4-pro", "DeepSeek V4 Pro", "NVIDIA free endpoint, stronger coding model", reasoning},
	{"zai-org/glm-5.1", "GLM 5.1", "NVIDIA free endpoint, strong coding and agents", reasoning},
	{"nvidia/nemotron-3-super-120b-a12b", "Nemotron 3 Super 120B", "NVIDIA free endpoint, strong agentic reasoning", reasoning},
	{"moonshotai/kimi-k2.6", "Kimi K2.6", "NVIDIA free endpoint, long-horizon coding and agents", reasoning},
	{"qwen/qwen3.5-122b-a10b", "Qwen3.5 122B", "NVIDIA free endpoint, coding and reasoning", reasoning},
	{"meta/llama-3.3-70b-instruct", "Llama 3.3 70B", "NVIDIA free endpoint, general reasoning and function calling", []Effort{EffortLow, EffortMedium, EffortHigh}},
	{"google/gemma-4-31b-it", "Gemma 4 31B", "NVIDIA free endpoint, light and fast general model", []Effort{EffortLow, EffortMedium}},
}

// Brands returns the full curated catalogue.
func Brands() []Brand {
	out := make([]Brand, len(brands))
	copy(out, brands)
	for i := range out {
		if out[i].Key == "free" {
			out[i].Name = "NVIDIA Free"
			out[i].Models = nvidiaFreeModels
			return out
		}
	}
	return append(out, Brand{Key: "free", Name: "NVIDIA Free", Models: nvidiaFreeModels})
}

// FindModel returns the model with the given id, if present.
//
// It searches the active catalogue (see Current), so a model the backend added
// after this binary was built resolves once the cache has been refreshed. Without
// that, a cached id would render in the selector and then fail to be selectable.
func FindModel(id string) (Model, Brand, bool) {
	for _, b := range Current() {
		for _, m := range b.Models {
			if m.ID == id {
				return m, b, true
			}
		}
	}
	return Model{}, Brand{}, false
}

// defaultModelID is the model the app starts on — a strong, cheap general-purpose
// workhorse.
const defaultModelID = "qwen/qwen3.7-plus"

// Default returns the model the app starts on.
//
// The fallback matters: the default is looked up by id, and the active catalogue
// may be a cached one that no longer lists it. Returning a zero Model there would
// start the session on a model with an empty id, which every request would reject.
func Default() Model {
	if m, _, ok := FindModel(defaultModelID); ok {
		return m
	}
	for _, b := range brands {
		for _, m := range b.Models {
			if m.ID == defaultModelID {
				return m
			}
		}
	}
	// Neither catalogue lists it: take the first model the active catalogue offers
	// rather than nothing at all.
	for _, b := range Current() {
		if len(b.Models) > 0 {
			return b.Models[0]
		}
	}
	return Model{}
}
