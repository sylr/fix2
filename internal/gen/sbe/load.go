package sbe

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// maxSchemaSize bounds the schema files Load reads.
const maxSchemaSize = 64 << 20

// node is an XML element, its attributes and children in document order.
type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Content  string     `xml:",chardata"`
	Children []node     `xml:",any"`
}

func (n *node) attr(name string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Name.Local == name && a.Name.Space == "" {
			return a.Value, true
		}
	}
	return "", false
}

func (n *node) get(name string) string {
	v, _ := n.attr(name)
	return strings.TrimSpace(v)
}

// what names n in errors: its element and name attribute.
func (n *node) what() string {
	if name := n.get("name"); name != "" {
		return fmt.Sprintf("<%s name=%q>", n.XMLName.Local, name)
	}
	return "<" + n.XMLName.Local + ">"
}

// xincludeSpace is the namespace of XInclude elements, which the loader does
// not follow.
const xincludeSpace = "http://www.w3.org/2001/XInclude"

// sbeSpace is the namespace of SBE 1.0's message schemas (§6.2), sbe.xsd's
// target namespace. SBE's release candidates and SBE 2.0 have others, which
// the loader refuses: their schemas differ.
const sbeSpace = "http://fixprotocol.io/2016/sbe"

// sbeElements lists, for each element of sbe.xsd the loader supports, the
// attributes it may carry, unqualified, and the elements it may hold. Of
// them, sbe.xsd declares messageSchema and message in sbeSpace, the others
// unqualified (elementFormDefault="unqualified").
var sbeElements = func() map[string]struct{ attrs, children []string } {
	version := []string{"sinceVersion", "deprecated"} // versionAttributes
	semantic := []string{"semanticType", "description"}
	join := func(lists ...[]string) []string { return slices.Concat(lists...) }
	field := join([]string{"name", "id", "type", "epoch", "timeUnit", "offset", "presence", "valueRef"}, semantic, version)
	block := join([]string{"name", "id", "blockLength"}, semantic, version)
	encoded := join([]string{"name", "encodingType", "offset"}, semantic, version)
	value := join([]string{"name", "description"}, version)
	types := []string{"type", "composite", "enum", "set"}
	members := []string{"field", "group", "data"}
	type element = struct{ attrs, children []string }
	return map[string]element{
		"messageSchema": {[]string{"package", "id", "version", "semanticVersion", "description", "byteOrder", "headerType"}, []string{"types", "message"}},
		"types":         {nil, types},
		"type": {join([]string{"name", "nullValue", "minValue", "maxValue", "length", "primitiveType", "characterEncoding",
			"offset", "presence", "valueRef"}, semantic, version), nil},
		"composite":  {join([]string{"name", "offset"}, semantic, version), append(slices.Clone(types), "ref")},
		"ref":        {join([]string{"name", "type", "offset"}, version), nil},
		"enum":       {encoded, []string{"validValue"}},
		"validValue": {value, nil},
		"set":        {encoded, []string{"choice"}},
		"choice":     {value, nil},
		"message":    {block, members},
		"group":      {join(block, []string{"dimensionType"}), members},
		"field":      {field, nil},
		"data":       {field, nil},
	}
}()

// xsiSpace is the namespace of XML Schema's instance attributes, four of
// which XML Schema allows on any element whatever its schema declares (XML
// Schema Part 1, §3.2.7): xsi:schemaLocation and
// xsi:noNamespaceSchemaLocation, hints of where schemas are, which the
// loader ignores; xsi:nil and xsi:type, which change how an element is
// validated, and which the loader refuses: sbe.xsd declares no element
// nillable, and the loader does not resolve the types they would name.
const xsiSpace = "http://www.w3.org/2001/XMLSchema-instance"

// LoadOptions are the departures from SBE 1.0 a loader accepts, all off by
// default.
type LoadOptions struct {
	// TolerateForeignMetadata accepts and ignores the elements and
	// attributes of namespaces other than SBE's and XML Schema instance's,
	// such as a venue's annotations, which sbe.xsd, normative (§6.1), does
	// not allow: it declares no wildcard. Off, they are errors. The flag
	// -tolerate-foreign-metadata of fix2gen sbe.
	TolerateForeignMetadata bool
	// TolerateUndeclaredPrimitiveTypes resolves the type of a field naming
	// an SBE primitive, such as uint32, that no encoding of <types> is
	// named, as that primitive, required unless the field says otherwise:
	// the encoding <type name="uint32" primitiveType="uint32"/> would be,
	// as Real Logic's SbeTool resolves it. A field's type must name an
	// encoding of <types> (§6.7, sbe.xsd), which an enum's encodingType
	// need not (§4.13.3). Off, it is an error. The flag
	// -tolerate-undeclared-primitive-types of fix2gen sbe.
	TolerateUndeclaredPrimitiveTypes bool
	// TolerateGroupRedefinitions accepts a group name, or id, standing for
	// different groups in different places, which a group's name and id
	// identify throughout a schema (§6.8, §6.9). Each group of such a name
	// gets a Go type of its own, named by its place (see docs/sbe.md). Off,
	// it is an error. The flag -tolerate-group-redefinitions of fix2gen sbe.
	TolerateGroupRedefinitions bool
	// TolerateFieldRedefinitions accepts a field name, or id, standing for
	// fields of different ids, names or encodings in different places,
	// which a field's name and id identify throughout a schema (§6.7,
	// §6.9), <data> included. The FIX Trading Community's own FIXP schema
	// needs it. Off, it is an error. The flag -tolerate-field-redefinitions
	// of fix2gen sbe.
	TolerateFieldRedefinitions bool
}

