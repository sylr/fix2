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
	Type        string         `xml:"type,attr"`
	Major       string         `xml:"major,attr"`
	Minor       string         `xml:"minor,attr"`
	ServicePack string         `xml:"servicepack,attr"`
	Header      xmlMsgBody     `xml:"header"`
	Trailer     xmlMsgBody     `xml:"trailer"`
	Messages    xmlMessages    `xml:"messages"`
	Components  xmlComponents  `xml:"components"`
	Fields      xmlFieldDefs   `xml:"fields"`
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

type xmlMsgBody struct {
	Fields     []xmlFieldRef     `xml:"field"`
	Components []xmlComponentRef `xml:"component"`
	Groups     []xmlGroup        `xml:"group"`
}

type xmlMessage struct {
	Name    string `xml:"name,attr"`
	MsgType string `xml:"msgtype,attr"`
	MsgCat  string `xml:"msgcat,attr"`
	xmlMsgBody
}

type xmlComponent struct {
	Name string `xml:"name,attr"`
	xmlMsgBody
}

type xmlFieldRef struct {
	Name     string `xml:"name,attr"`
	Required string `xml:"required,attr"`
}

type xmlComponentRef struct {
	Name     string `xml:"name,attr"`
	Required string `xml:"required,attr"`
}

type xmlGroup struct {
	Name       string            `xml:"name,attr"`
	Required   string            `xml:"required,attr"`
	Fields     []xmlFieldRef     `xml:"field"`
	Components []xmlComponentRef `xml:"component"`
	Groups     []xmlGroup        `xml:"group"`
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
	CounterTag int        // NUMINGROUP tag (e.g. 78)
	Name       string     // field name (e.g. "NoAllocs")
	Required   bool
	DelimTag   int        // first child tag — instance delimiter (e.g. 79)
	Fields     []FieldRef // child fields in order (delimiter is Fields[0])
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

	// 3. Flatten header.
	var err error
	d.Header, err = flatten(xd.Header.Fields, xd.Header.Components, xd.Header.Groups, d.fieldsByName, compMap)
	if err != nil {
		return nil, fmt.Errorf("dictionary: header: %w", err)
	}
	d.headerSet, d.headerReq = buildMaps(d.Header)

	// 4. Flatten trailer.
	d.Trailer, err = flatten(xd.Trailer.Fields, xd.Trailer.Components, xd.Trailer.Groups, d.fieldsByName, compMap)
	if err != nil {
		return nil, fmt.Errorf("dictionary: trailer: %w", err)
	}
	d.trailerSet, d.trailerReq = buildMaps(d.Trailer)

	// 5. Flatten each message and resolve groups.
	for _, xm := range xd.Messages.Messages {
		refs, err := flatten(xm.Fields, xm.Components, xm.Groups, d.fieldsByName, compMap)
		if err != nil {
			return nil, fmt.Errorf("dictionary: message %s: %w", xm.Name, err)
		}
		fs, req := buildMaps(refs)
		groups, err := resolveGroups(xm.Groups, d.fieldsByName, compMap)
		if err != nil {
			return nil, fmt.Errorf("dictionary: message %s: groups: %w", xm.Name, err)
		}
		// Also resolve groups from message-level components.
		for _, c := range xm.Components {
			comp, ok := compMap[c.Name]
			if !ok {
				continue // already caught by flatten
			}
			cg, err := resolveGroups(comp.Groups, d.fieldsByName, compMap)
			if err != nil {
				return nil, fmt.Errorf("dictionary: message %s: component %s: groups: %w", xm.Name, c.Name, err)
			}
			groups = append(groups, cg...)
		}
		d.Messages[xm.MsgType] = &MessageDef{
			Name:     xm.Name,
			MsgType:  xm.MsgType,
			MsgCat:   xm.MsgCat,
			Fields:   refs,
			Groups:   groups,
			fieldSet: fs,
			required: req,
		}
	}

	return d, nil
}

// flatten resolves field refs, component refs, and groups into a flat list of FieldRefs.
func flatten(
	fields []xmlFieldRef,
	components []xmlComponentRef,
	groups []xmlGroup,
	byName map[string]*FieldDef,
	compMap map[string]*xmlComponent,
) ([]FieldRef, error) {
	return flattenInner(fields, components, groups, byName, compMap, make(map[string]bool))
}

func flattenInner(
	fields []xmlFieldRef,
	components []xmlComponentRef,
	groups []xmlGroup,
	byName map[string]*FieldDef,
	compMap map[string]*xmlComponent,
	visiting map[string]bool,
) ([]FieldRef, error) {
	var refs []FieldRef

	// Process <field> entries.
	for _, f := range fields {
		fd, ok := byName[f.Name]
		if !ok {
			return nil, fmt.Errorf("unknown field %q", f.Name)
		}
		refs = append(refs, FieldRef{Tag: fd.Tag, Required: f.Required == "Y"})
	}

	// Process <component> entries.
	for _, c := range components {
		comp, ok := compMap[c.Name]
		if !ok {
			return nil, fmt.Errorf("unknown component %q", c.Name)
		}
		if visiting[c.Name] {
			return nil, fmt.Errorf("cycle detected in component %q", c.Name)
		}
		visiting[c.Name] = true
		inner, err := flattenInner(comp.Fields, comp.Components, comp.Groups, byName, compMap, visiting)
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", c.Name, err)
		}
		delete(visiting, c.Name)
		refs = append(refs, inner...)
	}

	// Process <group> entries.
	for _, g := range groups {
		// The group name is also a field name (the counter tag).
		fd, ok := byName[g.Name]
		if !ok {
			return nil, fmt.Errorf("unknown group field %q", g.Name)
		}
		refs = append(refs, FieldRef{Tag: fd.Tag, Required: g.Required == "Y"})

		// Recurse into group children; all children are marked not required
		// since group instance validation is out of scope.
		inner, err := flattenInner(g.Fields, g.Components, g.Groups, byName, compMap, visiting)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", g.Name, err)
		}
		for i := range inner {
			inner[i].Required = false
		}
		refs = append(refs, inner...)
	}

	return refs, nil
}

// resolveGroups converts XML groups into structured GroupDefs, recursively
// handling nested groups and expanding component refs within groups.
func resolveGroups(
	groups []xmlGroup,
	byName map[string]*FieldDef,
	compMap map[string]*xmlComponent,
) ([]GroupDef, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	var out []GroupDef
	for _, g := range groups {
		fd, ok := byName[g.Name]
		if !ok {
			return nil, fmt.Errorf("unknown group field %q", g.Name)
		}

		// Resolve child fields: direct fields + component-expanded fields.
		childRefs, err := flattenInner(g.Fields, g.Components, nil, byName, compMap, make(map[string]bool))
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", g.Name, err)
		}

		if len(childRefs) == 0 {
			return nil, fmt.Errorf("group %s: no child fields", g.Name)
		}

		// Recursively resolve nested groups.
		nested, err := resolveGroups(g.Groups, byName, compMap)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", g.Name, err)
		}

		out = append(out, GroupDef{
			CounterTag: fd.Tag,
			Name:       g.Name,
			Required:   g.Required == "Y",
			DelimTag:   childRefs[0].Tag,
			Fields:     childRefs,
			Groups:     nested,
		})
	}
	return out, nil
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
