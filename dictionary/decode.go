package dictionary

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxElements bounds the XML elements Load decodes. They are counted as they
// are read, so a dictionary cannot make the decoder build a tree the resolver
// limits would only reject afterwards. The full FIX 5.0 SP2 dictionary has
// about 10,000 elements.
const maxElements = 1 << 18

// decoder fills the xml* types from XML tokens, counting elements and group
// nesting as it reads them.
type decoder struct {
	d        *xml.Decoder
	elements int
}

// decode reads a QuickFIX dictionary. Like encoding/xml, it takes the first
// element whatever its name as the root, and skips unknown elements.
func decode(r io.Reader) (*xmlDict, error) {
	dec := &decoder{d: xml.NewDecoder(&markupLimiter{r: r})}
	var root xml.StartElement
	for {
		tok, err := dec.d.Token()
		if err != nil {
			return nil, err
		}
		if t, ok := tok.(xml.StartElement); ok {
			root = t
			break
		}
	}
	dec.elements++
	xd := &xmlDict{
		Type:        attr(root, "type"),
		Major:       attr(root, "major"),
		Minor:       attr(root, "minor"),
		ServicePack: attr(root, "servicepack"),
	}
	err := dec.children(func(start xml.StartElement) error {
		switch start.Name.Local {
		case "header":
			return dec.body(&xd.Header, 0)
		case "trailer":
			return dec.body(&xd.Trailer, 0)
		case "messages":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "message" {
					return dec.skip()
				}
				m := xmlMessage{Name: attr(start, "name"), MsgType: attr(start, "msgtype"), MsgCat: attr(start, "msgcat")}
				if err := dec.body(&m.xmlBody, 0); err != nil {
					return err
				}
				xd.Messages.Messages = append(xd.Messages.Messages, m)
				return nil
			})
		case "components":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "component" {
					return dec.skip()
				}
				c := xmlComponent{Name: attr(start, "name")}
				if err := dec.body(&c.xmlBody, 0); err != nil {
					return err
				}
				xd.Components.Components = append(xd.Components.Components, c)
				return nil
			})
		case "fields":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "field" {
					return dec.skip()
				}
				f, err := dec.fieldDef(start)
				if err != nil {
					return err
				}
				xd.Fields.Fields = append(xd.Fields.Fields, f)
				return nil
			})
		}
		return dec.skip()
	})
	if err != nil {
		return nil, err
	}
	return xd, nil
}

// fieldDef reads a <field> definition and its enum <value>s.
func (dec *decoder) fieldDef(start xml.StartElement) (xmlFieldDef, error) {
	f := xmlFieldDef{Name: attr(start, "name"), Type: attr(start, "type")}
	// Like encoding/xml: every matching attribute is parsed, the last one
	// wins, and only an empty value stands for 0.
	for _, a := range start.Attr {
		if a.Name.Local != "number" {
			continue
		}
		f.Number = 0
		if a.Value == "" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(a.Value), 10, strconv.IntSize)
		if err != nil {
			return f, fmt.Errorf("field %q: number: %w", f.Name, err)
		}
		f.Number = int(n)
	}
	err := dec.children(func(start xml.StartElement) error {
		if start.Name.Local == "value" {
			f.Values = append(f.Values, xmlEnumValue{Enum: attr(start, "enum"), Description: attr(start, "description")})
		}
		return dec.skip()
	})
	return f, err
}

// body reads the items of a header, trailer, message, component or group,
// depth being the number of enclosing groups. Only groups have children:
// anything inside a field or component reference is rejected rather than
// decoded and ignored.
func (dec *decoder) body(b *xmlBody, depth int) error {
	return dec.children(func(start xml.StartElement) error {
		it := xmlItem{XMLName: start.Name, Name: attr(start, "name"), Required: attr(start, "required")}
		if start.Name.Local == "group" {
			if depth == maxDepth {
				return fmt.Errorf("groups nested deeper than %d", maxDepth)
			}
			if err := dec.body(&it.xmlBody, depth+1); err != nil {
				return err
			}
		} else {
			child, ok, err := dec.next()
			if err != nil {
				return err
			}
			if ok {
				return fmt.Errorf("<%s name=%q> contains <%s>: only groups have children", start.Name.Local, it.Name, child.Name.Local)
			}
		}
		b.Items = append(b.Items, it)
		return nil
	})
}

// children calls f for each child element of the current element, until its
// end. f must consume the child up to and including its end element.
func (dec *decoder) children(f func(xml.StartElement) error) error {
	for {
		start, ok, err := dec.next()
		if err != nil || !ok {
			return err
		}
		if err := f(start); err != nil {
			return err
		}
	}
}

// skip consumes the rest of the current element, counting what it skips.
func (dec *decoder) skip() error {
	for depth := 1; depth > 0; {
		_, ok, err := dec.next()
		if err != nil {
			return err
		}
		if ok {
			depth++
		} else {
			depth--
		}
	}
	return nil
}