// flags returns the fix2gen sbe flags that set o.
func (o LoadOptions) flags() []string {
	var f []string
	if o.TolerateForeignMetadata {
		f = append(f, "-tolerate-foreign-metadata")
	}
	if o.TolerateUndeclaredPrimitiveTypes {
		f = append(f, "-tolerate-undeclared-primitive-types")
	}
	if o.TolerateGroupRedefinitions {
		f = append(f, "-tolerate-group-redefinitions")
	}
	if o.TolerateFieldRedefinitions {
		f = append(f, "-tolerate-field-redefinitions")
	}
	return f
}

// checkNames checks the elements and attributes of the schema root by their
// expanded names (§6.1, sbe.xsd). The root is SBE 1.0's messageSchema; an
// element or attribute of no namespace, or an element of SBE's, must be one
// sbe.xsd declares there, in its place. XML Schema instance's attributes
// are accepted, and ignored. Those of other namespaces, and their elements,
// are errors, unless opts tolerates them: then they are ignored, and the
// elements removed.
func checkNames(root *node, opts LoadOptions) error {
	if root.XMLName.Local != "messageSchema" {
		return fmt.Errorf("root element <%s>, want <messageSchema>", root.XMLName.Local)
	}
	if root.XMLName.Space != sbeSpace {
		return fmt.Errorf("root element <messageSchema> %s, want %q, SBE 1.0's", inSpace(root.XMLName.Space), sbeSpace)
	}
	return checkNode(root, "", opts)
}

// inSpace describes the namespace space for errors.
func inSpace(space string) string {
	if space == "" {
		return "in no namespace"
	}
	return fmt.Sprintf("in namespace %q", space)
}

// foreignHint is how an error about foreign metadata ends.
const foreignHint = "metadata of another namespace, which sbe.xsd does not allow: -tolerate-foreign-metadata ignores it"

// checkNode checks n, an element of sbeElements whose path in errors is
// path, and its descendants.
func checkNode(n *node, path string, opts LoadOptions) error {
	def := sbeElements[n.XMLName.Local]
	what := path + n.what()
	for _, a := range n.Attrs {
		switch {
		case a.Name.Space == "xmlns", a.Name.Space == "" && a.Name.Local == "xmlns":
			// a namespace declaration
		case a.Name.Space == "":
			if !slices.Contains(def.attrs, a.Name.Local) {
				return fmt.Errorf("%s: unknown attribute %q", what, a.Name.Local)
			}
		case a.Name.Space == sbeSpace:
			return fmt.Errorf("%s: attribute %q in the SBE namespace: sbe.xsd declares its attributes unqualified", what, a.Name.Local)
		case !strings.Contains(a.Name.Space, ":"):
			return fmt.Errorf("%s: attribute %q of the undeclared prefix %q", what, a.Name.Local, a.Name.Space)
		case a.Name.Space == xsiSpace:
			switch a.Name.Local {
			case "schemaLocation", "noNamespaceSchemaLocation":
				// a hint, ignored
			case "nil", "type":
				return fmt.Errorf("%s: xsi:%s is not supported: sbe.xsd declares no element nillable, nor types to substitute", what, a.Name.Local)
			default:
				return fmt.Errorf("%s: xsi:%s is not an attribute of XML Schema instances", what, a.Name.Local)
			}
		case opts.TolerateForeignMetadata:
			// ignored
		default:
			return fmt.Errorf("%s: attribute %q %s: %s", what, a.Name.Local, inSpace(a.Name.Space), foreignHint)
		}
	}
	// The path of the children: the schema and <types> are left out, as
	// they are of the loader's other errors.
	if local := n.XMLName.Local; local != "messageSchema" && local != "types" {
		path = what + ": "
	}
	kept := n.Children[:0]
	for i := range n.Children {
		c := &n.Children[i]
		space, local := c.XMLName.Space, c.XMLName.Local
		switch {
		case space == xincludeSpace:
			return fmt.Errorf("%s<%s>: XInclude is not supported: include the types in the schema", path, local)
		case space != "" && space != sbeSpace && !strings.Contains(space, ":"):
			return fmt.Errorf("%s<%s:%s>: undeclared prefix %q", path, space, local, space)
		case space != "" && space != sbeSpace && opts.TolerateForeignMetadata:
			continue // ignored
		case space != "" && space != sbeSpace:
			return fmt.Errorf("%s<%s> %s: %s", path, local, inSpace(space), foreignHint)
		case !slices.Contains(def.children, local):
			return fmt.Errorf("%s: unexpected element %s", what, c.what())
		case local == "message" && space != sbeSpace:
			return fmt.Errorf("%s%s: %s, want %q: sbe.xsd declares message in the SBE namespace", path, c.what(), inSpace(space), sbeSpace)
		case local != "message" && space != "":
			return fmt.Errorf("%s%s: in the SBE namespace: sbe.xsd declares it unqualified", path, c.what())
		}
		if err := checkNode(c, path, opts); err != nil {
			return err
		}
		kept = append(kept, *c)
	}
	n.Children = kept
	return nil
}

// LoadFile loads and checks the SBE message schema in the file at path.
func LoadFile(path string) (*Schema, error) {
	return LoadFileWith(path, LoadOptions{})
}

// LoadFileWith is LoadFile with options.
func LoadFileWith(path string, opts LoadOptions) (*Schema, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sbe: %w", err)
	}
	defer f.Close()
	return LoadWith(f, opts)
}

