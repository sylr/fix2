// Package dictionary parses QuickFIX XML data dictionaries and provides
// FIX message validation against them.
package dictionary

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

// ---------------------------------------------------------------------------
// XML unmarshalling types (internal)
// ---------------------------------------------------------------------------

type xmlDict struct {
	Type        string        `xml:"type,attr"`
	Major       string        `xml:"major,attr"`
	Minor       string        `xml:"minor,attr"`
	ServicePack string        `xml:"servicepack,attr"`
	Header      xmlBody       `xml:"header"`
	Trailer     xmlBody       `xml:"trailer"`
	Messages    xmlMessages   `xml:"messages"`
	Components  xmlComponents `xml:"components"`
	Fields      xmlFieldDefs  `xml:"fields"`
}

type xmlMessages struct {
	Messages []xmlMessage `xml:"message"`
}

type xmlComponents struct {
	Components []xmlComponent `xml:"component"`
}

type xmlFieldDefs struct {
	Fields []xmlFieldDef `xml:"field"`
}

// xmlBody is the content of a header, trailer, message, component or group.
// Children are kept in a single slice so their XML order is preserved: a
// group's first child, possibly inside a component, is its delimiter.
type xmlBody struct {
	Items []xmlItem `xml:",any"`
}

// xmlItem is a <field>, <component> or <group> reference, told apart by
// XMLName. Only groups have children.
type xmlItem struct {
	XMLName  xml.Name
	Name     string `xml:"name,attr"`
	Required string `xml:"required,attr"`
	xmlBody
}

type xmlMessage struct {
	Name    string `xml:"name,attr"`
	MsgType string `xml:"msgtype,attr"`
	MsgCat  string `xml:"msgcat,attr"`
	xmlBody
}

type xmlComponent struct {
	Name string `xml:"name,attr"`
	xmlBody
}

type xmlFieldDef struct {
	Number int            `xml:"number,attr"`
	Name   string         `xml:"name,attr"`
	Type   string         `xml:"type,attr"`
	Values []xmlEnumValue `xml:"value"`
}

type xmlEnumValue struct {
	Enum        string `xml:"enum,attr"`
	Description string `xml:"description,attr"`
}

// ---------------------------------------------------------------------------
// Exported types
// ---------------------------------------------------------------------------

// FieldDef describes a field from the <fields> section of a data dictionary.
type FieldDef struct {
	Tag   int
	Name  string
	Type  string
	Enums map[string]string // value -> description; nil if none
}

// FieldRef is a reference to a field within a message, header, or trailer.
type FieldRef struct {
	Tag      int
	Required bool
}

// GroupDef describes a repeating group within a message.
type GroupDef struct {
	CounterTag int    // NUMINGROUP tag (e.g. 78)
	Name       string // field name (e.g. "NoAllocs")
	Required   bool
	DelimTag   int        // first child tag in XML order — instance delimiter (e.g. 79)
	Fields     []FieldRef // child fields in XML order, components expanded, nested groups excluded
	Groups     []GroupDef // nested sub-groups
}

// MessageDef describes a single FIX message type.
type MessageDef struct {
	Name    string
	MsgType string
	MsgCat  string
	Fields  []FieldRef // flattened (components/groups resolved)
	Groups  []GroupDef // top-level groups in this message

	fieldSet map[int]bool
	required map[int]bool
}

// Dict is a parsed QuickFIX XML data dictionary.
type Dict struct {
	Type, Major, Minor string

	Fields   map[int]*FieldDef
	Messages map[string]*MessageDef // keyed by MsgType (e.g. "D")
	Header   []FieldRef
	Trailer  []FieldRef

	headerSet  map[int]bool
	headerReq  map[int]bool
	trailerSet map[int]bool
	trailerReq map[int]bool

	fieldsByName map[string]*FieldDef
}

