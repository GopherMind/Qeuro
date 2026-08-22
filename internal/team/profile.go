package team

import "strings"

// Profile bounds a team run's cost and quality. Manager roles (planner, critic,
// lead) use a strong model; workers and the tester use a cheaper one. The
// counts cap how much work — and therefore spend — a single run can incur.
//
// The profile is chosen from the user's subscription tier (ProfileForTier):
// free → Econom, pro → Balanced, mid/ultra → Quality. The backend still caps
// per-call spend (e.g. forcing free models when out of credits), so these model
// ids are a request, not a guarantee.
type Profile struct {
	Name string

	ManagerModel  string // planner / critic / lead
	ManagerEffort string
	WorkerModel   string // workers / tester
	WorkerEffort  string

	MaxWorkers     int // hard cap on subtasks dispatched
	CritiqueRounds int // plan critique→revise cycles
	FixRounds      int // test→fix cycles after the first build
}

// Econom is the cheap profile (free tier). Uses fully free ($0) models, so a
// team run costs nothing — which lets it afford a healthy appetite (more
// workers, a fix round) without draining the balance.
var Econom = Profile{
	Name:           "budget",
	ManagerModel:   "deepseek-ai/deepseek-v4-flash",
	ManagerEffort:  "medium",
	WorkerModel:    "nvidia/nemotron-3-super-120b-a12b",
	WorkerEffort:   "medium",
	MaxWorkers:     5,
	CritiqueRounds: 1,
	FixRounds:      2,
}

// Balanced is the default paid profile (pro tier).
var Balanced = Profile{
	Name:           "balanced",
	ManagerModel:   "anthropic/claude-sonnet-4.6",
	ManagerEffort:  "high",
	WorkerModel:    "qwen/qwen3.7-plus",
	WorkerEffort:   "medium",
	MaxWorkers:     5,
	CritiqueRounds: 1,
	FixRounds:      2,
}

// Quality is the top profile (mid / ultra tiers): a strong manager model, more
// workers and more review/fix cycles.
var Quality = Profile{
	Name:           "quality",
	ManagerModel:   "anthropic/claude-opus-4.8",
	ManagerEffort:  "high",
	WorkerModel:    "anthropic/claude-sonnet-4.6",
	WorkerEffort:   "medium",
	MaxWorkers:     8,
	CritiqueRounds: 2,
	FixRounds:      3,
}

// ProfileForTier maps a subscription tier to a run profile.
func ProfileForTier(tier string) Profile {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "ultra", "mid":
		return Quality
	case "pro":
		return Balanced
	default: // free or unknown
		return Econom
	}
}
