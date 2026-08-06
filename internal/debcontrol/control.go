// Package debcontrol reads and writes Debian control data: the RFC822-like
// paragraph format shared by a .deb's control file, apt's Packages index and
// the Release file.
//
// Paragraphs keep their fields in the order they were parsed. Artifactory
// emits each package's control fields in whatever order the package declared
// them, so preserving order is what lets us reproduce the existing index
// rather than a normalised approximation of it.
package debcontrol

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// A Field is one "Name: value" entry. Value holds continuation lines joined
// with "\n", with the single leading space or tab that marks a continuation
// removed, so a Description's body reads as ordinary text.
type Field struct {
	Name  string
	Value string
}

// A Paragraph is an ordered set of fields, terminated by a blank line.
type Paragraph struct {
	Fields []Field
}

// Get returns the value of the named field, matched case-insensitively, or ""
// if it is absent. An empty return does not distinguish "absent" from
// "present but empty"; use Index when that matters.
func (p *Paragraph) Get(name string) string {
	if i := p.Index(name); i >= 0 {
		return p.Fields[i].Value
	}
	return ""
}

// Index returns the position of the named field, or -1 if it is absent.
func (p *Paragraph) Index(name string) int {
	for i := range p.Fields {
		if strings.EqualFold(p.Fields[i].Name, name) {
			return i
		}
	}
	return -1
}

// Set replaces the named field's value in place, keeping its position, or
// appends the field if it is absent.
func (p *Paragraph) Set(name, value string) {
	if i := p.Index(name); i >= 0 {
		p.Fields[i].Value = value
		return
	}
	p.Fields = append(p.Fields, Field{Name: name, Value: value})
}

// Delete removes the named field if present.
func (p *Paragraph) Delete(name string) {
	if i := p.Index(name); i >= 0 {
		p.Fields = append(p.Fields[:i], p.Fields[i+1:]...)
	}
}

// Clone returns a deep copy, so callers can decorate a parsed control
// paragraph without mutating the stored original.
func (p *Paragraph) Clone() *Paragraph {
	out := &Paragraph{Fields: make([]Field, len(p.Fields))}
	copy(out.Fields, p.Fields)
	return out
}

// WriteTo renders the paragraph followed by the blank line that terminates it.
func (p *Paragraph) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	for _, f := range p.Fields {
		lines := strings.Split(f.Value, "\n")
		fmt.Fprintf(cw, "%s: %s\n", f.Name, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(cw, " %s\n", line)
		}
	}
	fmt.Fprint(cw, "\n")
	return cw.n, cw.err
}

// String renders the paragraph including its terminating blank line.
func (p *Paragraph) String() string {
	var sb strings.Builder
	p.WriteTo(&sb)
	return sb.String()
}

// Parse reads exactly one paragraph. It is an error for the input to hold no
// paragraph, but trailing content after the first blank line is ignored.
func Parse(s string) (*Paragraph, error) {
	p, err := NewReader(strings.NewReader(s)).Next()
	if err == io.EOF {
		return nil, fmt.Errorf("debcontrol: no paragraph in input")
	}
	return p, err
}

// A Reader yields paragraphs from a stream such as a Packages file.
type Reader struct {
	sc   *bufio.Scanner
	line int
}

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	// Description bodies and long Depends lists comfortably exceed the default
	// 64 KiB token limit on real packages.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &Reader{sc: sc}
}

// Next returns the next paragraph, or io.EOF when the stream is exhausted.
// Blank lines between paragraphs are skipped.
func (r *Reader) Next() (*Paragraph, error) {
	p := &Paragraph{}
	seenField := false

	for r.sc.Scan() {
		r.line++
		line := strings.TrimSuffix(r.sc.Text(), "\r")

		if line == "" {
			if seenField {
				return p, nil
			}
			continue // leading or repeated separator
		}

		if line[0] == ' ' || line[0] == '\t' {
			if !seenField {
				return nil, fmt.Errorf("debcontrol: line %d: continuation line with no preceding field", r.line)
			}
			last := &p.Fields[len(p.Fields)-1]
			last.Value += "\n" + line[1:]
			continue
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("debcontrol: line %d: missing colon in %q", r.line, line)
		}
		p.Fields = append(p.Fields, Field{
			Name:  strings.TrimSpace(name),
			Value: strings.TrimSpace(value),
		})
		seenField = true
	}

	if err := r.sc.Err(); err != nil {
		return nil, fmt.Errorf("debcontrol: line %d: %w", r.line, err)
	}
	if !seenField {
		return nil, io.EOF
	}
	return p, nil
}

// ReadAll reads every paragraph in the stream.
func ReadAll(r io.Reader) ([]*Paragraph, error) {
	dr := NewReader(r)
	var out []*Paragraph
	for {
		p, err := dr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
}

type countingWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.err = err
	return n, err
}
