package pcm

// G.711 is the telephone codec: eight kilohertz, one byte per sample, and two
// companding laws - µ-law in North America and Japan, A-law nearly everywhere
// else. It exists here because a telephone call reaching a realtime endpoint
// arrives in it, and the endpoint accepts it as one of its audio formats; the
// alternative is a caller's voice decoded to PCM and re-encoded for nothing.
//
// The tables are ITU-T G.711 as Sun's g711.c wrote them, which every softphone
// and PBX agrees with. Nothing is clever: the value of a codec this old is that
// there is exactly one right answer for every byte.

const (
	muLawBias = 0x84
	muLawClip = 32635
	aLawClip  = 32635
)

// segmentEnd marks the top of each companding segment.
var segmentEnd = [8]int32{0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF, 0x3FFF, 0x7FFF}

func segment(value int32) int32 {
	for index, end := range segmentEnd {
		if value <= end {
			return int32(index)
		}
	}
	return 8
}

// EncodeMuLawSample compands one 16-bit sample.
func EncodeMuLawSample(sample int16) byte {
	value := int32(sample)
	sign := int32(0)
	if value < 0 {
		value = -value
		sign = 0x80
	}
	if value > muLawClip {
		value = muLawClip
	}
	value += muLawBias
	exponent := segment(value)
	if exponent >= 8 {
		return byte(0x7F ^ sign)
	}
	mantissa := (value >> (exponent + 3)) & 0x0F
	return byte(^(sign | (exponent << 4) | mantissa))
}

// DecodeMuLawSample expands one µ-law byte.
func DecodeMuLawSample(value byte) int16 {
	value = ^value
	exponent := int32(value>>4) & 0x07
	mantissa := int32(value) & 0x0F
	sample := ((mantissa << 3) + muLawBias) << exponent
	sample -= muLawBias
	if value&0x80 != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

// EncodeALawSample compands one 16-bit sample.
func EncodeALawSample(sample int16) byte {
	value := int32(sample)
	sign := int32(0)
	if value >= 0 {
		sign = 0x80
	} else {
		value = -value - 1
		if value < 0 {
			value = 0
		}
	}
	if value > aLawClip {
		value = aLawClip
	}
	var encoded int32
	if value >= 256 {
		exponent := segment(value)
		mantissa := (value >> (exponent + 3)) & 0x0F
		encoded = (exponent << 4) | mantissa
	} else {
		encoded = value >> 4
	}
	return byte((encoded | sign) ^ 0x55)
}

// DecodeALawSample expands one A-law byte.
func DecodeALawSample(value byte) int16 {
	value ^= 0x55
	exponent := int32(value&0x70) >> 4
	mantissa := int32(value) & 0x0F
	var sample int32
	if exponent == 0 {
		sample = (mantissa << 4) + 8
	} else {
		sample = ((mantissa << 4) + 0x108) << (exponent - 1)
	}
	if value&0x80 == 0 {
		return int16(-sample)
	}
	return int16(sample)
}

// EncodeMuLaw compands little-endian 16-bit mono PCM. A trailing odd byte is
// ignored: it is not a sample.
func EncodeMuLaw(pcm16le []byte) []byte {
	out := make([]byte, len(pcm16le)/2)
	for index := range out {
		out[index] = EncodeMuLawSample(int16(uint16(pcm16le[index*2]) | uint16(pcm16le[index*2+1])<<8))
	}
	return out
}

// DecodeMuLaw expands µ-law bytes to little-endian 16-bit mono PCM.
func DecodeMuLaw(encoded []byte) []byte {
	out := make([]byte, len(encoded)*2)
	for index, value := range encoded {
		sample := uint16(DecodeMuLawSample(value))
		out[index*2] = byte(sample)
		out[index*2+1] = byte(sample >> 8)
	}
	return out
}

// EncodeALaw compands little-endian 16-bit mono PCM.
func EncodeALaw(pcm16le []byte) []byte {
	out := make([]byte, len(pcm16le)/2)
	for index := range out {
		out[index] = EncodeALawSample(int16(uint16(pcm16le[index*2]) | uint16(pcm16le[index*2+1])<<8))
	}
	return out
}

// DecodeALaw expands A-law bytes to little-endian 16-bit mono PCM.
func DecodeALaw(encoded []byte) []byte {
	out := make([]byte, len(encoded)*2)
	for index, value := range encoded {
		sample := uint16(DecodeALawSample(value))
		out[index*2] = byte(sample)
		out[index*2+1] = byte(sample >> 8)
	}
	return out
}
