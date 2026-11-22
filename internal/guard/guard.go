// Package guard detects and masks personal data and secrets before they reach
// the LLM or the database (data-protection spec, LGPD).
package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Policy is how findings are handled.
type Policy string

const (
	PolicyMask  Policy = "mask"
	PolicyBlock Policy = "block"
	PolicyOff   Policy = "off"
)

// Kinds of findings (also the mask tokens).
const (
	KindCPF    = "CPF"
	KindCNPJ   = "CNPJ"
	KindCard   = "CARTAO"
	KindEmail  = "EMAIL"
	KindPhone  = "TELEFONE"
	KindSecret = "SEGREDO"
)

// Result summarizes what was found (never the values).
type Result struct {
	Text     string         `json:"-"`
	Findings map[string]int `json:"findings"`
	Blocked  bool           `json:"blocked"`
}

// Any reports whether something was found.
func (r Result) Any() bool { return len(r.Findings) > 0 }

// Summary renders findings as "2 CPF, 1 e-mail".
func (r Result) Summary() string {
	names := map[string]string{KindCPF: "CPF", KindCNPJ: "CNPJ", KindCard: "cartão", KindEmail: "e-mail", KindPhone: "telefone", KindSecret: "credencial"}
	var keys []string
	for k := range r.Findings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", r.Findings[k], names[k]))
	}
	return strings.Join(parts, ", ")
}

// Guard applies a policy.
type Guard struct {
	Policy Policy
	// AllowEmailDomains are not masked (e.g. the company's own domain).
	AllowEmailDomains []string
}

var (
	reSecret = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`\b(?:sk|rk|pk)-(?:proj-|live-|test-)?[A-Za-z0-9_\-]{20,}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}\b`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/]{20,}=*`),
		regexp.MustCompile(`(?i)\b(?:senha|password|passwd|pwd|secret|token|api[_-]?key)\s*[:=]\s*\S{6,}`),
	}
	reEmail = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@([A-Za-z0-9.\-]+\.[A-Za-z]{2,})\b`)
	reCNPJ  = regexp.MustCompile(`\b\d{2}\.?\d{3}\.?\d{3}/?\d{4}-?\d{2}\b`)
	reCPF   = regexp.MustCompile(`\b\d{3}\.?\d{3}\.?\d{3}-?\d{2}\b`)
	reCard  = regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`)
	rePhone = regexp.MustCompile(`(?:\+?55[\s\-]?)?\(?\b[1-9]{2}\)?[\s\-]?9?\d{4}[\s\-]?\d{4}\b`)
)

// Scan applies the policy to text.
func (g *Guard) Scan(text string) Result {
	res := Result{Text: text, Findings: map[string]int{}}
	if g == nil || g.Policy == PolicyOff || text == "" {
		return res
	}
	out := text
	replace := func(re *regexp.Regexp, kind string, valid func(string) bool) {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			if valid != nil && !valid(m) {
				return m
			}
			res.Findings[kind]++
			return "[" + kind + "]"
		})
	}
	for _, re := range reSecret {
		replace(re, KindSecret, nil)
	}
	replace(reEmail, KindEmail, func(m string) bool {
		dom := strings.ToLower(m[strings.LastIndex(m, "@")+1:])
		for _, a := range g.AllowEmailDomains {
			a = strings.ToLower(strings.TrimSpace(a))
			if a != "" && (dom == a || strings.HasSuffix(dom, "."+a)) {
				return false
			}
		}
		return true
	})
	replace(reCNPJ, KindCNPJ, func(m string) bool { return validCNPJ(digits(m)) })
	replace(reCPF, KindCPF, func(m string) bool { return validCPF(digits(m)) })
	replace(reCard, KindCard, func(m string) bool { d := digits(m); return len(d) >= 13 && len(d) <= 19 && luhn(d) })
	replace(rePhone, KindPhone, func(m string) bool { d := digits(m); return len(d) >= 10 && len(d) <= 13 })
	res.Text = out
	if res.Any() && g.Policy == PolicyBlock {
		res.Blocked = true
		res.Text = text
	}
	if len(res.Findings) == 0 {
		res.Findings = nil
	}
	return res
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func allSame(d string) bool { return strings.Count(d, d[:1]) == len(d) }

func validCPF(d string) bool {
	if len(d) != 11 || allSame(d) {
		return false
	}
	for _, n := range []int{9, 10} {
		sum := 0
		for i := 0; i < n; i++ {
			sum += int(d[i]-'0') * (n + 1 - i)
		}
		dv := (sum * 10) % 11
		if dv == 10 {
			dv = 0
		}
		if dv != int(d[n]-'0') {
			return false
		}
	}
	return true
}

func validCNPJ(d string) bool {
	if len(d) != 14 || allSame(d) {
		return false
	}
	w1 := []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	w2 := append([]int{6}, w1...)
	for i, w := range [][]int{w1, w2} {
		sum := 0
		for j, k := range w {
			sum += int(d[j]-'0') * k
		}
		dv := sum % 11
		if dv < 2 {
			dv = 0
		} else {
			dv = 11 - dv
		}
		if dv != int(d[12+i]-'0') {
			return false
		}
	}
	return true
}

func luhn(d string) bool {
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
