package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/store"
)

// OIDC verifies bearer JWTs issued by an identity provider (Cognito, corporate
// SSO, Supabase Auth with asymmetric keys, Keycloak...) and provisions users
// on first access. Login itself happens at the IdP or at a proxy in front of
// the app (e.g. an AWS ALB with authenticate-cognito).
type OIDC struct {
	Store          *store.Store
	Issuer         string
	Audience       string
	JWKSURL        string
	EmailClaim     string
	NameClaim      string
	AllowedDomains []string
	// TokenHeader is read before Authorization (e.g. "X-Amzn-Oidc-Data" behind an ALB).
	TokenHeader string
	// ALBRegion enables the ALB key endpoint format (PEM per kid).
	ALBRegion  string
	HTTPClient *http.Client

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

func (o *OIDC) Mode() string { return "oidc" }

func (o *OIDC) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (o *OIDC) User(r *http.Request) (*store.User, error) {
	tok := ""
	if o.TokenHeader != "" {
		tok = r.Header.Get(o.TokenHeader)
	}
	if tok == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if tok == "" {
		return nil, ErrUnauthenticated
	}
	claims, err := o.Verify(r.Context(), tok)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	email, _ := claims[o.claim(o.EmailClaim, "email")].(string)
	email = store.NormalizeEmail(email)
	if email == "" {
		return nil, ErrUnauthenticated
	}
	if !DomainAllowed(email, o.AllowedDomains) {
		return nil, ErrDomainNotAllowed
	}
	ctx := r.Context()
	u, err := o.Store.GetUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		name, _ := claims[o.claim(o.NameClaim, "name")].(string)
		if name == "" {
			name = strings.Split(email, "@")[0]
		}
		sub, _ := claims["sub"].(string)
		u = &store.User{Email: email, Name: name, Role: store.RoleMember, ExternalID: sub}
		if n, _ := o.Store.CountUsers(ctx); n == 0 {
			u.Role = store.RoleAdmin
		}
		if err := o.Store.CreateUser(ctx, u); err != nil && !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
		u, err = o.Store.GetUserByEmail(ctx, email)
	}
	if err != nil {
		return nil, err
	}
	if u.Status != store.StatusActive {
		return nil, ErrUnauthenticated
	}
	return u, nil
}

func (o *OIDC) claim(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Verify checks signature, issuer, audience and time claims.
func (o *OIDC) Verify(ctx context.Context, tok string) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt malformado")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSeg(parts[0], &hdr); err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := decodeSeg(parts[1], &claims); err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil {
		return nil, err
	}
	key, err := o.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch hdr.Alg {
	case "RS256":
		pk, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(pk, crypto.SHA256, digest[:], sig) != nil {
			return nil, errors.New("assinatura inválida")
		}
	case "ES256":
		pk, ok := key.(*ecdsa.PublicKey)
		if !ok || len(sig) != 64 {
			return nil, errors.New("assinatura inválida")
		}
		rr, ss := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pk, digest[:], rr, ss) {
			return nil, errors.New("assinatura inválida")
		}
	default:
		return nil, fmt.Errorf("algoritmo não suportado: %s", hdr.Alg)
	}
	now := time.Now().Unix()
	if exp, ok := num(claims["exp"]); !ok || now > exp+60 {
		return nil, errors.New("token expirado")
	}
	if nbf, ok := num(claims["nbf"]); ok && now+60 < nbf {
		return nil, errors.New("token ainda não válido")
	}
	if o.Issuer != "" && claims["iss"] != o.Issuer {
		return nil, errors.New("emissor inválido")
	}
	if o.Audience != "" && !audOK(claims, o.Audience) {
		return nil, errors.New("audiência inválida")
	}
	return claims, nil
}

func audOK(claims map[string]any, want string) bool {
	switch v := claims["aud"].(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if a == want {
				return true
			}
		}
	}
	// Cognito access tokens carry client_id instead of aud.
	return claims["client_id"] == want
}

func num(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func decodeSeg(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (o *OIDC) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if k, ok := o.keys[kid]; ok && time.Since(o.fetched) < time.Hour {
		return k, nil
	}
	if o.keys == nil {
		o.keys = map[string]crypto.PublicKey{}
	}
	if o.ALBRegion != "" {
		u := fmt.Sprintf("https://public-keys.auth.elb.%s.amazonaws.com/%s", o.ALBRegion, kid)
		body, err := o.get(ctx, u)
		if err != nil {
			return nil, err
		}
		blk, _ := pem.Decode(body)
		if blk == nil {
			return nil, errors.New("chave pública inválida")
		}
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		o.keys[kid] = k
		return k, nil
	}
	jwks := o.JWKSURL
	if jwks == "" {
		disc, err := o.get(ctx, strings.TrimRight(o.Issuer, "/")+"/.well-known/openid-configuration")
		if err != nil {
			return nil, err
		}
		var d struct {
			JWKS string `json:"jwks_uri"`
		}
		if err := json.Unmarshal(disc, &d); err != nil || d.JWKS == "" {
			return nil, errors.New("descoberta OIDC sem jwks_uri")
		}
		o.JWKSURL, jwks = d.JWKS, d.JWKS
	}
	body, err := o.get(ctx, jwks)
	if err != nil {
		return nil, err
	}
	keys, err := ParseJWKS(body)
	if err != nil {
		return nil, err
	}
	o.keys, o.fetched = keys, time.Now()
	k, ok := keys[kid]
	if !ok {
		return nil, errors.New("chave do token desconhecida")
	}
	return k, nil
}

func (o *OIDC) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ParseJWKS parses RSA and P-256 keys from a JWKS document.
func ParseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var doc struct {
		Keys []struct {
			Kid, Kty, N, E, Crv, X, Y string
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := map[string]crypto.PublicKey{}
	b64 := func(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); return b }
	for _, k := range doc.Keys {
		switch k.Kty {
		case "RSA":
			e := new(big.Int).SetBytes(b64(k.E))
			out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(b64(k.N)), E: int(e.Int64())}
		case "EC":
			if k.Crv == "P-256" {
				out[k.Kid] = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(b64(k.X)), Y: new(big.Int).SetBytes(b64(k.Y))}
			}
		}
	}
	return out, nil
}
