// Package auth authenticates people: local accounts (bcrypt + server-side
// sessions + invites), OIDC bearer tokens (JWKS) or a dev identity.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// CookieName holds the opaque session token.
const CookieName = "alicerce_session"

// Errors.
var (
	ErrInvalidCredentials = errors.New("E-mail ou senha incorretos.")
	ErrUnauthenticated    = errors.New("Faça login para continuar.")
	ErrWeakPassword       = errors.New("A senha precisa ter pelo menos 10 caracteres.")
	ErrDomainNotAllowed   = errors.New("Este e-mail não pertence a um domínio autorizado.")
)

// Authenticator resolves the user of a request.
type Authenticator interface {
	Mode() string
	User(r *http.Request) (*store.User, error)
}

// HashToken returns the SHA-256 hex of a secret token (only hashes are stored).
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// NewToken returns a random URL-safe secret.
func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashPassword hashes with bcrypt.
func HashPassword(pw string) (string, error) {
	if len([]rune(pw)) < 10 {
		return "", ErrWeakPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// dummyHash keeps timing similar when the user does not exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

// --- local ------------------------------------------------------------------------

// Local is email + password with server-side sessions.
type Local struct {
	Store  *store.Store
	TTL    time.Duration
	Secure bool
}

func (l *Local) Mode() string { return "local" }

// Login verifies credentials and creates a session cookie.
func (l *Local) Login(ctx context.Context, w http.ResponseWriter, email, password, userAgent string) (*store.User, error) {
	u, err := l.Store.GetUserByEmail(ctx, email)
	if err != nil || u.PasswordHash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil || u.Status != store.StatusActive {
		return nil, ErrInvalidCredentials
	}
	if err := l.StartSession(ctx, w, u, userAgent); err != nil {
		return nil, err
	}
	return u, nil
}

// StartSession issues a cookie for a user.
func (l *Local) StartSession(ctx context.Context, w http.ResponseWriter, u *store.User, userAgent string) error {
	tok := NewToken()
	ttl := l.TTL
	if ttl == 0 {
		ttl = 7 * 24 * time.Hour
	}
	if err := l.Store.CreateSession(ctx, HashToken(tok), u.ID, time.Now().Add(ttl), userAgent); err != nil {
		return err
	}
	_ = l.Store.TouchLogin(ctx, u.ID)
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: tok, Path: "/", HttpOnly: true, Secure: l.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())})
	return nil
}

// Logout deletes the session.
func (l *Local) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		_ = l.Store.DeleteSession(ctx, HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: l.Secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (l *Local) User(r *http.Request) (*store.User, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, ErrUnauthenticated
	}
	u, err := l.Store.SessionUser(r.Context(), HashToken(c.Value))
	if err != nil {
		return nil, ErrUnauthenticated
	}
	return u, nil
}

// --- dev ----------------------------------------------------------------------------

// Dev authenticates everyone as one admin (local demos and E2E only).
type Dev struct{ Store *store.Store }

func (d *Dev) Mode() string { return "dev" }

const devEmail = "pessoa.demo@acme.example"

func (d *Dev) User(r *http.Request) (*store.User, error) {
	ctx := r.Context()
	u, err := d.Store.GetUserByEmail(ctx, devEmail)
	if errors.Is(err, store.ErrNotFound) {
		u = &store.User{Email: devEmail, Name: "Pessoa de Demonstração", Role: store.RoleAdmin}
		if err := d.Store.CreateUser(ctx, u); err != nil && !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
		return d.Store.GetUserByEmail(ctx, devEmail)
	}
	return u, err
}

// DomainAllowed checks an email against an allow list (empty = all).
func DomainAllowed(email string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	dom := strings.ToLower(email[at+1:])
	for _, d := range domains {
		if strings.EqualFold(strings.TrimSpace(d), dom) {
			return true
		}
	}
	return false
}
