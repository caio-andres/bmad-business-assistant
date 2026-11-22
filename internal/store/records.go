package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// --- publications -------------------------------------------------------------------

// Publication records documents sent to the central workspace.
type Publication struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId,omitempty"`
	ProjectID      string    `json:"projectId,omitempty"`
	Branch         string    `json:"branch"`
	PRURL          string    `json:"prUrl,omitempty"`
	CommitSHA      string    `json:"commitSha,omitempty"`
	Files          []string  `json:"files"`
	Status         string    `json:"status"` // published | conflict | failed
	CreatedBy      string    `json:"createdBy,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// AddPublication stores a publication result.
func (s *Store) AddPublication(ctx context.Context, p *Publication) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	p.CreatedAt = s.now().UTC()
	_, err := s.exec(ctx, `INSERT INTO publications (id, conversation_id, project_id, branch, pr_url, commit_sha, files, status, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, nullStr(p.ConversationID), nullStr(p.ProjectID), p.Branch, p.PRURL, p.CommitSHA, jsonText(p.Files), p.Status, nullStr(p.CreatedBy), ts(p.CreatedAt))
	return err
}

// ListPublications returns publications for a project or a conversation.
func (s *Store) ListPublications(ctx context.Context, projectID, convID string) ([]*Publication, error) {
	q := `SELECT id, conversation_id, project_id, branch, pr_url, commit_sha, files, status, created_by, created_at FROM publications WHERE `
	arg := projectID
	if convID != "" {
		q += `conversation_id = ?`
		arg = convID
	} else {
		q += `project_id = ?`
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at DESC LIMIT 100`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Publication
	for rows.Next() {
		var p Publication
		var conv, proj, by, created sql.NullString
		var files string
		if err := rows.Scan(&p.ID, &conv, &proj, &p.Branch, &p.PRURL, &p.CommitSHA, &files, &p.Status, &by, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(files), &p.Files)
		p.ConversationID, p.ProjectID, p.CreatedBy, p.CreatedAt = conv.String, proj.String, by.String, parseTS(created)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// --- usage ----------------------------------------------------------------------------

// UsageRecord is one provider call.
type UsageRecord struct {
	UserID         string
	ConversationID string
	Purpose        string
	Model          string
	InputTokens    int
	OutputTokens   int
	CostUSD        float64
}

// AddUsage stores token usage.
func (s *Store) AddUsage(ctx context.Context, u UsageRecord) error {
	_, err := s.exec(ctx, `INSERT INTO usage_records (id, user_id, conversation_id, purpose, model, input_tokens, output_tokens, cost_usd, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID(), nullStr(u.UserID), nullStr(u.ConversationID), u.Purpose, u.Model, u.InputTokens, u.OutputTokens, u.CostUSD, s.stamp())
	return err
}

// UsageTotals aggregates usage.
type UsageTotals struct {
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	Calls        int     `json:"calls"`
}

// UserUsageSince sums a user's usage since t (daily quota).
func (s *Store) UserUsageSince(ctx context.Context, userID string, since time.Time) (UsageTotals, error) {
	var t UsageTotals
	err := s.row(ctx, `SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*) FROM usage_records WHERE user_id = ? AND created_at >= ?`,
		userID, ts(since)).Scan(&t.InputTokens, &t.OutputTokens, &t.CostUSD, &t.Calls)
	return t, err
}

// UsageByUser is a report row.
type UsageByUser struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	UsageTotals
}

