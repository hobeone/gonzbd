pkg ./internal/job/
run TestIsRecoveryVolume

[a zero block count is accepted again]
file internal/job/par2name.go
--- anchor
\+(0*[1-9]\d*)
--- replace
\+(\d+)
--- end

[the minus form excludes zero too]
file internal/job/par2name.go
--- anchor
(?:-\d+|
--- replace
(?:-0*[1-9]\d*|
--- end
