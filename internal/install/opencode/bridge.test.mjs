import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import { mkdir, mkdtemp, readFile, realpath, rename, rm, symlink, unlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import test from "node:test"
import { createRecorder, targets } from "./bridge.mjs"

async function fixture(t) {
    const root = await realpath(await mkdtemp(join(tmpdir(), "blamely-opencode-")))
    t.after(() => rm(root, { recursive: true, force: true }))
    execFileSync("git", ["-C", root, "-c", "core.hooksPath=", "init", "-q", "-b", "main"])
    return root
}
const event = { sessionID: "ses_test", id: "call_test", tool: "shell", input: { command: "script" } }

test("both patch names include rename destinations and snake/camel file arguments", () => {
    const patch = "*** Update File: old.txt\r\n*** Move to: new ş.txt\r\n*** Add File: a.txt\r\n*** Delete File: b.txt\r\n"
    assert.deepEqual(targets("patch", { patchText: patch }), ["old.txt", "new ş.txt", "a.txt", "b.txt"])
    assert.deepEqual(targets("apply_patch", { patch }), ["old.txt", "new ş.txt", "a.txt", "b.txt"])
    assert.deepEqual(targets("edit", { filePath: "a.txt" }), ["a.txt"])
    assert.deepEqual(targets("multiedit", { file_path: "a.txt" }), ["a.txt"])
    assert.deepEqual(targets("read", { filePath: "a.txt" }), [])
})

test("shell observes actual edits/new/deleted files without claiming untouched human work or ignored output", async (t) => {
    const root = await fixture(t)
    await writeFile(join(root, ".gitignore"), "ignored/\n")
    await writeFile(join(root, "edited.txt"), "human\n")
    await writeFile(join(root, "untouched.txt"), "dirty human\n")
    await writeFile(join(root, "deleted.txt"), "deleted\n")
    const received = []
    const recorder = createRecorder(2, async (p) => received.push(p))
    await recorder.before(event, root, "openai/test")
    await writeFile(join(root, "edited.txt"), "human\nAI\n")
    await writeFile(join(root, "new ş.txt"), "new\n")
    await unlink(join(root, "deleted.txt"))
    await mkdir(join(root, "ignored"))
    await writeFile(join(root, "ignored", "output.txt"), "build\n")
    await recorder.after({ ...event, status: "error" })
    assert.deepEqual(received.map((p) => p.file_path).sort(), ["deleted.txt", "edited.txt", "new ş.txt"].map((p) => join(root, p)).sort())
    assert.equal(received.find((p) => p.file_path.endsWith("edited.txt")).before, "human\n")
    assert.ok(received.every((p) => p.model === "openai/test" && p.version === 2 && p.session_id === "opencode:ses_test"))
    await recorder.after(event)
    assert.equal(received.length, 3)
})

test("clean new checkout and linked worktrees can be observed", async (t) => {
    const root = await fixture(t)
    const received = []
    const recorder = createRecorder(1, async (p) => received.push(p))
    await recorder.before(event, root)
    await writeFile(join(root, "new.txt"), "new\n")
    await recorder.after(event)
    assert.equal(received.length, 1)
    execFileSync("git", ["-C", root, "add", "."])
    execFileSync("git", ["-C", root, "-c", "core.hooksPath=", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "initial"])
    const linked = join(root, "linked")
    execFileSync("git", ["-C", root, "worktree", "add", "-qb", "feature", linked])
    await recorder.before(event, linked)
    await writeFile(join(linked, "new.txt"), "worktree\n")
    await recorder.after(event)
    assert.equal(received[1].cwd, linked)
    assert.equal(received[1].before, "new\n")
})

test("patch moves and creations in nested directories are recorded", async (t) => {
    const root = await fixture(t)
    await writeFile(join(root, "old.txt"), "old\n")
    const received = []
    const recorder = createRecorder(2, async (p) => received.push(p))
    const patch = { ...event, tool: "patch", input: { patchText: "*** Update File: old.txt\n*** Move to: new/renamed.txt\n*** Add File: new/created.txt\n" } }
    await recorder.before(patch, root)
    await mkdir(join(root, "new"))
    await rename(join(root, "old.txt"), join(root, "new", "renamed.txt"))
    await writeFile(join(root, "new", "created.txt"), "created\n")
    await recorder.after(patch)
    assert.equal(received.length, 3)
})

test("skips branch changes, oversized/binary files and paths escaping the checkout", async (t) => {
    const root = await fixture(t)
    const received = []
    const recorder = createRecorder(2, async (p) => received.push(p))
    await writeFile(join(root, "example.txt"), "old\n")
    await recorder.before(event, root)
    execFileSync("git", ["-C", root, "symbolic-ref", "HEAD", "refs/heads/other"])
    await writeFile(join(root, "example.txt"), "new branch\n")
    await recorder.after(event)
    assert.equal(received.length, 0)
    await writeFile(join(root, "large.txt"), "x".repeat(1024 * 1024 + 1))
    await writeFile(join(root, "binary.txt"), Buffer.from([0]))
    await writeFile(join(root, "invalid.txt"), Buffer.from([255]))
    for (const file of ["large.txt", "binary.txt", "invalid.txt", "../escape.txt"]) {
        const edit = { ...event, tool: "write", input: { filePath: file } }
        await recorder.before(edit, root)
        if (!file.startsWith("..")) await writeFile(join(root, file), "now text\n")
        await recorder.after(edit)
    }
    assert.equal(received.length, 0)
    if (process.platform !== "win32") {
        await symlink(dirname(root), join(root, "escape"))
        const edit = { ...event, tool: "write", input: { filePath: "escape/missing.txt" } }
        await recorder.before(edit, root)
        await recorder.after(edit)
        assert.equal(received.length, 0)
    }
})

test("session/call IDs isolate pending captures; cleanup releases them", async (t) => {
    const root = await fixture(t)
    await writeFile(join(root, "example.txt"), "old\n")
    const received = []
    const recorder = createRecorder(2, async (p) => received.push(p))
    await recorder.before(event, root)
    await recorder.after({ ...event, sessionID: "other" })
    recorder.clear(event.sessionID)
    await writeFile(join(root, "example.txt"), "new\n")
    await recorder.after(event)
    assert.equal(received.length, 0)
})

// Load the actual adapters with only their emit boundary replaced. All hook
// registration, argument mapping, session location/model lookup remains real.
async function adapter(version, emit) {
    globalThis.__blamelyEmit = emit
    let source = await readFile(new URL(`./v${version}.mjs`, import.meta.url), "utf8")
    source = source.replace('"./bridge.mjs"', JSON.stringify(new URL("./bridge.mjs", import.meta.url).href))
    source = source.replace(`createRecorder(${version})`, `createRecorder(${version}, globalThis.__blamelyEmit)`)
    return (await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Math.random()}`)).default
}

test("V1 callback API maps callID/output.args, model changes and failed-tool events", async (t) => {
    const root = await fixture(t)
    const received = []
    const plugin = await adapter(1, async (p) => received.push(p))
    const hooks = await plugin({ directory: root })
    await writeFile(join(root, "example.txt"), "old\n")
    const input = { tool: "bash", sessionID: "ses_v1", callID: "call_v1" }
    await hooks["chat.params"]({ sessionID: input.sessionID, model: { providerID: "openai", id: "v1" } })
    await hooks["tool.execute.before"](input, { args: { command: "script" } })
    await writeFile(join(root, "example.txt"), "AI\n")
    await hooks.event({ event: { type: "message.part.updated", properties: { part: { type: "tool", sessionID: input.sessionID, callID: input.callID, state: { status: "error" } } } } })
    await hooks["tool.execute.after"](input)
    assert.equal(received.length, 1)
    assert.equal(received[0].version, 1)
    assert.equal(received[0].model, "openai/v1")
})

test("V2 definition registers domain hooks and uses each session's location, not plugin location", async (t) => {
    const root = await fixture(t)
    const received = []
    const plugin = await adapter(2, async (p) => received.push(p))
    assert.equal(plugin.id, "blamely.opencode")
    const hooks = new Map()
    const cleanup = await plugin.setup({
        location: { directory: "/wrong-location" },
        tool: { hook: async (name, callback) => hooks.set(name, callback) },
        session: { get: async () => ({ location: { directory: root }, model: { providerID: "anthropic", id: "v2" } }) },
    })
    await writeFile(join(root, "example.txt"), "old\n")
    await hooks.get("execute.before")(event)
    await writeFile(join(root, "example.txt"), "AI\n")
    await hooks.get("execute.after")({ ...event, status: "error" })
    assert.equal(received.length, 1)
    assert.equal(received[0].model, "anthropic/v2")
    assert.equal(received[0].version, 2)
    cleanup()
})
