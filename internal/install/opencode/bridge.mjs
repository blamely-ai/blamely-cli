// Blamely-managed OpenCode adapter. Shared by the V1 and V2 entrypoints.
import { execFile, spawn } from "node:child_process"
import { lstat, readFile, realpath } from "node:fs/promises"
import { dirname, isAbsolute, relative, resolve, sep } from "node:path"
import { promisify } from "node:util"

const exec = promisify(execFile)
const binary = "blamely" // replaced with the stable absolute path by the installer
const maxBytes = 1024 * 1024
const maxLines = 4000
const maxSnapshotBytes = 64 * 1024 * 1024

export function targets(tool, args = {}) {
    if (!args || typeof args !== "object") return []
    if (["write", "edit", "multiedit"].includes(tool)) {
        const path = args.filePath ?? args.file_path
        return typeof path === "string" ? [path] : []
    }
    if (!["patch", "apply_patch"].includes(tool)) return []
    const patch = args.patchText ?? args.patch ?? args.input
    if (typeof patch !== "string") return []
    return [...new Set([...patch.matchAll(/^\*\*\* (?:(?:Add|Update|Delete) File|Move to): ([^\r\n]+)\r?$/gm)].map((m) => m[1]))]
}

const shell = (tool) => tool === "shell" || tool === "bash"
const inside = (root, path) => {
    const rel = relative(root, path)
    return rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel)
}

async function git(root, ...args) {
    return (await exec("git", ["-C", root, ...args], { timeout: 5000, maxBuffer: 8 * 1024 * 1024 })).stdout
}

async function revision(root) {
    const values = await Promise.all([
        git(root, "rev-parse", "--verify", "HEAD").catch(() => ""),
        git(root, "symbolic-ref", "--quiet", "HEAD").catch(() => ""),
    ])
    return values.join("\0")
}

async function files(root) {
    return [...new Set((await git(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")).split("\0").filter(Boolean))]
}

async function content(root, path) {
    const abs = resolve(root, path)
    if (!inside(root, abs)) return undefined
    // Walk existing parents to reject directory symlinks, including new files
    // whose leaf or immediate parent does not exist yet.
    let parent = dirname(abs)
    while (true) {
        try {
            if (!inside(root, await realpath(parent))) return undefined
            break
        } catch (error) {
            if (error.code !== "ENOENT" || dirname(parent) === parent) return undefined
            parent = dirname(parent)
        }
    }
    try {
        const info = await lstat(abs)
        if (!info.isFile() || info.size > maxBytes) return undefined
        const data = await readFile(abs)
        if (data.length > maxBytes || data.includes(0)) return undefined
        const text = new TextDecoder("utf-8", { fatal: true }).decode(data)
        return text.split("\n").length > maxLines ? undefined : text
    } catch (error) {
        return error.code === "ENOENT" ? "" : undefined
    }
}

async function snapshot(root, paths) {
    const result = new Map()
    let bytes = 0
    for (let i = 0; i < paths.length; i += 16) {
        const batch = await Promise.all(paths.slice(i, i + 16).map(async (path) => [path, await content(root, path)]))
        for (const [path, text] of batch) {
            if (text === undefined) continue
            bytes += Buffer.byteLength(text)
            if (bytes > maxSnapshotBytes) return result
            result.set(path, text)
        }
    }
    return result
}

async function send(payload) {
    await new Promise((done) => {
        const child = spawn(binary, ["record", "opencode"], { stdio: ["pipe", "ignore", "ignore"], windowsHide: true })
        const timer = setTimeout(() => { child.kill(); done() }, 5000)
        const finish = () => { clearTimeout(timer); done() }
        child.on("error", finish)
        child.on("close", finish)
        child.stdin.on("error", () => {})
        child.stdin.end(JSON.stringify(payload))
    })
}

export function createRecorder(version, emit = send) {
    const pending = new Map()
    const key = (event) => `${event.sessionID}:${event.id}`
    return {
        async before(event, directory, model) {
            if (!shell(event.tool) && !targets(event.tool, event.input).length) return
            try {
                const root = (await git(directory, "rev-parse", "--show-toplevel")).trim()
                const args = event.input ?? {}
                if (shell(event.tool) && typeof args.workdir === "string") {
                    if ((await git(resolve(directory, args.workdir), "rev-parse", "--show-toplevel")).trim() !== root) return
                }
                const paths = shell(event.tool) ? await files(root) : targets(event.tool, args).map((path) => relative(root, resolve(directory, path)))
                pending.set(key(event), {
                    root, model, revision: await revision(root), tool: event.tool,
                    known: new Set(paths), before: await snapshot(root, paths), shell: shell(event.tool),
                })
            } catch { /* Attribution must never block the host tool. */ }
        },
        async after(event) {
            const state = pending.get(key(event))
            pending.delete(key(event))
            if (!state) return
            try {
                if (await revision(state.root) !== state.revision) return
                const paths = [...state.before.keys()]
                if (state.shell) paths.push(...(await files(state.root)).filter((path) => !state.known.has(path)))
                for (const [path, after] of await snapshot(state.root, paths)) {
                    const before = state.before.get(path) ?? ""
                    if (before === after) continue
                    await emit({
                        cwd: state.root, session_id: `opencode:${event.sessionID}`, call_id: event.id,
                        tool_name: state.tool, version, model: state.model ?? "",
                        file_path: resolve(state.root, path), before, after,
                    })
                }
            } catch { /* Best effort, including partial edits from failed tools. */ }
        },
        clear(sessionID) {
            if (!sessionID) return pending.clear()
            for (const id of pending.keys()) if (id.startsWith(`${sessionID}:`)) pending.delete(id)
        },
    }
}
