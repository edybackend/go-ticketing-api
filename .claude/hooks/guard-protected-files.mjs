// PreToolUse hook: blocks edits to secrets, databases, generated data and lock files.
// Exit code 2 rejects the tool call and returns the message to the agent.
import { readFileSync } from 'node:fs'
import path from 'node:path'

const input = JSON.parse(readFileSync(0, 'utf8'))
const filePath = (input.tool_input?.file_path ?? '').replaceAll('\\', '/')
const name = path.basename(filePath)

const rules = [
  { test: () => /^\.env(\..*)?$/.test(name), why: 'environment files may contain secrets' },
  { test: () => /\.(pem|key|pfx)$/.test(name), why: 'key material must never be written by the agent' },
  { test: () => /\.db(-wal|-shm)?$/.test(name) || /\/data\//.test(filePath), why: 'data is created by the service (sample events are seeded on first start)' },
  { test: () => name === 'go.sum', why: 'go.sum is generated: use go get / go mod tidy' },
]

const violated = rules.find((rule) => rule.test())
if (violated) {
  console.error(`Blocked edit to ${filePath}: ${violated.why}.`)
  process.exit(2)
}
