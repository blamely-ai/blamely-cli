// Blamely-managed OpenCode command-hook transport; all capture logic is Go.
import { spawn } from "node:child_process"

const binary = "blamely" // replaced with the stable absolute path by the installer

async function send(payload) {
    try { await new Promise((done) => {
        const args = ["record", "opencode", ...(payload.phase === "before" ? ["--pre"] : [])]
        const child = spawn(binary, args, { stdio: ["pipe", "ignore", "ignore"], windowsHide: true })
        const timer = setTimeout(() => { child.kill(); done() }, 30000)
        const finish = () => { clearTimeout(timer); done() }
        child.on("error", finish)
        child.on("close", finish)
        child.stdin.on("error", () => {})
        child.stdin.end(JSON.stringify(payload))
    }) } catch { /* Recording must not block OpenCode. */ }
}

export function createRecorder(version, emit = send) {
    const sessions = new Set()
    const identity = (event) => ({ version, session_id: `opencode:${event.sessionID}`, call_id: event.id })
    return {
        async before(event, directory, model) {
            sessions.add(event.sessionID)
            await emit({ ...identity(event), phase: "before", cwd: directory, model: model ?? "", tool_name: event.tool, tool_input: event.input })
        },
        async after(event) {
            await emit({ ...identity(event), phase: "after" })
        },
        async clear(sessionID) {
            for (const id of sessionID ? [sessionID] : sessions) {
                await emit({ version, session_id: `opencode:${id}`, phase: "discard" })
                sessions.delete(id)
            }
        },
    }
}
