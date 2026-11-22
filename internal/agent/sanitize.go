package agent

import (
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
)

// pathRe matches internal paths that must never reach the business user.
var pathRe = regexp.MustCompile("`?(?:\\{project-root\\}|\\{skill-root\\}|/project|_bmad-output|\\{planning_artifacts\\}|\\{doc_workspace\\})(?:/[\\w.@{}\\-]+)+/?`?")

// Sanitize replaces internal paths with friendly document names.
func Sanitize(s string) string {
	return pathRe.ReplaceAllStringFunc(s, func(m string) string {
		p := strings.TrimSuffix(strings.Trim(m, "`"), "/")
		if !strings.Contains(path.Base(p), ".") {
			return "a pasta do documento"
		}
		return "**" + runtime.FriendlyName(p) + "**"
	})
}

// streamSanitizer buffers streamed text so paths split across chunks are
// still caught; it flushes up to the last whitespace.
type streamSanitizer struct {
	mu   sync.Mutex
	buf  strings.Builder
	out  func(string)
	full strings.Builder
}

func newStreamSanitizer(out func(string)) *streamSanitizer {
	return &streamSanitizer{out: out}
}

func (s *streamSanitizer) Write(delta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.WriteString(delta)
	text := s.buf.String()
	cut := strings.LastIndexAny(text, " \n\t")
	if cut < 0 {
		if len(text) < 512 {
			return
		}
		cut = len(text) - 1
	}
	ready := text[:cut+1]
	rest := text[cut+1:]
	s.buf.Reset()
	s.buf.WriteString(rest)
	clean := Sanitize(ready)
	s.full.WriteString(clean)
	if s.out != nil && clean != "" {
		s.out(clean)
	}
}

func (s *streamSanitizer) Flush() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 0 {
		clean := Sanitize(s.buf.String())
		s.buf.Reset()
		s.full.WriteString(clean)
		if s.out != nil && clean != "" {
			s.out(clean)
		}
	}
	return s.full.String()
}
