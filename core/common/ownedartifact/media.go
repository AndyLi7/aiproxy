package ownedartifact

import (
	"bytes"
	"strconv"
)

// sniffBytes is how much of a file's start DetectType reads.
const sniffBytes = 512

// mediaTypes are the only types a file is ever served as; every other file is
// application/octet-stream. Text formats (HTML, SVG, JSON) are never on it, so
// no archived file can run script in a browser even if a header is lost.
var mediaTypes = map[string]string{
	"image/png":       "png",
	"image/jpeg":      "jpg",
	"image/gif":       "gif",
	"image/webp":      "webp",
	"image/avif":      "avif",
	"image/heic":      "heic",
	"audio/mpeg":      "mp3",
	"audio/aac":       "aac",
	"audio/wav":       "wav",
	"audio/ogg":       "ogg",
	"audio/flac":      "flac",
	"audio/mp4":       "m4a",
	"video/mp4":       "mp4",
	"video/quicktime": "mov",
	"video/webm":      "webm",
}

// DetectType reads a file's media type from its first bytes, never from a
// provider's declared type or file name. An unrecognised file is "".
func DetectType(head []byte) string {
	starts := func(prefix string) bool { return bytes.HasPrefix(head, []byte(prefix)) }
	at := func(offset int, value string) bool {
		return len(head) >= offset+len(value) && string(head[offset:offset+len(value)]) == value
	}
	switch {
	case starts("\x89PNG\r\n\x1a\n"):
		return "image/png"
	case starts("\xff\xd8\xff"):
		return "image/jpeg"
	case starts("GIF87a"), starts("GIF89a"):
		return "image/gif"
	case starts("RIFF") && at(8, "WEBP"):
		return "image/webp"
	case starts("RIFF") && at(8, "WAVE"):
		return "audio/wav"
	case at(4, "ftyp") && len(head) >= 12:
		switch string(head[8:12]) {
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "mif1", "msf1":
			return "image/heic"
		case "M4A ", "M4B ", "M4P ", "F4A ", "F4B ":
			return "audio/mp4"
		case "qt  ":
			return "video/quicktime"
		}
		return "video/mp4"
	case starts("\x1a\x45\xdf\xa3"):
		if bytes.Contains(head[:min(len(head), 64)], []byte("webm")) {
			return "video/webm"
		}
		return ""
	case starts("OggS"):
		return "audio/ogg"
	case starts("fLaC"):
		return "audio/flac"
	case starts("ID3"):
		return "audio/mpeg"
	case len(head) >= 2 && head[0] == 0xff && head[1]&0xf6 == 0xf0:
		// ADTS: frame sync with layer bits 00.
		return "audio/aac"
	case len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0 && head[1]&0x06 != 0:
		return "audio/mpeg"
	}
	return ""
}

// ContentType is the header value for a receipt's type.
func ContentType(mediaType string) string {
	if _, ok := mediaTypes[mediaType]; ok {
		return mediaType
	}
	return "application/octet-stream"
}

// FileName names a delivered file after the task, never after the provider's
// file: <task>.<ext> for the first file, <task>-<index>.<ext> for the others.
func FileName(task string, index int, mediaType string) string {
	extension, ok := mediaTypes[mediaType]
	if !ok {
		extension = "bin"
	}
	if index > 0 {
		return task + "-" + strconv.Itoa(index) + "." + extension
	}
	return task + "." + extension
}

func knownType(mediaType string) bool {
	_, ok := mediaTypes[mediaType]
	return ok
}
