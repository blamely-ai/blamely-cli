// Blamely-managed OpenCode V1 plugin; no npm dependencies required.
import { createRecorder } from "./bridge.mjs"

export default async function BlamelyPlugin({ directory }) {
    const recorder = createRecorder(1)
    const models = new Map()
    return {
        "chat.params": async (input) => {
            models.set(input.sessionID, `${input.model.providerID}/${input.model.id}`)
        },
        "tool.execute.before": async (input, output) => {
            await recorder.before({ ...input, id: input.callID, input: output.args }, directory, models.get(input.sessionID))
        },
        "tool.execute.after": async (input) => {
            await recorder.after({ ...input, id: input.callID })
        },
        event: async ({ event }) => {
            const part = event.properties?.part
            // V1 does not call tool.execute.after for a failed tool.
            if (event.type === "message.part.updated" && part?.type === "tool" && part.state?.status === "error") {
                await recorder.after({ sessionID: part.sessionID, id: part.callID })
            }
            if (event.type === "session.deleted") {
                const id = event.properties?.info?.id
                if (id) { await recorder.clear(id); models.delete(id) }
            }
            if (event.type === "session.idle" || event.type === "session.error") {
                const id = event.properties?.sessionID
                if (id) await recorder.clear(id)
            }
        },
    }
}
