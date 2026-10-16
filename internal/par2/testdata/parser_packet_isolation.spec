pkg ./internal/par2/
run TestReadNextPacketWithOptions|TestParseFileDescriptions_MalformedLength|TestParsePar2Set_PacketLengthBoundary|TestParserUnexportedHelpersDirect

[validPackets check on ErrUnexpectedEOF neutered]
file internal/par2/parser.go
--- anchor
		if errors.Is(err, io.EOF) || (errors.Is(err, io.ErrUnexpectedEOF) && validPackets > 0) {
--- replace
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
--- end

[isValidPacketLen modulo-4 check neutered]
file internal/par2/parser.go
--- anchor
	return packetLen >= 64 && packetLen <= fileSize && packetLen%4 == 0
--- replace
	return packetLen >= 64 && packetLen <= fileSize
--- end

[isValidPacketLen min header size check neutered]
file internal/par2/parser.go
--- anchor
	return packetLen >= 64 && packetLen <= fileSize && packetLen%4 == 0
--- replace
	return packetLen <= fileSize && packetLen%4 == 0
--- end

[isValidPacketLen fileSize upper bound check neutered]
file internal/par2/parser.go
--- anchor
	return packetLen >= 64 && packetLen <= fileSize && packetLen%4 == 0
--- replace
	return packetLen >= 64 && packetLen%4 == 0
--- end

[invalid packetLen rescan neutered]
file internal/par2/parser.go
--- anchor
		if !isValidPacketLen(packetLen, fileSize) {
			slog.Warn("par2: invalid packet length, scanning for next packet", "length", packetLen, "file_size", fileSize)
			if _, scanErr := scanForMagicWithOptions(f, magic, opts.MaxJunkScanBytes); scanErr != nil {
				return [16]byte{}, nil, scanErr
			}
			continue
		}
--- replace
		if !isValidPacketLen(packetLen, fileSize) {
			return [16]byte{}, nil, fmt.Errorf("invalid packet length: %d", packetLen)
		}
--- end

[packet extending past EOF rescan neutered]
file internal/par2/parser.go
--- anchor
				if _, scanErr := scanForMagicWithOptions(f, magic, opts.MaxJunkScanBytes); scanErr != nil {
					return [16]byte{}, nil, scanErr
				}
				continue
--- replace
				return [16]byte{}, nil, err
--- end

[rewindToEmbeddedMagic in MD5 validation neutered]
file internal/par2/parser.go
--- anchor
		if !validatePacketMD5(header, body) {
			slog.Warn("par2: packet MD5 mismatch, scanning for next packet", "length", packetLen)
			if seekErr := rewindToEmbeddedMagic(f, fileSize, header, body); seekErr != nil {
				return [16]byte{}, nil, seekErr
			}
			continue
		}
--- replace
		if !validatePacketMD5(header, body) {
			continue
		}
--- end

[rewindToEmbeddedMagic boundary check neutered]
file internal/par2/parser.go
--- anchor
	if idx := bytes.Index(boundary[:63+boundOverlap], magic); idx >= 0 {
--- replace
	if idx := bytes.Index(boundary[:63+boundOverlap], magic); false {
--- end

[rewindToEmbeddedMagic body check neutered]
file internal/par2/parser.go
--- anchor
	if idx := bytes.Index(body, magic); idx >= 0 {
--- replace
	if idx := bytes.Index(body, magic); false {
--- end

[rewindToEmbeddedMagic tail prefix check neutered]
file internal/par2/parser.go
--- anchor
		if bytes.Equal(tail[tailLen-k:tailLen], magic[:k]) {
--- replace
		if false {
--- end

[rewindToEmbeddedMagic fileSize header fit check neutered]
file internal/par2/parser.go
--- anchor
		return curPos >= backtrack && uint64(curPos-backtrack)+64 <= fileSize
--- replace
		return curPos >= 0
--- end
