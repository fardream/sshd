package sshd

import (
	"encoding/binary"
	"fmt"
)

func parseString(
	b []byte,
) (
	result string,
	consumed int,
	err error,
) {
	if len(b) < 4 {
		return "", 0, fmt.Errorf("number of bytes in less than 4: %d", len(b))
	}

	l := int(binary.BigEndian.Uint32(b[:4]))
	if len(b) < l+4 {
		return "", 0, fmt.Errorf("string length is %d, but input only has %d bytes (including the 4 bytes length prefix)", l, len(b))
	}

	result = string(b[4 : l+4])
	consumed = l + 4

	return result, consumed, nil
}

func parseWindowSize(b []byte) (
	widthCharacter uint32,
	heightCharacter uint32,
	widthPixel uint32,
	heightPixel uint32,
	err error,
) {
	if len(b) < 16 {
		return 0, 0, 0, 0, fmt.Errorf("number of bytes is less than 16: %d", len(b))
	}

	widthCharacter = binary.BigEndian.Uint32(b[0:4])
	heightCharacter = binary.BigEndian.Uint32(b[4:8])
	widthPixel = binary.BigEndian.Uint32(b[8:12])
	heightPixel = binary.BigEndian.Uint32(b[12:16])

	return
}