// FieldByName returns the field definition with the given name, or nil if not found.
func (d *Dict) FieldByName(name string) *FieldDef {
	return d.fieldsByName[name]
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// Load parses a QuickFIX XML dictionary from r.
func Load(r io.Reader) (*Dict, error) {
	var xd xmlDict
	if err := xml.NewDecoder(r).Decode(&xd); err != nil {
		return nil, fmt.Errorf("dictionary: %w", err)
	}
	return resolve(&xd)
}

// LoadFile parses a QuickFIX XML dictionary from the file at path.
func LoadFile(path string) (*Dict, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dictionary: %w", err)
	}
	defer f.Close()
	return Load(f)
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

func resolve(xd *xmlDict) (*Dict, error) {
	d := &Dict{
		Type:         xd.Type,
		Major:        xd.Major,
		Minor:        xd.Minor,
		Fields:       make(map[int]*FieldDef, len(xd.Fields.Fields)),
		Messages:     make(map[string]*MessageDef, len(xd.Messages.Messages)),
		fieldsByName: make(map[string]*FieldDef, len(xd.Fields.Fields)),
	}

	// 1. Build Fields map and fieldsByName index.
	for _, xf := range xd.Fields.Fields {
		fd := &FieldDef{
			Tag:  xf.Number,
			Name: xf.Name,
			Type: xf.Type,
		}
		if len(xf.Values) > 0 {
			fd.Enums = make(map[string]string, len(xf.Values))
			for _, v := range xf.Values {
				fd.Enums[v.Enum] = v.Description
			}
		}
		d.Fields[xf.Number] = fd
		d.fieldsByName[xf.Name] = fd
	}

	// 2. Build component map.
	compMap := make(map[string]*xmlComponent, len(xd.Components.Components))
	for i := range xd.Components.Components {
		c := &xd.Components.Components[i]
		compMap[c.Name] = c
	}

	r := &resolver{byName: d.fieldsByName, compMap: compMap, visiting: map[string]bool{}}

	// 3. Flatten header.
	header, err := r.resolve(xd.Header.Items)
	if err != nil {
		return nil, fmt.Errorf("dictionary: header: %w", err)
	}
	d.Header = header.all
	d.headerSet, d.headerReq = buildMaps(d.Header)

	// 4. Flatten trailer.
	trailer, err := r.resolve(xd.Trailer.Items)
	if err != nil {
		return nil, fmt.Errorf("dictionary: trailer: %w", err)
	}
	d.Trailer = trailer.all
	d.trailerSet, d.trailerReq = buildMaps(d.Trailer)

	// 5. Flatten each message and resolve its groups.
	for _, xm := range xd.Messages.Messages {
		body, err := r.resolve(xm.Items)
		if err != nil {
			return nil, fmt.Errorf("dictionary: message %s: %w", xm.Name, err)
		}
		fs, req := buildMaps(body.all)
		d.Messages[xm.MsgType] = &MessageDef{
			Name:     xm.Name,
			MsgType:  xm.MsgType,
			MsgCat:   xm.MsgCat,
			Fields:   body.all,
			Groups:   body.groups,
			fieldSet: fs,
			required: req,
		}
	}

	return d, nil
}

// resolver expands field, component and group references into FieldRefs and
// GroupDefs. It walks items in XML order and expands components in place, so
// fields and groups keep their position at any depth.
type resolver struct {
	byName   map[string]*FieldDef
	compMap  map[string]*xmlComponent
	visiting map[string]bool // components on the current path, for cycle detection
	visits   int             // fields and groups visited so far, bounded by maxVisits
}

// maxVisits bounds the fields and groups the resolver visits for a whole
// dictionary. Components are expanded in place, so a small acyclic dictionary
// where each component references the previous one twice expands to 2^n
// fields; cycle detection does not catch that. The full FIX 5.0 SP2
// dictionary needs about 40,000 visits.
const maxVisits = 1_000_000

// resolvedBody is the resolved content of a header, trailer, message or group.
type resolvedBody struct {
	all    []FieldRef // every tag the body can hold, group contents included
	direct []FieldRef // fields outside any group, components expanded
	groups []GroupDef // groups at this level, components expanded
	first  int        // first tag in XML order (field or group counter); 0 if none
}

func (b *resolvedBody) setFirst(tag int) {
	if b.first == 0 {
		b.first = tag
	}
}

func (r *resolver) resolve(items []xmlItem) (resolvedBody, error) {
	var b resolvedBody
	err := r.walk(items, &b)
	return b, err
}

func (r *resolver) walk(items []xmlItem, b *resolvedBody) error {
	for _, it := range items {
		if it.XMLName.Local != "component" {
			if r.visits++; r.visits > maxVisits {
				return fmt.Errorf("expansion exceeds %d fields: components are referenced too many times", maxVisits)
			}
		}
		switch it.XMLName.Local {
		case "field":
			fd, ok := r.byName[it.Name]
			if !ok {
				return fmt.Errorf("unknown field %q", it.Name)
			}
			ref := FieldRef{Tag: fd.Tag, Required: it.Required == "Y"}
			b.all = append(b.all, ref)
			b.direct = append(b.direct, ref)
			b.setFirst(fd.Tag)

		case "component":
			comp, ok := r.compMap[it.Name]
			if !ok {
				return fmt.Errorf("unknown component %q", it.Name)
			}
			if r.visiting[it.Name] {
				return fmt.Errorf("cycle detected in component %q", it.Name)
			}
			r.visiting[it.Name] = true
			err := r.walk(comp.Items, b)
			delete(r.visiting, it.Name)
			if err != nil {
				return fmt.Errorf("component %s: %w", it.Name, err)
			}

		case "group":
			// The group name is also a field name (the counter tag).
			fd, ok := r.byName[it.Name]
			if !ok {
				return fmt.Errorf("unknown group field %q", it.Name)
			}
			inner, err := r.resolve(it.Items)
			if err != nil {
				return fmt.Errorf("group %s: %w", it.Name, err)
			}
			if inner.first == 0 {
				return fmt.Errorf("group %s: no child fields", it.Name)
			}
			b.all = append(b.all, FieldRef{Tag: fd.Tag, Required: it.Required == "Y"})
			// Group children are never required at this level since group
			// instance validation is out of scope.
			for _, ref := range inner.all {
				b.all = append(b.all, FieldRef{Tag: ref.Tag})
			}
			b.groups = append(b.groups, GroupDef{
				CounterTag: fd.Tag,
				Name:       it.Name,
				Required:   it.Required == "Y",
				DelimTag:   inner.first,
				Fields:     inner.direct,
				Groups:     inner.groups,
			})
			b.setFirst(fd.Tag)

		default:
			return fmt.Errorf("unexpected element <%s>", it.XMLName.Local)
		}
	}
	return nil
}

// buildMaps precomputes allowed and required sets from a list of FieldRefs.
func buildMaps(refs []FieldRef) (fieldSet, required map[int]bool) {
	fieldSet = make(map[int]bool, len(refs))
	required = make(map[int]bool)
	for _, r := range refs {
		fieldSet[r.Tag] = true
		if r.Required {
			required[r.Tag] = true
		}
	}
	return
}
