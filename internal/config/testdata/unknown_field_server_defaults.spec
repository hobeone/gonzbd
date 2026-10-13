pkg ./internal/config/
run TestDecode_UnknownFieldStillDefaultsServers$

[early return on unknown YAML fields skips decodeServers]
file internal/config/loader.go
--- anchor
		if fatal != nil {
			return nil, unknowns, wrapYAMLError(fatal, b)
		}
	}
--- replace
		if fatal != nil {
			return nil, unknowns, wrapYAMLError(fatal, b)
		}
		return cfg, unknowns, nil
	}
--- end
