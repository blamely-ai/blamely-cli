// Blamely-managed OpenCode V2 plugin definition; no npm dependencies required.
import { createRecorder } from "./bridge.mjs"

export default {
    id: "blamely.opencode",
    async setup(ctx) {
        const recorder = createRecorder(2)
        await ctx.tool.hook("execute.before", async (event) => {
            try {
                const session = await ctx.session.get({ sessionID: event.sessionID })
                const model = session.model
                await recorder.before(event, session.location.directory, model ? `${model.providerID}/${model.id}` : "")
            } catch { /* Recording must not block OpenCode. */ }
        })
        await ctx.tool.hook("execute.after", (event) => recorder.after(event))
        return () => recorder.clear()
    },
}
