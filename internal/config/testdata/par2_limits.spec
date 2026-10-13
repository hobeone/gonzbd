pkg ./internal/config/
run ^(TestUIKeywordsAcceptTheirOwnValue|TestPostProcConfig_Par2LimitsValidation)$

[Set cannot convert an int64 field]
file internal/config/set.go
--- anchor
	case f.Kind() == reflect.Int64:
--- replace
	case false:
--- end

[the packet body floor is not enforced]
file internal/config/validate.go
--- anchor
	if p.Par2MaxPacketBodySize != 0 && p.Par2MaxPacketBodySize < constants.MinPar2PacketBodySize {
--- replace
	if false {
--- end

[the junk scan floor is not enforced]
file internal/config/validate.go
--- anchor
	if p.Par2MaxJunkScanBytes != 0 && p.Par2MaxJunkScanBytes < constants.MinPar2JunkScanBytes {
--- replace
	if false {
--- end
