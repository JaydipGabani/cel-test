# Future Examples

These examples demonstrate **planned features that are not yet implemented**.
They are provided for design context only and **will not pass** with the current tool.

Once the corresponding phase ships, these examples will be moved to `examples/`
and updated to use the real environment and variable model.

| Directory | Feature | Phase | Status |
|---|---|---|---|
| `dra-gpu-selector/` | DRA device selector expressions | Phase 3 | Not yet implemented — uses `object.*` as placeholder; real DRA uses typed `device` variable with `device.attributes["domain"].key` |
