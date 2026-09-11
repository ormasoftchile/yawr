# Package resolution vectors

`tv-pkg-resolve.yaml` was consolidated from `yawr-private`
commit `1990a6c7749dc6ee109d1e197815d6f2ed83d122`.

It uses the shared schema at `../enumdata/vector.schema.json`; the identical
private schema copy was deliberately deduplicated. The corpus integrity test
validates every stable ID and rejects differing
collisions. Runtime package-resolution tests provide executable evidence for
the currently adopted tool-package behavior. Vectors whose historical schema
shape differs from the executable v1 model remain contract evidence rather
than silently changing runtime behavior.
