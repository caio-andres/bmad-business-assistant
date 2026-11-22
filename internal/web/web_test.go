package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchBlocksInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("segredo interno")) }))
	defer srv.Close()
	p := New("", true)
	for _, u := range []string{srv.URL, "http://169.254.169.254/latest/meta-data/", "http://[::1]:80/", "http://10.0.0.1/"} {
		if _, err := p.Fetch(context.Background(), u); err == nil {
			t.Errorf("%s should be blocked", u)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "ftp://x.example/a", "https://user:pw@example.com/"} {
		if _, err := p.Fetch(context.Background(), u); err == nil || errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s should be refused by scheme/credentials: %v", u, err)
		}
	}
	if _, err := New("", false).Fetch(context.Background(), "http://example.com"); err == nil {
		t.Error("http should be refused when AllowHTTP=false")
	}
}

func TestFetchAndSearch(t *testing.T) {
	page := `<html><head><title>Cashback &amp; Pix</title><style>x{}</style></head><body><nav>menu</nav><h1>Mercado</h1><p>O cashback cresce 30% ao ano.</p><script>alert(1)</script></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[{"title":"A","url":"https://a.example","content":"aa"},{"title":"B","url":"https://b.example","content":"bb"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer srv.Close()
	p := New(srv.URL, true)
	p.AllowPrivate = true
	p.client = p.newClient()
	text, err := p.Fetch(context.Background(), srv.URL+"/artigo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "# Cashback & Pix") || !strings.Contains(text, "cresce 30%") || strings.Contains(text, "alert") || strings.Contains(text, "menu") {
		t.Fatalf("text = %q", text)
	}
	res, err := p.Search(context.Background(), "cashback", 1)
	if err != nil || len(res) != 1 || res[0].URL != "https://a.example" {
		t.Fatalf("search: %v %+v", err, res)
	}
}
