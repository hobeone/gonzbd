pkg ./internal/app/
run Test(ReadBackFile_VerifiesAZeroLengthRow|VerifyJobFiles_Outcomes)$

# A zero-length article's row is valid at a restart, as it is live: the
# read-back verifies it against CRC 0 and keeps it out of the intersection
# resolution, since it claims no range.

[the read-back drops a zero-length row as it once did]
file internal/app/verify.go
--- anchor
		case !r.HasValidShape():
--- replace
		case !r.HasValidShape() || r.Length == 0:
--- end

[a zero-length row joins the intersection resolution]
file internal/app/verify.go
--- anchor
		case r.Length == 0:
--- replace
		case false:
--- end

[a zero-length row is verified without comparing its CRC]
file internal/app/verify.go
--- anchor
		if ok {
			out.verified = append(out.verified, r)
--- replace
		if ok || true {
			out.verified = append(out.verified, r)
--- end
