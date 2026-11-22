package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
)

// Conversation states (orchestration spec).
const (
	StateIdle         = "idle"
	StateRouting      = "routing"
	StateExecuting    = "executing"
	StateAwaitingUser = "awaiting_user"
	StateError        = "error"
)

// Project groups conversations under a product initiative (sigla + slug).
type Project struct {
	ID          string    `json:"id"`
	Sigla       string    `json:"sigla"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Systems     []string  `json:"systems"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

const projectCols = `id, sigla, slug, name, description, systems, created_by, created_at, updated_at`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var systems string
	var by, created, updated sql.NullString
	if err := sc.Scan(&p.ID, &p.Sigla, &p.Slug, &p.Name, &p.Description, &systems, &by, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(systems), &p.Systems)
	if p.Systems == nil {
		p.Systems = []string{}
	}
	p.CreatedBy, p.CreatedAt, p.UpdatedAt = by.String, parseTS(created), parseTS(updated)
	return &p, nil
}

// CreateProject inserts a project; (sigla, slug) is unique.
func (s *Store) CreateProject(ctx context.Context, p *Project) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Systems == nil {
		p.Systems = []string{}
	}
	now := s.now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err := s.exec(ctx, `INSERT INTO projects (`+projectCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Sigla, p.Slug, p.Name, p.Description, jsonText(p.Systems), nullStr(p.CreatedBy), ts(now), ts(now))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// UpdateProject saves name, description and systems.
