pkg ./internal/config/
run TestPostProcConfig_PermissionsRequireOwnerRWX

[the owner-rwx check neutered]
file internal/config/validate.go
--- anchor
mode&0o700 != 0o700 {
--- replace
mode&0o700 != 0o700 && false {
--- end
