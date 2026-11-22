package repoctx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
)

// GitHubSource lists repositories of an organization by prefix.
type GitHubSource struct {
	APIURL     string
	Token      string
	Org        string
	HTTPClient *http.Client
}

func (g *GitHubSource) Name() string { return "github" }

func (g *GitHubSource) get(ctx context.Context, p string, out any) error {
	api := strings.TrimRight(g.APIURL, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", api+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "alicerce-repoctx")
	c := g.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return workspace.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("github: %d %s", resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHubSource) List(ctx context.Context, sigla string) ([]System, error) {
	prefix := strings.ToLower(g.Org) + "-"
	if sigla != "" {
		prefix += strings.ToLower(sigla) + "-"
	}
	var out []System
	for page := 1; page <= 10; page++ {
		var res struct {
			Items []struct {
				Name          string `json:"name"`
				Description   string `json:"description"`
				PushedAt      string `json:"pushed_at"`
				DefaultBranch string `json:"default_branch"`
			} `json:"items"`
		}
		q := url.Values{"q": {prefix + " in:name org:" + g.Org}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := g.get(ctx, "/search/repositories?"+q.Encode(), &res); err != nil {
			return nil, err
		}
		for _, r := range res.Items {
			if s, ok := ParseName(g.Org, r.Name); ok && strings.HasPrefix(strings.ToLower(r.Name), prefix) {
				s.Description, s.Head = r.Description, r.PushedAt
				out = append(out, s)
			}
		}
		if len(res.Items) < 100 {
			break
		}
	}
	return out, nil
}

func (g *GitHubSource) File(ctx context.Context, sys System, p string) ([]byte, error) {
	var f struct {
		Content string `json:"content"`
	}
	if err := g.get(ctx, "/repos/"+g.Org+"/"+sys.Name+"/contents/"+p, &f); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

func (g *GitHubSource) Tree(ctx context.Context, sys System) ([]string, error) {
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := g.get(ctx, "/repos/"+g.Org+"/"+sys.Name+"/git/trees/HEAD?recursive=1", &t); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range t.Tree {
		if e.Type == "blob" {
			out = append(out, e.Path)
		}
	}
	return out, nil
}
