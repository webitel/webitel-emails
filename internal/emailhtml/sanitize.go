package emailhtml

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Document is a sanitized body whose only URLs are allowed links and "cid:" image
// references, which Render resolves or removes.
type Document struct {
	body *html.Node
	// RemoteContentBlocked tells that the original referenced content outside
	// the email, such as remote images, which was removed.
	RemoteContentBlocked bool
	// RemovedElements and RemoteReferences count what sanitizing took away, for
	// aggregate logging only.
	RemovedElements  int
	RemoteReferences int
}

// Sanitize inspects the original for external content, then cleans it by an explicit
// allowlist and parses the result for Render.
func Sanitize(raw string) (*Document, error) {
	elements, remote, err := inspectOriginal(raw)
	if err != nil {
		return nil, err
	}

	clean := emailPolicy.Sanitize(raw)
	nodes, err := html.ParseFragment(strings.NewReader(clean), bodyContext())
	if err != nil {
		return nil, err
	}
	body := bodyContext()
	for _, node := range nodes {
		body.AppendChild(node)
	}

	removed := elements - countElements(body) + 1 // the body context is not content
	if removed < 0 {
		removed = 0
	}

	return &Document{
		body:                 body,
		RemoteContentBlocked: remote > 0,
		RemovedElements:      removed,
		RemoteReferences:     remote,
	}, nil
}

// ContentIDReferences returns the distinct "cid:" references of the images, in
// document order. Only an image source may refer to an inline part.
func (d *Document) ContentIDReferences() []string {
	var refs []string
	seen := map[string]bool{}
	walk(d.body, func(n *html.Node) {
		if n.DataAtom != atom.Img {
			return
		}
		if ref, ok := CIDReference(attr(n, "src")); ok && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	})

	return refs
}

// Render links the inline images through resolve; a final pass removes any link or
// image source this function did not allow or issue.
func (d *Document) Render(resolve func(reference string) (string, bool)) (string, error) {
	issued := map[string]bool{}
	walk(d.body, func(n *html.Node) {
		if n.DataAtom != atom.Img {
			return
		}
		ref, ok := CIDReference(attr(n, "src"))
		if !ok {
			removeAttr(n, "src")
			return
		}
		link, ok := "", false
		if resolve != nil {
			link, ok = resolve(ref)
		}
		// Even an issued link must be an absolute web URL.
		if !ok || !allowedImageLink(link) {
			removeAttr(n, "src")
			return
		}
		setAttr(n, "src", link)
		issued[link] = true
	})

	walk(d.body, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			switch {
			case a.Key == "href":
				if n.DataAtom != atom.A || !allowedLink(a.Val) {
					continue
				}
			case a.Key == "src":
				if n.DataAtom != atom.Img || !issued[a.Val] {
					continue
				}
			case a.Key == "style":
				if strings.Contains(strings.ToLower(a.Val), "url(") {
					continue
				}
			}
			kept = append(kept, a)
		}
		n.Attr = kept
	})

	var out bytes.Buffer
	for child := d.body.FirstChild; child != nil; child = child.NextSibling {
		if err := html.Render(&out, child); err != nil {
			return "", err
		}
	}

	return out.String(), nil
}

// inspectOriginal counts the content elements of the whole document and its
// references to content outside the email.
func inspectOriginal(raw string) (elements, remote int, err error) {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return 0, 0, err
	}

	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.DataAtom {
		case atom.Html, atom.Head, atom.Body:
		default:
			elements++
		}
		for _, a := range n.Attr {
			if referencesRemote(n, a) {
				remote++
			}
		}
		if n.DataAtom == atom.Style {
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.TextNode && remoteCSS(child.Data) {
					remote++
				}
			}
		}
	})

	return elements, remote, nil
}

func referencesRemote(n *html.Node, a html.Attribute) bool {
	value := strings.TrimSpace(a.Val)
	switch key := strings.ToLower(a.Key); {
	case value == "":
		return false
	case key == "href" && a.Namespace == "" && (n.DataAtom == atom.A || n.DataAtom == atom.Area):
		return false
	case key == "srcset" || key == "background":
		return true
	case key == "style":
		return remoteCSS(value)
	case key == "src" || key == "poster" || key == "data" || key == "href":
		// Any other href loads or bases something (link, base, svg image or
		// use), even a relative one; only a fragment stays in the document.
		_, inline := CIDReference(value)
		return !inline && !strings.HasPrefix(value, "#")
	case textAttribute(key):
		return false
	default:
		return remoteURL(value)
	}
}

// textAttribute is shown or kept as text and never loaded, even if it holds a
// URL.
func textAttribute(key string) bool {
	switch key {
	case "title", "alt", "value", "placeholder", "label":
		return true
	}

	return strings.HasPrefix(key, "data-") || strings.HasPrefix(key, "aria-")
}

func remoteURL(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"http:", "https:", "ftp:", "//"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}

	return false
}

func remoteCSS(value string) bool {
	lower := strings.ToLower(value)

	return strings.Contains(lower, "url(") || strings.Contains(lower, "@import") || strings.Contains(lower, "image-set(")
}

func countElements(root *html.Node) int {
	count := 0
	walk(root, func(n *html.Node) {
		if n.Type == html.ElementNode {
			count++
		}
	})

	return count
}

func allowedImageLink(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)

	return (scheme == "https" || scheme == "http") && u.Host != ""
}

