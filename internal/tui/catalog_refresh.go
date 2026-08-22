package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/catalog"
	"qeuro/internal/client"
)

// Roadmap §8 "Startup" wants the model catalogue to come from the backend without
// a request in front of the prompt. The cache half lives in internal/catalog; this
// is the part that schedules the refresh — after the first frame, never before it.

// catalogMsg reports a background catalogue refresh.
//
// quiet mirrors providersMsg: a refresh nobody asked for must not put an error on
// screen. A backend that is down is not something the user needs to hear about
// while typing, because the compiled-in catalogue still works.
type catalogMsg struct {
	changed bool
	quiet   bool
	err     error
}

// catalogFetcher adapts *client.Client to catalog.Fetcher.
//
// The adapter exists so internal/catalog does not import internal/client: catalog
// is imported by nearly everything and client pulls in net/http, so the dependency
// would run the wrong way. It also converts between the two Model shapes — the
// client speaks the wire's []string efforts, the catalogue its own typed ones — and
// this seam is the only place that has to know both.
type catalogFetcher struct{ cli *client.Client }

func (f catalogFetcher) Fetch(ctx context.Context, etag string) (catalog.Document, bool, string, error) {
	res, err := f.cli.ModelsWithETag(ctx, etag)
	if err != nil {
		return catalog.Document{}, false, "", err
	}
	if res.NotModified {
		return catalog.Document{}, false, res.ETag, nil
	}
	doc := catalog.Document{Models: make([]catalog.DocumentModel, 0, len(res.Models))}
	for _, m := range res.Models {
		doc.Models = append(doc.Models, catalog.DocumentModel{
			Brand:   m.Brand,
			ID:      m.ID,
			Label:   m.Label,
			Note:    m.Note,
			Efforts: m.Efforts,
		})
	}
	return doc, true, res.ETag, nil
}

// refreshCatalog is the quiet startup variant: it runs after the prompt is drawn,
// so it costs nothing before it, and stays silent on failure.
func refreshCatalog(cli *client.Client) tea.Cmd {
	return catalogCmd(cli, true)
}

// catalogCmd revalidates the cached catalogue against the backend.
func catalogCmd(cli *client.Client, quiet bool) tea.Cmd {
	if cli == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
		defer cancel()
		_, changed, err := catalog.Refresh(ctx, catalogFetcher{cli: cli})
		return catalogMsg{changed: changed, quiet: quiet, err: err}
	}
}
