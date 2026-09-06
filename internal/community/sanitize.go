package community

import (
	"html"
	"regexp"
	"strings"
)

var (
	htmlTagRegex    = regexp.MustCompile(`(?i)<[^>]*>`)
	markdownImgRegex = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
)

// SanitizeContent escapes raw HTML and neutralizes remote tracking images.
func SanitizeContent(content string) string {
	// 1. Remove remote tracking markdown images: ![alt](http://...) -> [Image: alt]
	cleaned := markdownImgRegex.ReplaceAllStringFunc(content, func(match string) string {
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

	// 2. Strip any raw HTML tags
	cleaned = htmlTagRegex.ReplaceAllString(cleaned, "")

	// 3. Escape HTML special characters
	cleaned = html.EscapeString(cleaned)

	return cleaned
}
