pkg ./internal/config/
run TestExampleConfigLoads|TestValidate_KeyErrorNamesARealRemedy

[the example ships an empty api_key]
file gonzbd.yaml.example
--- anchor
  api_key: 0123456789abcdef
--- replace
  api_key: ""
--- end

[the key refusal names the nonexistent gonzbd init]
file internal/config/validate.go
--- anchor
const keyRemedy = "must be 16 lowercase hex characters; generate one with `openssl rand -hex 8`"
--- replace
const keyRemedy = "must be 16 lowercase hex characters; regenerate via `gonzbd init`"
--- end
