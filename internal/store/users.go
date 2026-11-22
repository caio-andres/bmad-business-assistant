package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Roles and statuses.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"

	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// User is a person allowed to use the assistant.
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Name         string     `json:"name"`
	Role         string     `json:"role"`
	Status       string     `json:"status"`
	PasswordHash string     `json:"-"`
	ExternalID   string     `json:"-"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastLoginAt  *time.Time `json:"lastLoginAt,omitempty"`
}

// IsAdmin reports the admin role.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

const userCols = `id, email, name, role, status, password_hash, external_id, created_at, last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var ph, ext sql.NullString
	var created, last sql.NullString
	if err := sc.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Status, &ph, &ext, &created, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.PasswordHash, u.ExternalID = ph.String, ext.String
	u.CreatedAt, u.LastLoginAt = parseTS(created), parseTSPtr(last)
	return &u, nil
}

// NormalizeEmail lowercases and trims an email.
func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// CreateUser inserts a user (email must be unique).
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	u.Email = NormalizeEmail(u.Email)
	if u.Role == "" {
		u.Role = RoleMember
	}
	if u.Status == "" {
		u.Status = StatusActive
	}
	u.CreatedAt = s.now().UTC()
	_, err := s.exec(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		u.ID, u.Email, u.Name, u.Role, u.Status, nullStr(u.PasswordHash), nullStr(u.ExternalID), ts(u.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetUser loads a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	return scanUser(s.row(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// GetUserByEmail loads a user by email.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.row(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, NormalizeEmail(email)))
}

// ListUsers returns all users ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.query(ctx, `SELECT `+userCols+` FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns how many users exist.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.row(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UpdateUser saves name, role, status, password hash and external id.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	res, err := s.exec(ctx, `UPDATE users SET name = ?, role = ?, status = ?, password_hash = ?, external_id = ? WHERE id = ?`,
		u.Name, u.Role, u.Status, nullStr(u.PasswordHash), nullStr(u.ExternalID), u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLogin records a successful login.
func (s *Store) TouchLogin(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, s.stamp(), id)
	return err
}

// --- invites -----------------------------------------------------------------------

// Invite lets an admin onboard someone with a one-time link.
type Invite struct {
	TokenHash string     `json:"-"`
	Email     string     `json:"email"`
	Role      string     `json:"role"`
	CreatedBy string     `json:"createdBy"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// CreateInvite stores an invite (token already hashed by the caller).
func (s *Store) CreateInvite(ctx context.Context, inv *Invite) error {
	inv.Email = NormalizeEmail(inv.Email)
	inv.CreatedAt = s.now().UTC()
	_, err := s.exec(ctx, `INSERT INTO invites (token_hash, email, role, created_by, expires_at, used_at, created_at) VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		inv.TokenHash, inv.Email, inv.Role, nullStr(inv.CreatedBy), ts(inv.ExpiresAt), ts(inv.CreatedAt))
	return err
}

// GetInvite returns a valid (unused, unexpired) invite.
func (s *Store) GetInvite(ctx context.Context, tokenHash string) (*Invite, error) {
	var inv Invite
	var by, exp, used, created sql.NullString
	err := s.row(ctx, `SELECT token_hash, email, role, created_by, expires_at, used_at, created_at FROM invites WHERE token_hash = ?`, tokenHash).
		Scan(&inv.TokenHash, &inv.Email, &inv.Role, &by, &exp, &used, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	inv.CreatedBy, inv.ExpiresAt, inv.UsedAt, inv.CreatedAt = by.String, parseTS(exp), parseTSPtr(used), parseTS(created)
	if inv.UsedAt != nil || s.now().After(inv.ExpiresAt) {
		return nil, ErrNotFound
	}
	return &inv, nil
}

// UseInvite marks an invite as used; it fails if already used.
func (s *Store) UseInvite(ctx context.Context, tokenHash string) error {
	res, err := s.exec(ctx, `UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`, s.stamp(), tokenHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListInvites returns pending invites.
func (s *Store) ListInvites(ctx context.Context) ([]*Invite, error) {
	rows, err := s.query(ctx, `SELECT email, role, created_by, expires_at, created_at FROM invites WHERE used_at IS NULL AND expires_at > ? ORDER BY created_at DESC`, s.stamp())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invite
	for rows.Next() {
		var inv Invite
		var by, exp, created sql.NullString
		if err := rows.Scan(&inv.Email, &inv.Role, &by, &exp, &created); err != nil {
			return nil, err
		}
		inv.CreatedBy, inv.ExpiresAt, inv.CreatedAt = by.String, parseTS(exp), parseTS(created)
		out = append(out, &inv)
	}
	return out, rows.Err()
}

// --- sessions -------------------------------------------------------------------

// CreateSession stores a login session (id already hashed).
func (s *Store) CreateSession(ctx context.Context, idHash, userID string, expires time.Time, userAgent string) error {
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	_, err := s.exec(ctx, `INSERT INTO sessions (id_hash, user_id, expires_at, created_at, user_agent) VALUES (?, ?, ?, ?, ?)`,
		idHash, userID, ts(expires), s.stamp(), userAgent)
	return err
}

// SessionUser returns the active user for a session.
func (s *Store) SessionUser(ctx context.Context, idHash string) (*User, error) {
	u, err := scanUser(s.row(ctx, `SELECT u.id, u.email, u.name, u.role, u.status, u.password_hash, u.external_id, u.created_at, u.last_login_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id_hash = ? AND s.expires_at > ?`, idHash, s.stamp()))
	if err != nil {
		return nil, err
	}
	if u.Status != StatusActive {
		return nil, ErrNotFound
	}
	return u, nil
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteUserSessions logs a user out everywhere.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions deletes expired sessions.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.stamp())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
