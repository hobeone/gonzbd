pkg ./internal/decoder/
run TestBufferClass_Table|TestBufferClass_Properties|TestGetBuffer_CapacityIsBufferCap|TestPutBuffer_DropsOffClassCapacity

[a buffer of any capacity is pooled in the class its capacity rounds to]
file internal/decoder/decoder.go
--- anchor
	if size != c {
--- replace
	if size < 0 {
--- end

[requests are not rounded up to a class]
file internal/decoder/decoder.go
--- anchor
	size = (n + step - 1) / step * step
--- replace
	size = n
--- end
