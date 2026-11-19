---
"@cotal-ai/connector-core": patch
---

A DM now sends when the manager does not serve the `roster` command: to a principal key, reported as presence not checked, and to a name the retained roster already holds, reported as last-known. The mechanism: the presence read fails with `not-found` only when the describe lacks that command. Every other failed read still refuses, including a refused grant, a timeout and a transport error. Under any failed read, a peer matched from the retained roster is labelled `last-known`, which drops the offline notice that a successful read would give.