// Load loads and checks an SBE message schema.
func Load(r io.Reader) (*Schema, error) {
	return LoadWith(r, LoadOptions{})
}

// LoadWith is Load with options.
func LoadWith(r io.Reader, opts LoadOptions) (*Schema, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSchemaSize+1))
	if err != nil {
		return nil, fmt.Errorf("sbe: %w", err)
	}
	if len(data) > maxSchemaSize {
		return nil, fmt.Errorf("sbe: schema larger than %d bytes", maxSchemaSize)
	}
	var root node
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("sbe: %w", err)
	}
	if err := checkNames(&root, opts); err != nil {
		return nil, fmt.Errorf("sbe: %w", err)
	}
	l := &loader{opts: opts, named: map[string]*node{}, types: map[string]*Type{}, resolving: map[string]bool{},
		primitives: map[string]*Type{}, groups: map[string]string{}, groupIDs: map[uint64]string{},
		fields: map[string]fieldIdentity{}, fieldIDs: map[uint64]fieldIdentity{}}
	s, err := l.schema(&root)
	if err != nil {
		return nil, fmt.Errorf("sbe: %w", err)
	}
	s.Tolerated = opts
	return s, nil
}

type loader struct {
	s          *Schema
	opts       LoadOptions
	named      map[string]*node         // the types of <types>, by name
	order      []string                 // their names, in schema order
	types      map[string]*Type         // those resolved
	resolving  map[string]bool          // those being resolved, to find cycles
	primitives map[string]*Type         // the primitives fields name undeclared, by name, if tolerated
	groups     map[string]string        // the signature of each group, by name
	groupIDs   map[uint64]string        // and by id
	fields     map[string]fieldIdentity // the first field or data of each name
	fieldIDs   map[uint64]fieldIdentity // and of each id
	valueRefs  []valueRef               // the valueRefs of constant types, checked once all are loaded
}

// valueRef is the valueRef of a constant type, "Enum.Value".
type valueRef struct {
	n   *node
	ref string
}

func (l *loader) schema(root *node) (*Schema, error) {
	s := &Schema{
		Package:         root.get("package"),
		SemanticVersion: root.get("semanticVersion"),
		Description:     root.get("description"),
	}
	l.s = s
	id, err := parseUint(root, "id", true, 0)
	if err != nil {
		return nil, err
	}
	s.ID = id
	if s.Version, err = parseInt(root, "version", 0); err != nil {
		return nil, err
	}
	switch order := root.get("byteOrder"); order {
	case "", "littleEndian":
	case "bigEndian":
		s.BigEndian = true
	default:
		return nil, fmt.Errorf("<messageSchema>: byteOrder %q, want littleEndian or bigEndian", order)
	}

	var messages []*node
	for i := range root.Children {
		c := &root.Children[i]
		switch {
		case c.XMLName.Local == "types":
			for j := range c.Children {
				t := &c.Children[j]
				switch t.XMLName.Local {
				case "type", "composite", "enum", "set":
				default:
					return nil, fmt.Errorf("<types>: unexpected element %s", t.what())
				}
				name := t.get("name")
				if err := checkName(t, name); err != nil {
					return nil, err
				}
				if l.named[name] != nil {
					return nil, fmt.Errorf("%s: duplicate encoding name", t.what())
				}
				l.named[name] = t
				l.order = append(l.order, name)
			}
		case c.XMLName.Local == "message":
			messages = append(messages, c)
		default:
			return nil, fmt.Errorf("<messageSchema>: unexpected element %s", c.what())
		}
	}
	for _, name := range l.order {
		t, err := l.resolve(name)
		if err != nil {
			return nil, err
		}
		s.Types = append(s.Types, t)
	}
	for _, r := range l.valueRefs {
		enumName, valueName, _ := strings.Cut(r.ref, ".")
		t := l.types[enumName]
		if t == nil || t.Kind != KindEnum || enumValue(t, valueName) == nil {
			return nil, fmt.Errorf("%s: valueRef %q is not a value of an enum", r.n.what(), r.ref)
		}
	}

	headerName := root.get("headerType")
	if headerName == "" {
		headerName = "messageHeader"
	}
	h := l.types[headerName]
	if h == nil || h.Kind != KindComposite {
		return nil, fmt.Errorf("no composite %q for the message header (headerType)", headerName)
	}
	for _, name := range []string{"blockLength", "templateId", "schemaId", "version"} {
		if err := unsignedElement(h, name); err != nil {
			return nil, err
		}
	}
	// The counts are optional, but, present, unsigned like the others
	// (§5.2.1: "Each of these fields must be encoded as an unsigned integer
	// type."): a signed one would otherwise pass for absent, its counting
	// unchecked.
	for _, name := range []string{"numGroups", "numVarDataFields"} {
		if element(h, name) != nil {
			if err := unsignedElement(h, name); err != nil {
				return nil, err
			}
		}
	}
	if max := maxOf(element(h, "schemaId").Type.Primitive); s.ID > max {
		return nil, fmt.Errorf("schema id %d does not fit the header's schemaId", s.ID)
	}
	if max := maxOf(element(h, "version").Type.Primitive); uint64(s.Version) > max {
		return nil, fmt.Errorf("schema version %d does not fit the header's version", s.Version)
	}
	s.Header = h

	names := map[string]bool{}
	ids := map[uint64]bool{}
	for _, mn := range messages {
		m, err := l.message(mn)
		if err != nil {
			return nil, err
		}
		if names[m.Name] || ids[m.ID] {
			return nil, fmt.Errorf("%s: duplicate message name or id %d", mn.what(), m.ID)
		}
		names[m.Name], ids[m.ID] = true, true
		if max := maxOf(element(h, "templateId").Type.Primitive); m.ID > max {
			return nil, fmt.Errorf("%s: id %d does not fit the header's templateId", mn.what(), m.ID)
		}
		if max := maxOf(element(h, "blockLength").Type.Primitive); uint64(m.BlockLength) > max {
			return nil, fmt.Errorf("%s: block length %d does not fit the header's blockLength", mn.what(), m.BlockLength)
		}
		s.Messages = append(s.Messages, m)
	}
	return s, nil
}

