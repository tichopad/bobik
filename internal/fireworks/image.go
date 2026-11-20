package fireworks

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"slices"
)

// Check container headers and structure, not extensions. Decoding remains
// the provider's job, so large pixel counts do not allocate decoded images.
func detectImageType(b []byte) string {
	switch {
	case len(b) >= 45 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		if validPNG(b) {
			return "image/png"
		}
	case len(b) >= 14 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		if validGIF(b) {
			return "image/gif"
		}
	case len(b) >= 20 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		if uint64(binary.LittleEndian.Uint32(b[4:8])) == uint64(len(b)-8) && hasWebPImage(b[12:], true) {
			return "image/webp"
		}
	case len(b) >= 4 && bytes.HasPrefix(b, []byte{0xff, 0xd8}):
		if validJPEG(b) {
			return "image/jpeg"
		}
	}
	return ""
}

func validPNG(b []byte) bool {
	chunks := b[8:]
	if binary.BigEndian.Uint32(chunks[:4]) != 13 || string(chunks[4:8]) != "IHDR" ||
		binary.BigEndian.Uint32(b[16:20]) == 0 || binary.BigEndian.Uint32(b[20:24]) == 0 ||
		!validPNGDepth(b[25], b[24]) || b[26] != 0 || b[27] != 0 || b[28] > 1 {
		return false
	}
	imageData := false
	for len(chunks) >= 12 {
		size := binary.BigEndian.Uint32(chunks[:4])
		if uint64(size) > uint64(len(chunks)-12) {
			return false
		}
		end := 8 + int(size)
		if crc32.ChecksumIEEE(chunks[4:end]) != binary.BigEndian.Uint32(chunks[end:end+4]) {
			return false
		}
		kind := string(chunks[4:8])
		chunks = chunks[end+4:]
		if kind == "IDAT" && size > 0 {
			imageData = true
		}
		if kind == "IEND" {
			return size == 0 && imageData && len(chunks) == 0
		}
	}
	return false
}

func validPNGDepth(color, depth byte) bool {
	switch color {
	case 0:
		return slices.Contains([]byte{1, 2, 4, 8, 16}, depth)
	case 2, 4, 6:
		return depth == 8 || depth == 16
	case 3:
		return slices.Contains([]byte{1, 2, 4, 8}, depth)
	default:
		return false
	}
}

func validGIF(b []byte) bool {
	if binary.LittleEndian.Uint16(b[6:8]) == 0 || binary.LittleEndian.Uint16(b[8:10]) == 0 ||
		b[len(b)-1] != 0x3b {
		return false
	}
	offset := 13 + gifColorTableSize(b[10])
	imageData := false
	skipBlocks := func() bool {
		for offset < len(b) {
			size := int(b[offset])
			offset++
			if size == 0 {
				return true
			}
			offset += size
		}
		return false
	}
	for offset < len(b) {
		marker := b[offset]
		offset++
		switch marker {
		case 0x3b: // trailer
			return imageData && offset == len(b)
		case 0x21: // extension label and sub-blocks
			offset++
			if !skipBlocks() {
				return false
			}
		case 0x2c: // image descriptor and LZW data
			if offset+9 >= len(b) || binary.LittleEndian.Uint16(b[offset+4:offset+6]) == 0 ||
				binary.LittleEndian.Uint16(b[offset+6:offset+8]) == 0 {
				return false
			}
			offset += 9 + gifColorTableSize(b[offset+8])
			if offset+1 >= len(b) || b[offset] < 2 || b[offset] > 8 || b[offset+1] == 0 {
				return false
			}
			offset++
			if !skipBlocks() {
				return false
			}
			imageData = true
		default:
			return false
		}
	}
	return false
}

func gifColorTableSize(flags byte) int {
	if flags&0x80 != 0 {
		return 3 * (1 << ((flags & 7) + 1))
	}
	return 0
}

func validJPEG(b []byte) bool {
	if b[len(b)-2] != 0xff || b[len(b)-1] != 0xd9 {
		return false
	}
	frame := false
	for offset := 2; offset+4 <= len(b) && b[offset] == 0xff; {
		marker := b[offset+1]
		size := int(binary.BigEndian.Uint16(b[offset+2 : offset+4]))
		end := offset + 2 + size
		if size < 2 || end > len(b) {
			return false
		}
		switch marker {
		case 0xc0, 0xc1, 0xc2: // supported frame variants
			if size >= 11 && size == 8+3*int(b[offset+9]) &&
				binary.BigEndian.Uint16(b[offset+5:offset+7]) != 0 &&
				binary.BigEndian.Uint16(b[offset+7:offset+9]) != 0 {
				frame = true
			}
		case 0xda: // scan header and at least one byte before the end marker
			return frame && size >= 8 && size == 6+2*int(b[offset+4]) && end < len(b)-2
		}
		offset = end
	}
	return false
}

func hasWebPImage(chunks []byte, allowFrames bool) bool {
	imageData := false
	for len(chunks) >= 8 {
		kind := string(chunks[:4])
		size := binary.LittleEndian.Uint32(chunks[4:8])
		padded := uint64(size) + uint64(size%2)
		if padded > uint64(len(chunks)-8) {
			return false
		}
		data := chunks[8 : 8+int(size)]
		chunks = chunks[8+int(padded):]
		switch kind {
		case "VP8 ":
			if size >= 10 && bytes.Equal(data[3:6], []byte{0x9d, 0x01, 0x2a}) &&
				binary.LittleEndian.Uint16(data[6:8])&0x3fff != 0 &&
				binary.LittleEndian.Uint16(data[8:10])&0x3fff != 0 {
				imageData = true
			}
		case "VP8L":
			if size >= 5 && data[0] == 0x2f && data[4]&0xe0 == 0 {
				imageData = true
			}
		case "ANMF":
			if allowFrames && size > 24 && hasWebPImage(data[16:], false) {
				imageData = true
			}
		}
	}
	return imageData && len(chunks) == 0
}
