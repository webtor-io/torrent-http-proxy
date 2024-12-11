package services_test

import (
	"net/url"
	"os"
	"testing"

	"github.com/urfave/cli"
	s "github.com/webtor-io/torrent-http-proxy/services"
)

func TestUrlParse(t *testing.T) {
	app := cli.NewApp()
	app.Action = func(c *cli.Context) error {
		config, err := s.LoadServicesConfigFromYAML(c)
		if err != nil {
			return err
		}

		p := s.NewURLParser(config)
		u, _ := url.Parse("https://example.com/935d59df63e6b94305b5e2a32cdfd00488f1b055/%5BErai-raws%5D%20One%20Piece%20-%20401~500%20%5B1080p%5D%5BMultiple%20Subtitle%5D~arch/[Erai-raws]%20One%20Piece%20-%20401~500%20[1080p][Multiple%20Subtitle].zip~dp/[Erai-raws]%20One%20Piece%20-%20401~500%20[1080p][Multiple%20Subtitle].zip?user-id=32d150920bdb5ff511697f28b3437bf9&download-id=87275499d2e74a5257c810c2cb8085c1&token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhZ2VudCI6Ik1vemlsbGEvNS4wIChNYWNpbnRvc2g7IEludGVsIE1hYyBPUyBYIDEwXzE1XzcpIEFwcGxlV2ViS2l0LzUzNy4zNiAoS0hUTUwsIGxpa2UgR2Vja28pIENocm9tZS85MS4wLjQ0NzIuNzcgU2FmYXJpLzUzNy4zNiIsInJlbW90ZUFkZHJlc3MiOiI0Ni4xNjAuMjU1LjE5NyIsImRvbWFpbiI6IndlYnRvci5pbyIsImV4cCI6MTYyMzY4NTg1Nywic2Vzc2lvbklEIjoiU0VjYzcyck5KWFlRcS1UbUJaRkdxWkZjcUpJRlJXMDYiLCJyYXRlIjoiMTBNIiwicm9sZSI6Im5vYm9keSJ9.6UfWJa6vDZrqbwBlfq96_PUV3LZvodpkjhNFnZrE9r0&api-key=8acbcf1e-732c-4574-a3bf-27e6a85b86f1")
		src, _ := p.Parse(u)
		if src.Mod == nil {
			t.Fatalf("Got empty mod")
		}
		if src.Mod.Name != "download-progress" {
			t.Fatalf("Expected %v got %v", "download-progress", src.Mod.Name)
		}
		u, _ = url.Parse("https://example.com/08ada5a7a6183aae1e09d831df6748d566095a10/~tc/completed_pieces?download-id=812a10f280c6348bdd630f6a38e65fb6&token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhZ2VudCI6Ik1vemlsbGEvNS4wIChNYWNpbnRvc2g7IEludGVsIE1hYyBPUyBYIDEwXzE1XzcpIEFwcGxlV2ViS2l0LzUzNy4zNiAoS0hUTUwsIGxpa2UgR2Vja28pIENocm9tZS85MS4wLjQ0NzIuNzcgU2FmYXJpLzUzNy4zNiIsInJlbW90ZUFkZHJlc3MiOiI0Ni4xNjAuMjU1LjE5NyIsImRvbWFpbiI6IndlYnRvci5pbyIsImV4cCI6MTYyMzY5MzUyMCwic2Vzc2lvbklEIjoiU0VjYzcyck5KWFlRcS1UbUJaRkdxWkZjcUpJRlJXMDYiLCJyYXRlIjoiMTBNIiwicm9sZSI6Im5vYm9keSJ9.4RakJlhLxFPVTjwYlpcYDxR45s4gFFOYok4n8dA5IqI&api-key=8acbcf1e-732c-4574-a3bf-27e6a85b86f1")
		src, _ = p.Parse(u)
		if src.Mod == nil {
			t.Fatalf("Got empty mod")
		}
		if src.Mod.Name != "torrent-web-cache" {
			t.Fatalf("Expected %v got %v", "torrent-web-cache", src.Mod.Name)
		}
		return nil
	}
	args := os.Args[0:1]
	_ = app.Run(args)
}
