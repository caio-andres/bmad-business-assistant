package guard

import (
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	g := &Guard{Policy: PolicyMask, AllowEmailDomains: []string{"acme.example"}}
	in := "Cliente 529.982.247-25 (CNPJ 11.222.333/0001-81), cartão 4111 1111 1111 1111, " +
		"e-mail joao.silva@gmail.com, colega ana@acme.example, tel (11) 98765-4321, " +
		"chave sk-proj-abcdefghijklmnopqrstuvwxyz123456 e senha: hunter22!"
	r := g.Scan(in)
	for _, leak := range []string{"529.982.247-25", "11.222.333/0001-81", "4111 1111", "joao.silva", "98765-4321", "sk-proj", "hunter22"} {
		if strings.Contains(r.Text, leak) {
			t.Errorf("leaked %q in %q", leak, r.Text)
		}
	}
	if !strings.Contains(r.Text, "ana@acme.example") {
		t.Errorf("allowed domain masked: %s", r.Text)
	}
	want := map[string]int{KindCPF: 1, KindCNPJ: 1, KindCard: 1, KindEmail: 1, KindPhone: 1, KindSecret: 2}
	for k, v := range want {
		if r.Findings[k] != v {
			t.Errorf("findings[%s] = %d, want %d (%v)", k, r.Findings[k], v, r.Findings)
		}
	}
	if r.Summary() == "" {
		t.Error("empty summary")
	}
}

func TestNoFalsePositives(t *testing.T) {
	g := &Guard{Policy: PolicyMask}
	for _, s := range []string{
		"Meta de 30% de adesão em 2026, NPS 60, ticket médio R$ 1.250,00",
		"Pedido 123.456.789-00 inválido", // CPF with wrong check digits
		"Versão 6.12.4 do método, épico 3, história 3.2",
		"Número 1234 5678 9012 3456", // fails Luhn
	} {
		if r := g.Scan(s); r.Any() {
			t.Errorf("false positive in %q: %v -> %s", s, r.Findings, r.Text)
		}
	}
}

func TestBlockAndOff(t *testing.T) {
	in := "meu cpf 529.982.247-25"
	if r := (&Guard{Policy: PolicyBlock}).Scan(in); !r.Blocked || r.Text != in {
		t.Fatalf("block: %+v", r)
	}
	if r := (&Guard{Policy: PolicyOff}).Scan(in); r.Any() {
		t.Fatal("off should not scan")
	}
}
