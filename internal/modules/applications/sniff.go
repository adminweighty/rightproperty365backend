package applications

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// Content types of the platform allowlist.
var kindTypes = map[string]string{
	FilePDF:  "application/pdf",
	FileJPG:  "image/jpeg",
	FilePNG:  "image/png",
	FileWEBP: "image/webp",
	FileHEIC: "image/heic",
	FileDOCX: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
}

var kindExtensions = map[string]string{
	FilePDF: ".pdf", FileJPG: ".jpg", FilePNG: ".png", FileWEBP: ".webp", FileHEIC: ".heic", FileDOCX: ".docx",
}

// Sniff identifies a file by its content, never by its name or the type the
// client claims (as onboarding documents are): one of FileKinds, or "" when
// it is none of them.
func Sniff(content []byte) string {
	switch http.DetectContentType(content) {
	case "application/pdf":
		return FilePDF
	case "image/jpeg":
		return FileJPG
	case "image/png":
		return FilePNG
	case "image/webp":
		return FileWEBP
	case "application/zip":
		if isDOCX(content) {
			return FileDOCX
		}
		return ""
	}
	if isHEIC(content) {
		return FileHEIC
	}
	return ""
}

// isHEIC recognises an ISO base media file whose major or compatible brand
// is a HEIF image brand.
func isHEIC(b []byte) bool {
	if len(b) < 16 || string(b[4:8]) != "ftyp" {
		return false
	}
	size := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if size < 16 || size > len(b) || size > 256 {
		return false
	}
	brands := map[string]bool{"heic": true, "heix": true, "heim": true, "heis": true, "hevc": true, "hevx": true, "mif1": true, "msf1": true}
	if brands[string(b[8:12])] {
		return true
	}
	for i := 16; i+4 <= size; i += 4 {
		if brands[string(b[i:i+4])] {
			return true
		}
	}
	return false
}

// isDOCX recognises a Word document: a zip holding [Content_Types].xml and
// word/document.xml. Macro-enabled documents (.docm) are not accepted.
func isDOCX(b []byte) bool {
	r, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return false
	}
	var types, document bool
	for _, f := range r.File {
		switch f.Name {
		case "[Content_Types].xml":
			types = true
			rc, err := f.Open()
			if err != nil {
				return false
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(io.LimitReader(rc, 64<<10))
			_ = rc.Close()
			if strings.Contains(buf.String(), "macroEnabled") {
				return false
			}
		case "word/document.xml":
			document = true
		case "word/vbaProject.bin":
			return false
		}
	}
	return types && document
}
