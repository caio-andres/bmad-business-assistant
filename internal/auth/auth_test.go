package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/store"
)

func newStore(t *testing.T) *store.Store {
	s, err := store.Open(context.Background(), "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLocalLoginLogout(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if _, err := HashPassword("curta"); err != ErrWeakPassword {
		t.Fatal("weak password accepted")
	}
	h, _ := HashPassword("senha-bem-longa")
	_ = st.CreateUser(ctx, &store.User{Email: "ana@acme.example", Name: "Ana", PasswordHash: h})
	l := &Local{Store: st}
	if _, err := l.Login(ctx, httptest.NewRecorder(), "ana@acme.example", "errada-errada", "t"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := l.Login(ctx, httptest.NewRecorder(), "nao@existe.example", "x", "t"); err != ErrInvalidCredentials {
		t.Fatal("unknown user")
	}
	w := httptest.NewRecorder()
	if _, err := l.Login(ctx, w, "ANA@acme.example", "senha-bem-longa", "t"); err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: %+v", cookie)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	if u, err := l.User(r); err != nil || u.Name != "Ana" {
		t.Fatalf("user: %v", err)
	}
	l.Logout(ctx, httptest.NewRecorder(), r)
	if _, err := l.User(r); err == nil {
		t.Fatal("session survived logout")
	}
}

func signJWT(t *testing.T, alg, kid string, key crypto.Signer, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	head := enc(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		r, s, _ := ecdsa.Sign(rand.Reader, k, digest[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestOIDCVerifyAndProvision(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	jwks := fmt.Sprintf(`{"keys":[{"kid":"r1","kty":"RSA","n":%q,"e":%q},{"kid":"e1","kty":"EC","crv":"P-256","x":%q,"y":%q}]}`,
		b64(rk.N.Bytes()), b64(big.NewInt(int64(rk.E)).Bytes()), b64(ek.X.FillBytes(make([]byte, 32))), b64(ek.Y.FillBytes(make([]byte, 32))))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(jwks)) }))
	defer srv.Close()

	st := newStore(t)
	o := &OIDC{Store: st, Issuer: "https://idp.acme.example", Audience: "alicerce", JWKSURL: srv.URL, AllowedDomains: []string{"acme.example"}}
	now := time.Now().Unix()
	good := map[string]any{"iss": "https://idp.acme.example", "aud": "alicerce", "exp": now + 300, "email": "Carla@Acme.example", "name": "Carla", "sub": "abc"}
	for _, c := range []struct {
		alg, kid string
		key      crypto.Signer
	}{{"RS256", "r1", rk}, {"ES256", "e1", ek}} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+signJWT(t, c.alg, c.kid, c.key, good))
		u, err := o.User(r)
		if err != nil || u.Email != "carla@acme.example" {
			t.Fatalf("%s: %v %+v", c.alg, err, u)
		}
		if !u.IsAdmin() {
			t.Fatalf("first provisioned user should be admin")
		}
	}
	bad := []map[string]any{
		{"iss": "https://evil.example", "aud": "alicerce", "exp": now + 300, "email": "x@acme.example"},
		{"iss": "https://idp.acme.example", "aud": "outro", "exp": now + 300, "email": "x@acme.example"},
		{"iss": "https://idp.acme.example", "aud": "alicerce", "exp": now - 3600, "email": "x@acme.example"},
		{"iss": "https://idp.acme.example", "aud": "alicerce", "exp": now + 300, "email": "x@gmail.com"},
	}
	for i, cl := range bad {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+signJWT(t, "RS256", "r1", rk, cl))
		if _, err := o.User(r); err == nil {
			t.Errorf("bad token %d accepted", i)
		}
	}
	// tampered signature
	tok := signJWT(t, "RS256", "r1", rk, good)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok[:len(tok)-4]+"AAAA")
	if _, err := o.User(r); err == nil {
		t.Fatal("tampered token accepted")
	}
}
