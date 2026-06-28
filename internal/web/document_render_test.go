package web

import (
	"strings"
	"testing"
)

func TestRenderDocumentMarkdownPreservesWindowsPaths(t *testing.T) {
	html := string(renderDocumentMarkdown("在 C:\\Users\\<your-username>\\.codex 路径下，新建 config.toml"))

	if !strings.Contains(html, `C:\Users\&lt;your-username&gt;\.codex`) {
		t.Fatalf("rendered document should preserve Windows path backslashes: %s", html)
	}
}

func TestEscapeDocumentWindowsPathsSkipsCode(t *testing.T) {
	body := strings.Join([]string{
		"普通文本 C:\\Users\\admin\\.codex",
		"行内 `C:\\Users\\admin\\.codex` 保持原样",
		"    C:\\Users\\admin\\.codex",
		"```",
		"C:\\Users\\admin\\.codex",
		"```",
	}, "\n")

	got := escapeDocumentWindowsPaths(body)

	if !strings.Contains(got, `普通文本 C:\\Users\\admin\\.codex`) {
		t.Fatalf("plain Windows path was not escaped: %q", got)
	}
	if !strings.Contains(got, "`C:\\Users\\admin\\.codex`") {
		t.Fatalf("inline code Windows path should stay raw: %q", got)
	}
	if !strings.Contains(got, "    C:\\Users\\admin\\.codex") {
		t.Fatalf("indented code Windows path should stay raw: %q", got)
	}
	if !strings.Contains(got, "```\nC:\\Users\\admin\\.codex\n```") {
		t.Fatalf("fenced code Windows path should stay raw: %q", got)
	}
}

func TestRenderDocumentMarkdownKeepsExistingMarkdown(t *testing.T) {
	html := string(renderDocumentMarkdown("[配置目录](https://example.com/docs) 和 **加粗**"))

	if !strings.Contains(html, `<a href="https://example.com/docs"`) || !strings.Contains(html, `<strong>加粗</strong>`) {
		t.Fatalf("rendered document should preserve normal markdown features: %s", html)
	}
}
