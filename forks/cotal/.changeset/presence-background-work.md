---
"@cotal-ai/core": patch
"@cotal-ai/connector-core": patch
"@cotal-ai/pi": patch
"@cotal-ai/manager": patch
"@cotal-ai/cli": patch
---

A session with running background jobs or subagents now shows `busy` on its presence card, so the wave relauncher can avoid relaunching it. The card carries `meta.background = "running"` while work runs and drops the key when it stops; `cotal ps --json` rows carry the same `background` field.