func (s *Store) UpdateProject(ctx context.Context, p *Project) error {
	p.UpdatedAt = s.now().UTC()
	res, err := s.exec(ctx, `UPDATE projects SET name = ?, description = ?, systems = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Description, jsonText(p.Systems), ts(p.UpdatedAt), p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetProject loads a project.
func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	return scanProject(s.row(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
}

// FindProject loads a project by sigla and slug.
func (s *Store) FindProject(ctx context.Context, sigla, slug string) (*Project, error) {
	return scanProject(s.row(ctx, `SELECT `+projectCols+` FROM projects WHERE sigla = ? AND slug = ?`, sigla, slug))
}

// ListProjects returns all projects.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.query(ctx, `SELECT `+projectCols+` FROM projects ORDER BY sigla, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- conversations ------------------------------------------------------------------

// Conversation is one chat thread with its own workspace.
type Conversation struct {
	ID          string    `json:"id"`
	UserID      string    `json:"userId"`
	Title       string    `json:"title"`
	ProjectID   string    `json:"projectId,omitempty"`
	State       string    `json:"state"`
	ActiveSkill string    `json:"activeSkill,omitempty"`
	ActiveAgent string    `json:"activeAgent,omitempty"`
	Archived    bool      `json:"archived"`
	Busy        bool      `json:"busy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

const convCols = `id, user_id, title, project_id, state, active_skill, active_agent, archived, lease_until, created_at, updated_at`

func (s *Store) scanConv(sc interface{ Scan(...any) error }) (*Conversation, error) {
	var c Conversation
	var project, lease, created, updated sql.NullString
	var archived int
	if err := sc.Scan(&c.ID, &c.UserID, &c.Title, &project, &c.State, &c.ActiveSkill, &c.ActiveAgent, &archived, &lease, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.ProjectID, c.Archived = project.String, archived == 1
	c.Busy = lease.Valid && parseTS(lease).After(s.now())
	c.CreatedAt, c.UpdatedAt = parseTS(created), parseTS(updated)
	return &c, nil
}

// CreateConversation inserts a conversation.
func (s *Store) CreateConversation(ctx context.Context, c *Conversation) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.State == "" {
		c.State = StateIdle
	}
	now := s.now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.exec(ctx, `INSERT INTO conversations (id, user_id, title, project_id, state, active_skill, active_agent, transcript, archived, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '[]', 0, ?, ?)`,
		c.ID, c.UserID, c.Title, nullStr(c.ProjectID), c.State, c.ActiveSkill, c.ActiveAgent, ts(now), ts(now))
	return err
}

// GetConversation loads conversation metadata.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	return s.scanConv(s.row(ctx, `SELECT `+convCols+` FROM conversations WHERE id = ?`, id))
}

// ListConversations returns a user's conversations, most recent first.
func (s *Store) ListConversations(ctx context.Context, userID string, archived bool) ([]*Conversation, error) {
	a := 0
	if archived {
		a = 1
	}
	rows, err := s.query(ctx, `SELECT `+convCols+` FROM conversations WHERE user_id = ? AND archived = ? ORDER BY updated_at DESC LIMIT 200`, userID, a)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		c, err := s.scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListProjectConversations returns conversations linked to a project.
func (s *Store) ListProjectConversations(ctx context.Context, projectID string) ([]*Conversation, error) {
	rows, err := s.query(ctx, `SELECT `+convCols+` FROM conversations WHERE project_id = ? ORDER BY updated_at DESC LIMIT 200`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		c, err := s.scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateConversationMeta saves title, project and archived flag.
func (s *Store) UpdateConversationMeta(ctx context.Context, c *Conversation) error {
	a := 0
	if c.Archived {
		a = 1
	}
	c.UpdatedAt = s.now().UTC()
	_, err := s.exec(ctx, `UPDATE conversations SET title = ?, project_id = ?, archived = ?, updated_at = ? WHERE id = ?`,
		c.Title, nullStr(c.ProjectID), a, ts(c.UpdatedAt), c.ID)
	return err
}

// SetState updates state and the active skill/agent.
func (s *Store) SetState(ctx context.Context, id, state, skill, agent string) error {
	_, err := s.exec(ctx, `UPDATE conversations SET state = ?, active_skill = ?, active_agent = ?, updated_at = ? WHERE id = ?`,
		state, skill, agent, s.stamp(), id)
	return err
}

// LoadTranscript returns the interpreter transcript.
func (s *Store) LoadTranscript(ctx context.Context, id string) ([]llm.Message, error) {
	var raw string
	err := s.row(ctx, `SELECT transcript FROM conversations WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// SaveTurn persists the transcript, state and active skill after a turn.
func (s *Store) SaveTurn(ctx context.Context, id string, transcript []llm.Message, state, skill, agent string) error {
	_, err := s.exec(ctx, `UPDATE conversations SET transcript = ?, state = ?, active_skill = ?, active_agent = ?, updated_at = ? WHERE id = ?`,
		jsonText(transcript), state, skill, agent, s.stamp(), id)
	return err
}

// AcquireLease takes the per-conversation turn lock; ErrConflict if held.
func (s *Store) AcquireLease(ctx context.Context, id, owner string, ttl time.Duration) error {
	now := s.now()
	res, err := s.exec(ctx, `UPDATE conversations SET lease_owner = ?, lease_until = ? WHERE id = ? AND (lease_until IS NULL OR lease_until < ? OR lease_owner = ?)`,
		owner, ts(now.Add(ttl)), id, ts(now), owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetConversation(ctx, id); err != nil {
			return err
		}
		return ErrConflict
	}
	return nil
}

// ReleaseLease frees the turn lock if owned.
func (s *Store) ReleaseLease(ctx context.Context, id, owner string) error {
	_, err := s.exec(ctx, `UPDATE conversations SET lease_owner = NULL, lease_until = NULL WHERE id = ? AND lease_owner = ?`, id, owner)
	return err
}

// DeleteConversation removes a conversation and everything under it.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	for _, q := range []string{
		`DELETE FROM workspace_bases WHERE conversation_id = ?`,
		`DELETE FROM workspace_versions WHERE conversation_id = ?`,
		`DELETE FROM workspace_files WHERE conversation_id = ?`,
		`DELETE FROM messages WHERE conversation_id = ?`,
		`UPDATE publications SET conversation_id = NULL WHERE conversation_id = ?`,
		`DELETE FROM conversations WHERE id = ?`,
	} {
		if _, err := s.exec(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// --- visible messages ----------------------------------------------------------------

// Message is what the chat UI shows (the transcript keeps tool details).
type Message struct {
	ID             string                `json:"id"`
	ConversationID string                `json:"conversationId"`
	Role           string                `json:"role"` // user | assistant | notice
	Content        string                `json:"content"`
	Options        []string              `json:"options,omitempty"`
	Activities     []events.ActivityInfo `json:"activities,omitempty"`
	CreatedAt      time.Time             `json:"createdAt"`
}

// AddMessage appends a visible message.
func (s *Store) AddMessage(ctx context.Context, m *Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	m.CreatedAt = s.now().UTC()
	if m.Options == nil {
		m.Options = []string{}
	}
	if m.Activities == nil {
		m.Activities = []events.ActivityInfo{}
	}
	_, err := s.exec(ctx, `INSERT INTO messages (id, conversation_id, role, content, options, activities, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, m.Role, m.Content, jsonText(m.Options), jsonText(m.Activities), ts(m.CreatedAt))
	return err
}

// ListMessages returns visible messages in order.
func (s *Store) ListMessages(ctx context.Context, convID string) ([]*Message, error) {
	rows, err := s.query(ctx, `SELECT id, conversation_id, role, content, options, activities, created_at FROM messages WHERE conversation_id = ? ORDER BY created_at, id`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		var opts, acts string
		var created sql.NullString
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &opts, &acts, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(opts), &m.Options)
		_ = json.Unmarshal([]byte(acts), &m.Activities)
		m.CreatedAt = parseTS(created)
		out = append(out, &m)
	}
	return out, rows.Err()
}
