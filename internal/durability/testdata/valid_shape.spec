pkg ./internal/durability/
run TestWrittenRow_HasValidShape$

[the shape rule rejects a zero-length row]
file internal/durability/written.go
--- anchor
r.Offset >= 0 && r.Length >= 0 }
--- replace
r.Offset >= 0 && r.Length > 0 }
--- end

[the shape rule accepts a negative offset]
file internal/durability/written.go
--- anchor
r.Offset >= 0 && r.Length >= 0 }
--- replace
r.Length >= 0 }
--- end

[the shape rule accepts a negative length]
file internal/durability/written.go
--- anchor
r.Offset >= 0 && r.Length >= 0 }
--- replace
r.Offset >= 0 }
--- end
