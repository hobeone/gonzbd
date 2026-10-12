pkg ./internal/config/
run TestPostProcConfig_PermissionsRequireOwnerRWX

[the owner-rwx check neutered]
file internal/config/validate.go
--- anchor
mode&0o700 != 0o700 {
--- replace
mode&0o700 != 0o700 && false {
--- end

[owner read no longer required]
file internal/config/validate.go
--- anchor
mode&0o700 != 0o700 {
--- replace
mode&0o300 != 0o300 {
--- end

[owner execute no longer required]
file internal/config/validate.go
--- anchor
mode&0o700 != 0o700 {
--- replace
mode&0o600 != 0o600 {
--- end

[owner write no longer required]
file internal/config/validate.go
--- anchor
mode&0o700 != 0o700 {
--- replace
mode&0o500 != 0o500 {
--- end
