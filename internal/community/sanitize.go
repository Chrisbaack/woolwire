package community

import (
	"regexp"
	"strings"
)

var markdownImgRegex = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// SanitizeContent neutralizes remote tracking images and nothing else.
//
// Escaping is deliberately not done here: the web client renders message text
// through React, which escapes on output. Escaping a second time on the way in
// turned "&" into "&amp;" on screen and permanently corrupted the stored text.
// Stripping anything that looks like a tag was worse still — it deleted "<3"
// and mangled "a < b > c" in ordinary prose.
//
// If Markdown rendering is added later, sanitize the rendered tree with a real
// HTML sanitizer rather than reintroducing regexes over the source text.
func SanitizeContent(content string) string {
	return markdownImgRegex.ReplaceAllStringFunc(content, func(match string) string {
		submatches := markdownImgRegex.FindStringSubmatch(match)
		if len(submatches) >= 2 {
			alt := strings.TrimSpace(submatches[1])
			if alt == "" {
				alt = "Blocked Remote Image"
			}
			return "[Image: " + alt + "]"
		}
		return "[Image Blocked]"
	})
}