// UsageReport aggregates usage per user since t.
func (s *Store) UsageReport(ctx context.Context, since time.Time) ([]UsageByUser, error) {
	rows, err := s.query(ctx, `SELECT COALESCE(u.id,''), COALESCE(u.name,''), COALESCE(u.email,''), COALESCE(SUM(r.input_tokens),0), COALESCE(SUM(r.output_tokens),0), COALESCE(SUM(r.cost_usd),0), COUNT(*)
		FROM usage_records r LEFT JOIN users u ON u.id = r.user_id WHERE r.created_at >= ? GROUP BY u.id, u.name, u.email ORDER BY 6 DESC`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageByUser
	for rows.Next() {
		var r UsageByUser
		if err := rows.Scan(&r.UserID, &r.Name, &r.Email, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.Calls); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- audit ---------------------------------------------------------------------------

// AuditEntry is an append-only security/compliance event.
type AuditEntry struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	IP        string    `json:"ip,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Audit appends an entry (errors are returned but callers usually log them).
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if len(e.Detail) > 2000 {
		e.Detail = e.Detail[:2000]
	}
	_, err := s.exec(ctx, `INSERT INTO audit_log (id, user_id, action, target, detail, ip, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		NewID(), nullStr(e.UserID), e.Action, e.Target, e.Detail, e.IP, s.stamp())
	return err
}

// ListAudit returns recent entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.query(ctx, `SELECT id, user_id, action, target, detail, ip, created_at FROM audit_log ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var uid, created sql.NullString
		if err := rows.Scan(&e.ID, &uid, &e.Action, &e.Target, &e.Detail, &e.IP, &created); err != nil {
			return nil, err
		}
		e.UserID, e.CreatedAt = uid.String, parseTS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- product context summaries ---------------------------------------------------------

// RepoSummary caches the business summary of a product system.
type RepoSummary struct {
	System      string    `json:"system"`
	Summary     string    `json:"summary"`
	SourceRef   string    `json:"sourceRef"`
	GeneratedAt time.Time `json:"generatedAt"`
}

// GetRepoSummary returns a cached summary.
func (s *Store) GetRepoSummary(ctx context.Context, system string) (*RepoSummary, error) {
	var r RepoSummary
	var gen sql.NullString
	err := s.row(ctx, `SELECT system, summary, source_ref, generated_at FROM repo_summaries WHERE system = ?`, system).Scan(&r.System, &r.Summary, &r.SourceRef, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.GeneratedAt = parseTS(gen)
	return &r, nil
}

// PutRepoSummary stores a summary.
func (s *Store) PutRepoSummary(ctx context.Context, r *RepoSummary) error {
	r.GeneratedAt = s.now().UTC()
	_, err := s.exec(ctx, `INSERT INTO repo_summaries (system, summary, source_ref, generated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (system) DO UPDATE SET summary = excluded.summary, source_ref = excluded.source_ref, generated_at = excluded.generated_at`,
		r.System, r.Summary, r.SourceRef, ts(r.GeneratedAt))
	return err
}

// --- retention -------------------------------------------------------------------------

// PurgeInactiveConversations deletes conversations not updated since t.
// Usage and audit records are kept (they hold no document content).
func (s *Store) PurgeInactiveConversations(ctx context.Context, before time.Time) (int, error) {
	rows, err := s.query(ctx, `SELECT id FROM conversations WHERE updated_at < ?`, ts(before))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := s.DeleteConversation(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// --- workspace bases -------------------------------------------------------------------

// SetBases records blob SHAs (path relative to the project dir) for a conversation.
func (s *Store) SetBases(ctx context.Context, convID string, bases map[string]string, replace bool) error {
	if replace {
		if _, err := s.exec(ctx, `DELETE FROM workspace_bases WHERE conversation_id = ?`, convID); err != nil {
			return err
		}
	}
	for p, sha := range bases {
		if _, err := s.exec(ctx, `INSERT INTO workspace_bases (conversation_id, path, sha) VALUES (?, ?, ?)
			ON CONFLICT (conversation_id, path) DO UPDATE SET sha = excluded.sha`, convID, p, sha); err != nil {
			return err
		}
	}
	return nil
}

// Bases returns the recorded blob SHAs.
func (s *Store) Bases(ctx context.Context, convID string) (map[string]string, error) {
	rows, err := s.query(ctx, `SELECT path, sha FROM workspace_bases WHERE conversation_id = ?`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, sha string
		if err := rows.Scan(&p, &sha); err != nil {
			return nil, err
		}
		out[p] = sha
	}
	return out, rows.Err()
}
