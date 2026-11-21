---
"@cotal-ai/core": patch
"@cotal-ai/cli": patch
---

Core exports `isIssuedOneRouteRow`, which says whether a publish-allow grant row names the issued rail's `one` route. The CLI uses it to refuse an issued agent credential that reads the roster from a one-shot call, instead of matching the rail as a string of its own. Behaviour is unchanged.
