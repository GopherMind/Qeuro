package skills

import "testing"

// TestLoadAndFind loads the real library (discovered by walking up to the repo's
// backend/officialskills_skills folder) and checks indexing + search work.
func TestLoadAndFind(t *testing.T) {
	lib, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if lib.Count() == 0 {
		t.Skip("skills library not found from this working directory; nothing to test")
	}
	t.Logf("loaded %d skills from %s", lib.Count(), lib.Dir())

	// accessibility.md is a known skill with rich front-matter.
	hits := lib.Find("accessibility a11y wcag", 5)
	if len(hits) == 0 {
		t.Fatalf("Find returned no matches for an accessibility query")
	}
	// Its body should be loadable and non-empty, with front-matter stripped.
	body, err := lib.Body(hits[0].Name)
	if err != nil {
		t.Fatalf("Body(%q): %v", hits[0].Name, err)
	}
	if len(body) == 0 {
		t.Fatalf("Body(%q) is empty", hits[0].Name)
	}
	if body[:3] == "---" {
		t.Fatalf("front-matter was not stripped from body")
	}
}

func TestFindRanking(t *testing.T) {
	lib := &Library{
		byName: map[string]Skill{},
		skills: []Skill{
			{Name: "go-rest-api", Description: "Build REST APIs in Go with net/http"},
			{Name: "tailwind-landing", Description: "Design landing pages with Tailwind CSS"},
			{Name: "postgres-schema", Description: "Model relational schemas in PostgreSQL"},
		},
	}
	for _, s := range lib.skills {
		lib.byName[s.Name] = s
	}
	hits := lib.Find("go rest api", 3)
	if len(hits) == 0 || hits[0].Name != "go-rest-api" {
		t.Fatalf("expected go-rest-api first, got %+v", hits)
	}
}
