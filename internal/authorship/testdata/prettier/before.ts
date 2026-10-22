import { readFile } from 'fs/promises'
export async function loadConfig(path: string, fallback: Record<string, unknown> = {}): Promise<Record<string, unknown>> {
  const raw = await readFile(path, "utf8")
  const items = [1, 2, 3].map(x => x * 2)
  const opts = { name: 'demo', retries: 3, verbose: true, timeoutMs: 5000, endpoint: "https://example.com" }
  if (!raw) { return fallback }
  return JSON.parse(raw)
}
