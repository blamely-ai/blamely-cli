import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import test from "node:test"
import { createRecorder } from "./bridge.mjs"

const event = { sessionID: "ses_test", id: "call_test", tool: "shell", input: { command: "script" } }

test("transport forwards lifecycle events, not file content or snapshots", async () => {
    const received = []
    const recorder = createRecorder(2, async (p) => received.push(p))
    await recorder.before(event, "/session/checkout", "openai/test")
    await recorder.after({ ...event, status: "error" })
    await recorder.clear(event.sessionID)
    assert.deepEqual(received, [
        { version: 2, session_id: "opencode:ses_test", call_id: "call_test", phase: "before", cwd: "/session/checkout", model: "openai/test", tool_name: "shell", tool_input: { command: "script" } },
        { version: 2, session_id: "opencode:ses_test", call_id: "call_test", phase: "after" },
        { version: 2, session_id: "opencode:ses_test", phase: "discard" },
    ])
    assert.ok(received.every((p) => !("before" in p) && !("after" in p) && !("file_path" in p)))
})

test("cleanup forwards each session identity and retains no capture contents", async () => {
    const received = []
    const recorder = createRecorder(2, async (p) => received.push(p))
    await recorder.before(event, "/one")
    await recorder.before({ ...event, sessionID: "other" }, "/two")
    await recorder.clear()
    assert.deepEqual(received.filter((p) => p.phase === "discard").map((p) => p.session_id), ["opencode:ses_test", "opencode:other"])
    await recorder.clear()
    assert.equal(received.length, 4)
})

// Replace only process transport. The API-specific callback mappings remain real.
async function adapter(version, emit) {
    globalThis.__blamelyEmit = emit
    let source = await readFile(new URL(`./v${version}.mjs`, import.meta.url), "utf8")
    source = source.replace('"./bridge.mjs"', JSON.stringify(new URL("./bridge.mjs", import.meta.url).href))
    source = source.replace(`createRecorder(${version})`, `createRecorder(${version}, globalThis.__blamelyEmit)`)
    return (await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Math.random()}`)).default
}

test("V1 maps callID/output.args and model selection, including failed-tool callbacks", async () => {
    const received = []
    const plugin = await adapter(1, async (p) => received.push(p))
    const hooks = await plugin({ directory: "/v1/checkout" })
    const input = { tool: "bash", sessionID: "ses_v1", callID: "call_v1" }
    await hooks["chat.params"]({ sessionID: input.sessionID, model: { providerID: "openai", id: "v1" } })
    await hooks["tool.execute.before"](input, { args: { command: "script" } })
    await hooks.event({ event: { type: "message.part.updated", properties: { part: { type: "tool", sessionID: input.sessionID, callID: input.callID, state: { status: "error" } } } } })
    await hooks["tool.execute.after"](input)
    assert.equal(received[0].cwd, "/v1/checkout")
    assert.equal(received[0].model, "openai/v1")
    assert.equal(received[0].call_id, "call_v1")
    assert.deepEqual(received[0].tool_input, { command: "script" })
    assert.equal(received.filter((p) => p.phase === "after").length, 2) // native Go consumption handles duplicates
    await hooks.event({ event: { type: "session.deleted", properties: { info: { id: input.sessionID } } } })
    assert.equal(received.at(-1).phase, "discard")
})

test("V2 forwards each session's location/model and awaits cleanup", async () => {
    const received = []
    const plugin = await adapter(2, async (p) => received.push(p))
    assert.equal(plugin.id, "blamely.opencode")
    const hooks = new Map()
    const cleanup = await plugin.setup({
        location: { directory: "/wrong-location" },
        tool: { hook: async (name, callback) => hooks.set(name, callback) },
        session: { get: async () => ({ location: { directory: "/v2/checkout" }, model: { providerID: "anthropic", id: "v2" } }) },
    })
    await hooks.get("execute.before")(event)
    await hooks.get("execute.after")({ ...event, status: "error" })
    assert.equal(received[0].cwd, "/v2/checkout")
    assert.equal(received[0].model, "anthropic/v2")
    assert.equal(received[0].version, 2)
    assert.equal(received[1].phase, "after")
    await cleanup()
    assert.equal(received[2].phase, "discard")
})
