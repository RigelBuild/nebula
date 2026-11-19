---
"@cotal-ai/connector-core": patch
---

A DM to a principal key now sends when the manager does not serve the `roster` command, and the result says presence was not checked. The mechanism: the presence read fails with `not-found` only when the manager's describe lacks that command, so an older manager build no longer blocks DMs. Every other failed read still refuses, including a refused grant, a timeout and a transport error.
