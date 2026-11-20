---
"@cotal-ai/cli": patch
---

`cotal send --creds` proves a credential's display name from a public pins file beside the creds file, `<creds>.pins.json`, on a host with no meshes registry and no space seeds. The pins hold the space's account and signing public keys and its account JWT, and never a seed. A registered space's seed record is final: when it refuses a credential, the send falls back to `login@host` and never reads the pins. The pins file is used only when no seed record exists. Whoever writes the pins file chooses the trust anchor, and the pins path has no operator key of its own; its account JWT must name the pinned account and signing key, and its revocation, expiry and validity window are checked. A malformed pins file falls back without failing the send.