// resolve returns the type of <types> named name.
func (l *loader) resolve(name string) (*Type, error) {
	if t := l.types[name]; t != nil {
		return t, nil
	}
	n := l.named[name]
	if n == nil {
		return nil, fmt.Errorf("no encoding named %q", name)
	}
	if l.resolving[name] {
		return nil, fmt.Errorf("%s: refers to itself", n.what())
	}
	l.resolving[name] = true
	defer delete(l.resolving, name)
	t, err := l.typ(n, false)
	if err != nil {
		return nil, err
	}
	l.types[name] = t
	return t, nil
}

// typ builds the type n defines; inComposite allows its offset attribute.
func (l *loader) typ(n *node, inComposite bool) (*Type, error) {
	name := n.get("name")
	if err := checkName(n, name); err != nil {
		return nil, err
	}
	t := &Type{
		Name:         name,
		Description:  n.get("description"),
		SemanticType: n.get("semanticType"),
	}
	var err error
	if t.SinceVersion, err = l.version(n, "sinceVersion"); err != nil {
		return nil, err
	}
	if t.Deprecated, err = l.version(n, "deprecated"); err != nil {
		return nil, err
	}
	if _, ok := n.attr("offset"); ok && !inComposite {
		return nil, fmt.Errorf("%s: offset outside a composite", n.what())
	}
	switch n.XMLName.Local {
	case "type":
		err = l.simple(n, t)
	case "composite":
		err = l.composite(n, t)
	case "enum":
		err = l.enum(n, t)
	case "set":
		err = l.set(n, t)
	default:
		err = fmt.Errorf("unexpected element %s", n.what())
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (l *loader) simple(n *node, t *Type) error {
	t.Kind = KindSimple
	p, ok := primitiveNames[n.get("primitiveType")]
	if !ok {
		return fmt.Errorf("%s: primitiveType %q is not an SBE primitive", n.what(), n.get("primitiveType"))
	}
	t.Primitive = p
	for _, r := range []struct {
		attr string
		dst  *string
	}{{"minValue", &t.MinValue}, {"maxValue", &t.MaxValue}} {
		if v := n.get(r.attr); v != "" {
			parsed, err := p.parseValue(v)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", n.what(), r.attr, err)
			}
			*r.dst = parsed
		}
	}
	t.CharacterEncoding = n.get("characterEncoding")
	presence, err := parsePresence(n)
	if err != nil {
		return err
	}
	t.Presence = presence
	content := strings.TrimSpace(n.Content)
	length, hasLength := n.attr("length")
	t.Length = 1
	if hasLength {
		if t.Length, err = strconv.Atoi(strings.TrimSpace(length)); err != nil || t.Length < 0 {
			return fmt.Errorf("%s: length %q", n.what(), length)
		}
	} else if presence == Constant && p == Char && len(content) > 1 {
		t.Length = len(content) // inferred from the constant (§4.7.2)
	}
	nullValue, hasNull := n.attr("nullValue")
	switch presence {
	case Required:
		if hasNull {
			return fmt.Errorf("%s: nullValue on a required type", n.what())
		}
	case Optional:
		t.Null = p.defaultNull()
		if hasNull {
			if t.Null, err = p.parseValue(strings.TrimSpace(nullValue)); err != nil {
				return fmt.Errorf("%s: nullValue: %w", n.what(), err)
			}
		}
	case Constant:
		if hasNull {
			return fmt.Errorf("%s: nullValue on a constant", n.what())
		}
		if ref := n.get("valueRef"); ref != "" {
			t.ValueRef = ref
			l.valueRefs = append(l.valueRefs, valueRef{n, ref})
			break
		}
		if content == "" {
			return fmt.Errorf("%s: constant without a value", n.what())
		}
		t.Constant = content
		if t.Length == 1 {
			if _, err := p.parseValue(content); err != nil {
				return fmt.Errorf("%s: constant: %w", n.what(), err)
			}
		} else if p != Char {
			return fmt.Errorf("%s: a constant array must be of char", n.what())
		} else if len(content) > t.Length {
			return fmt.Errorf("%s: constant %q longer than its length %d", n.what(), content, t.Length)
		}
	}
	if t.Length == 0 && p != Uint8 && p != Char {
		return fmt.Errorf("%s: variable-length data must be of uint8 or char", n.what())
	}
	if presence != Constant {
		t.Size = p.Size() * t.Length
	}
	return nil
}

func (l *loader) composite(n *node, t *Type) error {
	t.Kind = KindComposite
	if len(n.Children) == 0 {
		return fmt.Errorf("%s: no elements", n.what())
	}
	names := map[string]bool{}
	end := 0
	for i := range n.Children {
		c := &n.Children[i]
		e := &Element{Name: c.get("name")}
		if err := checkName(c, e.Name); err != nil {
			return fmt.Errorf("%s: %w", n.what(), err)
		}
		if names[e.Name] {
			return fmt.Errorf("%s: duplicate element %q", n.what(), e.Name)
		}
		names[e.Name] = true
		var err error
		if c.XMLName.Local == "ref" {
			e.Ref = true
			if e.Type, err = l.resolve(c.get("type")); err != nil {
				return fmt.Errorf("%s: %s: %w", n.what(), c.what(), err)
			}
		} else if e.Type, err = l.typ(c, true); err != nil {
			return fmt.Errorf("%s: %w", n.what(), err)
		}
		if e.Type.Kind == KindSimple && e.Type.Length == 0 && i != len(n.Children)-1 {
			return fmt.Errorf("%s: variable-length element %q is not the last", n.what(), e.Name)
		}
		if e.Offset, err = offset(c, end); err != nil {
			return fmt.Errorf("%s: %w", n.what(), err)
		}
		end = e.Offset + e.Type.Size
		t.Elements = append(t.Elements, e)
	}
	t.Size = end
	return nil
}

func (l *loader) enum(n *node, t *Type) error {
	t.Kind = KindEnum
	enc, err := l.encodingType(n, true)
	if err != nil {
		return err
	}
	t.Encoding = enc
	t.Size = enc.Size
	names, values := map[string]bool{}, map[string]bool{}
	for i := range n.Children {
		c := &n.Children[i]
		if c.XMLName.Local != "validValue" {
			return fmt.Errorf("%s: unexpected element %s", n.what(), c.what())
		}
		v, err := l.value(c)
		if err != nil {
			return fmt.Errorf("%s: %w", n.what(), err)
		}
		if v.Value, err = enc.Primitive.parseValue(strings.TrimSpace(c.Content)); err != nil {
			return fmt.Errorf("%s: %s: %w", n.what(), c.what(), err)
		}
		if names[v.Name] || values[v.Value] {
			return fmt.Errorf("%s: %s: duplicate name or value", n.what(), c.what())
		}
		names[v.Name], values[v.Value] = true, true
		t.Values = append(t.Values, v)
	}
	if len(t.Values) == 0 {
		return fmt.Errorf("%s: no validValue", n.what())
	}
	return nil
}

func (l *loader) set(n *node, t *Type) error {
	t.Kind = KindSet
	enc, err := l.encodingType(n, false)
	if err != nil {
		return err
	}
	t.Encoding = enc
	t.Size = enc.Size
	names, bits := map[string]bool{}, map[string]bool{}
	for i := range n.Children {
		c := &n.Children[i]
		if c.XMLName.Local != "choice" {
			return fmt.Errorf("%s: unexpected element %s", n.what(), c.what())
		}
		v, err := l.value(c)
		if err != nil {
			return fmt.Errorf("%s: %w", n.what(), err)
		}
		bit, err := strconv.ParseUint(strings.TrimSpace(c.Content), 10, 8)
		if err != nil || int(bit) >= enc.Size*8 {
			return fmt.Errorf("%s: %s: bit %q out of a %s", n.what(), c.what(), c.Content, enc.Primitive)
		}
		v.Value = strconv.FormatUint(bit, 10)
		if names[v.Name] || bits[v.Value] {
			return fmt.Errorf("%s: %s: duplicate name or bit", n.what(), c.what())
		}
		names[v.Name], bits[v.Value] = true, true
		t.Values = append(t.Values, v)
	}
	return nil
}

// encodingType resolves the encodingType of an enum or a set: a primitive
// or a scalar simple type of <types>, unsigned, or char for an enum.
func (l *loader) encodingType(n *node, enum bool) (*Type, error) {
	name := n.get("encodingType")
	var enc *Type
	if p, ok := primitiveNames[name]; ok {
		enc = &Type{Kind: KindSimple, Name: name, Primitive: p, Length: 1, Size: p.Size(), Presence: Required}
	} else {
		t, err := l.resolve(name)
		if err != nil {
			return nil, fmt.Errorf("%s: encodingType: %w", n.what(), err)
		}
		if !t.Scalar() || t.Presence == Constant {
			return nil, fmt.Errorf("%s: encodingType %q is not a scalar type", n.what(), name)
		}
		enc = t
	}
	if !enc.Primitive.Unsigned() && !(enum && enc.Primitive == Char) {
		return nil, fmt.Errorf("%s: encodingType %q is not an unsigned integer%s", n.what(), name, map[bool]string{true: " or char"}[enum])
	}
	return enc, nil
}

func (l *loader) value(n *node) (*Value, error) {
	v := &Value{Name: n.get("name"), Description: n.get("description")}
	if err := checkName(n, v.Name); err != nil {
		return nil, err
	}
	var err error
	if v.SinceVersion, err = l.version(n, "sinceVersion"); err != nil {
		return nil, err
	}
	if v.Deprecated, err = l.version(n, "deprecated"); err != nil {
		return nil, err
	}
	return v, nil
}

func (l *loader) message(n *node) (*Message, error) {
	m := &Message{}
	if err := l.block(n, &m.Block); err != nil {
		return nil, err
	}
	return m, nil
}

// block reads a message or a group: its attributes and its fields, groups
// and data, in that order.
func (l *loader) block(n *node, b *Block) error {
	b.Name = n.get("name")
	if err := checkName(n, b.Name); err != nil {
		return err
	}
	b.Description, b.SemanticType = n.get("description"), n.get("semanticType")
	var err error
	if b.ID, err = parseUint(n, "id", true, 0); err != nil {
		return err
	}
	if b.SinceVersion, err = l.version(n, "sinceVersion"); err != nil {
		return err
	}
	if b.Deprecated, err = l.version(n, "deprecated"); err != nil {
		return err
	}
	stage := 0 // 0: fields, 1: groups, 2: data
	names := map[string]bool{}
	end := 0
	for i := range n.Children {
		c := &n.Children[i]
		name := c.get("name")
		if names[name] {
			return fmt.Errorf("%s: duplicate member %q", n.what(), name)
		}
		names[name] = true
		switch c.XMLName.Local {
		case "field":
			if stage > 0 {
				return fmt.Errorf("%s: field %q after a group or data", n.what(), name)
			}
			f, err := l.field(c, end)
			if err != nil {
				return fmt.Errorf("%s: %w", n.what(), err)
			}
			end = f.Offset + fieldSize(f)
			b.Fields = append(b.Fields, f)
		case "group":
			if stage > 1 {
				return fmt.Errorf("%s: group %q after data", n.what(), name)
			}
			stage = 1
			g, err := l.group(c)
			if err != nil {
				return fmt.Errorf("%s: %w", n.what(), err)
			}
			b.Groups = append(b.Groups, g)
		case "data":
			stage = 2
			d, err := l.data(c)
			if err != nil {
				return fmt.Errorf("%s: %w", n.what(), err)
			}
			b.Data = append(b.Data, d)
		default:
			return fmt.Errorf("%s: unexpected element %s", n.what(), c.what())
		}
	}
	b.BlockLength = end
	if _, ok := n.attr("blockLength"); ok {
		bl, err := parseUint(n, "blockLength", true, 0)
		if err != nil {
			return err
		}
		if bl < uint64(end) {
			return fmt.Errorf("%s: blockLength %d less than its fields' %d", n.what(), bl, end)
		}
		b.BlockLength = int(bl)
	}
	return nil
}

func fieldSize(f *Field) int {
	if f.Presence == Constant {
		return 0
	}
	return f.Type.Size
}

func (l *loader) field(n *node, end int) (*Field, error) {
	f := &Field{Name: n.get("name"), Description: n.get("description"), ValueRef: n.get("valueRef")}
	if err := checkName(n, f.Name); err != nil {
		return nil, err
	}
	var err error
	if f.ID, err = parseUint(n, "id", true, 0); err != nil {
		return nil, err
	}
	if f.SinceVersion, err = l.version(n, "sinceVersion"); err != nil {
		return nil, err
	}
	if f.Deprecated, err = l.version(n, "deprecated"); err != nil {
		return nil, err
	}
	if f.Type, err = l.fieldType(n.get("type")); err != nil {
		return nil, fmt.Errorf("%s: %w", n.what(), err)
	}
	if err := l.identify(n, f.Name, f.ID, f.Type); err != nil {
		return nil, err
	}
	t := f.Type
	f.SemanticType = n.get("semanticType")
	if f.SemanticType == "" {
		f.SemanticType = t.SemanticType
	} else if t.SemanticType != "" && !strings.EqualFold(t.SemanticType, f.SemanticType) {
		return nil, fmt.Errorf("%s: semanticType %q, its type's %q", n.what(), f.SemanticType, t.SemanticType)
	}
	if t.Kind == KindSimple && t.Length == 0 || t.Kind == KindComposite && hasVarData(t) {
		return nil, fmt.Errorf("%s: type %q is variable-length: use <data>", n.what(), t.Name)
	}
	if _, ok := n.attr("presence"); ok {
		if f.Presence, err = parsePresence(n); err != nil {
			return nil, err
		}
		if t.Kind == KindSimple && t.Presence != Required && t.Presence != f.Presence {
			return nil, fmt.Errorf("%s: presence %s, its type's %s", n.what(), f.Presence, t.Presence)
		}
	} else if t.Kind == KindSimple {
		f.Presence = t.Presence
	}
	switch f.Presence {
	case Constant:
		if f.ValueRef != "" {
			if t.Kind != KindEnum {
				return nil, fmt.Errorf("%s: valueRef on a field whose type is not an enum", n.what())
			}
			enumName, valueName, _ := strings.Cut(f.ValueRef, ".")
			if enumName != t.Name || enumValue(t, valueName) == nil {
				return nil, fmt.Errorf("%s: valueRef %q is not a value of %q", n.what(), f.ValueRef, t.Name)
			}
		} else if t.Kind != KindSimple || t.Presence != Constant {
			return nil, fmt.Errorf("%s: constant field without valueRef nor a constant type", n.what())
		}
	case Optional:
		// A composite is null when its first element is (§6.5.4.4): it is
		// optional through that element, whose type tells its null.
		if t.Kind == KindComposite && (!t.Elements[0].Type.Scalar() || t.Elements[0].Type.Presence != Optional) {
			return nil, fmt.Errorf("%s: an optional composite's first element must be an optional scalar, its null", n.what())
		}
	default:
		if f.ValueRef != "" {
			return nil, fmt.Errorf("%s: valueRef on a field that is not constant", n.what())
		}
	}
	if f.Offset, err = offset(n, end); err != nil {
		return nil, fmt.Errorf("%s: %w", n.what(), err)
	}
	return f, nil
}

// fieldType resolves the type of a field: an encoding of <types> (§6.7,
// sbe.xsd: "Must match the name of an encoding contained by 'types'
// element"), or, if tolerated, a primitive no encoding is named: a
// required scalar of it, one per primitive, as the encoding
// <type name="uint32" primitiveType="uint32"/> would be.
func (l *loader) fieldType(name string) (*Type, error) {
	p, primitive := primitiveNames[name]
	if l.named[name] != nil || !primitive {
		return l.resolve(name)
	}
	if !l.opts.TolerateUndeclaredPrimitiveTypes {
		return nil, fmt.Errorf("no encoding named %q: a field's type names an encoding of <types> (§6.7), not a primitive: -tolerate-undeclared-primitive-types resolves it as the primitive", name)
	}
	t := l.primitives[name]
	if t == nil {
		t = &Type{Kind: KindSimple, Name: name, Primitive: p, Length: 1, Size: p.Size(), Presence: Required}
		l.primitives[name] = t
	}
	return t, nil
}

func (l *loader) group(n *node) (*Group, error) {
	g := &Group{}
	if err := l.block(n, &g.Block); err != nil {
		return nil, err
	}
	dim := n.get("dimensionType")
	if dim == "" {
		dim = "groupSizeEncoding"
	}
	t, err := l.resolve(dim)
	if err != nil {
		return nil, fmt.Errorf("%s: dimensionType: %w", n.what(), err)
	}
	if t.Kind != KindComposite {
		return nil, fmt.Errorf("%s: dimensionType %q is not a composite", n.what(), dim)
	}
	for _, name := range []string{"blockLength", "numInGroup"} {
		if err := unsignedElement(t, name); err != nil {
			return nil, fmt.Errorf("%s: %w", n.what(), err)
		}
	}
	if max := maxOf(element(t, "blockLength").Type.Primitive); uint64(g.BlockLength) > max {
		return nil, fmt.Errorf("%s: block length %d does not fit its dimension's blockLength", n.what(), g.BlockLength)
	}
	g.Dimension = t
	count := element(t, "numInGroup").Type
	g.MaxEntries = maxOf(count.Primitive) - 1
	if count.MinValue != "" {
		g.MinEntries, _ = strconv.ParseUint(count.MinValue, 10, 64)
	}
	if count.MaxValue != "" {
		g.MaxEntries, _ = strconv.ParseUint(count.MaxValue, 10, 64)
		g.MaxEntriesSet = true
	}
	if g.MinEntries > g.MaxEntries {
		return nil, fmt.Errorf("%s: its dimension allows %d to %d entries", n.what(), g.MinEntries, g.MaxEntries)
	}
	// A group's name and id identify it throughout the schema: appearing
	// in several places, it must be the same group in each, "Each of those
	// instances must match on both id and name attributes" (§6.8, §6.9).
	// One Go type is generated for it. Tolerated, each group of a name
	// that differs gets one of its own.
	if l.opts.TolerateGroupRedefinitions {
		return g, nil
	}
	sig := groupSignature(g)
	const hint = ": -tolerate-group-redefinitions generates a type for each"
	if prev, ok := l.groups[g.Name]; ok && prev != sig {
		return nil, fmt.Errorf("%s: a different group of the same name is defined elsewhere%s", n.what(), hint)
	}
	if prev, ok := l.groupIDs[g.ID]; ok && prev != sig {
		return nil, fmt.Errorf("%s: a different group of the same id %d is defined elsewhere%s", n.what(), g.ID, hint)
	}
	l.groups[g.Name], l.groupIDs[g.ID] = sig, sig
	return g, nil
}

// groupSignature describes g's layout and members, to compare groups of the
// same name.
func groupSignature(g *Group) string {
	var b strings.Builder
	var block func(b *strings.Builder, bl *Block)
	block = func(b *strings.Builder, bl *Block) {
		fmt.Fprintf(b, "%s#%d/%d/%d{", bl.Name, bl.ID, bl.BlockLength, bl.SinceVersion)
		for _, f := range bl.Fields {
			fmt.Fprintf(b, "f%s#%d:%p@%d/%d/%s/%d;", f.Name, f.ID, f.Type, f.Offset, f.Presence, f.ValueRef, f.SinceVersion)
		}
		for _, sub := range bl.Groups {
			fmt.Fprintf(b, "g%p;", sub.Dimension)
			block(b, &sub.Block)
		}
		for _, d := range bl.Data {
			fmt.Fprintf(b, "d%s#%d:%p/%d;", d.Name, d.ID, d.Type, d.SinceVersion)
		}
		b.WriteString("}")
	}
	fmt.Fprintf(&b, "%p", g.Dimension)
	block(&b, &g.Block)
	return b.String()
}

// fieldIdentity is what identifies a field, or data, throughout a schema.
type fieldIdentity struct {
	name string
	id   uint64
	typ  *Type
}

// identify checks a field's, or data's, name, id and encoding against
// those of the fields of that name or id elsewhere: "Attributes id and
// name must uniquely identify a type within a message schema. This applies
// to fields and groups. To be clear, the same field or group ID may be used
// in multiple messages, but each instance must represent the same type.
// Each of those instances must match on both id and name attributes."
// (§6.9). Its presence may differ, as a FIX field's does between messages.
func (l *loader) identify(n *node, name string, id uint64, typ *Type) error {
	if l.opts.TolerateFieldRedefinitions {
		return nil
	}
	const hint = ": -tolerate-field-redefinitions accepts it"
	this := fieldIdentity{name, id, typ}
	if prev, ok := l.fields[name]; ok && prev != this {
		return fmt.Errorf("%s: field %q is id %d of type %q elsewhere, id %d of type %q here (§6.9)%s", n.what(), name, prev.id, prev.typ.Name, id, typ.Name, hint)
	}
	if prev, ok := l.fieldIDs[id]; ok && prev != this {
		return fmt.Errorf("%s: field id %d is %q of type %q elsewhere, %q of type %q here (§6.9)%s", n.what(), id, prev.name, prev.typ.Name, name, typ.Name, hint)
	}
	l.fields[name], l.fieldIDs[id] = this, this
	return nil
}

func (l *loader) data(n *node) (*Data, error) {
	d := &Data{Name: n.get("name"), Description: n.get("description")}
	if err := checkName(n, d.Name); err != nil {
		return nil, err
	}
	var err error
	if d.ID, err = parseUint(n, "id", true, 0); err != nil {
		return nil, err
	}
	if d.SinceVersion, err = l.version(n, "sinceVersion"); err != nil {
		return nil, err
	}
	if d.Deprecated, err = l.version(n, "deprecated"); err != nil {
		return nil, err
	}
	if d.Type, err = l.resolve(n.get("type")); err != nil {
		return nil, fmt.Errorf("%s: %w", n.what(), err)
	}
	if err := l.identify(n, d.Name, d.ID, d.Type); err != nil {
		return nil, err
	}
	d.SemanticType = n.get("semanticType")
	if d.SemanticType == "" {
		d.SemanticType = d.Type.SemanticType
	}
	t := d.Type
	if t.Kind != KindComposite || len(t.Elements) != 2 || !t.Elements[0].Type.Scalar() ||
		!t.Elements[0].Type.Primitive.Unsigned() || t.Elements[0].Type.Presence == Constant ||
		t.Elements[1].Type.Kind != KindSimple || t.Elements[1].Type.Length != 0 {
		return nil, fmt.Errorf("%s: type %q is not a composite of an unsigned length and variable-length data", n.what(), t.Name)
	}
	if t.Elements[0].Offset != 0 || t.Elements[1].Offset != t.Elements[0].Type.Size {
		return nil, fmt.Errorf("%s: type %q has offsets: its data must follow its length", n.what(), t.Name)
	}
	length := t.Elements[0].Type
	d.MaxLength = maxOf(length.Primitive) - 1
	if length.MaxValue != "" {
		d.MaxLength, _ = strconv.ParseUint(length.MaxValue, 10, 64)
	}
	if length.MinValue != "" {
		d.MinLength, _ = strconv.ParseUint(length.MinValue, 10, 64)
	}
	if d.MinLength > d.MaxLength {
		return nil, fmt.Errorf("%s: its length allows %d to %d bytes", n.what(), d.MinLength, d.MaxLength)
	}
	return d, nil
}

func hasVarData(t *Type) bool {
	for _, e := range t.Elements {
		if e.Type.Kind == KindSimple && e.Type.Length == 0 || e.Type.Kind == KindComposite && hasVarData(e.Type) {
			return true
		}
	}
	return false
}

func element(t *Type, name string) *Element {
	for _, e := range t.Elements {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func enumValue(t *Type, name string) *Value {
	for _, v := range t.Values {
		if v.Name == name {
			return v
		}
	}
	return nil
}

// unsignedElement checks t has an element name, an unsigned scalar sent on
// the wire.
func unsignedElement(t *Type, name string) error {
	e := element(t, name)
	if e == nil || !e.Type.Scalar() || !e.Type.Primitive.Unsigned() || e.Type.Presence != Required {
		return fmt.Errorf("composite %q has no required unsigned element %q", t.Name, name)
	}
	return nil
}

func maxOf(p Primitive) uint64 {
	if p.Size() == 8 {
		return 1<<64 - 1
	}
	return 1<<(8*p.Size()) - 1
}

// offset returns the offset attribute of n, or end if it has none.
func offset(n *node, end int) (int, error) {
	if _, ok := n.attr("offset"); !ok {
		return end, nil
	}
	o, err := parseUint(n, "offset", true, 0)
	if err != nil {
		return 0, err
	}
	if o < uint64(end) {
		return 0, fmt.Errorf("%s: offset %d overlaps the previous member, which ends at %d", n.what(), o, end)
	}
	if o > 1<<20 {
		return 0, fmt.Errorf("%s: offset %d too large", n.what(), o)
	}
	return int(o), nil
}

func parsePresence(n *node) (Presence, error) {
	switch p := n.get("presence"); p {
	case "", "required":
		return Required, nil
	case "optional":
		return Optional, nil
	case "constant":
		return Constant, nil
	default:
		return 0, fmt.Errorf("%s: presence %q", n.what(), p)
	}
}

func parseUint(n *node, name string, required bool, def uint64) (uint64, error) {
	s, ok := n.attr(name)
	if !ok {
		if required {
			return 0, fmt.Errorf("%s: no %s", n.what(), name)
		}
		return def, nil
	}
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %s %q", n.what(), name, s)
	}
	return v, nil
}

func parseInt(n *node, name string, def int) (int, error) {
	v, err := parseUint(n, name, false, uint64(def))
	if err != nil {
		return 0, err
	}
	if v > 1<<31-1 {
		return 0, fmt.Errorf("%s: %s %d too large", n.what(), name, v)
	}
	return int(v), nil
}

// version reads a sinceVersion or deprecated attribute, no greater than
// the schema's version.
func (l *loader) version(n *node, name string) (int, error) {
	v, err := parseInt(n, name, 0)
	if err != nil {
		return 0, err
	}
	if v > l.s.Version {
		return 0, fmt.Errorf("%s: %s %d beyond the schema's version %d", n.what(), name, v, l.s.Version)
	}
	return v, nil
}

var errName = errors.New("names are letters, digits and underscores, not starting with a digit")

func checkName(n *node, name string) error {
	if name == "" {
		return fmt.Errorf("%s: no name", n.what())
	}
	for i, c := range name {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return fmt.Errorf("%s: name %q: %w", n.what(), name, errName)
		}
	}
	return nil
}
