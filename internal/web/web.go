// Package web provides optional research tools: search through a self-hosted
// SearXNG and a fetcher hardened against SSRF.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
)

// MaxFetchBytes bounds downloaded pages.
const MaxFetchBytes = 1 << 20

// ErrBlockedAddress is returned for private, loopback or link-local targets.
var ErrBlockedAddress = errors.New("endereço não permitido (rede interna)")

// Provider implements runtime.WebProvider.
type Provider struct {
	SearXNGURL string
	AllowHTTP  bool
	// AllowPrivate disables the SSRF guard (tests only).
	AllowPrivate bool
	client       *http.Client
}

// New returns a provider with a guarded HTTP client.
func New(searxngURL string, allowHTTP bool) *Provider {
	p := &Provider{SearXNGURL: strings.TrimRight(searxngURL, "/"), AllowHTTP: allowHTTP}
	p.client = p.newClient()
	return p
}

func (p *Provider) newClient() *http.Client {
	dialer := &net.Dialer{Timeout: 8 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if p.AllowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || blockedIP(ip) {
			return ErrBlockedAddress
		}
		return nil
	}}
	tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 12 * time.Second, MaxIdleConns: 10, Proxy: nil}
	return &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("redirecionamentos demais")
		}
		return p.checkURL(req.URL)
	}}
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64) || // CGNAT 100.64/10
		(ip.To4() != nil && ip.To4()[0] == 169 && ip.To4()[1] == 254) // metadata
}

func (p *Provider) checkURL(u *url.URL) error {
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return errors.New("só links https são permitidos")
		}
	default:
		return errors.New("só links http(s) são permitidos")
	}
	if u.User != nil {
		return errors.New("links com credenciais não são permitidos")
	}
	return nil
}

// Search queries SearXNG (JSON API).
func (p *Provider) Search(ctx context.Context, query string, max int) ([]runtime.WebResult, error) {
	if p.SearXNGURL == "" {
		return nil, errors.New("pesquisa não configurada")
	}
	q := url.Values{"q": {query}, "format": {"json"}, "language": {"pt-BR"}, "safesearch": {"1"}}
	req, err := http.NewRequestWithContext(ctx, "GET", p.SearXNGURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	// SearXNG is an internal, trusted service: use a plain client.
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serviço de pesquisa indisponível: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("serviço de pesquisa respondeu %s", resp.Status)
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); err != nil {
		return nil, err
	}
	var res []runtime.WebResult
	for _, r := range out.Results {
		if len(res) >= max {
			break
		}
		res = append(res, runtime.WebResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return res, nil
}

// Fetch downloads a public page and returns readable text.
func (p *Provider) Fetch(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("link inválido")
	}
	if err := p.checkURL(u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AlicerceResearch/1.0)")
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml;q=0.9,*/*;q=0.5")
	if p.client == nil {
		p.client = p.newClient()
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return "", ErrBlockedAddress
		}
		return "", fmt.Errorf("não foi possível abrir a página: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("a página respondeu %s", resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "text/") && !strings.Contains(ct, "xhtml") && !strings.Contains(ct, "json") {
		return "", fmt.Errorf("tipo de conteúdo não suportado: %s", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBytes))
	if err != nil {
		return "", err
	}
	if strings.Contains(ct, "html") || strings.Contains(strings.ToLower(string(body[:min(len(body), 512)])), "<html") {
		return HTMLToText(string(body)), nil
	}
	return string(body), nil
}

var (
	reDrop  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|footer|header|form|iframe)[^>]*>.*?</(script|style|noscript|svg|nav|footer|header|form|iframe)>`)
	reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reBlock = regexp.MustCompile(`(?i)</?(p|div|br|li|h[1-6]|tr|section|article|ul|ol|table)[^>]*>`)
	reTag   = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace = regexp.MustCompile(`[ \t\r\f\v]+`)
	reLines = regexp.MustCompile(`\n\s*\n+`)
)

// HTMLToText is a small, dependency-free readability pass.
func HTMLToText(h string) string {
	title := ""
	if m := reTitle.FindStringSubmatch(h); m != nil {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	s := reDrop.ReplaceAllString(h, " ")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reSpace.ReplaceAllString(s, " ")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	out := reLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	if title != "" {
		out = "# " + title + "\n\n" + out
	}
	return out
}
