package rpmmd

import (
	"bytes"
	"fmt"
	"strings"
)

// The repodata XML is written by hand rather than through encoding/xml.
//
// Go's encoder cannot produce self-closing elements, does not let attribute
// order be controlled, and escapes newlines inside text as character
// references — all of which would make our output differ from createrepo_c's
// byte for byte. Writing it directly keeps the golden tests strict, and a
// strict comparison against the canonical tool is the strongest correctness
// signal available to us.

// An attr is one XML attribute. Attributes are emitted in the order given.
type attr struct {
	name  string
	value string
	omit  bool // drop the attribute entirely
}

// at returns an attribute that is always emitted.
func at(name, value string) attr { return attr{name: name, value: value} }

// atNum returns an integer attribute.
func atNum[T int | int64](name string, value T) attr {
	return attr{name: name, value: fmt.Sprint(value)}
}

// atOpt returns an attribute that is dropped when its value is empty, for the
// places where createrepo_c omits rather than empties an attribute.
func atOpt(name, value string) attr {
	return attr{name: name, value: value, omit: value == ""}
}

// A writer builds an indented XML document.
type writer struct {
	buf bytes.Buffer
}

const indentUnit = "  "

func (w *writer) indent(depth int) {
	w.buf.WriteString(strings.Repeat(indentUnit, depth))
}

func (w *writer) attrs(as []attr) {
	for _, a := range as {
		if a.omit {
			continue
		}
		fmt.Fprintf(&w.buf, " %s=%q", a.name, escapeAttr(a.value))
	}
}

// decl writes the XML declaration.
func (w *writer) decl() {
	w.buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
}

// open writes a start tag on its own line.
func (w *writer) open(depth int, name string, as ...attr) {
	w.indent(depth)
	w.buf.WriteString("<" + name)
	w.attrs(as)
	w.buf.WriteString(">\n")
}

// close writes an end tag on its own line.
func (w *writer) close(depth int, name string) {
	w.indent(depth)
	w.buf.WriteString("</" + name + ">\n")
}

// empty writes a self-closing element.
func (w *writer) empty(depth int, name string, as ...attr) {
	w.indent(depth)
	w.buf.WriteString("<" + name)
	w.attrs(as)
	w.buf.WriteString("/>\n")
}

// text writes an element with a text body on one line. An empty body still
// produces <name></name>, which is what createrepo_c emits for a package with
// no packager or URL.
func (w *writer) text(depth int, name, body string, as ...attr) {
	w.indent(depth)
	w.buf.WriteString("<" + name)
	w.attrs(as)
	w.buf.WriteString(">")
	w.buf.WriteString(escapeText(body))
	w.buf.WriteString("</" + name + ">\n")
}

// document finishes the document and returns it. createrepo_c writes no
// trailing newline after the root element's closing tag, and matching that is
// what makes a byte-for-byte comparison possible.
func (w *writer) document() []byte {
	return bytes.TrimSuffix(w.buf.Bytes(), []byte("\n"))
}

// escapeText escapes the three characters libxml2 escapes in element content.
// Newlines are left alone: a package description spans lines in the output.
func escapeText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeAttr escapes what libxml2 escapes inside a double-quoted attribute.
// Line breaks and tabs become character references because an XML parser would
// otherwise normalise them to spaces.
func escapeAttr(s string) string {
	if !strings.ContainsAny(s, "&<>\"\n\r\t") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\n':
			b.WriteString("&#10;")
		case '\r':
			b.WriteString("&#13;")
		case '\t':
			b.WriteString("&#9;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
