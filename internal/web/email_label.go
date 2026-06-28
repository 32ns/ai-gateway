package web

import (
	"html/template"
	"regexp"
	"strings"
)

var emailLabelPattern = regexp.MustCompile(`([A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,})`)

func displayAccountLabel(label string) template.HTML {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}

	matches := emailLabelPattern.FindAllStringIndex(label, -1)
	if len(matches) == 0 {
		return template.HTML(template.HTMLEscapeString(label))
	}

	var b strings.Builder
	last := 0
	for _, match := range matches {
		if match[0] > last {
			b.WriteString(template.HTMLEscapeString(label[last:match[0]]))
		}

		email := label[match[0]:match[1]]
		local, domain, ok := strings.Cut(email, "@")
		if !ok {
			b.WriteString(template.HTMLEscapeString(email))
			last = match[1]
			continue
		}

		b.WriteString(`<span class="email-display"><span class="email-local">`)
		b.WriteString(template.HTMLEscapeString(local))
		b.WriteString(`</span><span class="email-at">@</span><span class="email-domain">`)
		b.WriteString(template.HTMLEscapeString(domain))
		b.WriteString(`</span></span>`)
		last = match[1]
	}

	if last < len(label) {
		b.WriteString(template.HTMLEscapeString(label[last:]))
	}
	return template.HTML(b.String())
}
