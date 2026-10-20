package emailhtml

import (
	"strings"
	"testing"
)

func render(t *testing.T, raw string, links map[string]string) (string, *Document) {
	t.Helper()
	doc, err := Sanitize(raw)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	out, err := doc.Render(func(ref string) (string, bool) {
		link, ok := links[ref]
		return link, ok
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out, doc
}

// forbidden must never appear in output, whatever the input.
var forbidden = []string{
	"<script", "</script", "onerror", "onload", "onclick", "onmouseover", "javascript:", "vbscript:",
	"data:", "blob:", "file:", "<svg", "<math", "<iframe", "<frame", "<object", "<embed", "<applet",
	"<form", "<input", "<button", "<textarea", "<select", "<meta", "<base", "<link", "<style",
	"expression", "url(", "@import", "position", "z-index", "srcset", "background=", " id=", " class=",
	"evil.example", "alert(", "<video", "<audio", "<source", "formaction", "<template", "<noscript",
}

func TestSanitizeRemovesActiveAndExternalContent(t *testing.T) {
	corpus := map[string]string{
		"script":            `<p>hi</p><script>alert(1)</script>`,
		"script uppercase":  `<SCRIPT SRC=https://evil.example/x.js></SCRIPT>`,
		"event handler":     `<img src="cid:a" onerror="alert(1)"><b onclick="alert(1)">x</b>`,
		"javascript link":   `<a href="javascript:alert(1)">x</a><a href=" JaVaScRiPt:alert(1)">y</a>`,
		"entity javascript": `<a href="&#106;avascript:alert(1)">x</a>`,
		"vbscript link":     `<a href="vbscript:msgbox(1)">x</a>`,
		"data image":        `<img src="data:image/png;base64,AAAA"><a href="data:text/html,<script>alert(1)</script>">x</a>`,
		"svg":               `<svg onload="alert(1)"><script>alert(1)</script><a href="https://evil.example">x</a></svg>`,
		"math":              `<math><mtext><img src=x onerror=alert(1)></mtext></math>`,
		"iframe":            `<iframe src="https://evil.example"></iframe><frame src="https://evil.example">`,
		"object embed":      `<object data="https://evil.example/x.swf"></object><embed src="https://evil.example/x">`,
		"form":              `<form action="https://evil.example"><input name="p"><button formaction="https://evil.example">go</button></form>`,
		"meta refresh":      `<meta http-equiv="refresh" content="0;url=https://evil.example">`,
		"base":              `<base href="https://evil.example/"><a href="x">x</a>`,
		"link stylesheet":   `<link rel="stylesheet" href="https://evil.example/x.css">`,
		"style block":       `<style>body{background:url(https://evil.example/t.png)}</style><p>x</p>`,
		"css url":           `<p style="background-image:url(https://evil.example/t.png);color:red">x</p>`,
		"css expression":    `<p style="width:expression(alert(1))">x</p>`,
		"css import":        `<p style="@import 'https://evil.example/x.css'">x</p>`,
		"css position":      `<div style="position:fixed;top:0;left:0;z-index:9999">overlay</div>`,
		"css escape":        `<p style="background:u\72l(https://evil.example/t.png)">x</p>`,
		"css comment":       `<p style="color:red/**/;width:exp/**/ression(alert(1))">x</p>`,
		"remote image":      `<img src="https://evil.example/t.png"><img src="//evil.example/t.png">`,
		"srcset":            `<img src="cid:a" srcset="https://evil.example/t.png 2x">`,
		"background attr":   `<table background="https://evil.example/t.png"><tr><td>x</td></tr></table>`,
		"video audio":       `<video src="https://evil.example/v.mp4"><source src="https://evil.example/v.mp4"></video><audio src="https://evil.example/a.mp3"></audio>`,
		"id and class":      `<p id="app" class="modal">x</p>`,
		"nested unclosed":   `<div><p><b><i><a href="https://ok.example/">x<img src=x onerror=alert(1)`,
		"mutation":          `<noscript><p title="</noscript><img src=x onerror=alert(1)>"></noscript>`,
		"template":          `<template><img src=x onerror=alert(1)></template>`,
		"comment":           `<!--<img src=x onerror=alert(1)>-->`,
	}

	for name, raw := range corpus {
		t.Run(name, func(t *testing.T) {
			out, _ := render(t, raw, map[string]string{"a": "https://storage.example/a"})
			lower := strings.ToLower(out)
			for _, bad := range forbidden {
				if strings.Contains(lower, bad) {
					t.Errorf("output keeps %q: %s", bad, out)
				}
			}
		})
	}
}

func TestSanitizeFlagsBlockedExternalContent(t *testing.T) {
	for raw, want := range map[string]bool{
		`<img src="https://evil.example/t.png">`:               true,
		`<img src="//evil.example/t.png">`:                     true,
		`<p style="background:url(https://evil.example)">`:     true,
		`<style>p{background:url(x)}</style>`:                  true,
		`<table background="t.png"><tr><td></td></tr></table>`: true,
		`<iframe src="https://evil.example"></iframe>`:         true,
		`<img src="cid:a">`:                                    false,
		`<p style="color:red">plain</p>`:                       false,
		`<a href="https://ok.example/">link</a>`:               false,
	} {
		doc, err := Sanitize(raw)
		if err != nil {
			t.Fatal(err)
		}
		if doc.RemoteContentBlocked != want {
			t.Errorf("%s: blocked = %v, want %v", raw, doc.RemoteContentBlocked, want)
		}
	}
}

func TestSanitizeKeepsEmailFormatting(t *testing.T) {
	raw := `<table width="600" cellpadding="4" border="0" bgcolor="#ffffff" align="center">` +
		`<tr><td valign="top" colspan="2" style="color:#333333;font-family:Arial, sans-serif;font-size:14px;padding:8px 12px;border:1px solid rgb(0, 0, 0)">` +
		`<font color="red" face="Arial" size="3">Hello</font> <b>bold</b> <i>it</i> <u>u</u></td></tr></table>` +
		`<p style="display:block;text-align:right">right</p><ul><li>one</li></ul><blockquote>quoted</blockquote>`
	out, _ := render(t, raw, nil)
	for _, want := range []string{
		`width="600"`, `cellpadding="4"`, `bgcolor="#ffffff"`, `align="center"`, `valign="top"`, `colspan="2"`,
		`color: #333333`, `font-size: 14px`, `padding: 8px 12px`, `rgb(0, 0, 0)`,
		`<font color="red" face="Arial" size="3">`, `<b>bold</b>`, `<i>it</i>`, `<u>u</u>`,
		`display: block`, `text-align: right`, `<ul><li>one</li></ul>`, `<blockquote>quoted</blockquote>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatting %q lost: %s", want, out)
		}
	}
}

func TestSanitizeLinks(t *testing.T) {
	out, _ := render(t, `<a href="https://ok.example/page?a=1&b=2">web</a> <a href="mailto:x@y.com">mail</a> `+
		`<a href="/relative">rel</a> <a href="cid:a">cid</a> <a href="ftp://ok.example/">ftp</a>`, map[string]string{"a": "https://storage.example/a"})
	for _, want := range []string{`href="https://ok.example/page?a=1&amp;b=2"`, `target="_blank"`, `noopener`,
		`noreferrer`, `nofollow`, `href="mailto:x@y.com"`} {
		if !strings.Contains(out, want) {
			t.Errorf("link %q missing: %s", want, out)
		}
	}
	for _, bad := range []string{`/relative`, `cid:a`, `ftp:`, `storage.example`} {
		if strings.Contains(out, bad) {
			t.Errorf("link %q kept: %s", bad, out)
		}
	}
}

func TestRenderResolvesOnlyImageReferences(t *testing.T) {
	raw := `<img src="cid:logo@x" alt="logo"><img src="CID:logo%40x" alt="encoded">` +
		`<img src="cid:unknown" alt="unknown"><p style="color:red">cid:logo@x</p>`
	doc, err := Sanitize(raw)
	if err != nil {
		t.Fatal(err)
	}
	refs := doc.ContentIDReferences()
	if len(refs) != 3 || refs[0] != "logo@x" || refs[1] != "logo%40x" || refs[2] != "unknown" {
		t.Fatalf("references %v", refs)
	}

	out, err := doc.Render(func(ref string) (string, bool) {
		if MatchesContentID(ref, "logo@x") {
			return "https://storage.example/logo?sig=1", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, `src="https://storage.example/logo?sig=1"`) != 2 {
		t.Errorf("resolved images: %s", out)
	}
	if strings.Contains(out, "unknown\" src") || strings.Contains(out, `src="cid:`) {
		t.Errorf("an unresolved reference kept its source: %s", out)
	}
	if !strings.Contains(out, `alt="unknown"`) || !strings.Contains(out, "cid:logo@x</p>") {
		t.Errorf("alternative text or plain text lost: %s", out)
	}

	plain, _ := render(t, `<img src="cid:logo@x">`, nil)
	if strings.Contains(plain, "src=") {
		t.Errorf("an image without a link kept a source: %s", plain)
	}
}

func TestRenderRefusesASourceItDidNotIssue(t *testing.T) {
	out, _ := render(t, `<img src="cid:a">`, map[string]string{"a": "javascript:alert(1)"})
	if strings.Contains(out, "javascript") {
		t.Fatalf("a resolver value was trusted blindly: %s", out)
	}
}

func TestContentIDRules(t *testing.T) {
	for _, c := range []struct {
		value string
		ref   string
		ok    bool
	}{
		{"cid:a@b", "a@b", true},
		{"  CID:a@b ", "a@b", true},
		{"Cid:", "", true},
		{"https://x/cid:a", "", false},
		{"ci:a", "", false},
	} {
		ref, ok := CIDReference(c.value)
		if ref != c.ref || ok != c.ok {
			t.Errorf("%q: %q, %v", c.value, ref, ok)
		}
	}
	for _, c := range []struct {
		ref, contentID string
		want           bool
	}{
		{"a@b", "a@b", true},
		{"a%40b", "a@b", true},
		{"a+b", "a b", true},
		{"A@b", "a@b", false},
		{"", "", false},
		{"a@b", "", false},
	} {
		if got := MatchesContentID(c.ref, c.contentID); got != c.want {
			t.Errorf("%q vs %q: %v", c.ref, c.contentID, got)
		}
	}
}
