pkg ./internal/app/
run ^TestHandOver_AVolumeNeverFedToTheDirectUnpack_DoesNotHoldTheJob$

# A finished download whose DirectUnpacker was not fed every volume of the set
# it is extracting does not hold the job at the hand-over: Wait ends the input,
# and the set fails for the volume that never arrived.

[a set past volume 1 waits for a later volume after the input ended]
file internal/directunpack/directunpack.go
--- anchor
		if d.inputEnded {
--- replace
		if vol == 1 && d.inputEnded {
--- end
