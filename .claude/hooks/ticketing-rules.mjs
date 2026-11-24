// PreToolUse hook: rules that keep the box office from selling a seat twice or hanging on a slow client.
//   - Taking a seat is a compare-and-set: every UPDATE that sets a seat to 'held' must require it to be free.
//   - No root contexts and no time.Sleep in internal/: stages and streams must stop when the caller cancels.
//   - No huge channel buffers: slow SSE subscribers are dropped, not buffered without limit.
import { readFileSync } from 'node:fs'

const input = JSON.parse(readFileSync(0, 'utf8'))
const filePath = (input.tool_input?.file_path ?? '').replaceAll('\\', '/')
if (!/\/internal\/.*\.go$/.test(filePath) || filePath.endsWith('_test.go')) process.exit(0)

const before = (() => {
  try {
    return readFileSync(filePath, 'utf8')
  } catch {
    return ''
  }
})()
const oldText = input.tool_input?.old_string
const after = oldText !== undefined ? before.replace(oldText, input.tool_input?.new_string ?? '') : (input.tool_input?.content ?? '')
const problems = []

// Each statement that sets seats to 'held', with the lines that follow it (the WHERE clause).
const lines = after.split('\n')
lines.forEach((line, i) => {
  if (!/UPDATE seats SET status = 'held'/.test(line)) return
  const statement = lines.slice(i, i + 3).join(' ')
  if (!/status = 'available'/.test(statement)) {
    problems.push(`line ${i + 1}: an UPDATE that holds seats must include "status = 'available'" in its WHERE (compare-and-set)`)
  }
})
if (/context\.(Background|TODO)\(\)/.test(after)) problems.push('creates a root context; accept ctx from the caller')
if (/time\.Sleep\(/.test(after)) problems.push('uses time.Sleep; wait with select on ctx.Done() and a timer')
if (/make\(chan [^,)]+,\s*[0-9_]{4,}\)/.test(after)) problems.push('creates a channel with a huge buffer; slow subscribers must be dropped')

if (problems.length > 0) {
  console.error(`Blocked: ${filePath}\n- ${problems.join('\n- ')}`)
  process.exit(2)
}