func allowedLink(value string) bool {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host != ""
	case "mailto":
		return true
	default:
		return false
	}
}

var (
	// 0 to 2000 pixels or 0 to 100 percent.
	sizeValue  = regexp.MustCompile(`^(?:(?:[0-9]{1,3}|1[0-9]{3}|2000)(?:px)?|(?:[0-9]{1,2}|100)%)$`)
	smallInt   = regexp.MustCompile(`^[0-9]{1,3}$`)
	colorValue = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{1,20})$`)
	linkValue  = regexp.MustCompile(`(?i)^\s*(https?://|mailto:)`)
	cidValue   = regexp.MustCompile(`(?i)^\s*cid:`)
)

// styleProperties may appear in a style attribute; positioning, transforms, opacity
// and visibility are left out so a message cannot cover the interface.
var styleProperties = []string{
	"color", "background", "background-color",
	"font", "font-family", "font-size", "font-style", "font-weight", "font-variant",
	"text-align", "text-decoration", "text-transform", "text-indent",
	"letter-spacing", "word-spacing", "line-height", "white-space", "vertical-align", "direction",
	"margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
	"padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
	"border", "border-top", "border-right", "border-bottom", "border-left",
	"border-width", "border-style", "border-color", "border-collapse", "border-spacing", "border-radius",
	"width", "height", "max-width", "min-width", "max-height", "min-height",
	"list-style-type", "float", "clear", "table-layout",
}

// safeStyleValue accepts plain values only: no function but colors, no escape,
// no comment and nothing that loads or runs anything.
func safeStyleValue(value string) bool {
	lower := strings.ToLower(value)
	if len(value) > 200 {
		return false
	}
	for _, refused := range []string{"url(", "expression", "javascript", "vbscript", "@import", "\\", "/*", "<", ">", "&"} {
		if strings.Contains(lower, refused) {
			return false
		}
	}
	for _, r := range lower {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune(" #.,%()'\"!-_+/", r)) {
			return false
		}
	}
	// Every function must be a color function.
	for i, r := range lower {
		if r != '(' {
			continue
		}
		before := strings.TrimRight(lower[:i], " ")
		fn := before[strings.LastIndexAny(before, " ,(")+1:]
		if fn != "rgb" && fn != "rgba" && fn != "hsl" && fn != "hsla" {
			return false
		}
	}

	return true
}

var emailPolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()

	p.AllowElements(
		"a", "abbr", "b", "blockquote", "br", "caption", "center", "code", "col", "colgroup",
		"dd", "div", "dl", "dt", "em", "font", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "img",
		"li", "ol", "p", "pre", "s", "small", "span", "strike", "strong", "sub", "sup",
		"table", "tbody", "td", "tfoot", "th", "thead", "tr", "u", "ul",
	)
	// The content of these is code, markup for another renderer or a document
	// of its own, never text a reader should see.
	p.SkipElementsContent(
		"script", "style", "title", "head", "noscript", "template", "svg", "math",
		"iframe", "frame", "frameset", "object", "embed", "applet", "noembed", "noframes",
		"select", "textarea", "button", "video", "audio",
	)

	p.RequireParseableURLs(true)
	p.AllowURLSchemes("http", "https", "mailto", "cid")
	p.AllowAttrs("href").Matching(linkValue).OnElements("a")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	p.AllowAttrs("src").Matching(cidValue).OnElements("img")
	p.AllowAttrs("alt").OnElements("img")
	p.AllowAttrs("width", "height").Matching(sizeValue).OnElements("img", "table", "td", "th", "col", "colgroup", "hr")
	p.AllowAttrs("border", "cellpadding", "cellspacing").Matching(smallInt).OnElements("table")
	p.AllowAttrs("colspan", "rowspan").Matching(smallInt).OnElements("td", "th")
	p.AllowAttrs("bgcolor").Matching(colorValue).OnElements("table", "tr", "td", "th")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`(?i)^(left|right|center|justify)$`)).Globally()
	p.AllowAttrs("valign").Matching(regexp.MustCompile(`(?i)^(top|middle|bottom|baseline)$`)).
		OnElements("td", "th", "tr", "tbody", "thead", "tfoot", "col", "colgroup")
	p.AllowAttrs("color").Matching(colorValue).OnElements("font")
	p.AllowAttrs("face").Matching(regexp.MustCompile(`^[\w ,'"-]{1,100}$`)).OnElements("font")
	p.AllowAttrs("size").Matching(regexp.MustCompile(`^[+-]?[1-7]$`)).OnElements("font")
	p.AllowAttrs("start").Matching(smallInt).OnElements("ol")
	p.AllowAttrs("dir").Matching(regexp.MustCompile(`(?i)^(ltr|rtl|auto)$`)).Globally()
	p.AllowAttrs("title").Globally()

	for _, property := range styleProperties {
		check, bounded := boundedStyles[property]
		if !bounded {
			check = safeStyleValue
		}
		p.AllowStyles(property).MatchingHandler(check).Globally()
	}
	p.AllowStyles("display").
		MatchingEnum("block", "inline", "inline-block", "table", "table-row", "table-cell", "list-item").Globally()

	return p
}()

func bodyContext() *html.Node {
	return &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}

	return ""
}

func setAttr(n *html.Node, key, value string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: value})
}

func removeAttr(n *html.Node, key string) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			kept = append(kept, a)
		}
	}
	n.Attr = kept
}
