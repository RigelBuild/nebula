---
"@cotal-ai/core": patch
"@cotal-ai/connector-core": patch
"@cotal-ai/pi": patch
"@cotal-ai/manager": patch
"@cotal-ai/cli": patch
---

A session with the background publisher now reports `meta.background` as `running` or `idle` on its presence card. The idle case used to delete the key, so the wave relauncher could not tell an idle seat from an older omp with no publisher. `cotal ps --json` rows now carry `background: "idle"` for those seats too, and the key is absent only for a seat without the publisher, before its first read at session start, or an offline seat. The manager cluster revision moves to 54.
