// Stop hook: the agent cannot finish while gofmt, vet or the race tests are red. Mirrors CI.
import { execSync } from 'node:child_process'
import { readFileSync } from 'node:fs'

const input = JSON.parse(readFileSync(0, 'utf8'))
if (input.stop_hook_active) process.exit(0) // already blocked once: avoid an endless loop

const root = process.env.CLAUDE_PROJECT_DIR ?? process.cwd()
const sh = (command) => execSync(command, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })

const changed = sh('git status --porcelain').split('\n').filter(Boolean).map((line) => line.slice(3))
if (!changed.some((file) => file.endsWith('.go') || file.startsWith('go.'))) process.exit(0)

for (const command of ['test -z "$(gofmt -l .)"', 'go vet ./...', 'go test -race ./...']) {
  try {
    sh(command)
  } catch (error) {
    const output = `${error.stdout ?? ''}${error.stderr ?? ''}`.split('\n').slice(-40).join('\n')
    console.error(`Quality gate failed: \`${command}\`. Fix it before finishing.\n${output}`)
    process.exit(2)
  }
}
