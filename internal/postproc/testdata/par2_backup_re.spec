pkg ./internal/postproc/
run TestCleanupPar2Backups_ArbitraryExtensionsAndNoZeroPaddedSplit

[par2BackupRe restricted to hardcoded extension list]
file internal/postproc/stage_repair.go
--- anchor
var par2BackupRe = regexp.MustCompile(`(?i)\.[a-z0-9]{1,10}\.[1-9]\d{0,2}$`)
--- replace
var par2BackupRe = regexp.MustCompile(`(?i)\.(rar|r\d+|7z|zip|mkv|avi|mp4|flac|mp3|srt|nfo)\.[1-9]\d{0,2}$`)
--- end

[par2BackupRe accepts zero-padded split suffix]
file internal/postproc/stage_repair.go
--- anchor
var par2BackupRe = regexp.MustCompile(`(?i)\.[a-z0-9]{1,10}\.[1-9]\d{0,2}$`)
--- replace
var par2BackupRe = regexp.MustCompile(`(?i)\.[a-z0-9]{1,10}\.\d{1,3}$`)
--- end
