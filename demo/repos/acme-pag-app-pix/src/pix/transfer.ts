// Fluxo fictício de transferência Pix (dados de demonstração).
export type PixKey = { type: 'cpf' | 'email' | 'phone' | 'random'; value: string };
export async function transfer(key: PixKey, amountCents: number): Promise<string> {
  if (amountCents <= 0) throw new Error('valor inválido');
  return 'E2E-DEMO-' + Date.now();
}
