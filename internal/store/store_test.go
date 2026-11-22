package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/llm"
)

// openTest opens SQLite in a temp dir, or Postgres when TEST_DATABASE_URL is set.
func openTest(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	s, err := Open(context.Background(), url, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if url != "" {
		for _, tbl := range []string{"workspace_versions", "workspace_files", "messages", "publications", "conversations", "projects", "sessions", "invites", "usage_records", "audit_log", "repo_summaries", "users"} {
			if _, err := s.db.Exec("DELETE FROM " + tbl); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsersInvitesSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := &User{Email: " Ana@Acme.example ", Name: "Ana", Role: RoleAdmin, PasswordHash: "h"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, &User{Email: "ana@acme.example", Name: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate email: %v", err)
	}
	got, err := s.GetUserByEmail(ctx, "ANA@acme.example")
	if err != nil || got.ID != u.ID || !got.IsAdmin() {
		t.Fatalf("get by email: %v %+v", err, got)
	}
	if err := s.CreateSession(ctx, "sess", u.ID, time.Now().Add(time.Hour), "ua"); err != nil {
		t.Fatal(err)
	}
	if su, err := s.SessionUser(ctx, "sess"); err != nil || su.Email != "ana@acme.example" {
		t.Fatalf("session user: %v", err)
	}
	got.Status = StatusDisabled
	if err := s.UpdateUser(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, "sess"); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled user must not have a session")
	}
	inv := &Invite{TokenHash: "th", Email: "bia@acme.example", Role: RoleMember, CreatedBy: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInvite(ctx, "th"); err != nil {
		t.Fatal(err)
	}
	if err := s.UseInvite(ctx, "th"); err != nil {
		t.Fatal(err)
	}
	if err := s.UseInvite(ctx, "th"); !errors.Is(err, ErrNotFound) {
		t.Fatal("invite reused")
	}
}

func TestConversationLeaseTranscriptAndWorkspace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := &User{Email: "a@acme.example", Name: "A"}
	_ = s.CreateUser(ctx, u)
	p := &Project{Sigla: "PAG", Slug: "cashback", Name: "Cashback no Pix", Systems: []string{"acme-pag-app-pix"}}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, &Project{Sigla: "PAG", Slug: "cashback", Name: "dup"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup project: %v", err)
	}
	c := &Conversation{UserID: u.ID, Title: "Brief", ProjectID: p.ID}
	if err := s.CreateConversation(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLease(ctx, c.ID, "turn-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLease(ctx, c.ID, "turn-2", time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("second lease: %v", err)
	}
	if got, _ := s.GetConversation(ctx, c.ID); !got.Busy {
		t.Fatal("conversation should be busy")
	}
	_ = s.ReleaseLease(ctx, c.ID, "turn-1")
	if err := s.AcquireLease(ctx, c.ID, "turn-2", time.Minute); err != nil {
		t.Fatalf("lease after release: %v", err)
	}
	tr := []llm.Message{{Role: llm.RoleUser, Content: "oi"}, {Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "ask_user", Arguments: `{}`}}}}
	if err := s.SaveTurn(ctx, c.ID, tr, StateAwaitingUser, "bmad-product-brief", "bmad-agent-analyst"); err != nil {
		t.Fatal(err)
	}
	back, err := s.LoadTranscript(ctx, c.ID)
	if err != nil || len(back) != 2 || back[1].ToolCalls[0].Name != "ask_user" {
		t.Fatalf("transcript: %v %+v", err, back)
	}
	got, _ := s.GetConversation(ctx, c.ID)
	if got.State != StateAwaitingUser || got.ActiveSkill != "bmad-product-brief" {
		t.Fatalf("state: %+v", got)
	}
	_ = s.AddMessage(ctx, &Message{ConversationID: c.ID, Role: "user", Content: "oi"})
	_ = s.AddMessage(ctx, &Message{ConversationID: c.ID, Role: "assistant", Content: "olá", Options: []string{"A", "B"}})
	msgs, _ := s.ListMessages(ctx, c.ID)
	if len(msgs) != 2 || len(msgs[1].Options) != 2 {
		t.Fatalf("messages: %+v", msgs)
	}

	wb, err := s.NewWorkspaceBackend(ctx, c.ID, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.Write("planning-artifacts/briefs/b/brief.md", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	_ = wb.Write("planning-artifacts/briefs/b/brief.md", []byte("v2"))
	_ = wb.Write("planning-artifacts/briefs/b/brief.md", []byte("v2")) // no-op
	_ = wb.Write("planning-artifacts/briefs/b/.memlog.md", []byte("log"))
	wb2, _ := s.NewWorkspaceBackend(ctx, c.ID, "user")
	list, _ := wb2.List()
	sort.Strings(list)
	if len(list) != 2 {
		t.Fatalf("list = %v", list)
	}
	if d, _ := wb2.Read("planning-artifacts/briefs/b/brief.md"); string(d) != "v2" {
		t.Fatalf("read = %s", d)
	}
	vers, _ := s.ListVersions(ctx, c.ID, "planning-artifacts/briefs/b/brief.md")
	if len(vers) != 1 {
		t.Fatalf("versions = %+v", vers)
	}
	if _, data, _ := s.ReadVersion(ctx, c.ID, vers[0].ID); string(data) != "v1" {
		t.Fatalf("version content = %s", data)
	}
	if err := wb2.Delete("planning-artifacts/briefs/b/.memlog.md"); err != nil {
		t.Fatal(err)
	}
	files, _ := s.ListFiles(ctx, c.ID)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	convs, _ := s.ListProjectConversations(ctx, p.ID)
	if len(convs) != 1 {
		t.Fatal("project conversations")
	}
	if err := s.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
}

func TestUsageAuditRetention(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })
	u := &User{Email: "u@acme.example", Name: "U"}
	_ = s.CreateUser(ctx, u)
	old := &Conversation{UserID: u.ID, Title: "velha"}
	_ = s.CreateConversation(ctx, old)
	_ = s.AddUsage(ctx, UsageRecord{UserID: u.ID, Purpose: "interpreter", Model: "m", InputTokens: 100, OutputTokens: 10, CostUSD: 0.01})
	s.SetClock(func() time.Time { return base.Add(48 * time.Hour) })
	_ = s.AddUsage(ctx, UsageRecord{UserID: u.ID, Purpose: "router", Model: "m", InputTokens: 5, OutputTokens: 1})
	recent := &Conversation{UserID: u.ID, Title: "nova"}
	_ = s.CreateConversation(ctx, recent)
	tot, _ := s.UserUsageSince(ctx, u.ID, base.Add(24*time.Hour))
	if tot.InputTokens != 5 || tot.Calls != 1 {
		t.Fatalf("since: %+v", tot)
	}
	rep, _ := s.UsageReport(ctx, base.Add(-time.Hour))
	if len(rep) != 1 || rep[0].InputTokens != 105 {
		t.Fatalf("report: %+v", rep)
	}
	_ = s.Audit(ctx, AuditEntry{UserID: u.ID, Action: "login", IP: "127.0.0.1"})
	if a, _ := s.ListAudit(ctx, 10); len(a) != 1 || a[0].Action != "login" {
		t.Fatalf("audit: %+v", a)
	}
	n, err := s.PurgeInactiveConversations(ctx, base.Add(24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := s.GetConversation(ctx, recent.ID); err != nil {
		t.Fatal("recent conversation purged")
	}
}

func TestMigrateIdempotent(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") != "" {
		t.Skip("sqlite only")
	}
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), "", dir)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := s.SchemaVersion(context.Background())
		if v != len(migrations) {
			t.Fatalf("version = %d", v)
		}
		s.Close()
	}
	if _, err := os.Stat(filepath.Join(dir, "alicerce.db")); err != nil {
		t.Fatal(err)
	}
}
