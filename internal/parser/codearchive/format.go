package codearchive

import "encoding/binary"

// archive/zip also accepts executables with appended archives and ignores
// several multi-disk fields. Enforce our single-volume, unprefixed format
// policy before letting it interpret member offsets and payloads.
//
// It returns the validated member count. The record checked here must be the
// one archive/zip reads: the last end-record signature, and ZIP64 data only when
// the end record carries archive/zip's ZIP64 sentinels.
func checkArchiveFormat(blob []byte) (uint64, error) {
	invalid := func() (uint64, error) { return 0, fail("ZIP_INVALID_FORMAT") }
	if len(blob) < 22 {
		return invalid()
	}
	le := binary.LittleEndian
	signature := le.Uint32(blob)
	if signature != 0x04034b50 && signature != 0x06054b50 {
		return invalid()
	}
	end := -1
	for at := len(blob) - 22; at >= max(0, len(blob)-22-65535); at-- {
		if le.Uint32(blob[at:]) == 0x06054b50 {
			end = at
			break
		}
	}
	if end < 0 || end+22+int(le.Uint16(blob[end+20:])) != len(blob) {
		return invalid()
	}
	eocd := blob[end:]
	if le.Uint16(eocd[4:]) != 0 || le.Uint16(eocd[6:]) != 0 || le.Uint16(eocd[8:]) != le.Uint16(eocd[10:]) {
		return invalid()
	}
	count := uint64(le.Uint16(eocd[10:]))
	size, offset := uint64(le.Uint32(eocd[12:])), uint64(le.Uint32(eocd[16:]))
	directoryEnd := uint64(end)
	zip64 := count == 0xffff || size == 0xffffffff || offset == 0xffffffff
	hasLocator := end >= 20 && le.Uint32(blob[end-20:]) == 0x07064b50
	if hasLocator && !zip64 {
		return invalid()
	}
	if hasLocator {
		locator := blob[end-20 : end]
		if le.Uint32(locator[4:]) != 0 || le.Uint32(locator[16:]) != 1 {
			return invalid()
		}
		at := le.Uint64(locator[8:])
		if at > uint64(end-20) || uint64(end-20)-at < 56 {
			return invalid()
		}
		record := blob[at : end-20]
		if le.Uint32(record) != 0x06064b50 || le.Uint64(record[4:]) != uint64(len(record)-12) || le.Uint32(record[16:]) != 0 || le.Uint32(record[20:]) != 0 || le.Uint64(record[24:]) != le.Uint64(record[32:]) {
			return invalid()
		}
		count, size, offset = le.Uint64(record[32:]), le.Uint64(record[40:]), le.Uint64(record[48:])
		directoryEnd = at
	} else if count == 0xffff || size == 0xffffffff || offset == 0xffffffff {
		return invalid()
	}
	if offset > directoryEnd || size != directoryEnd-offset || count > size/46 {
		return invalid()
	}
	// Member disk-start fields must also identify this volume, even for ignored
	// paths. The central directory must occupy its declared absolute position.
	at := offset
	for i := uint64(0); i < count; i++ {
		if directoryEnd-at < 46 {
			return invalid()
		}
		h := blob[at:directoryEnd]
		if le.Uint32(h) != 0x02014b50 || le.Uint16(h[34:]) != 0 {
			return invalid()
		}
		n := uint64(46) + uint64(le.Uint16(h[28:])) + uint64(le.Uint16(h[30:])) + uint64(le.Uint16(h[32:]))
		if n > directoryEnd-at {
			return invalid()
		}
		at += n
	}
	if at != directoryEnd {
		return invalid()
	}
	return count, nil
}
