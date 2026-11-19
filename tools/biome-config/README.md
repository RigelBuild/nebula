# @rigelbuild/biome-config

Shared Biome preset for nebula tools. It is not published to npm. Tools in this
repository extend it from source by relative path:

```json
{ "extends": ["../biome-config/biome.json"] }
```

`biome.json` is the preset. `shared-configs.test.ts` checks its behaviour.