// next returns the next child element of the current element, or false at
// the current element's end. Text, comments and other tokens are skipped.
func (dec *decoder) next() (xml.StartElement, bool, error) {
	for {
		tok, err := dec.d.Token()
		if err == io.EOF {
			return xml.StartElement{}, false, io.ErrUnexpectedEOF
		}
		if err != nil {
			return xml.StartElement{}, false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if dec.elements++; dec.elements > maxElements {
				return xml.StartElement{}, false, fmt.Errorf("more than %d elements", maxElements)
			}
			return t, true, nil
		case xml.EndElement:
			return xml.StartElement{}, false, nil
		}
	}
}

// attr returns the value of the attribute with the given local name; the last
// one wins if it is repeated, as with encoding/xml.
func attr(start xml.StartElement, name string) string {
	var v string
	for _, a := range start.Attr {
		if a.Name.Local == name {
			v = a.Value
		}
	}
	return v
}

// maxTagLen and maxAttrs bound one tag, or directive, before encoding/xml
// decodes it: it builds a tag's whole attribute list at once, before any
// element budget can stop it. QuickFIX dictionaries use at most four
// attributes per element.
const (
	maxTagLen = 64 << 10
	maxAttrs  = 8
)

// Lexer states of markupLimiter.
const (
	inText    = iota
	inOpen    // right after '<'
	inBang    // matching "<!--" or "<![CDATA[" after "<!"
	inTag     // start or end tag
	inDecl    // directive, e.g. <!DOCTYPE ...>
	inComment // until "-->"
	inCDATA   // until "]]>"
	inPI      // processing instruction, until "?>"
)

// markupLimiter enforces maxTagLen and maxAttrs on the XML read through it.
// Its lexer is just precise enough to find tags: text, comments, CDATA
// sections and processing instructions end exactly where encoding/xml ends
// them, and pass through unbounded since decoding them costs about their
// size. A directive, whose end it finds less precisely, is bounded like a
// tag, so a disagreement can only make it reject input.
type markupLimiter struct {
	r      io.Reader
	state  int
	want   string // inBang: the opening being matched
	match  int    // bytes of want, or of the state's terminator, matched
	quote  byte   // closing quote inside an attribute value, or 0
	tagLen int
	attrs  int
	depth  int // '<' nesting inside a directive
}

func (l *markupLimiter) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	for _, c := range p[:n] {
		if lerr := l.step(c); lerr != nil {
			return 0, lerr
		}
	}
	return n, err
}

func (l *markupLimiter) step(c byte) error {
	switch l.state {
	case inText:
		if c == '<' {
			l.state, l.tagLen, l.attrs, l.depth, l.quote = inOpen, 1, 0, 0, 0
		}
		return nil
	case inOpen:
		switch c {
		case '?':
			l.state, l.match = inPI, 0
			return nil
		case '!':
			l.state, l.want, l.match = inBang, "", 1
			return l.grow()
		}
		l.state = inTag
	case inBang:
		if l.want == "" {
			switch c {
			case '-':
				l.want = "!--"
			case '[':
				l.want = "![CDATA["
			}
		}
		if l.want != "" && c == l.want[l.match] {
			if l.match++; l.match == len(l.want) {
				l.state, l.match = inComment, 0
				if l.want != "!--" {
					l.state = inCDATA
				}
			}
			return l.grow()
		}
		l.state = inDecl
	case inComment, inCDATA:
		end := byte('-')
		if l.state == inCDATA {
			end = ']'
		}
		switch {
		case c == end:
			l.match = min(l.match+1, 2)
		case c == '>' && l.match == 2:
			l.state = inText
		default:
			l.match = 0
		}
		return nil
	case inPI:
		switch {
		case c == '?':
			l.match = 1
		case c == '>' && l.match == 1:
			l.state = inText
		default:
			l.match = 0
		}
		return nil
	}

	// inTag or inDecl.
	if err := l.grow(); err != nil {
		return err
	}
	switch {
	case l.quote != 0:
		if c == l.quote {
			l.quote = 0
		}
	case c == '"' || c == '\'':
		l.quote = c
	case c == '=' && l.state == inTag:
		if l.attrs++; l.attrs > maxAttrs {
			return fmt.Errorf("element with more than %d attributes", maxAttrs)
		}
	case c == '<' && l.state == inDecl:
		l.depth++
	case c == '>':
		if l.depth == 0 {
			l.state = inText
		} else {
			l.depth--
		}
	}
	return nil
}

// grow counts a byte of the current tag or directive.
func (l *markupLimiter) grow() error {
	if l.tagLen++; l.tagLen > maxTagLen {
		return fmt.Errorf("tag longer than %d bytes", maxTagLen)
	}
	return nil
}
